// Package trigger executes Lamplight's non-HTTP triggers. Its trigger set and
// wire semantics follow Tracetest's backend trigger implementations.
package trigger

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/IBM/sarama"
	"lamplight/internal/debuglog"
	"lamplight/internal/model"
)

type commandFunc func(context.Context, string, ...string) *exec.Cmd

type Executor struct {
	HTTP     model.HTTPExecutor
	command  commandFunc
	lookPath func(string) (string, error)
}

func New(http model.HTTPExecutor) *Executor {
	return &Executor{HTTP: http, command: exec.CommandContext, lookPath: exec.LookPath}
}

func (e *Executor) Execute(ctx context.Context, request model.TriggerRequest, cfg model.HTTPClientConfig, trace *model.TestTraceContext) (model.Response, error) {
	debuglog.Debug(ctx, "trigger execution started", "kind", request.Kind)
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
	case model.TriggerK6:
		if _, legacy := request.Attributes["id"]; !legacy {
			return e.executeK6(ctx, request, cfg, trace)
		}
		fallthrough
	case model.TriggerTraceID, model.TriggerCypress, model.TriggerPlaywright, model.TriggerArtillery:
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

const defaultK6OutputLimit = int64(10 << 20)

func (e *Executor) executeK6(ctx context.Context, request model.TriggerRequest, cfg model.HTTPClientConfig, trace *model.TestTraceContext) (model.Response, error) {
	script := stringAttr(request, "script")
	if script == "" {
		return model.Response{}, errors.New("k6.script is required")
	}
	lookPath := e.lookPath
	if lookPath == nil {
		lookPath = exec.LookPath
	}
	k6Path, err := lookPath("k6")
	if err != nil {
		return model.Response{}, errors.New("k6 executable not found in PATH; install k6 and ensure the k6 binary is available")
	}
	summary, err := os.CreateTemp("", "lamplight-k6-summary-*.json")
	if err != nil {
		return model.Response{}, fmt.Errorf("create k6 summary file: %w", err)
	}
	summaryPath := summary.Name()
	if err := summary.Close(); err != nil {
		_ = os.Remove(summaryPath)
		return model.Response{}, fmt.Errorf("close k6 summary file: %w", err)
	}
	defer func() { _ = os.Remove(summaryPath) }()

	args := []string{"run", "--summary-export", summaryPath}
	arguments, err := k6Arguments(request.Attributes["arguments"])
	if err != nil {
		return model.Response{}, err
	}
	args = append(args, arguments...)
	args = append(args, filepath.Base(script))
	command := e.command
	if command == nil {
		command = exec.CommandContext
	}
	cmd := command(ctx, k6Path, args...)
	cmd.Dir = filepath.Dir(script)
	cmd.Env = k6Environment(request, trace)
	limit := cfg.MaxResponseBodyBytes
	if limit <= 0 {
		limit = defaultK6OutputLimit
	}
	stdout, stderr := newCappedBuffer(limit), newCappedBuffer(limit)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	runErr := cmd.Run()
	exitCode := 0
	if runErr != nil {
		var exit *exec.ExitError
		if errors.As(runErr, &exit) {
			exitCode = exit.ExitCode()
		} else {
			return model.Response{}, fmt.Errorf("execute k6: %w", runErr)
		}
	}

	result := map[string]any{"exit_code": exitCode, "stdout": stdout.String(), "stderr": stderr.String()}
	if encoded, readErr := os.ReadFile(summaryPath); readErr == nil && len(encoded) > 0 {
		if int64(len(encoded)) > limit {
			return model.Response{}, fmt.Errorf("k6 summary exceeds %d bytes", limit)
		}
		var decoded any
		if err := json.Unmarshal(encoded, &decoded); err != nil {
			return model.Response{}, fmt.Errorf("decode k6 summary: %w", err)
		}
		result["summary"] = decoded
	} else if runErr == nil {
		if readErr != nil {
			return model.Response{}, fmt.Errorf("read k6 summary: %w", readErr)
		}
		return model.Response{}, errors.New("k6 completed without writing a summary")
	}
	response := model.Response{StatusCode: exitCode, Headers: map[string][]string{}, Body: stdout.String(), JSON: result}
	if runErr != nil {
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = strings.TrimSpace(stdout.String())
		}
		return response, fmt.Errorf("k6 exited with code %d: %s", exitCode, message)
	}
	return response, nil
}

func k6Arguments(value any) ([]string, error) {
	if value == nil {
		return nil, nil
	}
	values, ok := value.(map[string]any)
	if !ok {
		return nil, errors.New("k6.arguments must be a map")
	}
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	arguments := []string{}
	for _, name := range names {
		flagName := strings.ReplaceAll(strings.TrimSpace(name), "_", "-")
		if flagName == "" || strings.HasPrefix(flagName, "-") {
			return nil, fmt.Errorf("k6.arguments key %q must be a flag name without leading dashes", name)
		}
		if flagName == "summary-export" {
			return nil, errors.New("k6.arguments cannot override Lamplight-managed summary_export")
		}
		flag := "--" + flagName
		appendValue := func(raw any) error {
			switch typed := raw.(type) {
			case bool:
				if typed {
					arguments = append(arguments, flag)
				}
			case string, float64:
				arguments = append(arguments, flag+"="+fmt.Sprint(typed))
			default:
				return fmt.Errorf("k6.arguments.%s must be a string, number, boolean, or list of those values", name)
			}
			return nil
		}
		if repeated, ok := values[name].([]any); ok {
			for _, item := range repeated {
				if err := appendValue(item); err != nil {
					return nil, err
				}
			}
			continue
		}
		if err := appendValue(values[name]); err != nil {
			return nil, err
		}
	}
	return arguments, nil
}

func k6Environment(request model.TriggerRequest, trace *model.TestTraceContext) []string {
	values := map[string]string{}
	for _, entry := range os.Environ() {
		name, value, ok := strings.Cut(entry, "=")
		if ok {
			values[name] = value
		}
	}
	for name, value := range stringMap(request.Attributes["env"]) {
		if name != "" && !strings.Contains(name, "=") {
			values[name] = value
		}
	}
	if trace != nil {
		values["LAMPLIGHT_TRACEPARENT"] = trace.TraceParent()
		values["LAMPLIGHT_TRACESTATE"] = trace.TraceState
	}
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	environment := make([]string, 0, len(names))
	for _, name := range names {
		environment = append(environment, name+"="+values[name])
	}
	return environment
}

type cappedBuffer struct {
	mu        sync.Mutex
	buffer    bytes.Buffer
	limit     int64
	truncated bool
}

func newCappedBuffer(limit int64) *cappedBuffer { return &cappedBuffer{limit: limit} }

func (b *cappedBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	original := len(data)
	remaining := b.limit - int64(b.buffer.Len())
	if remaining <= 0 {
		b.truncated = b.truncated || original > 0
		return original, nil
	}
	if int64(len(data)) > remaining {
		data = data[:remaining]
		b.truncated = true
	}
	_, _ = b.buffer.Write(data)
	return original, nil
}

func (b *cappedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	value := b.buffer.String()
	if b.truncated {
		value += "\n[output truncated]"
	}
	return value
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
	defer func() { _ = producer.Close() }()
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
	defer func() { _ = os.Remove(path) }()
	if _, err := tmp.WriteString(stringAttr(request, "script")); err != nil {
		_ = tmp.Close()
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
