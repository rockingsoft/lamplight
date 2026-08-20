package result

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"tracetest/internal/model"
)

func TestAggregateAndExitCode(t *testing.T) {
	tests := []model.TestResult{
		{Name: "pass", Status: model.StatusPassed},
		{Name: "failure", Status: model.StatusFailed},
		{Name: "cancelled", Status: model.StatusCancelled},
	}
	status, summary := Aggregate(tests)
	if status != model.StatusCancelled {
		t.Fatalf("status = %q, want cancelled", status)
	}
	if summary != (model.RunSummary{TestsTotal: 3, TestsPassed: 1, TestsFailed: 1, Errors: 1}) {
		t.Fatalf("summary = %#v", summary)
	}
	run := model.RunResult{Status: model.StatusFailed, Tests: tests[:2]}
	run.Tests[0].Steps = []model.StepResult{{Status: model.StatusError}}
	if got := ExitCode(run); got != ExitTechnicalError {
		t.Fatalf("ExitCode() = %d, want %d", got, ExitTechnicalError)
	}
}

func TestRedactorRedactsStructuredAndKnownSecrets(t *testing.T) {
	redactor := NewRedactor("very-secret")
	value := redactor.RedactValue(map[string]any{
		"authorization": "Bearer very-secret",
		"url":           "https://example.test/?access_token=very-secret&safe=ok",
		"body":          `{"password":"very-secret","name":"ok"}`,
		"nested":        map[string]any{"api_key": "very-secret"},
	}).(map[string]any)
	if value["authorization"] != Redacted || value["nested"].(map[string]any)["api_key"] != Redacted {
		t.Fatalf("credential fields were not redacted: %#v", value)
	}
	if got := value["url"].(string); strings.Contains(got, "very-secret") || !strings.Contains(got, "access_token=%5BREDACTED%5D") {
		t.Fatalf("URL was not redacted: %s", got)
	}
	if got := value["body"].(string); strings.Contains(got, "very-secret") || !strings.Contains(got, Redacted) {
		t.Fatalf("body was not redacted: %s", got)
	}
}

func TestMarshalJSONV1OmitsInternalStepDuration(t *testing.T) {
	run := sampleRun()
	encoded, err := NewRedactor("secret").MarshalJSONV1(run, "")
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(encoded, &document); err != nil {
		t.Fatal(err)
	}
	step := document["tests"].([]any)[0].(map[string]any)["steps"].([]any)[0].(map[string]any)
	if _, found := step["duration_ms"]; found {
		t.Fatalf("schema-incompatible step duration_ms in %#v", step)
	}
	if got := step["request"].(map[string]any)["headers"].(map[string]any)["authorization"]; got != Redacted {
		t.Fatalf("authorization = %#v, want %q", got, Redacted)
	}
}

func sampleRun() model.RunResult {
	return model.RunResult{
		SchemaVersion: 1,
		RunID:         "run-1",
		Status:        model.StatusPassed,
		StartedAt:     time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC),
		DurationMS:    42,
		Summary:       model.RunSummary{TestsTotal: 1, TestsPassed: 1},
		Tests: []model.TestResult{{
			Name: "one", Tags: []string{"smoke"}, File: "test.hcl", Status: model.StatusPassed, DurationMS: 42,
			Steps: []model.StepResult{{
				Name: "request", ExecutionID: "step-1", Status: model.StatusPassed, DurationMS: 42,
				Request: &model.HTTPRequest{Headers: map[string]string{"authorization": "Bearer secret"}},
				Checks:  []model.CheckResult{},
			}},
		}},
	}
}
