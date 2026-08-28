// Package otlp implements Lamplight's embedded OTLP/HTTP trace and metrics receiver.
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
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/otlptranslator"
	metriccollector "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	collector "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	common "go.opentelemetry.io/proto/otlp/common/v1"
	metricpb "go.opentelemetry.io/proto/otlp/metrics/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"lamplight/internal/model"
	"lamplight/internal/promqlstore"
)

type Config struct{ Endpoint string }

type Store struct {
	endpoint *url.URL
	mu       sync.RWMutex
	traces   map[string][]model.Span
	metrics  map[string]model.MetricSample
	queries  *promqlstore.Store
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
	return &Store{endpoint: u, traces: map[string][]model.Span{}, metrics: map[string]model.MetricSample{}, queries: promqlstore.New()}, nil
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
		metricsPath := strings.TrimSuffix(s.endpoint.Path, "/") + "/v1/metrics"
		mux.HandleFunc(metricsPath, s.exportMetrics)
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

func (s *Store) Snapshot(ctx context.Context, query string) (model.MetricSnapshot, error) {
	if err := s.TestConnection(ctx); err != nil {
		return model.MetricSnapshot{}, err
	}
	return s.queries.Snapshot(ctx, query)
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

func (s *Store) exportMetrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	request := &metriccollector.ExportMetricsServiceRequest{}
	contentType := r.Header.Get("Content-Type")
	if strings.Contains(contentType, "json") {
		err = protojson.Unmarshal(body, request)
	} else {
		err = proto.Unmarshal(body, request)
	}
	if err != nil {
		http.Error(w, "invalid OTLP metrics payload", http.StatusBadRequest)
		return
	}
	if err := s.ingestMetrics(request.ResourceMetrics); err != nil {
		http.Error(w, "invalid OTLP metrics: "+err.Error(), http.StatusBadRequest)
		return
	}
	response := &metriccollector.ExportMetricsServiceResponse{}
	if strings.Contains(contentType, "json") {
		w.Header().Set("Content-Type", "application/json")
		out, _ := protojson.Marshal(response)
		_, _ = w.Write(out)
	} else {
		w.Header().Set("Content-Type", "application/x-protobuf")
		out, _ := proto.Marshal(response)
		_, _ = w.Write(out)
	}
}

func (s *Store) ingestMetrics(groups []*metricpb.ResourceMetrics) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := make(map[string]model.MetricSample, len(s.metrics))
	for key, value := range s.metrics {
		next[key] = value
	}
	for _, group := range groups {
		resource := normalizedAttrs(group.GetResource().GetAttributes())
		for _, scope := range group.GetScopeMetrics() {
			for _, metric := range scope.GetMetrics() {
				s.ingestMetric(next, metric, resource)
			}
		}
	}
	samples := make([]model.MetricSample, 0, len(next))
	for _, sample := range next {
		samples = append(samples, sample)
	}
	if err := s.queries.Ingest(samples, time.Now()); err != nil {
		return err
	}
	s.metrics = next
	return nil
}

func (s *Store) ingestMetric(metrics map[string]model.MetricSample, metric *metricpb.Metric, resource map[string]any) {
	if gauge := metric.GetGauge(); gauge != nil {
		for _, point := range gauge.DataPoints {
			putMetric(metrics, numberSample(metric, "gauge", otlptranslator.MetricTypeGauge, numberValue(point), point.Attributes, resource), false)
		}
		return
	}
	if sum := metric.GetSum(); sum != nil {
		metricType := "sum"
		if sum.IsMonotonic {
			metricType = "counter"
		}
		translationType := otlptranslator.MetricType(otlptranslator.MetricTypeNonMonotonicCounter)
		if sum.IsMonotonic {
			translationType = otlptranslator.MetricTypeMonotonicCounter
		}
		add := sum.AggregationTemporality == metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA
		for _, point := range sum.DataPoints {
			putMetric(metrics, numberSample(metric, metricType, translationType, numberValue(point), point.Attributes, resource), add)
		}
		return
	}
	if histogram := metric.GetHistogram(); histogram != nil {
		add := histogram.AggregationTemporality == metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA
		for _, point := range histogram.DataPoints {
			base := translatedMetricName(metric, otlptranslator.MetricTypeHistogram)
			putMetric(metrics, sample(base+"_count", "histogram", float64(point.Count), point.Attributes, resource), add)
			putMetric(metrics, sample(base+"_sum", "histogram", point.GetSum(), point.Attributes, resource), add)
			cumulative := uint64(0)
			for index, count := range point.BucketCounts {
				cumulative += count
				upperBound := "+Inf"
				if index < len(point.ExplicitBounds) {
					upperBound = strconv.FormatFloat(point.ExplicitBounds[index], 'g', -1, 64)
				}
				bucket := sample(base+"_bucket", "histogram", float64(cumulative), point.Attributes, resource)
				bucket.Labels["le"] = upperBound
				putMetric(metrics, bucket, add)
			}
		}
		return
	}
	if histogram := metric.GetExponentialHistogram(); histogram != nil {
		add := histogram.AggregationTemporality == metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA
		for _, point := range histogram.DataPoints {
			base := translatedMetricName(metric, otlptranslator.MetricTypeExponentialHistogram)
			putMetric(metrics, sample(base+"_count", "exponential_histogram", float64(point.Count), point.Attributes, resource), add)
			putMetric(metrics, sample(base+"_sum", "exponential_histogram", point.GetSum(), point.Attributes, resource), add)
		}
		return
	}
	if summary := metric.GetSummary(); summary != nil {
		for _, point := range summary.DataPoints {
			base := translatedMetricName(metric, otlptranslator.MetricTypeSummary)
			putMetric(metrics, sample(base+"_count", "summary", float64(point.Count), point.Attributes, resource), false)
			putMetric(metrics, sample(base+"_sum", "summary", point.Sum, point.Attributes, resource), false)
			for _, quantileValue := range point.QuantileValues {
				quantile := sample(base, "summary", quantileValue.Value, point.Attributes, resource)
				quantile.Labels["quantile"] = strconv.FormatFloat(quantileValue.Quantile, 'g', -1, 64)
				putMetric(metrics, quantile, false)
			}
		}
	}
}

func numberValue(point *metricpb.NumberDataPoint) float64 {
	switch value := point.Value.(type) {
	case *metricpb.NumberDataPoint_AsDouble:
		return value.AsDouble
	case *metricpb.NumberDataPoint_AsInt:
		return float64(value.AsInt)
	default:
		return 0
	}
}

func numberSample(metric *metricpb.Metric, metricType string, translationType otlptranslator.MetricType, value float64, attributes []*common.KeyValue, resource map[string]any) model.MetricSample {
	return sample(translatedMetricName(metric, translationType), metricType, value, attributes, resource)
}

func sample(name, metricType string, value float64, attributes []*common.KeyValue, resource map[string]any) model.MetricSample {
	values := attrs(attributes)
	normalized := normalizedAttrs(attributes)
	labels := make(map[string]string, len(values))
	for key, attribute := range normalized {
		labels[key] = fmt.Sprint(attribute)
	}
	return model.MetricSample{Name: name, Type: metricType, Value: value, Labels: labels, Attributes: values, Resource: clone(resource)}
}

var (
	metricNamer = otlptranslator.NewMetricNamer("", otlptranslator.UnderscoreEscapingWithSuffixes)
	labelNamer  = otlptranslator.LabelNamer{}
)

func translatedMetricName(metric *metricpb.Metric, metricType otlptranslator.MetricType) string {
	name, err := metricNamer.Build(otlptranslator.Metric{Name: metric.Name, Unit: metric.Unit, Type: metricType})
	if err != nil {
		return metric.Name
	}
	return name
}

func normalizedAttrs(attributes []*common.KeyValue) map[string]any {
	result := map[string]any{}
	for key, attribute := range attrs(attributes) {
		name, err := labelNamer.Build(key)
		if err == nil {
			result[name] = attribute
		}
	}
	return result
}

func putMetric(metrics map[string]model.MetricSample, sample model.MetricSample, additive bool) {
	key := metricKey(sample)
	if additive {
		sample.Value += metrics[key].Value
	}
	metrics[key] = sample
}

func metricKey(sample model.MetricSample) string {
	labels := make([]string, 0, len(sample.Labels))
	for name := range sample.Labels {
		labels = append(labels, name)
	}
	sort.Strings(labels)
	var key strings.Builder
	key.WriteString(sample.Name)
	for _, name := range labels {
		fmt.Fprintf(&key, "\x00%s=%s", name, sample.Labels[name])
	}
	resourceNames := make([]string, 0, len(sample.Resource))
	for name := range sample.Resource {
		resourceNames = append(resourceNames, name)
	}
	sort.Strings(resourceNames)
	for _, name := range resourceNames {
		fmt.Fprintf(&key, "\x00resource.%s=%v", name, sample.Resource[name])
	}
	return key.String()
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

var _ model.MetricStore = (*Store)(nil)
