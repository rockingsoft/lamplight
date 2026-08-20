package render

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"tracetest/internal/model"
	"tracetest/internal/result"
)

func TestRenderersConsumeSameRedactedResult(t *testing.T) {
	run := renderRun()
	redactor := result.NewRedactor("token-value")
	jsonOutput, err := NewJSONRenderer(redactor).Render(run)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(jsonOutput), "token-value") {
		t.Fatalf("JSON leaked secret: %s", jsonOutput)
	}
	var document map[string]any
	if err := json.Unmarshal(jsonOutput, &document); err != nil {
		t.Fatal(err)
	}
	step := document["tests"].([]any)[0].(map[string]any)["steps"].([]any)[0].(map[string]any)
	if _, found := step["duration_ms"]; found {
		fatalf := "JSON must follow v1 schema, got %#v"
		t.Fatalf(fatalf, step)
	}
	textOutput, err := NewTextRenderer(redactor).Render(run)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(textOutput), "\x1b[") || strings.Contains(string(textOutput), "token-value") {
		t.Fatalf("text output is not stable/redacted: %q", textOutput)
	}
	prettyOutput, err := NewPrettyRenderer(false, redactor).Render(run)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(prettyOutput), "\x1b[") || !strings.Contains(string(prettyOutput), "FAILED") {
		t.Fatalf("unexpected pretty output: %q", prettyOutput)
	}
}

func TestPrettyColorIsOptIn(t *testing.T) {
	run := renderRun()
	output, err := NewPrettyRenderer(true).Render(run)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(output), "\x1b[") {
		t.Fatalf("color-enabled renderer emitted no ANSI: %q", output)
	}
}

func renderRun() model.RunResult {
	return model.RunResult{
		SchemaVersion: 1, RunID: "run", Status: model.StatusFailed,
		StartedAt: time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC), DurationMS: 5,
		Summary: model.RunSummary{TestsTotal: 1, TestsFailed: 1},
		Tests: []model.TestResult{{
			Name: "login", Tags: []string{"smoke"}, File: "login.hcl", Status: model.StatusFailed, DurationMS: 5,
			Steps: []model.StepResult{{
				Name: "request", ExecutionID: "step", Status: model.StatusFailed, DurationMS: 5,
				Request: &model.HTTPRequest{Headers: map[string]string{"authorization": "Bearer token-value"}},
				Checks:  []model.CheckResult{{Name: "response", Status: model.StatusFailed, Reason: "token-value"}},
			}},
		}},
	}
}
