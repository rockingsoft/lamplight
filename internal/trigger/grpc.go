package trigger

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/fullstorydev/grpcurl"
	"github.com/golang/protobuf/jsonpb"
	"github.com/golang/protobuf/proto"
	"github.com/jhump/protoreflect/desc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"lamplight/internal/model"
)

func executeGRPC(ctx context.Context, request model.TriggerRequest, trace *model.TestTraceContext) (model.Response, error) {
	content := stringAttr(request, "protobuf")
	if content == "" {
		return model.Response{}, fmt.Errorf("grpc_request.protobuf is required (server reflection is not supported)")
	}
	tmp, err := os.CreateTemp("", "lamplight-*.proto")
	if err != nil {
		return model.Response{}, err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err = tmp.WriteString(content); err != nil {
		return model.Response{}, err
	}
	if err = tmp.Close(); err != nil {
		return model.Response{}, err
	}
	source, err := grpcurl.DescriptorSourceFromProtoFiles([]string{os.TempDir()}, tmp.Name())
	if err != nil {
		return model.Response{}, fmt.Errorf("parse protobuf: %w", err)
	}
	conn, err := grpcurl.BlockingDial(ctx, "tcp", stringAttr(request, "address"), nil)
	if err != nil {
		return model.Response{}, fmt.Errorf("dial gRPC service: %w", err)
	}
	defer func() { _ = conn.Close() }()
	parser, _, err := grpcurl.RequestParserAndFormatter(grpcurl.FormatJSON, source, strings.NewReader(stringAttr(request, "request")), grpcurl.FormatOptions{EmitJSONDefaultFields: true, AllowUnknownFields: true})
	if err != nil {
		return model.Response{}, err
	}
	h := &grpcHandler{marshaller: jsonpb.Marshaler{EmitDefaults: true, Indent: "  ", AnyResolver: grpcurl.AnyResolverFromDescriptorSource(source)}}
	headers := stringMap(request.Attributes["metadata"])
	if trace != nil {
		headers["traceparent"] = trace.TraceParent()
		if trace.TraceState != "" {
			headers["tracestate"] = trace.TraceState
		}
	}
	var rawHeaders []string
	for key, value := range headers {
		rawHeaders = append(rawHeaders, key+": "+value)
	}
	if err := grpcurl.InvokeRPC(ctx, source, conn, stringAttr(request, "method"), rawHeaders, h, parser.Next); err != nil {
		return model.Response{}, err
	}
	return model.Response{StatusCode: int(h.code), Headers: metadataHeaders(h.md), Body: h.body, JSON: jsonBody(h.body)}, nil
}

type grpcHandler struct {
	marshaller jsonpb.Marshaler
	body       string
	code       codes.Code
	md         metadata.MD
}

func (h *grpcHandler) OnResolveMethod(*desc.MethodDescriptor) {}
func (h *grpcHandler) OnSendHeaders(metadata.MD)              {}
func (h *grpcHandler) OnReceiveHeaders(md metadata.MD)        { h.md = md }
func (h *grpcHandler) OnReceiveResponse(message proto.Message) {
	h.body, _ = h.marshaller.MarshalToString(message)
}
func (h *grpcHandler) OnReceiveTrailers(s *status.Status, md metadata.MD) {
	h.code = s.Code()
	for k, v := range md {
		h.md[k] = append(h.md[k], v...)
	}
}
func metadataHeaders(md metadata.MD) map[string][]string {
	out := map[string][]string{}
	for k, v := range md {
		out[k] = v
	}
	return out
}
func jsonBody(body string) any { var value any; _ = json.Unmarshal([]byte(body), &value); return value }
