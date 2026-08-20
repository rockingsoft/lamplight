// Package tempo adapts Tempo's trace-by-ID HTTP API to model.DataStore.
package tempo

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"tracetest/internal/model"
)

// Config owns only Tempo datasource concerns; HTTP step settings never leak
// into this adapter.
type Config struct {
	Endpoint      string
	Headers       map[string]string
	BearerToken   string
	TLSSkipVerify bool
	HTTPClient    *http.Client
}

// Store is a Tempo DataStore implementation.
type Store struct {
	endpoint *url.URL
	headers  http.Header
	client   *http.Client
}

// New validates endpoint configuration and builds an adapter. No connection is
// made until TestConnection or Observe is called.
func New(config Config) (*Store, error) {
	endpoint, err := url.Parse(config.Endpoint)
	if err != nil || !endpoint.IsAbs() || (endpoint.Scheme != "http" && endpoint.Scheme != "https") || endpoint.Host == "" {
		return nil, fmt.Errorf("Tempo endpoint must be absolute http or https: %q", config.Endpoint)
	}
	headers := make(http.Header, len(config.Headers)+1)
	for key, value := range config.Headers {
		headers.Set(key, value)
	}
	if config.BearerToken != "" {
		headers.Set("Authorization", "Bearer "+config.BearerToken)
	}
	client := config.HTTPClient
	if client == nil {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		if config.TLSSkipVerify {
			transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
		} // #nosec G402 -- explicit datasource opt-in.
		client = &http.Client{Transport: transport, Timeout: 30 * time.Second}
	}
	return &Store{endpoint: endpoint, headers: headers, client: client}, nil
}

// TestConnection probes Tempo's standard readiness endpoint.
func (s *Store) TestConnection(ctx context.Context) error {
	response, err := s.do(ctx, "ready")
	if err != nil {
		return classifyTransport(err)
	}
	defer response.Body.Close()
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		return nil
	}
	return classifyStatus(response)
}

// Observe fetches the normalized trace represented by traceID.
func (s *Store) Observe(ctx context.Context, traceID model.TraceID) (model.TraceObservation, error) {
	if !validTraceID(traceID) {
		return model.TraceObservation{}, &model.ObservationError{Err: errors.New("invalid trace ID"), Retriable: false}
	}
	response, err := s.do(ctx, "api", "traces", string(traceID))
	if err != nil {
		return model.TraceObservation{}, classifyTransport(err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return model.TraceObservation{}, classifyStatus(response)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 32<<20))
	if err != nil {
		return model.TraceObservation{}, &model.ObservationError{Err: fmt.Errorf("read Tempo response: %w", err), Retriable: true}
	}
	return decodeTrace(body, traceID)
}

func (s *Store) do(ctx context.Context, segments ...string) (*http.Response, error) {
	endpoint := *s.endpoint
	parts := append([]string{endpoint.Path}, segments...)
	endpoint.Path = path.Join(parts...)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, err
	}
	request.Header = s.headers.Clone()
	return s.client.Do(request)
}

func classifyStatus(response *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(response.Body, 4<<10))
	message := fmt.Sprintf("Tempo returned HTTP %d", response.StatusCode)
	if text := strings.TrimSpace(string(body)); text != "" {
		message += ": " + text
	}
	retriable := response.StatusCode == http.StatusNotFound || response.StatusCode == http.StatusRequestTimeout || response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= 500
	retryAfter := time.Duration(0)
	if retriable {
		retryAfter = parseRetryAfter(response.Header.Get("Retry-After"))
	}
	return &model.ObservationError{Err: errors.New(message), Retriable: retriable, RetryAfter: retryAfter}
}

func classifyTransport(err error) error {
	if err == nil {
		return nil
	}
	retriable := true
	var dnsError *net.DNSError
	if errors.As(err, &dnsError) && dnsError.IsNotFound {
		retriable = false
	}
	var urlError *url.Error
	if errors.As(err, &urlError) && strings.Contains(strings.ToLower(urlError.Err.Error()), "certificate") {
		retriable = false
	}
	if errors.Is(err, context.Canceled) {
		retriable = false
	}
	return &model.ObservationError{Err: err, Retriable: retriable}
}

// IsRetriable reports Tempo's normalized error classification.
func IsRetriable(err error) bool {
	var observationError *model.ObservationError
	return errors.As(err, &observationError) && observationError.Retriable
}

func parseRetryAfter(value string) time.Duration {
	seconds, err := strconv.Atoi(strings.TrimSpace(value))
	if err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	return 0
}

func validTraceID(value model.TraceID) bool {
	if len(value) != 32 {
		return false
	}
	for _, rune := range value {
		if !(rune >= '0' && rune <= '9' || rune >= 'a' && rune <= 'f') {
			return false
		}
	}
	return strings.Trim(string(value), "0") != ""
}

type tracePayload struct {
	Batches       []batch `json:"batches"`
	ResourceSpans []batch `json:"resourceSpans"`
	Partial       bool    `json:"partial"`
	Complete      *bool   `json:"complete"`
}
type batch struct {
	Resource   resource     `json:"resource"`
	ScopeSpans []scopeSpans `json:"scopeSpans"`
	// InstrumentationLibrarySpans is used by older OTLP JSON responses.
	InstrumentationLibrarySpans []scopeSpans `json:"instrumentationLibrarySpans"`
}
type resource struct {
	Attributes []attribute `json:"attributes"`
}
type scopeSpans struct {
	Spans []tempoSpan `json:"spans"`
}
type tempoSpan struct {
	TraceID           string      `json:"traceId"`
	SpanID            string      `json:"spanId"`
	ParentSpanID      string      `json:"parentSpanId"`
	Name              string      `json:"name"`
	Kind              any         `json:"kind"`
	StartTimeUnixNano json.Number `json:"startTimeUnixNano"`
	EndTimeUnixNano   json.Number `json:"endTimeUnixNano"`
	Attributes        []attribute `json:"attributes"`
	Status            status      `json:"status"`
}
type attribute struct {
	Key   string `json:"key"`
	Value any    `json:"value"`
}
type status struct {
	Code    any    `json:"code"`
	Message string `json:"message"`
}

func decodeTrace(body []byte, traceID model.TraceID) (model.TraceObservation, error) {
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.UseNumber()
	var payload tracePayload
	if err := decoder.Decode(&payload); err != nil {
		return model.TraceObservation{}, &model.ObservationError{Err: fmt.Errorf("decode Tempo response: %w", err), Retriable: false}
	}
	batches := append(payload.Batches, payload.ResourceSpans...)
	if batches == nil {
		return model.TraceObservation{}, &model.ObservationError{Err: errors.New("Tempo response has no batches or resourceSpans"), Retriable: false}
	}
	complete := !payload.Partial
	if payload.Complete != nil {
		complete = *payload.Complete
	}
	observation := model.TraceObservation{Found: true, Valid: true, Partial: payload.Partial, Complete: complete, Raw: body}
	for _, currentBatch := range batches {
		resourceAttributes := attributes(currentBatch.Resource.Attributes)
		scopes := append(currentBatch.ScopeSpans, currentBatch.InstrumentationLibrarySpans...)
		for _, scope := range scopes {
			for _, source := range scope.Spans {
				normalizedTraceID, err := normalizeID(source.TraceID, 16)
				if err != nil {
					return model.TraceObservation{}, &model.ObservationError{Err: fmt.Errorf("invalid Tempo trace ID: %w", err), Retriable: false}
				}
				if normalizedTraceID != "" && !strings.EqualFold(normalizedTraceID, string(traceID)) {
					return model.TraceObservation{}, &model.ObservationError{Err: errors.New("Tempo response trace ID does not match requested trace"), Retriable: false}
				}
				source.TraceID = normalizedTraceID
				span, err := normalizeSpan(source, resourceAttributes)
				if err != nil {
					return model.TraceObservation{}, &model.ObservationError{Err: err, Retriable: false}
				}
				observation.Spans = append(observation.Spans, span)
			}
		}
	}
	return observation, nil
}

func normalizeSpan(source tempoSpan, resourceAttributes map[string]any) (model.Span, error) {
	spanID, err := normalizeID(source.SpanID, 8)
	if err != nil {
		return model.Span{}, fmt.Errorf("invalid span ID: %w", err)
	}
	parentSpanID, err := normalizeID(source.ParentSpanID, 8)
	if err != nil {
		return model.Span{}, fmt.Errorf("invalid parent span ID: %w", err)
	}
	start, err := source.StartTimeUnixNano.Int64()
	if err != nil {
		return model.Span{}, fmt.Errorf("invalid span start time: %w", err)
	}
	end, err := source.EndTimeUnixNano.Int64()
	if err != nil {
		return model.Span{}, fmt.Errorf("invalid span end time: %w", err)
	}
	if end < start {
		return model.Span{}, errors.New("span end precedes start")
	}
	return model.Span{TraceID: source.TraceID, SpanID: spanID, ParentSpanID: parentSpanID, Name: source.Name, Kind: spanKind(source.Kind), Status: spanStatus(source.Status.Code), StatusMessage: source.Status.Message, Duration: time.Duration(end - start), Attributes: attributes(source.Attributes), Resource: resourceAttributes}, nil
}

func normalizeID(value string, size int) (string, error) {
	if value == "" {
		return "", nil
	}
	if decoded, err := hex.DecodeString(value); err == nil && len(decoded) == size {
		return strings.ToLower(value), nil
	}
	decoded, err := base64.StdEncoding.DecodeString(value)
	if err != nil || len(decoded) != size {
		return "", fmt.Errorf("expected %d bytes", size)
	}
	return hex.EncodeToString(decoded), nil
}

func attributes(values []attribute) map[string]any {
	result := make(map[string]any, len(values))
	for _, attribute := range values {
		result[attribute.Key] = attributeValue(attribute.Value)
	}
	return result
}
func attributeValue(value any) any {
	if object, ok := value.(map[string]any); ok {
		if result, found := object["intValue"]; found {
			if number, err := strconv.ParseInt(fmt.Sprint(result), 10, 64); err == nil {
				return number
			}
		}
		if result, found := object["doubleValue"]; found {
			if number, err := strconv.ParseFloat(fmt.Sprint(result), 64); err == nil {
				return number
			}
		}
		if result, found := object["boolValue"]; found {
			if boolean, err := strconv.ParseBool(fmt.Sprint(result)); err == nil {
				return boolean
			}
		}
		for _, key := range []string{"stringValue", "bytesValue"} {
			if result, found := object[key]; found {
				return result
			}
		}
	}
	return value
}
func spanKind(value any) string {
	switch typed := value.(type) {
	case string:
		return strings.TrimPrefix(strings.ToLower(typed), "span_kind_")
	case json.Number:
		if typed.String() == "2" {
			return "server"
		}
		if typed.String() == "3" {
			return "client"
		}
	}
	return ""
}
func spanStatus(value any) string {
	switch typed := value.(type) {
	case string:
		return strings.TrimPrefix(strings.ToLower(typed), "status_code_")
	case json.Number:
		if typed.String() == "2" {
			return "error"
		}
		if typed.String() == "1" {
			return "ok"
		}
	}
	return ""
}

var _ model.DataStore = (*Store)(nil)
