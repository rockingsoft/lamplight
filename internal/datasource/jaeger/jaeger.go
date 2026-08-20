// Package jaeger adapts the Jaeger query HTTP API to model.DataStore.
package jaeger

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"lamplight/internal/model"
)

type Config struct {
	Endpoint      string
	Headers       map[string]string
	BearerToken   string
	TLSSkipVerify bool
	HTTPClient    *http.Client
}

type Store struct {
	endpoint *url.URL
	headers  http.Header
	client   *http.Client
}

func New(c Config) (*Store, error) {
	u, err := url.Parse(c.Endpoint)
	if err != nil || !u.IsAbs() || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("Jaeger endpoint must be absolute http or https: %q", c.Endpoint)
	}
	h := make(http.Header, len(c.Headers)+1)
	for k, v := range c.Headers {
		h.Set(k, v)
	}
	if c.BearerToken != "" {
		h.Set("Authorization", "Bearer "+c.BearerToken)
	}
	client := c.HTTPClient
	if client == nil {
		tr := http.DefaultTransport.(*http.Transport).Clone()
		if c.TLSSkipVerify {
			tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
		}
		client = &http.Client{Transport: tr, Timeout: 30 * time.Second}
	} // #nosec G402 -- explicit opt-in.
	return &Store{u, h, client}, nil
}

func (s *Store) TestConnection(ctx context.Context) error {
	r, err := s.do(ctx, "api", "services")
	if err != nil {
		return observationError(err, true)
	}
	defer func() { _ = r.Body.Close() }()
	if r.StatusCode >= 200 && r.StatusCode < 300 {
		return nil
	}
	return statusError(r)
}

func (s *Store) Observe(ctx context.Context, id model.TraceID) (model.TraceObservation, error) {
	if !validID(id) {
		return model.TraceObservation{}, observationError(errors.New("invalid trace ID"), false)
	}
	r, err := s.do(ctx, "api", "traces", string(id))
	if err != nil {
		return model.TraceObservation{}, observationError(err, true)
	}
	defer r.Body.Close()
	if r.StatusCode == http.StatusNotFound {
		return model.TraceObservation{Found: false, Valid: true}, nil
	}
	if r.StatusCode < 200 || r.StatusCode >= 300 {
		return model.TraceObservation{}, statusError(r)
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
	if err != nil {
		return model.TraceObservation{}, observationError(err, true)
	}
	var payload struct {
		Data []struct {
			TraceID string `json:"traceID"`
			Spans   []struct {
				TraceID       string                                      `json:"traceID"`
				SpanID        string                                      `json:"spanID"`
				OperationName string                                      `json:"operationName"`
				References    []struct{ RefType, TraceID, SpanID string } `json:"references"`
				StartTime     int64                                       `json:"startTime"`
				Duration      int64                                       `json:"duration"`
				Tags          []tag                                       `json:"tags"`
				ProcessID     string                                      `json:"processID"`
			} `json:"spans"`
			Processes map[string]struct {
				ServiceName string `json:"serviceName"`
				Tags        []tag  `json:"tags"`
			} `json:"processes"`
		} `json:"data"`
		Errors any `json:"errors"`
	}
	if err := json.Unmarshal(b, &payload); err != nil {
		return model.TraceObservation{}, observationError(fmt.Errorf("decode Jaeger response: %w", err), false)
	}
	if len(payload.Data) == 0 {
		return model.TraceObservation{Found: false, Valid: true, Raw: b}, nil
	}
	spans := make([]model.Span, 0)
	for _, trace := range payload.Data {
		for _, sp := range trace.Spans {
			attrs := tags(sp.Tags)
			resource := map[string]any{}
			if p, ok := trace.Processes[sp.ProcessID]; ok {
				resource = tags(p.Tags)
				resource["service.name"] = p.ServiceName
			}
			parent := ""
			for _, ref := range sp.References {
				if ref.RefType == "CHILD_OF" {
					parent = ref.SpanID
					break
				}
			}
			status := ""
			if v, ok := attrs["otel.status_code"]; ok {
				status = fmt.Sprint(v)
			}
			spans = append(spans, model.Span{TraceID: sp.TraceID, SpanID: sp.SpanID, ParentSpanID: parent, Name: sp.OperationName, Status: status, Duration: time.Duration(sp.Duration) * time.Microsecond, Attributes: attrs, Resource: resource})
		}
	}
	sum := sha256.Sum256(b)
	return model.TraceObservation{Found: true, Valid: true, Complete: true, Spans: spans, Raw: b, Fingerprint: hex.EncodeToString(sum[:])}, nil
}

type tag struct {
	Key   string `json:"key"`
	Type  string `json:"type"`
	Value any    `json:"value"`
}

func tags(in []tag) map[string]any {
	out := make(map[string]any, len(in))
	for _, t := range in {
		out[t.Key] = t.Value
	}
	return out
}
func (s *Store) do(ctx context.Context, p ...string) (*http.Response, error) {
	u := *s.endpoint
	u.Path = path.Join(append([]string{u.Path}, p...)...)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header = s.headers.Clone()
	return s.client.Do(req)
}
func statusError(r *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(r.Body, 4096))
	return observationError(fmt.Errorf("Jaeger returned HTTP %d: %s", r.StatusCode, strings.TrimSpace(string(b))), r.StatusCode == 404 || r.StatusCode == 429 || r.StatusCode >= 500)
}
func observationError(err error, retry bool) error {
	return &model.ObservationError{Err: err, Retriable: retry}
}
func validID(id model.TraceID) bool {
	if len(id) != 32 {
		return false
	}
	_, err := hex.DecodeString(string(id))
	return err == nil && strings.Trim(string(id), "0") != ""
}
