package tempo

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"tracetest/internal/model"
)

type tempoRoundTrip func(*http.Request) (*http.Response, error)

func (r tempoRoundTrip) RoundTrip(request *http.Request) (*http.Response, error) { return r(request) }

func TestNewValidatesEndpointHeadersAndTLSOptions(t *testing.T) {
	for _, endpoint := range []string{"", "://bad", "/relative", "ftp://tempo.example", "https:///missing-host"} {
		if _, err := New(Config{Endpoint: endpoint}); err == nil {
			t.Errorf("endpoint %q was accepted", endpoint)
		}
	}
	custom := &http.Client{Timeout: time.Second}
	store, err := New(Config{Endpoint: "https://tempo.example/base", Headers: map[string]string{"X-Test": "yes"}, BearerToken: "secret", HTTPClient: custom})
	if err != nil || store.client != custom || store.headers.Get("X-Test") != "yes" || store.headers.Get("Authorization") != "Bearer secret" {
		t.Fatalf("custom store=%#v err=%v", store, err)
	}
	store, err = New(Config{Endpoint: "https://tempo.example", TLSSkipVerify: true})
	if err != nil {
		t.Fatal(err)
	}
	transport, ok := store.client.Transport.(*http.Transport)
	if !ok || transport.TLSClientConfig == nil || !transport.TLSClientConfig.InsecureSkipVerify {
		t.Fatalf("TLS options not applied: %#v", store.client.Transport)
	}
}

func TestTestConnectionClassifiesSuccessStatusAndTransport(t *testing.T) {
	for _, test := range []struct {
		name    string
		status  int
		body    string
		wantErr bool
	}{
		{name: "success", status: http.StatusNoContent},
		{name: "failure", status: http.StatusServiceUnavailable, body: "busy", wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, err := New(Config{Endpoint: "http://tempo.example", HTTPClient: &http.Client{Transport: tempoRoundTrip(func(request *http.Request) (*http.Response, error) {
				if request.URL.Path != "/ready" {
					t.Fatalf("path = %q", request.URL.Path)
				}
				return &http.Response{StatusCode: test.status, Body: io.NopCloser(strings.NewReader(test.body)), Header: make(http.Header)}, nil
			})}})
			if err != nil {
				t.Fatal(err)
			}
			err = store.TestConnection(context.Background())
			if (err != nil) != test.wantErr {
				t.Fatalf("TestConnection err=%v, wantErr=%t", err, test.wantErr)
			}
		})
	}
	store, err := New(Config{Endpoint: "http://tempo.example", HTTPClient: &http.Client{Transport: tempoRoundTrip(func(*http.Request) (*http.Response, error) { return nil, context.Canceled })}})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.TestConnection(context.Background()); err == nil || IsRetriable(err) {
		t.Fatalf("transport cancellation err=%v", err)
	}
}

func TestStatusAndTransportClassification(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusRequestTimeout, http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadRequest} {
		response := &http.Response{StatusCode: status, Header: http.Header{"Retry-After": {"3"}}, Body: io.NopCloser(strings.NewReader("detail"))}
		err := classifyStatus(response)
		var observationError *model.ObservationError
		if !errors.As(err, &observationError) || IsRetriable(err) != (status == 404 || status == 408 || status == 429 || status >= 500) {
			t.Fatalf("status %d error=%v", status, err)
		}
		if observationError.RetryAfter != 0 && observationError.RetryAfter != 3*time.Second {
			t.Fatalf("status %d retry-after=%v", status, observationError.RetryAfter)
		}
	}
	if classifyTransport(nil) != nil || !IsRetriable(classifyTransport(errors.New("network"))) {
		t.Fatal("generic transport classification incorrect")
	}
	dns := &net.DNSError{Err: "not found", IsNotFound: true}
	if IsRetriable(classifyTransport(dns)) {
		t.Fatal("DNS not-found was retriable")
	}
	certificate := &url.Error{Op: "Get", URL: "https://tempo", Err: errors.New("certificate verify failed")}
	if IsRetriable(classifyTransport(certificate)) {
		t.Fatal("certificate error was retriable")
	}
	if IsRetriable(classifyTransport(context.Canceled)) {
		t.Fatal("cancellation was retriable")
	}
	if parseRetryAfter(" 4 ") != 4*time.Second || parseRetryAfter("0") != 0 || parseRetryAfter("bad") != 0 || parseRetryAfter("-1") != 0 {
		t.Fatal("retry-after parsing incorrect")
	}
}

func TestTraceIDValidation(t *testing.T) {
	for _, value := range []model.TraceID{"", "short", model.TraceID(strings.Repeat("0", 32)), model.TraceID(strings.Repeat("g", 32)), "0123456789abcdef0123456789abcdeF"} {
		if validTraceID(value) {
			t.Errorf("validTraceID(%q) = true", value)
		}
	}
	if !validTraceID("0123456789abcdef0123456789abcdef") {
		t.Fatal("valid trace ID rejected")
	}
}

func TestDecodeTraceSupportsOTLPShapesAndCompleteness(t *testing.T) {
	body := []byte(`{"batches":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"checkout"}}]},"instrumentationLibrarySpans":[{"spans":[{"traceId":"` + traceID + `","spanId":"0123456789abcdef","parentSpanId":"fedcba9876543210","name":"request","kind":"SERVER","startTimeUnixNano":"100","endTimeUnixNano":"200","attributes":[{"key":"bool","value":{"boolValue":true}},{"key":"int","value":{"intValue":"7"}},{"key":"double","value":{"doubleValue":1.5}},{"key":"bytes","value":{"bytesValue":"AQ=="}}],"status":{"code":"ERROR","message":"failed"}}]}]}],"resourceSpans":[{"scopeSpans":[{"spans":[]}]}],"partial":true,"complete":false}`)
	observation, err := decodeTrace(body, traceID)
	if err != nil || !observation.Found || !observation.Valid || !observation.Partial || observation.Complete || len(observation.Spans) != 1 {
		t.Fatalf("observation=%#v err=%v", observation, err)
	}
	span := observation.Spans[0]
	if span.Kind != "server" || span.Status != "error" || span.StatusMessage != "failed" || span.Duration != 100 || span.Resource["service.name"] != "checkout" || span.Attributes["bool"] != true {
		t.Fatalf("span=%#v", span)
	}
	for _, body := range []string{`{"resourceSpans":[]}`, `{"resourceSpans":[],"complete":true}`} {
		if _, err := decodeTrace([]byte(body), traceID); err == nil {
			t.Fatalf("empty Tempo payload %s was accepted", body)
		}
	}
	if _, err := decodeTrace([]byte(`{"resourceSpans":[{"scopeSpans":[{"spans":[{"traceId":"ffffffffffffffffffffffffffffffff","startTimeUnixNano":"1","endTimeUnixNano":"2"}]}]}]}`), traceID); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("mismatched trace error=%v", err)
	}
	for _, body := range []string{"not-json", `{}`, `{"batches":null,"resourceSpans":null}`} {
		if _, err := decodeTrace([]byte(body), traceID); err == nil {
			t.Errorf("invalid payload %s accepted", body)
		}
	}
}

func TestNormalizeSpanAndAttributeHelpers(t *testing.T) {
	base := tempoSpan{StartTimeUnixNano: json.Number("10"), EndTimeUnixNano: json.Number("20"), Kind: json.Number("3"), Status: status{Code: json.Number("2")}}
	span, err := normalizeSpan(base, map[string]any{"service": "test"})
	if err != nil || span.Kind != "client" || span.Status != "error" || span.Duration != 10 {
		t.Fatalf("span=%#v err=%v", span, err)
	}
	for _, source := range []tempoSpan{{StartTimeUnixNano: json.Number("bad"), EndTimeUnixNano: json.Number("20")}, {StartTimeUnixNano: json.Number("20"), EndTimeUnixNano: json.Number("bad")}, {StartTimeUnixNano: json.Number("20"), EndTimeUnixNano: json.Number("10")}} {
		if _, err := normalizeSpan(source, nil); err == nil {
			t.Errorf("invalid span %#v accepted", source)
		}
	}
	if spanKind("SERVER") != "server" || spanKind(json.Number("2")) != "server" || spanKind(json.Number("3")) != "client" || spanKind(json.Number("9")) != "" || spanKind(1) != "" {
		t.Fatal("spanKind branches incorrect")
	}
	if spanStatus("ERROR") != "error" || spanStatus(json.Number("2")) != "error" || spanStatus(json.Number("1")) != "ok" || spanStatus(json.Number("9")) != "" || spanStatus(1) != "" {
		t.Fatal("spanStatus branches incorrect")
	}
	for _, value := range []any{"stringValue", true, 4, 1.5, []any{"bytes"}} {
		_ = attributeValue(value)
	}
	if got := attributeValue(map[string]any{"doubleValue": 1.5}); got != 1.5 {
		t.Fatalf("double attribute=%#v", got)
	}
	if got := attributeValue(map[string]any{"unknown": "value"}); got.(map[string]any)["unknown"] != "value" {
		t.Fatalf("unknown attribute=%#v", got)
	}
}
