// Package signalfx adapts the Splunk Observability (SignalFx) trace API.
package signalfx

import (
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
	"strconv"
	"strings"
	"time"
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
	u, e := url.Parse(c.Endpoint)
	if e != nil || !u.IsAbs() || u.Host == "" {
		return nil, fmt.Errorf("SignalFx endpoint must be an absolute URL: %q", c.Endpoint)
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
		transport := http.DefaultTransport.(*http.Transport).Clone()
		if c.TLSSkipVerify {
			transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
		} // #nosec G402 -- explicit datasource opt-in.
		cl = &http.Client{Transport: transport, Timeout: 30 * time.Second}
	}
	return &Store{u, h, cl}, nil
}
func (s *Store) TestConnection(ctx context.Context) error {
	r, e := s.get(ctx, "v2", "apm", "trace", "00000000000000000000000000000001", "segments")
	if e != nil {
		return obs(e, true)
	}
	defer func() { _ = r.Body.Close() }()
	if r.StatusCode == 200 || r.StatusCode == 404 {
		return nil
	}
	return status(r)
}
func (s *Store) Observe(ctx context.Context, id model.TraceID) (model.TraceObservation, error) {
	r, e := s.get(ctx, "v2", "apm", "trace", string(id), "segments")
	if e != nil {
		return model.TraceObservation{}, obs(e, true)
	}
	defer func() { _ = r.Body.Close() }()
	if r.StatusCode == 404 {
		return model.TraceObservation{Found: false, Valid: true}, nil
	}
	if r.StatusCode != 200 {
		return model.TraceObservation{}, status(r)
	}
	var times []int64
	if e = json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&times); e != nil {
		return model.TraceObservation{}, obs(e, false)
	}
	spans := []model.Span{}
	raw := []byte{}
	for _, ts := range times {
		res, e := s.get(ctx, "v2", "apm", "trace", string(id), strconv.FormatInt(ts, 10))
		if e != nil {
			return model.TraceObservation{}, obs(e, true)
		}
		b, _ := io.ReadAll(io.LimitReader(res.Body, 32<<20))
		_ = res.Body.Close()
		raw = append(raw, b...)
		if res.StatusCode != 200 {
			continue
		}
		var items []struct {
			TraceID     string            `json:"traceId"`
			SpanID      string            `json:"spanId"`
			ParentID    string            `json:"parentId"`
			Name        string            `json:"operationName"`
			Duration    int64             `json:"durationMicros"`
			Tags        map[string]string `json:"tags"`
			ProcessTags map[string]string `json:"processTags"`
		}
		if e = json.Unmarshal(b, &items); e != nil {
			return model.TraceObservation{}, obs(e, false)
		}
		for _, x := range items {
			a := map[string]any{}
			for k, v := range x.Tags {
				a[k] = v
			}
			resource := map[string]any{}
			for k, v := range x.ProcessTags {
				resource[k] = v
			}
			spans = append(spans, model.Span{TraceID: x.TraceID, SpanID: x.SpanID, ParentSpanID: x.ParentID, Name: x.Name, Duration: time.Duration(x.Duration) * time.Microsecond, Attributes: a, Resource: resource})
		}
	}
	if len(spans) == 0 {
		return model.TraceObservation{Found: false, Valid: true}, nil
	}
	sum := sha256.Sum256(raw)
	return model.TraceObservation{Found: true, Valid: true, Complete: true, Spans: spans, Raw: raw, Fingerprint: hex.EncodeToString(sum[:])}, nil
}
func (s *Store) get(ctx context.Context, parts ...string) (*http.Response, error) {
	u := *s.endpoint
	u.Path = path.Join(append([]string{u.Path}, parts...)...)
	r, e := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
	if e != nil {
		return nil, e
	}
	r.Header = s.headers.Clone()
	return s.client.Do(r)
}
func status(r *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(r.Body, 4096))
	return obs(fmt.Errorf("SignalFx returned HTTP %d: %s", r.StatusCode, strings.TrimSpace(string(b))), r.StatusCode == 404 || r.StatusCode == 429 || r.StatusCode >= 500)
}
func obs(e error, retry bool) error { return &model.ObservationError{Err: e, Retriable: retry} }
