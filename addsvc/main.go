package main

import (
	"flag"
	"fmt"
	"io/ioutil"
	stdlog "log"
	"math/rand"
	"net"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/apache/thrift/lib/go/thrift"
	stdprometheus "github.com/prometheus/client_golang/prometheus"
	"github.com/streadway/handy/cors"
	"github.com/streadway/handy/encoding"
	"golang.org/x/net/context"
	"google.golang.org/grpc"

	thriftadd "github.com/go-kit/kit/addsvc/_thrift/gen-go/add"
	"github.com/go-kit/kit/addsvc/pb"
	"github.com/go-kit/kit/endpoint"
	kitlog "github.com/go-kit/kit/log"
	"github.com/go-kit/kit/metrics"
	"github.com/go-kit/kit/metrics/expvar"
	"github.com/go-kit/kit/metrics/prometheus"
	"github.com/go-kit/kit/metrics/statsd"
	"github.com/go-kit/kit/tracing/zipkin"
	jsoncodec "github.com/go-kit/kit/transport/codec/json"
	httptransport "github.com/go-kit/kit/transport/http"
)

func main() {
	// Flag domain. Note that gRPC transitively registers flags via its import
	// of glog. So, we define a new flag set, to keep those domains distinct.
	fs := flag.NewFlagSet("", flag.ExitOnError)
	var (
		debugAddr        = fs.String("debug.addr", ":8000", "Address for HTTP debug/instrumentation server")
		httpAddr         = fs.String("http.addr", ":8001", "Address for HTTP (JSON) server")
		grpcAddr         = fs.String("grpc.addr", ":8002", "Address for gRPC server")
		thriftAddr       = fs.String("thrift.addr", ":8003", "Address for Thrift server")
		thriftProtocol   = fs.String("thrift.protocol", "binary", "binary, compact, json, simplejson")
		thriftBufferSize = fs.Int("thrift.buffer.size", 0, "0 for unbuffered")
		thriftFramed     = fs.Bool("thrift.framed", false, "true to enable framing")

		proxyHTTPAddr = fs.String("proxy.http.url", "", "if set, proxy requests over HTTP to this addsvc")

		zipkinServiceName            = fs.String("zipkin.service.name", "addsvc", "Zipkin service name")
		zipkinCollectorAddr          = fs.String("zipkin.collector.addr", "", "Zipkin Scribe collector address (empty will log spans)")
		zipkinCollectorTimeout       = fs.Duration("zipkin.collector.timeout", time.Second, "Zipkin collector timeout")
		zipkinCollectorBatchSize     = fs.Int("zipkin.collector.batch.size", 100, "Zipkin collector batch size")
		zipkinCollectorBatchInterval = fs.Duration("zipkin.collector.batch.interval", time.Second, "Zipkin collector batch interval")
	)
	flag.Usage = fs.Usage // only show our flags
	fs.Parse(os.Args[1:])

	// `package log` domain
	var logger kitlog.Logger
	logger = kitlog.NewPrefixLogger(os.Stderr)
	logger = kitlog.With(logger, "ts", kitlog.DefaultTimestampUTC, "caller", kitlog.DefaultCaller)
	stdlog.SetOutput(kitlog.NewStdlibAdapter(logger)) // redirect stdlib logging to us
	stdlog.SetFlags(0)                                // flags are handled in our logger

	// `package metrics` domain
	requests := metrics.NewMultiCounter(
		expvar.NewCounter("requests"),
		statsd.NewCounter(ioutil.Discard, "requests_total", time.Second),
		prometheus.NewCounter(stdprometheus.CounterOpts{
			Namespace: "addsvc",
			Subsystem: "add",
			Name:      "requests_total",
			Help:      "Total number of received requests.",
		}, []string{}),
	)
	duration := metrics.NewMultiHistogram(
		expvar.NewHistogram("duration_nanoseconds_total", 0, 100000000, 3),
		statsd.NewHistogram(ioutil.Discard, "duration_nanoseconds_total", time.Second),
		prometheus.NewSummary(stdprometheus.SummaryOpts{
			Namespace: "addsvc",
			Subsystem: "add",
			Name:      "duration_nanoseconds_total",
			Help:      "Total nanoseconds spend serving requests.",
		}, []string{}),
	)

	// `package tracing` domain
	zipkinHostPort := "localhost:1234" // TODO Zipkin makes overly simple assumptions about services
	var zipkinCollector zipkin.Collector = loggingCollector{logger}
	if *zipkinCollectorAddr != "" {
		var err error
		if zipkinCollector, err = zipkin.NewScribeCollector(
			*zipkinCollectorAddr,
			*zipkinCollectorTimeout,
			*zipkinCollectorBatchSize,
			*zipkinCollectorBatchInterval,
		); err != nil {
			logger.Log("err", err)
			os.Exit(1)
		}
	}
	zipkinMethodName := "add"
	zipkinSpanFunc := zipkin.MakeNewSpanFunc(zipkinHostPort, *zipkinServiceName, zipkinMethodName)
	zipkin.Log.Swap(logger) // log diagnostic/error details

	// Our business and operational domain
	var a Add = pureAdd
	if *proxyHTTPAddr != "" {
		codec := jsoncodec.New()
		makeResponse := func() interface{} { return &addResponse{} }

		var e endpoint.Endpoint
		e = httptransport.NewClient(*proxyHTTPAddr, codec, makeResponse, httptransport.ClientBefore(zipkin.ToRequest(zipkinSpanFunc)))
		e = zipkin.AnnotateClient(zipkinSpanFunc, zipkinCollector)(e)

		a = proxyAdd(e, logger)
	}
	a = logging(logger)(a)

	// Server domain
	var e endpoint.Endpoint
	e = makeEndpoint(a)
	e = zipkin.AnnotateServer(zipkinSpanFunc, zipkinCollector)(e)

	// Mechanical stuff
	rand.Seed(time.Now().UnixNano())
	root := context.Background()
	errc := make(chan error)

	go func() {
		errc <- interrupt()
	}()

	// Transport: HTTP (debug/instrumentation)
	go func() {
		logger.Log("addr", *debugAddr, "transport", "debug")
		errc <- http.ListenAndServe(*debugAddr, nil)
	}()

	// Transport: HTTP (JSON)
	go func() {
		ctx, cancel := context.WithCancel(root)
		defer cancel()

		field := metrics.Field{Key: "transport", Value: "http"}
		before := httptransport.BindingBefore(zipkin.ToContext(zipkinSpanFunc))
		after := httptransport.BindingAfter(httptransport.SetContentType("application/json"))
		makeRequest := func() interface{} { return &addRequest{} }

		var handler http.Handler
		handler = httptransport.NewBinding(ctx, makeRequest, jsoncodec.New(), e, before, after)
		handler = encoding.Gzip(handler)
		handler = cors.Middleware(cors.Config{})(handler)
		handler = httpInstrument(requests.With(field), duration.With(field))(handler)

		mux := http.NewServeMux()
		mux.Handle("/add", handler)
		logger.Log("addr", *httpAddr, "transport", "HTTP")
		errc <- http.ListenAndServe(*httpAddr, mux)
	}()

	// Transport: gRPC
	go func() {
		ln, err := net.Listen("tcp", *grpcAddr)
		if err != nil {
			errc <- err
			return
		}
		s := grpc.NewServer() // uses its own context?
		field := metrics.Field{Key: "transport", Value: "grpc"}

		var addServer pb.AddServer
		addServer = grpcBinding{e}
		addServer = grpcInstrument(requests.With(field), duration.With(field))(addServer)

		pb.RegisterAddServer(s, addServer)
		logger.Log("addr", *grpcAddr, "transport", "gRPC")
		errc <- s.Serve(ln)
	}()

	// Transport: Thrift
	go func() {
		ctx, cancel := context.WithCancel(root)
		defer cancel()

		var protocolFactory thrift.TProtocolFactory
		switch *thriftProtocol {
		case "binary":
			protocolFactory = thrift.NewTBinaryProtocolFactoryDefault()
		case "compact":
			protocolFactory = thrift.NewTCompactProtocolFactory()
		case "json":
			protocolFactory = thrift.NewTJSONProtocolFactory()
		case "simplejson":
			protocolFactory = thrift.NewTSimpleJSONProtocolFactory()
		default:
			errc <- fmt.Errorf("invalid Thrift protocol %q", *thriftProtocol)
			return
		}

		var transportFactory thrift.TTransportFactory
		if *thriftBufferSize > 0 {
			transportFactory = thrift.NewTBufferedTransportFactory(*thriftBufferSize)
		} else {
			transportFactory = thrift.NewTTransportFactory()
		}

		if *thriftFramed {
			transportFactory = thrift.NewTFramedTransportFactory(transportFactory)
		}

		transport, err := thrift.NewTServerSocket(*thriftAddr)
		if err != nil {
			errc <- err
			return
		}

		field := metrics.Field{Key: "transport", Value: "thrift"}

		var service thriftadd.AddService
		service = thriftBinding{ctx, e}
		service = thriftInstrument(requests.With(field), duration.With(field))(service)

		logger.Log("addr", *thriftAddr, "transport", "Thrift")
		errc <- thrift.NewTSimpleServer4(
			thriftadd.NewAddServiceProcessor(service),
			transport,
			transportFactory,
			protocolFactory,
		).Serve()
	}()

	logger.Log("fatal", <-errc)
}

func interrupt() error {
	c := make(chan os.Signal)
	signal.Notify(c, syscall.SIGINT, syscall.SIGTERM)
	return fmt.Errorf("%s", <-c)
}

type loggingCollector struct{ kitlog.Logger }

func (c loggingCollector) Collect(s *zipkin.Span) error {
	annotations := s.Encode().GetAnnotations()
	values := make([]string, len(annotations))
	for i, a := range annotations {
		values[i] = a.Value
	}
	c.Logger.Log(
		"trace_id", s.TraceID(),
		"span_id", s.SpanID(),
		"parent_span_id", s.ParentSpanID(),
		"annotations", strings.Join(values, " "),
	)
	return nil
}
