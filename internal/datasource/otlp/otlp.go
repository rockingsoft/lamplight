// Package otlp implements the local OTLP/HTTP trace store used by Tracetest's
// collector-backed integrations.
package otlp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	collector "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	common "go.opentelemetry.io/proto/otlp/common/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"lamplight/internal/model"
)

type Config struct{ Endpoint string }

type Store struct {
	endpoint *url.URL
	mu       sync.RWMutex
	traces   map[string][]model.Span
	listener net.Listener
	server   *http.Server
	start    sync.Once
	startErr error
}

func New(c Config) (*Store, error) {
	u, err := url.Parse(c.Endpoint)
	if err != nil || u.Scheme != "http" || u.Host == "" {
		return nil, fmt.Errorf("OTLP endpoint must be an absolute http URL: %q", c.Endpoint)
	}
	host, _, err := net.SplitHostPort(u.Host)
	if err != nil {
		return nil, fmt.Errorf("OTLP endpoint must include a port: %q", c.Endpoint)
	}
	if host != "localhost" && net.ParseIP(host) == nil {
		return nil, fmt.Errorf("OTLP endpoint must bind a local address: %q", c.Endpoint)
	}
	return &Store{endpoint: u, traces: map[string][]model.Span{}}, nil
}

func (s *Store) TestConnection(ctx context.Context) error {
	s.start.Do(func() {
		ln, err := net.Listen("tcp", s.endpoint.Host)
		if err != nil {
			s.startErr = fmt.Errorf("listen for OTLP/HTTP: %w", err)
			return
		}
		s.listener = ln
		mux := http.NewServeMux()
		p := strings.TrimSuffix(s.endpoint.Path, "/") + "/v1/traces"
		mux.HandleFunc(p, s.export)
		s.server = &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
		go func() { _ = s.server.Serve(ln) }()
	})
	if s.startErr != nil {
		return s.startErr
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}

func (s *Store) Close() error {
	if s.server == nil {
		return nil
	}
	return s.server.Close()
}

func (s *Store) Observe(_ context.Context, id model.TraceID) (model.TraceObservation, error) {
	s.mu.RLock()
	spans := append([]model.Span(nil), s.traces[string(id)]...)
	s.mu.RUnlock()
	if len(spans) == 0 {
		return model.TraceObservation{Found: false, Valid: true}, nil
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i].SpanID < spans[j].SpanID })
	h := sha256.New()
	for _, sp := range spans {
		_, _ = fmt.Fprintf(h, "%s\x00%s\x00%s\x00%d\n", sp.SpanID, sp.ParentSpanID, sp.Name, sp.Duration)
	}
	return model.TraceObservation{Found: true, Valid: true, Complete: false, Spans: spans, Fingerprint: hex.EncodeToString(h.Sum(nil))}, nil
}

func (s *Store) export(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	req := &collector.ExportTraceServiceRequest{}
	ct := r.Header.Get("Content-Type")
	if strings.Contains(ct, "json") {
		err = protojson.Unmarshal(body, req)
	} else {
		err = proto.Unmarshal(body, req)
	}
	if err != nil {
		http.Error(w, "invalid OTLP trace payload", 400)
		return
	}
	s.ingest(req.ResourceSpans)
	resp := &collector.ExportTraceServiceResponse{}
	if strings.Contains(ct, "json") {
		w.Header().Set("Content-Type", "application/json")
		out, _ := protojson.Marshal(resp)
		_, _ = w.Write(out)
	} else {
		w.Header().Set("Content-Type", "application/x-protobuf")
		out, _ := proto.Marshal(resp)
		_, _ = w.Write(out)
	}
}

func (s *Store) ingest(groups []*tracepb.ResourceSpans) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, group := range groups {
		resource := attrs(group.GetResource().GetAttributes())
		for _, scope := range group.GetScopeSpans() {
			for _, sp := range scope.GetSpans() {
				tid := hex.EncodeToString(sp.TraceId)
				if tid == "" {
					continue
				}
				item := model.Span{TraceID: tid, SpanID: hex.EncodeToString(sp.SpanId), ParentSpanID: hex.EncodeToString(sp.ParentSpanId), Name: sp.Name, Kind: sp.Kind.String(), Status: sp.GetStatus().GetCode().String(), StatusMessage: sp.GetStatus().GetMessage(), Duration: time.Duration(int64(sp.EndTimeUnixNano - sp.StartTimeUnixNano)), Attributes: attrs(sp.Attributes), Resource: clone(resource)}
				s.traces[tid] = upsert(s.traces[tid], item)
			}
		}
	}
}
func upsert(in []model.Span, span model.Span) []model.Span {
	for i := range in {
		if in[i].SpanID == span.SpanID {
			in[i] = span
			return in
		}
	}
	return append(in, span)
}
func attrs(in []*common.KeyValue) map[string]any {
	out := map[string]any{}
	for _, kv := range in {
		out[kv.Key] = value(kv.Value)
	}
	return out
}
func value(v *common.AnyValue) any {
	if v == nil {
		return nil
	}
	switch x := v.Value.(type) {
	case *common.AnyValue_StringValue:
		return x.StringValue
	case *common.AnyValue_BoolValue:
		return x.BoolValue
	case *common.AnyValue_IntValue:
		return x.IntValue
	case *common.AnyValue_DoubleValue:
		return x.DoubleValue
	case *common.AnyValue_BytesValue:
		return x.BytesValue
	case *common.AnyValue_ArrayValue:
		a := make([]any, 0, len(x.ArrayValue.Values))
		for _, v := range x.ArrayValue.Values {
			a = append(a, value(v))
		}
		return a
	case *common.AnyValue_KvlistValue:
		return attrs(x.KvlistValue.Values)
	}
	return nil
}
func clone(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
