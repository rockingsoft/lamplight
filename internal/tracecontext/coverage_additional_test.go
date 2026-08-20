package tracecontext

import (
	"net/http"
	"strings"
	"testing"

	"lamplight/internal/model"
)

func TestInjectNilAndTraceStateBranches(t *testing.T) {
	headers := http.Header{"traceparent": {"user"}, "tracestate": {"user"}}
	Inject(headers, nil)
	if headers.Get("traceparent") != "" || headers.Get("tracestate") != "" {
		t.Fatalf("nil injection left headers: %#v", headers)
	}
	trace := model.TestTraceContext{TraceID: "0123456789abcdef0123456789abcdef", SpanID: "0123456789abcdef", TraceFlags: 1, TraceState: "vendor=value"}
	Inject(headers, &trace)
	if headers.Get("tracestate") != trace.TraceState {
		t.Fatalf("tracestate = %q", headers.Get("tracestate"))
	}
}

func TestParseTraceParentRejectsInvalidForms(t *testing.T) {
	valid := "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01"
	for _, value := range []string{
		"", "01-0123456789abcdef0123456789abcdef-0123456789abcdef-01",
		"00-short-0123456789abcdef-01", "00-0123456789abcdef0123456789abcdef-short-01",
		"00-0123456789abcdef0123456789abcdef-0123456789abcdef-000",
		"00-0123456789abcdef0123456789abcdef-0123456789abcdeg-01",
		"00-00000000000000000000000000000000-0123456789abcdef-01",
		"00-0123456789abcdef0123456789abcdef-0000000000000000-01",
	} {
		if _, err := ParseTraceParent(value); err == nil {
			t.Errorf("ParseTraceParent(%q) accepted", value)
		}
	}
	parsed, err := ParseTraceParent(valid)
	if err != nil || parsed.TraceFlags != 1 {
		t.Fatalf("valid parse=%#v err=%v", parsed, err)
	}
	if !allZeroHex(strings.Repeat("0", 8)) || allZeroHex("00000001") {
		t.Fatal("allZeroHex branches incorrect")
	}
}
