// Package search implements the Elasticsearch APM and OpenSearch adapters.
package search

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"lamplight/internal/model"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
)

type Config struct {
	Kind, Endpoint string
	Headers        map[string]string
	BearerToken    string
	TLSSkipVerify  bool
	HTTPClient     *http.Client
}
type Store struct {
	kind     string
	endpoint *url.URL
	headers  http.Header
	client   *http.Client
}

func New(c Config) (*Store, error) {
	u, e := url.Parse(c.Endpoint)
	if e != nil || !u.IsAbs() || u.Host == "" {
		return nil, fmt.Errorf("%s endpoint must be an absolute URL: %q", c.Kind, c.Endpoint)
	}
	h := make(http.Header)
	for k, v := range c.Headers {
		h.Set(k, v)
	}
	if c.BearerToken != "" {
		h.Set("Authorization", "Bearer "+c.BearerToken)
	}
	cl := c.HTTPClient
	if cl == nil {
		tr := http.DefaultTransport.(*http.Transport).Clone()
		if c.TLSSkipVerify {
			tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
		}
		cl = &http.Client{Transport: tr, Timeout: 30 * time.Second}
	}
	return &Store{c.Kind, u, h, cl}, nil
} // #nosec G402 explicit opt-in
func (s *Store) TestConnection(ctx context.Context) error {
	r, e := s.request(ctx, http.MethodGet, nil)
	if e != nil {
		return obs(e, true)
	}
	defer func() { _ = r.Body.Close() }()
	if r.StatusCode >= 200 && r.StatusCode < 300 {
		return nil
	}
	return status(s.kind, r)
}
func (s *Store) Observe(ctx context.Context, id model.TraceID) (model.TraceObservation, error) {
	query := map[string]any{"query": map[string]any{"match": map[string]any{"trace.id": string(id)}}}
	if s.kind == "opensearch" {
		query = map[string]any{"query": map[string]any{"bool": map[string]any{"should": []any{
			map[string]any{"match": map[string]any{"traceId": string(id)}},
			map[string]any{"match": map[string]any{"traceID": string(id)}},
		}}}}
	}
	b, _ := json.Marshal(query)
	r, e := s.request(ctx, http.MethodPost, b)
	if e != nil {
		return model.TraceObservation{}, obs(e, true)
	}
	defer func() { _ = r.Body.Close() }()
	if r.StatusCode < 200 || r.StatusCode >= 300 {
		return model.TraceObservation{}, status(s.kind, r)
	}
	raw, e := io.ReadAll(io.LimitReader(r.Body, 32<<20))
	if e != nil {
		return model.TraceObservation{}, obs(e, true)
	}
	var payload struct {
		Hits struct {
			Hits []struct {
				Source map[string]any `json:"_source"`
			} `json:"hits"`
		} `json:"hits"`
	}
	if e = json.Unmarshal(raw, &payload); e != nil {
		return model.TraceObservation{}, obs(e, false)
	}
	if len(payload.Hits.Hits) == 0 {
		return model.TraceObservation{Found: false, Valid: true, Raw: raw}, nil
	}
	spans := make([]model.Span, 0, len(payload.Hits.Hits))
	for _, hit := range payload.Hits.Hits {
		f := map[string]any{}
		flatten("", hit.Source, f)
		res := map[string]any{}
		if svc := str(f, "service.name", "resource.service.name"); svc != "" {
			res["service.name"] = svc
		}
		spans = append(spans, model.Span{TraceID: str(f, "trace.id", "traceId", "traceID"), SpanID: str(f, "span.id", "spanId", "transaction.id"), ParentSpanID: str(f, "parent.id", "parentSpanId", "parentSpanID"), Name: str(f, "span.name", "name", "operationName", "transaction.name"), Duration: duration(f), Attributes: f, Resource: res})
	}
	sum := sha256.Sum256(raw)
	return model.TraceObservation{Found: true, Valid: true, Complete: true, Spans: spans, Raw: raw, Fingerprint: hex.EncodeToString(sum[:])}, nil
}
func (s *Store) request(ctx context.Context, method string, body []byte) (*http.Response, error) {
	u := *s.endpoint
	if method == http.MethodPost {
		u.Path = path.Join(u.Path, "_search")
	}
	r, e := http.NewRequestWithContext(ctx, method, u.String(), bytes.NewReader(body))
	if e != nil {
		return nil, e
	}
	r.Header = s.headers.Clone()
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	return s.client.Do(r)
}
func flatten(prefix string, v map[string]any, out map[string]any) {
	for k, x := range v {
		key := k
		if prefix != "" {
			key = prefix + "." + k
		}
		if child, ok := x.(map[string]any); ok {
			flatten(key, child, out)
		} else {
			out[key] = x
		}
	}
}
func str(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			return fmt.Sprint(v)
		}
	}
	return ""
}
func duration(m map[string]any) time.Duration {
	for _, p := range []struct {
		k    string
		unit time.Duration
	}{{"durationNano", 1}, {"durationNanos", 1}, {"span.duration.us", time.Microsecond}, {"transaction.duration.us", time.Microsecond}, {"duration", time.Microsecond}} {
		if v, ok := m[p.k].(float64); ok {
			return time.Duration(v * float64(p.unit))
		}
	}
	return 0
}
func status(kind string, r *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(r.Body, 4096))
	return obs(fmt.Errorf("%s returned HTTP %d: %s", kind, r.StatusCode, strings.TrimSpace(string(b))), r.StatusCode == 404 || r.StatusCode == 429 || r.StatusCode >= 500)
}
func obs(e error, retry bool) error { return &model.ObservationError{Err: e, Retriable: retry} }
