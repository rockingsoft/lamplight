// Package httpstep executes normalized HTTP step requests.
package httpstep

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"unicode/utf8"

	"lamplight/internal/model"
	"lamplight/internal/tracecontext"
)

// Executor is an HTTP executor whose optional Transport makes unit tests and
// custom network stacks deterministic. A nil transport uses a configured
// standard library transport.
type Executor struct{ Transport http.RoundTripper }

func New(transport http.RoundTripper) *Executor { return &Executor{Transport: transport} }

func (e *Executor) Execute(ctx context.Context, request model.HTTPRequest, config model.HTTPClientConfig, trace *model.TestTraceContext) (model.Response, error) {
	config = defaults(config)
	method := strings.TrimSpace(request.Method)
	if method == "" {
		return model.Response{}, errors.New("HTTP method is required")
	}
	parsedURL, err := url.Parse(request.URL)
	if err != nil || !parsedURL.IsAbs() || (parsedURL.Scheme != "http" && parsedURL.Scheme != "https") || parsedURL.Host == "" {
		return model.Response{}, fmt.Errorf("HTTP URL must be absolute http or https: %q", request.URL)
	}
	if int64(len(request.Body)) > config.MaxRequestBodyBytes {
		return model.Response{}, fmt.Errorf("request body exceeds %d byte limit", config.MaxRequestBodyBytes)
	}
	httpRequest, err := http.NewRequestWithContext(ctx, method, parsedURL.String(), bytes.NewBufferString(request.Body))
	if err != nil {
		return model.Response{}, fmt.Errorf("build HTTP request: %w", err)
	}
	for key, value := range request.Headers {
		httpRequest.Header.Set(key, value)
	}
	tracecontext.Inject(httpRequest.Header, trace)

	transport, err := e.transport(config)
	if err != nil {
		return model.Response{}, err
	}
	client := &http.Client{Timeout: config.Timeout, Transport: transport}
	if !config.FollowRedirects {
		client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	}
	response, err := client.Do(httpRequest)
	if err != nil {
		return model.Response{}, fmt.Errorf("execute HTTP request: %w", err)
	}
	defer response.Body.Close()
	body, err := readBody(response.Body, config.MaxResponseBodyBytes)
	if err != nil {
		return model.Response{}, err
	}
	return normalize(response, body)
}

func defaults(config model.HTTPClientConfig) model.HTTPClientConfig {
	defaults := model.DefaultHTTPClientConfig()
	if config == (model.HTTPClientConfig{}) {
		return defaults
	}
	if config.Timeout == 0 {
		config.Timeout = defaults.Timeout
	}
	if config.MaxRequestBodyBytes == 0 {
		config.MaxRequestBodyBytes = defaults.MaxRequestBodyBytes
	}
	if config.MaxResponseBodyBytes == 0 {
		config.MaxResponseBodyBytes = defaults.MaxResponseBodyBytes
	}
	return config
}

func (e *Executor) transport(config model.HTTPClientConfig) (http.RoundTripper, error) {
	if e != nil && e.Transport != nil {
		return e.Transport, nil
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if config.Proxy != "" {
		proxyURL, err := url.Parse(config.Proxy)
		if err != nil || !proxyURL.IsAbs() || proxyURL.Host == "" {
			return nil, fmt.Errorf("HTTP proxy must be absolute: %q", config.Proxy)
		}
		transport.Proxy = http.ProxyURL(proxyURL)
	}
	if config.TLSSkipVerify {
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	} // #nosec G402 -- explicit DSL opt-in.
	return transport, nil
}

func readBody(body io.Reader, limit int64) ([]byte, error) {
	limited := io.LimitReader(body, limit+1)
	contents, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("read HTTP response body: %w", err)
	}
	if int64(len(contents)) > limit {
		return nil, fmt.Errorf("response body exceeds %d byte limit", limit)
	}
	return contents, nil
}

func normalize(response *http.Response, body []byte) (model.Response, error) {
	mediaType, params, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil {
		return model.Response{}, fmt.Errorf("invalid response content type: %w", err)
	}
	mediaType = strings.ToLower(mediaType)
	if mediaType != "" && !strings.HasPrefix(mediaType, "text/") && mediaType != "application/json" && !strings.HasSuffix(mediaType, "+json") {
		return model.Response{}, fmt.Errorf("unsupported binary response content type %q", mediaType)
	}
	if charset := strings.ToLower(params["charset"]); charset != "" && charset != "utf-8" && charset != "utf8" {
		return model.Response{}, fmt.Errorf("unsupported response charset %q", charset)
	}
	if !utf8.Valid(body) {
		return model.Response{}, errors.New("unsupported binary response body")
	}
	normalized := model.Response{StatusCode: response.StatusCode, Headers: response.Header.Clone(), Body: string(body)}
	if mediaType == "application/json" || strings.HasSuffix(mediaType, "+json") {
		if len(bytes.TrimSpace(body)) == 0 {
			return normalized, nil
		}
		if err := json.Unmarshal(body, &normalized.JSON); err != nil {
			return model.Response{}, fmt.Errorf("invalid JSON response: %w", err)
		}
	}
	return normalized, nil
}

var _ model.HTTPExecutor = (*Executor)(nil)
