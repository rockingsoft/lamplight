// Package trigger executes Lamplight's non-HTTP triggers. Its trigger set and
// wire semantics follow Tracetest's backend trigger implementations.
package trigger

import (
	"context"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/IBM/sarama"
	"lamplight/internal/model"
)

type Executor struct{ HTTP model.HTTPExecutor }

func New(http model.HTTPExecutor) *Executor { return &Executor{HTTP: http} }

func (e *Executor) Execute(ctx context.Context, request model.TriggerRequest, cfg model.HTTPClientConfig, trace *model.TestTraceContext) (model.Response, error) {
	switch request.Kind {
	case model.TriggerGraphQL:
		if e.HTTP == nil {
			return model.Response{}, fmt.Errorf("HTTP executor is not configured")
		}
		body, err := json.Marshal(map[string]any{"query": stringAttr(request, "query"), "variables": request.Attributes["variables"], "operationName": stringAttr(request, "operation_name")})
		if err != nil {
			return model.Response{}, err
		}
		return e.HTTP.Execute(ctx, model.HTTPRequest{Method: "POST", URL: stringAttr(request, "url"), Headers: stringMap(request.Attributes["headers"]), Body: string(body)}, cfg, trace)
	case model.TriggerGRPC:
		return executeGRPC(ctx, request, trace)
	case model.TriggerKafka:
		return executeKafka(ctx, request, trace)
	case model.TriggerTraceID, model.TriggerCypress, model.TriggerPlaywright, model.TriggerArtillery, model.TriggerK6:
		id := stringAttr(request, "id")
		if len(id) != 32 || !isHex(id) {
			return model.Response{}, fmt.Errorf("%s.id must be a 32-character trace ID", request.Kind)
		}
		return model.Response{StatusCode: 0, Headers: map[string][]string{}, Body: id, JSON: map[string]any{"trace_id": id}}, nil
	case model.TriggerPlaywrightEngine:
		return executePlaywright(ctx, request, trace)
	default:
		return model.Response{}, fmt.Errorf("unsupported trigger %q", request.Kind)
	}
}

func isHex(value string) bool { _, err := hex.DecodeString(value); return err == nil }

func executeKafka(ctx context.Context, request model.TriggerRequest, trace *model.TestTraceContext) (model.Response, error) {
	config := sarama.NewConfig()
	config.Producer.Return.Successes = true
	config.Producer.RequiredAcks = sarama.WaitForAll
	config.Producer.Retry.Max = 5
	if boolAttr(request, "tls") {
		config.Net.TLS.Enable = true
		config.Net.TLS.Config = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	if username := stringAttr(request, "username"); username != "" {
		config.Net.SASL.Enable, config.Net.SASL.User, config.Net.SASL.Password = true, username, stringAttr(request, "password")
		config.Net.SASL.Mechanism = sarama.SASLTypePlaintext
	}
	producer, err := sarama.NewSyncProducer(stringSlice(request.Attributes["broker_urls"]), config)
	if err != nil {
		return model.Response{}, fmt.Errorf("create kafka producer: %w", err)
	}
	defer producer.Close()
	headers := stringMap(request.Attributes["headers"])
	if trace != nil {
		headers["traceparent"] = trace.TraceParent()
		if trace.TraceState != "" {
			headers["tracestate"] = trace.TraceState
		}
	}
	message := &sarama.ProducerMessage{Topic: stringAttr(request, "topic"), Key: sarama.StringEncoder(stringAttr(request, "message_key")), Value: sarama.StringEncoder(stringAttr(request, "message_value"))}
	for key, value := range headers {
		message.Headers = append(message.Headers, sarama.RecordHeader{Key: []byte(key), Value: []byte(value)})
	}
	partition, offset, err := producer.SendMessage(message)
	if err != nil {
		return model.Response{}, fmt.Errorf("send kafka message: %w", err)
	}
	result := map[string]any{"partition": partition, "offset": offset}
	encoded, _ := json.Marshal(result)
	return model.Response{Headers: map[string][]string{}, Body: string(encoded), JSON: result}, nil
}

func executePlaywright(ctx context.Context, request model.TriggerRequest, trace *model.TestTraceContext) (model.Response, error) {
	if _, err := exec.LookPath("npx"); err != nil {
		return model.Response{}, fmt.Errorf("npx not found in PATH")
	}
	tmp, err := os.CreateTemp("", "lamplight-playwright-*.js")
	if err != nil {
		return model.Response{}, err
	}
	path := tmp.Name()
	defer os.Remove(path)
	if _, err := tmp.WriteString(stringAttr(request, "script")); err != nil {
		tmp.Close()
		return model.Response{}, err
	}
	if err := tmp.Close(); err != nil {
		return model.Response{}, err
	}
	abs, _ := filepath.Abs(path)
	args := []string{"--yes", "@tracetest/playwright-engine", "--scriptPath", abs, "--url", stringAttr(request, "target"), "--method", defaultString(stringAttr(request, "method"), "GET")}
	if trace != nil {
		args = append(args, "--traceId", string(trace.TraceID), "--spanId", trace.SpanID)
	}
	out, err := exec.CommandContext(ctx, "npx", args...).CombinedOutput()
	if err != nil {
		return model.Response{}, fmt.Errorf("execute playwright engine: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return model.Response{Headers: map[string][]string{}, Body: string(out), JSON: map[string]any{"success": true, "out": string(out)}}, nil
}

func stringAttr(r model.TriggerRequest, name string) string {
	value, _ := r.Attributes[name].(string)
	return value
}
func boolAttr(r model.TriggerRequest, name string) bool {
	value, _ := r.Attributes[name].(bool)
	return value
}
func defaultString(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
func stringMap(value any) map[string]string {
	out := map[string]string{}
	if values, ok := value.(map[string]any); ok {
		for k, v := range values {
			out[k] = fmt.Sprint(v)
		}
	}
	return out
}
func stringSlice(value any) []string {
	var out []string
	if values, ok := value.([]any); ok {
		for _, v := range values {
			out = append(out, fmt.Sprint(v))
		}
	}
	return out
}
