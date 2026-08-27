package cli

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path/filepath"

	"github.com/hashicorp/hcl/v2"
	"lamplight/internal/config"
	"lamplight/internal/datasource"
	"lamplight/internal/hclloader"
	"lamplight/internal/mcpserver"
	"lamplight/internal/model"
	"lamplight/internal/result"
	"lamplight/internal/runtimevars"
)

const maxMCPTraceSpans = 500

func observeTraceForMCP(ctx context.Context, options config.Options, request mcpserver.ObserveTraceRequest) (mcpserver.TraceEvidence, error) {
	out := mcpserver.TraceEvidence{TraceID: request.TraceID}
	if len(request.TraceID) != 32 {
		return out, errors.New("trace_id must be a 32-character hexadecimal value")
	}
	if _, err := hex.DecodeString(request.TraceID); err != nil {
		return out, errors.New("trace_id must be a 32-character hexadecimal value")
	}
	definition, diags := (hclloader.Loader{}).LoadProject(options)
	if hasDiagnosticErrors(diags) {
		return out, fmt.Errorf("project validation failed: %s", firstDiagnosticMessage(diags))
	}
	if definition == nil || definition.Datasource == nil {
		return out, errors.New("project has no configured datasource")
	}
	target, ok := selectTarget(definition, request.Target)
	if !ok {
		return out, fmt.Errorf("target %q is not declared", request.Target)
	}
	targetValues, targetDiags := evaluateTargetVariables(target, definition.Variables)
	if hasDiagnosticErrors(targetDiags) {
		return out, fmt.Errorf("target variables are invalid: %s", firstDiagnosticMessage(targetDiags))
	}
	expressions := datasourceExpressions(definition.Datasource)
	values, variableDiags := runtimevars.Resolve(definition.Variables, runtimevars.Input{Vars: request.Variables, Target: targetValues}, expressions...)
	if hasDiagnosticErrors(variableDiags) {
		return out, fmt.Errorf("runtime variables are invalid: %s", firstDiagnosticMessage(variableDiags))
	}
	datasourceConfig, err := resolveDatasourceConfig(definition.Datasource, values)
	if err != nil {
		return out, fmt.Errorf("resolve datasource: %w", err)
	}

	var observation model.TraceObservation
	if target.Runtime == "local" {
		store, err := datasource.New(datasourceConfig)
		if err != nil {
			return out, err
		}
		if closer, ok := store.(io.Closer); ok {
			defer func() { _ = closer.Close() }()
		}
		observation, err = store.Observe(ctx, model.TraceID(request.TraceID))
		if err != nil {
			return out, err
		}
	} else {
		client, closeExecutor, err := startRemoteExecutor(ctx, target, filepath.Dir(definition.ConfigPath), &datasourceConfig, nil, io.Discard)
		if err != nil {
			return out, err
		}
		observation, err = client.Observe(ctx, model.TraceID(request.TraceID))
		closeErr := closeExecutor()
		if err != nil {
			return out, err
		}
		if closeErr != nil {
			return out, closeErr
		}
	}

	redactor := result.NewRedactor(sensitiveStrings(values)...)
	out.Found, out.Valid, out.Partial, out.Complete = observation.Found, observation.Valid, observation.Partial, observation.Complete
	out.Fingerprint = redactor.RedactString(observation.Fingerprint)
	out.SpanCount = len(observation.Spans)
	spans := observation.Spans
	if len(spans) > maxMCPTraceSpans {
		spans = spans[:maxMCPTraceSpans]
		out.Truncated = true
	}
	out.Spans = make([]model.Span, len(spans))
	for index, span := range spans {
		span.Name = redactor.RedactString(span.Name)
		span.StatusMessage = redactor.RedactString(span.StatusMessage)
		span.Attributes = redactedMap(redactor, span.Attributes)
		span.Resource = redactedMap(redactor, span.Resource)
		out.Spans[index] = span
	}
	return out, nil
}

func datasourceExpressions(definition *model.DatasourceDefinition) []hcl.Expression {
	expressions := []hcl.Expression{definition.Endpoint, definition.BearerToken}
	for _, expression := range definition.Headers {
		expressions = append(expressions, expression)
	}
	return expressions
}

func redactedMap(redactor result.Redactor, values map[string]any) map[string]any {
	if values == nil {
		return nil
	}
	redacted, _ := redactor.RedactValue(values).(map[string]any)
	return redacted
}

func hasDiagnosticErrors(diags []model.Diagnostic) bool {
	for _, item := range diags {
		if item.Severity == "error" {
			return true
		}
	}
	return false
}

func firstDiagnosticMessage(diags []model.Diagnostic) string {
	for _, item := range diags {
		if item.Severity == "error" {
			return item.Message
		}
	}
	return "unknown diagnostic"
}
