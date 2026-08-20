package httpstep

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"lamplight/internal/model"
)

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("read failed") }

func TestExecuteValidatesMethodURLBodyTransportAndResponseErrors(t *testing.T) {
	executor := New(roundTrip(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("network down")
	}))
	base := model.DefaultHTTPClientConfig()
	for _, request := range []model.HTTPRequest{{URL: "https://example.test"}, {Method: "GET", URL: "::bad"}, {Method: "GET", URL: "ftp://example.test"}} {
		if _, err := executor.Execute(context.Background(), request, base, nil); err == nil {
			t.Errorf("invalid request %q was accepted", request.URL)
		}
	}
	if _, err := executor.Execute(context.Background(), model.HTTPRequest{Method: "GET", URL: "https://example.test", Body: "x"}, model.HTTPClientConfig{MaxRequestBodyBytes: 0}, nil); err == nil {
		t.Fatal("zero request limit unexpectedly rejected defaults")
	}
	if _, err := executor.Execute(context.Background(), model.HTTPRequest{Method: "GET", URL: "https://example.test"}, base, nil); err == nil || !strings.Contains(err.Error(), "execute HTTP request") {
		t.Fatalf("transport error = %v", err)
	}
	if _, err := executor.Execute(context.Background(), model.HTTPRequest{Method: "GET", URL: "https://example.test"}, model.HTTPClientConfig{MaxResponseBodyBytes: 1}, nil); err == nil {
		t.Fatal("response error transport did not fail before body read")
	}
	if _, err := readBody(failingReader{}, 10); err == nil || !strings.Contains(err.Error(), "read HTTP response body") {
		t.Fatalf("read error = %v", err)
	}
}

func TestExecuteRedirectAndPartialDefaults(t *testing.T) {
	count := 0
	executor := New(roundTrip(func(request *http.Request) (*http.Response, error) {
		count++
		if count == 1 {
			return &http.Response{StatusCode: http.StatusFound, Header: http.Header{"Location": {"https://example.test/next"}, "Content-Type": {"text/plain"}}, Body: io.NopCloser(strings.NewReader("redirect")), Request: request}, nil
		}
		return &http.Response{StatusCode: http.StatusNoContent, Header: http.Header{"Content-Type": {"text/plain; charset=utf-8"}}, Body: io.NopCloser(strings.NewReader("")), Request: request}, nil
	}))
	request := model.HTTPRequest{Method: "GET", URL: "https://example.test/start"}
	config := model.DefaultHTTPClientConfig()
	config.FollowRedirects = false
	response, err := executor.Execute(context.Background(), request, config, nil)
	if err != nil || response.StatusCode != http.StatusFound || count != 1 {
		t.Fatalf("no redirect response=%#v err=%v count=%d", response, err, count)
	}
	count = 0
	response, err = executor.Execute(context.Background(), request, model.HTTPClientConfig{FollowRedirects: true, Timeout: 0, MaxResponseBodyBytes: 0, MaxRequestBodyBytes: 0}, nil)
	if err != nil || response.StatusCode != http.StatusNoContent || count != 2 {
		t.Fatalf("redirect response=%#v err=%v count=%d", response, err, count)
	}
}

func TestTransportOptionsAndDefaults(t *testing.T) {
	if got := defaults(model.HTTPClientConfig{}); got != model.DefaultHTTPClientConfig() {
		t.Fatalf("empty defaults = %#v", got)
	}
	partial := defaults(model.HTTPClientConfig{FollowRedirects: false, Proxy: "http://proxy.example"})
	if partial.Timeout == 0 || partial.MaxRequestBodyBytes == 0 || partial.MaxResponseBodyBytes == 0 || partial.FollowRedirects {
		t.Fatalf("partial defaults = %#v", partial)
	}
	transport, err := (&Executor{}).transport(model.HTTPClientConfig{Proxy: "http://proxy.example", TLSSkipVerify: true})
	if err != nil || transport == nil {
		t.Fatalf("transport = %v, err = %v", transport, err)
	}
	if _, err := (&Executor{}).transport(model.HTTPClientConfig{Proxy: "://bad"}); err == nil {
		t.Fatal("invalid proxy accepted")
	}
	custom := roundTrip(func(*http.Request) (*http.Response, error) { return nil, nil })
	if got, err := (&Executor{Transport: custom}).transport(model.HTTPClientConfig{}); err != nil || got == nil {
		t.Fatalf("custom transport = %v, err = %v", got, err)
	}
}

func TestNormalizeContentTypesAndJSONBranches(t *testing.T) {
	cases := []struct {
		name     string
		content  string
		body     []byte
		wantErr  string
		wantJSON bool
	}{
		{name: "text", content: "text/plain; charset=utf8", body: []byte("hello")},
		{name: "empty json", content: "application/json", body: []byte("  ")},
		{name: "json suffix", content: "application/problem+json", body: []byte(`{"error":true}`), wantJSON: true},
		{name: "bad media", content: "application/octet-stream", body: []byte("x"), wantErr: "unsupported binary"},
		{name: "bad charset", content: "text/plain; charset=latin1", body: []byte("x"), wantErr: "unsupported response charset"},
		{name: "bad json", content: "application/json", body: []byte("{"), wantErr: "invalid JSON"},
		{name: "bad content type", content: "text/plain; charset=\"", body: []byte("x"), wantErr: "invalid response content type"},
		{name: "invalid utf8", content: "text/plain", body: []byte{0xff}, wantErr: "unsupported binary response body"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			response, err := normalize(&http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {test.content}}}, test.body)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("err = %v, want %q", err, test.wantErr)
				}
				return
			}
			if err != nil || (response.JSON != nil) != test.wantJSON || response.Body != string(test.body) {
				t.Fatalf("response=%#v err=%v", response, err)
			}
		})
	}
	if body, err := readBody(strings.NewReader("abc"), 2); err == nil || body != nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("body limit result=%q err=%v", body, err)
	}
}
