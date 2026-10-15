package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/hashicorp/hcl/v2"
	"github.com/zclconf/go-cty/cty"
	"lamplight/internal/expr"
	"lamplight/internal/model"
	"lamplight/internal/requesthook"
)

func runRequestHook(ctx context.Context, hook *model.RequestHookDefinition, project *model.Project, steps, params cty.Value, request any, replacement any) error {
	evalContext := &hcl.EvalContext{Variables: map[string]cty.Value{"var": expr.Variables(project.Variables), "steps": steps, "params": params}, Functions: expr.Functions()}
	value, diags := expr.Evaluate(hook.Command, evalContext)
	if diags.HasErrors() || !value.IsKnown() || value.IsNull() || !(value.Type().IsListType() || value.Type().IsTupleType()) {
		return fmt.Errorf("request_hook.command must evaluate to a list of strings: %s", diags.Error())
	}
	command := []string{}
	iterator := value.ElementIterator()
	for iterator.Next() {
		_, item := iterator.Element()
		if !item.IsKnown() || item.IsNull() || item.Type() != cty.String {
			return fmt.Errorf("request_hook.command must evaluate to a list of strings")
		}
		command = append(command, item.AsString())
	}
	env := map[string]string{}
	for name, expression := range hook.Env {
		value, ds := expr.Evaluate(expression, evalContext)
		if ds.HasErrors() || !value.IsKnown() || value.IsNull() || value.Type() != cty.String {
			return fmt.Errorf("request_hook.env.%s must evaluate to a string: %s", name, ds.Error())
		}
		env[name] = value.AsString()
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		return fmt.Errorf("encode request_hook input: %w", err)
	}
	timeout := project.HTTPClient.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	hookCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	base := "."
	if project.Definition != nil && project.Definition.BaseDir != "" {
		base = project.Definition.BaseDir
	}
	output, err := requesthook.Run(hookCtx, base, command, env, encoded)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(output))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(replacement); err != nil {
		return fmt.Errorf("decode request_hook output: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return fmt.Errorf("request_hook stdout must contain exactly one JSON object")
	}
	return nil
}

func applyHTTPHook(ctx context.Context, hook *model.RequestHookDefinition, project *model.Project, steps, params cty.Value, request model.HTTPRequest, trace *model.TestTraceContext) (model.HTTPRequest, error) {
	request.Headers = canonicalTraceHeaders(request.Headers, trace)
	var changed model.HTTPRequest
	input := map[string]any{"method": request.Method, "url": request.URL, "headers": request.Headers, "body": request.Body}
	if err := runRequestHook(ctx, hook, project, steps, params, input, &changed); err != nil {
		return request, err
	}
	if changed.Method == "" || changed.URL == "" {
		return request, fmt.Errorf("request_hook output requires method and url")
	}
	if err := checkTraceHeaders(changed.Headers, trace); err != nil {
		return request, err
	}
	return changed, nil
}

func applyTriggerHook(ctx context.Context, hook *model.RequestHookDefinition, project *model.Project, steps, params cty.Value, request model.TriggerRequest, trace *model.TestTraceContext) (model.TriggerRequest, error) {
	field := "headers"
	if request.Kind == model.TriggerGRPC {
		field = "metadata"
	}
	request.Attributes[field] = canonicalTraceAttributes(request.Attributes[field], trace)
	var changed map[string]any
	if err := runRequestHook(ctx, hook, project, steps, params, request.Attributes, &changed); err != nil {
		return request, err
	}
	if changed == nil {
		return request, fmt.Errorf("request_hook output must be a request object")
	}
	if err := validateTriggerHookOutput(request.Kind, changed); err != nil {
		return request, err
	}
	if err := checkTraceAttributes(changed[field], trace); err != nil {
		return request, err
	}
	request.Attributes = changed
	return request, nil
}

func canonicalTraceHeaders(headers map[string]string, trace *model.TestTraceContext) map[string]string {
	clean := map[string]string{}
	for key, value := range headers {
		if !isTraceHeader(key) {
			clean[key] = value
		}
	}
	if trace != nil {
		clean["traceparent"] = trace.TraceParent()
		if trace.TraceState != "" {
			clean["tracestate"] = trace.TraceState
		}
	}
	return clean
}

func canonicalTraceAttributes(raw any, trace *model.TestTraceContext) map[string]any {
	clean := map[string]any{}
	if headers, ok := raw.(map[string]any); ok {
		for key, value := range headers {
			if !isTraceHeader(key) {
				clean[key] = value
			}
		}
	}
	if trace != nil {
		clean["traceparent"] = trace.TraceParent()
		if trace.TraceState != "" {
			clean["tracestate"] = trace.TraceState
		}
	}
	return clean
}

func checkTraceHeaders(headers map[string]string, trace *model.TestTraceContext) error {
	values := map[string]string{}
	for key, value := range headers {
		if isTraceHeader(key) {
			lower := strings.ToLower(key)
			if _, duplicate := values[lower]; duplicate {
				return fmt.Errorf("request_hook cannot duplicate %s", lower)
			}
			values[lower] = value
		}
	}
	if trace == nil {
		if len(values) != 0 {
			return fmt.Errorf("request_hook cannot set trace headers")
		}
		return nil
	}
	if values["traceparent"] != trace.TraceParent() || values["tracestate"] != trace.TraceState {
		return fmt.Errorf("request_hook cannot change trace headers")
	}
	return nil
}

func checkTraceAttributes(raw any, trace *model.TestTraceContext) error {
	headers := map[string]string{}
	values, ok := raw.(map[string]any)
	if !ok {
		return fmt.Errorf("request_hook headers or metadata must be an object")
	}
	for key, value := range values {
		if !isTraceHeader(key) {
			continue
		}
		text, ok := value.(string)
		if !ok {
			return fmt.Errorf("request_hook trace header %s must be a string", key)
		}
		headers[key] = text
	}
	return checkTraceHeaders(headers, trace)
}

func isTraceHeader(key string) bool {
	return strings.EqualFold(key, "traceparent") || strings.EqualFold(key, "tracestate")
}

func validateTriggerHookOutput(kind model.TriggerKind, attributes map[string]any) error {
	var allowed map[string]string
	var required []string
	switch kind {
	case model.TriggerGraphQL:
		allowed = map[string]string{"url": "string", "query": "string", "variables": "object", "operation_name": "string", "headers": "headers"}
		required = []string{"url", "query"}
	case model.TriggerGRPC:
		allowed = map[string]string{"protobuf": "string", "address": "string", "method": "string", "request": "string", "metadata": "headers"}
		required = []string{"protobuf", "address", "method", "request"}
	case model.TriggerKafka:
		allowed = map[string]string{"broker_urls": "strings", "topic": "string", "message_value": "string", "message_key": "string", "headers": "headers", "username": "string", "password": "string", "tls": "bool"}
		required = []string{"broker_urls", "topic", "message_value"}
	default:
		return fmt.Errorf("request_hook does not support %s", kind)
	}
	for _, name := range required {
		if _, exists := attributes[name]; !exists {
			return fmt.Errorf("request_hook output requires %s", name)
		}
	}
	for name, value := range attributes {
		typeName, exists := allowed[name]
		if !exists {
			return fmt.Errorf("request_hook output contains unsupported field %s", name)
		}
		valid := false
		switch typeName {
		case "string":
			_, valid = value.(string)
		case "bool":
			_, valid = value.(bool)
		case "object":
			_, valid = value.(map[string]any)
		case "headers":
			if fields, ok := value.(map[string]any); ok {
				valid = true
				for _, field := range fields {
					if _, ok := field.(string); !ok {
						valid = false
					}
				}
			}
		case "strings":
			if values, ok := value.([]any); ok {
				valid = true
				for _, item := range values {
					if _, ok := item.(string); !ok {
						valid = false
					}
				}
			}
		}
		if !valid {
			return fmt.Errorf("request_hook output field %s must be %s", name, typeName)
		}
	}
	return nil
}
