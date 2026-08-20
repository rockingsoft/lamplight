package executorproto

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"lamplight/internal/model"
)

func TestClientAndServerExecuteHTTPWithoutProjectFiles(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"traceparent":%q}`, r.Header.Get("traceparent"))
	}))
	defer server.Close()
	client, stop := protocolPair(t)
	trace := &model.TestTraceContext{TraceID: "0123456789abcdef0123456789abcdef", SpanID: "0123456789abcdef", TraceFlags: 1}
	response, err := client.Execute(context.Background(), model.HTTPRequest{Method: "GET", URL: server.URL}, model.DefaultHTTPClientConfig(), trace)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || response.JSON == nil {
		t.Fatalf("response: %#v", response)
	}
	stop()
}

func TestClientAndServerExecuteTypedTrigger(t *testing.T) {
	client, stop := protocolPair(t)
	response, err := (TriggerClient{Client: client}).Execute(context.Background(), model.TriggerRequest{Kind: model.TriggerTraceID, Attributes: map[string]any{"id": "0123456789abcdef0123456789abcdef"}}, model.HTTPClientConfig{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if response.Body != "0123456789abcdef0123456789abcdef" {
		t.Fatalf("response: %#v", response)
	}
	stop()
}

func protocolPair(t *testing.T) (*Client, func()) {
	t.Helper()
	requestsReader, requestsWriter := io.Pipe()
	responsesReader, responsesWriter := io.Pipe()
	done := make(chan error, 1)
	go func() {
		err := Serve(context.Background(), requestsReader, responsesWriter)
		_ = responsesWriter.CloseWithError(err)
		done <- err
	}()
	return NewClient(requestsWriter, responsesReader, nil), func() {
		t.Helper()
		_ = requestsWriter.Close()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		_ = responsesReader.Close()
	}
}
