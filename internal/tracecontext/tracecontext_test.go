package tracecontext

import (
	"net/http"
	"strings"
	"testing"
)

func TestFactoryCreatesValidDistinctContexts(t *testing.T) {
	factory := NewFactory()
	first, err := factory.New()
	if err != nil {
		t.Fatal(err)
	}
	second, err := factory.New()
	if err != nil {
		t.Fatal(err)
	}
	if first.TraceID == second.TraceID || len(first.TraceID) != 32 || len(first.SpanID) != 16 {
		t.Fatalf("invalid contexts: %#v %#v", first, second)
	}
	parsed, err := ParseTraceParent(first.TraceParent())
	if err != nil || parsed.TraceID != first.TraceID || parsed.SpanID != first.SpanID {
		t.Fatalf("parse: %#v, %v", parsed, err)
	}
}

func TestInjectReplacesUserPropagation(t *testing.T) {
	headers := http.Header{"Traceparent": {"user"}, "Tracestate": {"user"}}
	factory := NewFactory()
	trace, _ := factory.New()
	Inject(headers, &trace)
	if got := headers.Get("traceparent"); got != trace.TraceParent() {
		t.Fatalf("traceparent = %q", got)
	}
	if headers.Get("tracestate") != "" || strings.Contains(headers.Get("traceparent"), "user") {
		t.Fatal("untrusted propagation survived")
	}
}
