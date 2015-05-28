

# package log

    import "/Users/harlow/code/go/src/github.com/go-kit/kit/log"

### Rationale

Package log provides basic interfaces for structured logging.

The fundamental interface is Logger. Loggers create log events from
key/value data.



### Examples


#### NewJSONLogger

Code:
```go
&{17007 [0xc2080dce00 0xc2080d5500 0xc2080dcec0 0xc2080dcf00] 17227}
```

Output:
```
{&#34;answer&#34;:42,&#34;question&#34;:&#34;what is the meaning of life?&#34;}

```


#### NewPrefixLogger

Code:
```go
&{17336 [0xc2080dd020 0xc2080d57c0 0xc2080dd0e0 0xc2080dd120] 17550}
```

Output:
```
question=what is the meaning of life? answer=42

```



