package otlp

import (
	"bytes"
	"context"
	"encoding/hex"
	metriccollector "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	collector "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	common "go.opentelemetry.io/proto/otlp/common/v1"
	metricpb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resource "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
	"lamplight/internal/model"
	"math"
	"net"
	"net/http"
	"testing"
)

func TestReceiver(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
	_ = ln.Close()
	s, err := New(Config{Endpoint: "http://" + addr})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if err = s.TestConnection(context.Background()); err != nil {
		t.Fatal(err)
	}
	tid, _ := hex.DecodeString("0123456789abcdef0123456789abcdef")
	sid, _ := hex.DecodeString("0123456789abcdef")
	payload := &collector.ExportTraceServiceRequest{ResourceSpans: []*tracepb.ResourceSpans{{Resource: &resource.Resource{Attributes: []*common.KeyValue{{Key: "service.name", Value: &common.AnyValue{Value: &common.AnyValue_StringValue{StringValue: "shop"}}}}}, ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{{TraceId: tid, SpanId: sid, Name: "checkout", StartTimeUnixNano: 1, EndTimeUnixNano: 10}}}}}}}
	b, _ := proto.Marshal(payload)
	r, err := http.Post("http://"+addr+"/v1/traces", "application/x-protobuf", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	_ = r.Body.Close()
	got, err := s.Observe(context.Background(), model.TraceID(hex.EncodeToString(tid)))
	if err != nil || len(got.Spans) != 1 || got.Spans[0].Resource["service.name"] != "shop" {
		t.Fatalf("%#v %v", got, err)
	}
}

func TestReceiverIngestsCumulativeAndDeltaMetrics(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
	_ = ln.Close()
	store, err := New(Config{Endpoint: "http://" + addr})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	if err := store.TestConnection(context.Background()); err != nil {
		t.Fatal(err)
	}
	post := func(value int64, temporality metricpb.AggregationTemporality) {
		t.Helper()
		payload := &metriccollector.ExportMetricsServiceRequest{ResourceMetrics: []*metricpb.ResourceMetrics{{Resource: &resource.Resource{Attributes: []*common.KeyValue{{Key: "service.name", Value: &common.AnyValue{Value: &common.AnyValue_StringValue{StringValue: "shop"}}}}}, ScopeMetrics: []*metricpb.ScopeMetrics{{Metrics: []*metricpb.Metric{{Name: "orders.created", Data: &metricpb.Metric_Sum{Sum: &metricpb.Sum{IsMonotonic: true, AggregationTemporality: temporality, DataPoints: []*metricpb.NumberDataPoint{{Attributes: []*common.KeyValue{{Key: "result", Value: &common.AnyValue{Value: &common.AnyValue_StringValue{StringValue: "ok"}}}}, Value: &metricpb.NumberDataPoint_AsInt{AsInt: value}}}}}}}}}}}}
		encoded, _ := proto.Marshal(payload)
		response, postErr := http.Post("http://"+addr+"/v1/metrics", "application/x-protobuf", bytes.NewReader(encoded))
		if postErr != nil {
			t.Fatal(postErr)
		}
		_ = response.Body.Close()
	}
	post(4, metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE)
	post(1, metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA)
	snapshot, err := store.Snapshot(context.Background(), `orders_created_total{result="ok",resource_service_name="shop"}`)
	if err != nil || len(snapshot.Samples) != 1 || snapshot.Samples[0].Value != 5 || snapshot.Samples[0].Labels["resource_service_name"] != "shop" {
		t.Fatalf("snapshot=%#v err=%v", snapshot, err)
	}
}

func TestReceiverTranslatesOBIHistogramForPromQL(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
	_ = ln.Close()
	store, err := New(Config{Endpoint: "http://" + addr})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	if err := store.TestConnection(context.Background()); err != nil {
		t.Fatal(err)
	}
	sum := 0.3
	payload := &metriccollector.ExportMetricsServiceRequest{ResourceMetrics: []*metricpb.ResourceMetrics{{Resource: &resource.Resource{Attributes: []*common.KeyValue{{Key: "service.name", Value: &common.AnyValue{Value: &common.AnyValue_StringValue{StringValue: "shop"}}}}}, ScopeMetrics: []*metricpb.ScopeMetrics{{Metrics: []*metricpb.Metric{{Name: "http.server.request.duration", Unit: "s", Data: &metricpb.Metric_Histogram{Histogram: &metricpb.Histogram{AggregationTemporality: metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE, DataPoints: []*metricpb.HistogramDataPoint{{Count: 2, Sum: &sum, ExplicitBounds: []float64{0.1}, BucketCounts: []uint64{1, 1}}}}}}}}}}}}
	encoded, _ := proto.Marshal(payload)
	response, err := http.Post("http://"+addr+"/v1/metrics", "application/x-protobuf", bytes.NewReader(encoded))
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	snapshot, err := store.Snapshot(context.Background(), `http_server_request_duration_seconds_count{resource_service_name="shop"}`)
	if err != nil || len(snapshot.Samples) != 1 || snapshot.Samples[0].Value != 2 {
		t.Fatalf("snapshot=%#v err=%v", snapshot, err)
	}
}

func TestReceiverRejectsInvalidMetricExportAtomically(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
	_ = ln.Close()
	store, err := New(Config{Endpoint: "http://" + addr})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	if err := store.TestConnection(context.Background()); err != nil {
		t.Fatal(err)
	}

	post := func(metrics ...*metricpb.Metric) *http.Response {
		t.Helper()
		payload := &metriccollector.ExportMetricsServiceRequest{ResourceMetrics: []*metricpb.ResourceMetrics{{ScopeMetrics: []*metricpb.ScopeMetrics{{Metrics: metrics}}}}}
		encoded, marshalErr := proto.Marshal(payload)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		response, postErr := http.Post("http://"+addr+"/v1/metrics", "application/x-protobuf", bytes.NewReader(encoded))
		if postErr != nil {
			t.Fatal(postErr)
		}
		return response
	}
	gauge := func(name string, value float64) *metricpb.Metric {
		return &metricpb.Metric{Name: name, Data: &metricpb.Metric_Gauge{Gauge: &metricpb.Gauge{DataPoints: []*metricpb.NumberDataPoint{{Value: &metricpb.NumberDataPoint_AsDouble{AsDouble: value}}}}}}
	}

	response := post(gauge("stable", 3))
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("valid export status=%d", response.StatusCode)
	}
	response = post(gauge("stable", 4), gauge("invalid", math.NaN()))
	_ = response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid export status=%d", response.StatusCode)
	}

	snapshot, err := store.Snapshot(context.Background(), `stable`)
	if err != nil || len(snapshot.Samples) != 1 || snapshot.Samples[0].Value != 3 {
		t.Fatalf("invalid export changed store: snapshot=%#v err=%v", snapshot, err)
	}
}
