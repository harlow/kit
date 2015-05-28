package log_test

import (
	"bytes"
	"fmt"

	"github.com/go-kit/kit/log"
)

func ExampleNewPrefixLogger() {
	var buf bytes.Buffer

	logger := log.NewPrefixLogger(&buf)
	logger.Log("question", "what is the meaning of life?", "answer", 42)

	fmt.Print(&buf)
	// Output:
	// question=what is the meaning of life? answer=42
}
