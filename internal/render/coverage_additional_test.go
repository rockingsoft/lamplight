package render

import (
	"strings"
	"testing"
	"time"

	"lamplight/internal/model"
	"lamplight/internal/result"
)

func TestTextRendererWritesDiagnosticsEvidenceAndArtifacts(t *testing.T) {
	run := model.RunResult{
		RunID: "run-secret", Status: model.StatusError,
		StartedAt: time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC), DurationMS: 9,
		Summary: model.RunSummary{TestsTotal: 1, TestsPassed: 0, TestsFailed: 0, Errors: 1},
		Tests: []model.TestResult{{
			Name: "test", File: "file.hcl", Status: model.StatusError, DurationMS: 8,
			Error: &model.Diagnostic{Severity: "error", Code: "E_TEST", Message: "secret"},
			Steps: []model.StepResult{{
				Name: "step", ExecutionID: "exec", Status: model.StatusCancelled, TraceID: "trace-secret", DurationMS: 7,
				Error: &model.Diagnostic{Severity: "warning", Code: "E_STEP", Message: "step secret"},
				Checks: []model.CheckResult{{
					Name: "check", Status: model.StatusFailed, Reason: "secret",
					ResponseEvidence: []model.AssertionEvidence{
						{Name: "ok", Passed: true, Value: map[string]any{"value": "secret"}},
						{Name: "bad", Passed: false, Error: "secret"},
						{Name: "unsupported", Passed: false, Value: func() {}},
					},
					SpanEvidence: &model.SpanEvidence{Rule: model.QuantityRule{Kind: "at_most", Value: 1}, MatchCount: 2, Reason: "secret", Assertions: []model.AssertionEvidence{{Name: "span", Passed: false, Value: "secret"}}},
				}},
			}},
		}},
		Artifacts: []model.ArtifactReference{{Kind: "run", Path: "/tmp/secret"}},
	}
	output, err := NewTextRenderer(result.NewRedactor("secret")).Render(run)
	if err != nil {
		t.Fatal(err)
	}
	text := string(output)
	for _, expected := range []string{"diagnostic severity=error", "diagnostic severity=warning", "response evidence", "value=", "value_error=", "spans rule=at_most:1 match_count=2", "artifact kind=run"} {
		if !strings.Contains(text, expected) {
			t.Errorf("text output missing %q:\n%s", expected, text)
		}
	}
	if strings.Contains(text, "secret") {
		t.Fatalf("text leaked secret: %s", text)
	}
}

func TestPrettyMarkersCoverStatusesAndColorModes(t *testing.T) {
	renderer := NewPrettyRenderer(false)
	for _, test := range []struct {
		status model.Status
		want   string
	}{
		{model.StatusPassed, "✓"}, {model.StatusFailed, "✗"}, {model.StatusError, "!"},
		{model.StatusCancelled, "■"}, {model.StatusSkipped, "-"}, {model.Status("unknown"), "?"},
	} {
		if got := renderer.statusMarker(test.status); got != test.want {
			t.Errorf("marker(%q) = %q, want %q", test.status, got, test.want)
		}
	}
	color := NewPrettyRenderer(true)
	for _, status := range []model.Status{model.StatusPassed, model.StatusFailed, model.StatusError, model.StatusCancelled, model.StatusSkipped, model.Status("unknown")} {
		if got := color.statusMarker(status); !strings.Contains(got, "\x1b[") || !strings.Contains(got, color.statusMarkerWithoutColor(status)) {
			t.Errorf("colored marker(%q) = %q", status, got)
		}
	}
	if renderer.statusLabel(model.StatusCancelled) != "CANCELLED" {
		t.Fatal("statusLabel did not uppercase status")
	}
}

func TestPrettyRendererKeepsErrorSummaryCompactAndRedacted(t *testing.T) {
	run := model.RunResult{
		RunID: "run", Status: model.StatusError, StartedAt: time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC),
		Summary: model.RunSummary{Errors: 1},
		Tests: []model.TestResult{{
			Name: "run", Status: model.StatusError,
			Error: &model.Diagnostic{Code: "datasource_connection", Message: "lookup secret: no such host"},
			Steps: []model.StepResult{{
				Name: "request", Status: model.StatusError,
				Error: &model.Diagnostic{Code: "http_execution", Message: "connect: connection refused"},
			}},
		}},
	}

	output, err := NewPrettyRenderer(false, result.NewRedactor("secret")).Render(run)
	if err != nil {
		t.Fatal(err)
	}
	text := string(output)
	for _, expected := range []string{"ERROR · 1 errors", "Tests", "! run"} {
		if !strings.Contains(text, expected) {
			t.Errorf("pretty output missing %q:\n%s", expected, text)
		}
	}
	if strings.Contains(text, "secret") {
		t.Fatalf("pretty output leaked secret: %s", text)
	}
}

func TestFriendlyDiagnosticUsesAuthoredSuggestionAndFallback(t *testing.T) {
	summary, suggestion := friendlyDiagnostic(&model.Diagnostic{Code: "datasource_connection", Message: "unknown", Suggestion: "fix it this way"})
	if summary == "" || suggestion != "fix it this way" {
		t.Fatalf("friendlyDiagnostic() = %q, %q", summary, suggestion)
	}
	summary, suggestion = friendlyDiagnostic(&model.Diagnostic{Code: "custom", Message: "boom"})
	if summary != "Run error [custom]." || suggestion != "" {
		t.Fatalf("fallback = %q, %q", summary, suggestion)
	}
}

func TestFriendlyDiagnosticExplainsUnavailableKafkaBroker(t *testing.T) {
	summary, suggestion := friendlyDiagnostic(&model.Diagnostic{
		Code:    "trigger_execution",
		Message: "create kafka producer: kafka: client has run out of available brokers to talk to: dial tcp [::1]:9092: connect: connection refused",
	})
	if summary != "Lamplight could not connect to a Kafka broker." {
		t.Fatalf("summary = %q", summary)
	}
	for _, expected := range []string{"Kafka is running", "host-published", "different host port"} {
		if !strings.Contains(suggestion, expected) {
			t.Errorf("suggestion %q missing %q", suggestion, expected)
		}
	}
}

func TestPrettyRendererShowsCancelledTestInCompactSummary(t *testing.T) {
	run := model.RunResult{
		RunID: "run", Status: model.StatusCancelled, StartedAt: time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC),
		Summary: model.RunSummary{Errors: 1},
		Tests: []model.TestResult{{
			Name: "cancelled test", Status: model.StatusCancelled,
			Steps: []model.StepResult{{
				Name: "request", Status: model.StatusCancelled,
				Checks: []model.CheckResult{
					{Name: "already passed", Status: model.StatusPassed},
					{Name: "still pending", Status: model.StatusCancelled, Reason: "cancelled"},
				},
			}},
		}},
	}

	output, err := NewPrettyRenderer(false).Render(run)
	if err != nil {
		t.Fatal(err)
	}
	text := string(output)
	for _, expected := range []string{"CANCELLED", "Tests", "■ cancelled test"} {
		if !strings.Contains(text, expected) {
			t.Errorf("pretty output missing %q:\n%s", expected, text)
		}
	}
	if strings.Contains(text, "already passed") || strings.Contains(text, "still pending") {
		t.Fatalf("compact summary should not include check details:\n%s", text)
	}
	colored, err := NewPrettyRenderer(true).Render(run)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(colored), NewPrettyRenderer(true).statusMarker(model.StatusCancelled)+" cancelled test") {
		t.Fatalf("cancelled test did not use the warning-colored marker: %q", colored)
	}
}

func (r *PrettyRenderer) statusMarkerWithoutColor(status model.Status) string {
	copy := *r
	copy.Color = false
	return copy.statusMarker(status)
}

func TestNewSelectsFormatsAndRejectsUnknown(t *testing.T) {
	redactor := result.NewRedactor("secret")
	for _, format := range []Format{FormatJSON, FormatText, FormatPretty} {
		renderer, err := New(format, redactor)
		if err != nil || renderer == nil {
			t.Fatalf("New(%q) = %#v, err = %v", format, renderer, err)
		}
	}
	if renderer, err := New(Format("yaml")); err == nil || renderer != nil || !strings.Contains(err.Error(), "unsupported output format") {
		t.Fatalf("unknown format = %#v, err = %v", renderer, err)
	}
	if _, err := NewJSONRenderer().Render(model.RunResult{}); err != nil {
		t.Fatal(err)
	}
}
