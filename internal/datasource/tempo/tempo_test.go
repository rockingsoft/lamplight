package tempo

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"tracetest/internal/model"
)

const traceID = "0123456789abcdef0123456789abcdef"

func TestObserveNormalizesTempoTrace(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/tempo/api/traces/"+traceID || request.Header.Get("Authorization") != "Bearer secret" {
			t.Fatalf("request: %s auth=%q", request.URL, request.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"resourceSpans":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"checkout"}}]},"scopeSpans":[{"spans":[{"traceId":"` + traceID + `","spanId":"0123456789abcdef","name":"GET /checkout","kind":2,"startTimeUnixNano": "100", "endTimeUnixNano":"150","attributes":[{"key":"http.status_code","value":{"intValue":"200"}}],"status":{"code":1}}]}]}]}`))
	}))
	defer server.Close()
	store, err := New(Config{Endpoint: server.URL + "/tempo", BearerToken: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	observation, err := store.Observe(context.Background(), traceID)
	if err != nil || !observation.Found || !observation.Complete || len(observation.Spans) != 1 || observation.Spans[0].Duration != 50 || observation.Spans[0].Resource["service.name"] != "checkout" {
		t.Fatalf("observation=%#v err=%v", observation, err)
	}
}

func TestObserveNormalizesOTLPBase64IDsAndValues(t *testing.T) {
	traceBytes, _ := hex.DecodeString(traceID)
	encodedTrace := base64.StdEncoding.EncodeToString(traceBytes)
	spanBytes, _ := hex.DecodeString("0123456789abcdef")
	encodedSpan := base64.StdEncoding.EncodeToString(spanBytes)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		_, _ = fmt.Fprintf(w, `{"batches":[{"resource":{"attributes":[]},"scopeSpans":[{"spans":[{"traceId":%q,"spanId":%q,"name":"work","kind":"SPAN_KIND_SERVER","startTimeUnixNano":"100","endTimeUnixNano":"150","attributes":[{"key":"code","value":{"intValue":"200"}}],"status":{"code":"STATUS_CODE_OK"}}]}]}]}`, encodedTrace, encodedSpan)
	}))
	defer server.Close()
	store, _ := New(Config{Endpoint: server.URL})
	observation, err := store.Observe(context.Background(), traceID)
	if err != nil || len(observation.Spans) != 1 {
		t.Fatalf("observation=%#v err=%v", observation, err)
	}
	span := observation.Spans[0]
	if span.TraceID != traceID || span.SpanID != "0123456789abcdef" || span.Kind != "server" || span.Status != "ok" || span.Attributes["code"] != int64(200) {
		t.Fatalf("unexpected normalized span: %#v", span)
	}
}

func TestObserveClassifiesStatuses(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) { w.WriteHeader(http.StatusTooManyRequests) }))
	defer server.Close()
	store, _ := New(Config{Endpoint: server.URL})
	_, err := store.Observe(context.Background(), traceID)
	var observationError *model.ObservationError
	if !errors.As(err, &observationError) || !IsRetriable(err) {
		t.Fatalf("error=%v", err)
	}
}

func TestObserveRejectsSchemaAndBadTraceID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) { _, _ = w.Write([]byte(`{}`)) }))
	defer server.Close()
	store, _ := New(Config{Endpoint: server.URL})
	_, err := store.Observe(context.Background(), "bad")
	var observationError *model.ObservationError
	if !errors.As(err, &observationError) || observationError.Retriable {
		t.Fatalf("error=%v", err)
	}
}
