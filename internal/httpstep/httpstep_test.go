package httpstep

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"tracetest/internal/model"
)

type roundTrip func(*http.Request) (*http.Response, error)

func (r roundTrip) RoundTrip(request *http.Request) (*http.Response, error) { return r(request) }

func TestExecuteNormalizesResponsesAndPropagation(t *testing.T) {
	executor := New(roundTrip(func(request *http.Request) (*http.Response, error) {
		if request.Header.Get("traceparent") != "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01" {
			t.Fatalf("traceparent %q", request.Header.Get("traceparent"))
		}
		if request.Header.Get("tracestate") != "vendor=value" {
			t.Fatal("missing tracestate")
		}
		return &http.Response{StatusCode: 503, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"ok":false}`))}, nil
	}))
	trace := model.TestTraceContext{TraceID: "0123456789abcdef0123456789abcdef", SpanID: "0123456789abcdef", TraceState: "vendor=value"}
	result, err := executor.Execute(context.Background(), model.HTTPRequest{Method: "GET", URL: "https://example.test", Headers: map[string]string{"TraceParent": "attacker", "Tracestate": "attacker"}}, model.DefaultHTTPClientConfig(), &trace)
	if err != nil || result.StatusCode != 503 || result.JSON.(map[string]any)["ok"] != false {
		t.Fatalf("result=%#v err=%v", result, err)
	}
}

func TestExecuteRejectsLimitsAndBinary(t *testing.T) {
	executor := New(roundTrip(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/octet-stream"}}, Body: io.NopCloser(strings.NewReader("x"))}, nil
	}))
	config := model.DefaultHTTPClientConfig()
	if _, err := executor.Execute(context.Background(), model.HTTPRequest{Method: "GET", URL: "/relative"}, config, nil); err == nil {
		t.Fatal("relative URL accepted")
	}
	if _, err := executor.Execute(context.Background(), model.HTTPRequest{Method: "POST", URL: "https://example.test", Body: "xx"}, model.HTTPClientConfig{MaxRequestBodyBytes: 1}, nil); err == nil {
		t.Fatal("request limit accepted")
	}
	if _, err := executor.Execute(context.Background(), model.HTTPRequest{Method: "GET", URL: "https://example.test"}, config, nil); err == nil {
		t.Fatal("binary response accepted")
	}
	if _, err := executor.Execute(context.Background(), model.HTTPRequest{Method: "GET", URL: "https://example.test"}, model.HTTPClientConfig{Proxy: "not-a-proxy"}, nil); err == nil {
		t.Fatal("invalid proxy accepted")
	}
}
