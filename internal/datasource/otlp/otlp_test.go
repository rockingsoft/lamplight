package otlp

import (
	"bytes"
	"context"
	"encoding/hex"
	collector "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	common "go.opentelemetry.io/proto/otlp/common/v1"
	resource "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
	"lamplight/internal/model"
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
