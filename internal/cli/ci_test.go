package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"lamplight/internal/engine"
	"lamplight/internal/model"
	"lamplight/internal/result"
)

func TestIsCIEnvironmentRecognizesStandardRunnerVariables(t *testing.T) {
	for _, name := range standardCIEnvironmentVariables {
		t.Run(name, func(t *testing.T) {
			if !isCIEnvironment(func(candidate string) string {
				if candidate == name {
					return "true"
				}
				return ""
			}) {
				t.Fatalf("%s was not detected", name)
			}
		})
	}
	if isCIEnvironment(func(string) string { return "false" }) {
		t.Fatal("false values must not enable CI mode")
	}
}

func TestCIRunProgressUsesStablePlaywrightStyleLines(t *testing.T) {
	var output bytes.Buffer
	progress := newCIRunProgress(&output, result.NewRedactor())
	progress.Report(engine.ProgressEvent{Kind: engine.ProgressRunStarted, TestsTotal: 2})
	progress.Report(engine.ProgressEvent{Kind: engine.ProgressTestStarted, TestName: "import"})
	progress.Report(engine.ProgressEvent{Kind: engine.ProgressRemoteTrigger, RemotePhase: "running", CompletedShards: 1, TotalShards: 4, Elapsed: 30 * time.Second})
	progress.Report(engine.ProgressEvent{Kind: engine.ProgressTraceObserved, Checks: []engine.ProgressCheck{
		{Name: "worker called", Status: model.StatusFailed, Reason: "count_not_satisfied", Rule: model.QuantityRule{Kind: "at_least", Value: 1}},
	}})
	progress.Report(engine.ProgressEvent{Kind: engine.ProgressTestCompleted, TestName: "import", Status: model.StatusFailed, DurationMS: 1_250})
	progress.Report(engine.ProgressEvent{Kind: engine.ProgressTestStarted, TestName: "list"})
	progress.Report(engine.ProgressEvent{Kind: engine.ProgressTestCompleted, TestName: "list", Status: model.StatusPassed, DurationMS: 50})

	text := output.String()
	for _, expected := range []string{"Running 2 tests", "Cloud Run k6 running · 1/4 shards completed · elapsed 30s", "✗ 1 import (1.2s)", "worker called", "Expected at least 1 matching spans", "✓ 2 list (0.1s)"} {
		if !strings.Contains(text, expected) {
			t.Errorf("CI progress missing %q:\n%s", expected, text)
		}
	}
	if strings.Contains(text, "\x1b[") || strings.Contains(text, "attempt") {
		t.Fatalf("CI progress must be ANSI-free and omit polling attempts: %q", text)
	}
}

func TestRunProgressOmitsPartialUpdatesWithoutTTY(t *testing.T) {
	var output bytes.Buffer
	progress := newParallelRunProgress(&output, result.NewRedactor())
	progress.Report(engine.ProgressEvent{Kind: engine.ProgressRunStarted, TestsTotal: 2})
	progress.Report(engine.ProgressEvent{Kind: engine.ProgressTestStarted, TestName: "alpha"})
	progress.Report(engine.ProgressEvent{Kind: engine.ProgressStepStarted, TestName: "alpha", StepName: "shared step"})
	progress.Report(engine.ProgressEvent{Kind: engine.ProgressStepCompleted, TestName: "alpha", StepName: "shared step", Status: model.StatusPassed, DurationMS: 50})
	progress.Report(engine.ProgressEvent{Kind: engine.ProgressTestCompleted, TestName: "beta", Status: model.StatusPassed, DurationMS: 100})
	progress.Report(engine.ProgressEvent{Kind: engine.ProgressTestCompleted, TestName: "alpha", Status: model.StatusFailed, DurationMS: 200})
	progress.Report(engine.ProgressEvent{Kind: engine.ProgressRunCompleted})
	if got := output.String(); got != "" {
		t.Fatalf("non-TTY progress must be silent before the final summary: %q", got)
	}
}

func TestRunProgressShowsOneAnimatedRowPerTestAtAnyWorkerCount(t *testing.T) {
	var output bytes.Buffer
	progress := newParallelRunProgress(&output, result.NewRedactor())
	progress.terminal = true
	progress.Report(engine.ProgressEvent{Kind: engine.ProgressRunStarted, TestsTotal: 2})
	progress.Report(engine.ProgressEvent{Kind: engine.ProgressTestStarted, TestName: "alpha"})
	progress.Report(engine.ProgressEvent{Kind: engine.ProgressStepStarted, TestName: "alpha", StepName: "request"})
	progress.Report(engine.ProgressEvent{Kind: engine.ProgressTestStarted, TestName: "beta"})
	progress.Report(engine.ProgressEvent{Kind: engine.ProgressTestCompleted, TestName: "beta", Status: model.StatusFailed, DurationMS: 100})
	progress.Report(engine.ProgressEvent{Kind: engine.ProgressTestCompleted, TestName: "alpha", Status: model.StatusPassed, DurationMS: 200})
	progress.Report(engine.ProgressEvent{Kind: engine.ProgressRunCompleted})
	got := output.String()
	for _, want := range []string{"\x1b[2A", "alpha · request", "beta (0.1s)", "alpha (0.2s)", "✓", "✗"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in %q", want, got)
		}
	}
}
