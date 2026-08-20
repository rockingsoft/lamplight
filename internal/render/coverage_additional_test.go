package render

import (
	"strings"
	"testing"
	"time"

	"tracetest/internal/model"
	"tracetest/internal/result"
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
