package executorproto

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

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

func TestConcurrentRequestsCompleteOutOfOrder(t *testing.T) {
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/first" {
			close(firstStarted)
			<-releaseFirst
		}
		_, _ = fmt.Fprint(w, r.URL.Path)
	}))
	defer server.Close()
	client, stop := protocolPair(t)
	defer stop()
	var wg sync.WaitGroup
	wg.Add(1)
	firstDone := make(chan error, 1)
	go func() {
		defer wg.Done()
		_, err := client.Execute(context.Background(), model.HTTPRequest{Method: "GET", URL: server.URL + "/first"}, model.DefaultHTTPClientConfig(), nil)
		firstDone <- err
	}()
	select {
	case <-firstStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("first request did not start")
	}
	secondCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	second, err := client.Execute(secondCtx, model.HTTPRequest{Method: "GET", URL: server.URL + "/second"}, model.DefaultHTTPClientConfig(), nil)
	close(releaseFirst)
	wg.Wait()
	if err != nil || second.Body != "/second" {
		t.Fatalf("second request did not overtake first: response=%#v err=%v", second, err)
	}
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
}

func TestCancellationReachesRemoteRequest(t *testing.T) {
	started := make(chan struct{})
	cancelled := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
		close(cancelled)
	}))
	defer server.Close()
	client, stop := protocolPair(t)
	defer stop()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := client.Execute(ctx, model.HTTPRequest{Method: "GET", URL: server.URL}, model.DefaultHTTPClientConfig(), nil)
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("remote request did not start")
	}
	cancel()
	select {
	case err := <-done:
		if err != context.Canceled {
			t.Fatalf("client error = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("client cancellation did not return")
	}
	select {
	case <-cancelled:
	case <-time.After(3 * time.Second):
		t.Fatal("remote request was not cancelled")
	}
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
