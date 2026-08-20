// Package render turns a completed RunResult into JSON or human-readable
// output. It never changes run semantics or computes a new aggregate result.
package render

import (
	"bytes"
	"fmt"
	"strings"

	"tracetest/internal/model"
	"tracetest/internal/result"
)

// Format is one of the output formats accepted by the MVP.
type Format string

const (
	FormatJSON   Format = "json"
	FormatText   Format = "text"
	FormatPretty Format = "pretty"
)

// JSONRenderer renders the frozen JSON v1 projection of RunResult.
type JSONRenderer struct{ Redactor result.Redactor }

func NewJSONRenderer(redactors ...result.Redactor) *JSONRenderer {
	return &JSONRenderer{Redactor: firstRedactor(redactors)}
}

func (r *JSONRenderer) Render(run model.RunResult) ([]byte, error) {
	encoded, err := r.Redactor.MarshalJSONV1(run, "  ")
	if err != nil {
		return nil, err
	}
	return append(encoded, '\n'), nil
}

// TextRenderer renders a deterministic, ANSI-free result suitable for CI.
type TextRenderer struct{ Redactor result.Redactor }

func NewTextRenderer(redactors ...result.Redactor) *TextRenderer {
	return &TextRenderer{Redactor: firstRedactor(redactors)}
}

func (r *TextRenderer) Render(run model.RunResult) ([]byte, error) {
	var output bytes.Buffer
	fmt.Fprintf(&output, "run status=%s id=%s started_at=%s duration_ms=%d\n", run.Status, r.Redactor.RedactString(run.RunID), run.StartedAt.Format("2006-01-02T15:04:05.999999999Z07:00"), run.DurationMS)
	fmt.Fprintf(&output, "summary tests_total=%d tests_passed=%d tests_failed=%d errors=%d\n", run.Summary.TestsTotal, run.Summary.TestsPassed, run.Summary.TestsFailed, run.Summary.Errors)
	for _, test := range run.Tests {
		fmt.Fprintf(&output, "test status=%s name=%s file=%s duration_ms=%d tags=%s\n", test.Status, r.Redactor.RedactString(test.Name), r.Redactor.RedactString(test.File), test.DurationMS, strings.Join(redactStrings(r.Redactor, test.Tags), ","))
		writeDiagnostic(&output, "  ", r.Redactor, test.Error)
		for _, step := range test.Steps {
			fmt.Fprintf(&output, "  step status=%s name=%s execution_id=%s", step.Status, r.Redactor.RedactString(step.Name), r.Redactor.RedactString(step.ExecutionID))
			if step.TraceID != "" {
				fmt.Fprintf(&output, " trace_id=%s", r.Redactor.RedactString(step.TraceID))
			}
			fmt.Fprintf(&output, " duration_ms=%d\n", step.DurationMS)
			writeDiagnostic(&output, "    ", r.Redactor, step.Error)
			for _, check := range step.Checks {
				fmt.Fprintf(&output, "    check status=%s name=%s", check.Status, r.Redactor.RedactString(check.Name))
				if check.Reason != "" {
					fmt.Fprintf(&output, " reason=%s", r.Redactor.RedactString(check.Reason))
				}
				output.WriteByte('\n')
				for _, evidence := range check.ResponseEvidence {
					writeEvidence(&output, "      response", r.Redactor, evidence)
				}
				if check.SpanEvidence != nil {
					fmt.Fprintf(&output, "      spans rule=%s:%d match_count=%d", check.SpanEvidence.Rule.Kind, check.SpanEvidence.Rule.Value, check.SpanEvidence.MatchCount)
					if check.SpanEvidence.Reason != "" {
						fmt.Fprintf(&output, " reason=%s", r.Redactor.RedactString(check.SpanEvidence.Reason))
					}
					output.WriteByte('\n')
					for _, evidence := range check.SpanEvidence.Assertions {
						writeEvidence(&output, "        span", r.Redactor, evidence)
					}
				}
			}
		}
	}
	for _, artifact := range run.Artifacts {
		fmt.Fprintf(&output, "artifact kind=%s path=%s\n", r.Redactor.RedactString(artifact.Kind), r.Redactor.RedactString(artifact.Path))
	}
	return output.Bytes(), nil
}

func writeDiagnostic(output *bytes.Buffer, indent string, redactor result.Redactor, diagnostic *model.Diagnostic) {
	if diagnostic == nil {
		return
	}
	fmt.Fprintf(output, "%sdiagnostic severity=%s code=%s message=%s\n", indent, diagnostic.Severity, diagnostic.Code, redactor.RedactString(diagnostic.Message))
}

func writeEvidence(output *bytes.Buffer, prefix string, redactor result.Redactor, evidence model.AssertionEvidence) {
	fmt.Fprintf(output, "%s evidence passed=%t name=%s", prefix, evidence.Passed, redactor.RedactString(evidence.Name))
	if evidence.Error != "" {
		fmt.Fprintf(output, " error=%s", redactor.RedactString(evidence.Error))
	}
	if evidence.Value != nil {
		encoded, err := redactor.Marshal(evidence.Value, "")
		if err != nil {
			fmt.Fprintf(output, " value_error=%s", redactor.RedactString(err.Error()))
		} else {
			fmt.Fprintf(output, " value=%s", encoded)
		}
	}
	output.WriteByte('\n')
}

func redactStrings(redactor result.Redactor, values []string) []string {
	copy := make([]string, len(values))
	for i, value := range values {
		copy[i] = redactor.RedactString(value)
	}
	return copy
}

// PrettyRenderer is a human-oriented renderer. Color is opt-in so callers can
// enable it only after confirming stdout is a TTY.
type PrettyRenderer struct {
	Redactor result.Redactor
	Color    bool
}

func NewPrettyRenderer(color bool, redactors ...result.Redactor) *PrettyRenderer {
	return &PrettyRenderer{Color: color, Redactor: firstRedactor(redactors)}
}

func (r *PrettyRenderer) Render(run model.RunResult) ([]byte, error) {
	var output bytes.Buffer
	fmt.Fprintf(&output, "%s %s (%s)\n", r.statusMarker(run.Status), r.statusLabel(run.Status), r.Redactor.RedactString(run.RunID))
	fmt.Fprintf(&output, "Started %s · %d ms · %d passed, %d failed, %d errors\n", run.StartedAt.Format("2006-01-02 15:04:05Z07:00"), run.DurationMS, run.Summary.TestsPassed, run.Summary.TestsFailed, run.Summary.Errors)
	for _, test := range run.Tests {
		fmt.Fprintf(&output, "%s %s", r.statusMarker(test.Status), r.Redactor.RedactString(test.Name))
		if len(test.Tags) > 0 {
			fmt.Fprintf(&output, " [%s]", strings.Join(redactStrings(r.Redactor, test.Tags), ", "))
		}
		fmt.Fprintf(&output, " · %d ms\n", test.DurationMS)
		for _, step := range test.Steps {
			fmt.Fprintf(&output, "  %s %s · %d ms\n", r.statusMarker(step.Status), r.Redactor.RedactString(step.Name), step.DurationMS)
			for _, check := range step.Checks {
				fmt.Fprintf(&output, "    %s %s", r.statusMarker(check.Status), r.Redactor.RedactString(check.Name))
				if check.Reason != "" {
					fmt.Fprintf(&output, " — %s", r.Redactor.RedactString(check.Reason))
				}
				output.WriteByte('\n')
			}
		}
	}
	if len(run.Artifacts) > 0 {
		for _, artifact := range run.Artifacts {
			fmt.Fprintf(&output, "Artifacts: %s\n", r.Redactor.RedactString(artifact.Path))
		}
	}
	return output.Bytes(), nil
}

func (r *PrettyRenderer) statusMarker(status model.Status) string {
	marker := "?"
	switch status {
	case model.StatusPassed:
		marker = "✓"
	case model.StatusFailed:
		marker = "✗"
	case model.StatusError:
		marker = "!"
	case model.StatusCancelled:
		marker = "■"
	case model.StatusSkipped:
		marker = "-"
	}
	if !r.Color {
		return marker
	}
	code := "33"
	switch status {
	case model.StatusPassed:
		code = "32"
	case model.StatusFailed, model.StatusError:
		code = "31"
	case model.StatusCancelled:
		code = "33"
	}
	return "\x1b[" + code + "m" + marker + "\x1b[0m"
}

func (r *PrettyRenderer) statusLabel(status model.Status) string {
	return strings.ToUpper(string(status))
}

// New returns a renderer for format. Pretty starts without ANSI; the CLI can
// construct a color-enabled PrettyRenderer after checking stdout itself.
func New(format Format, redactors ...result.Redactor) (model.Renderer, error) {
	redactor := firstRedactor(redactors)
	switch format {
	case FormatJSON:
		return NewJSONRenderer(redactor), nil
	case FormatText:
		return NewTextRenderer(redactor), nil
	case FormatPretty:
		return NewPrettyRenderer(false, redactor), nil
	default:
		return nil, fmt.Errorf("unsupported output format %q", format)
	}
}

func firstRedactor(redactors []result.Redactor) result.Redactor {
	if len(redactors) > 0 {
		return redactors[0]
	}
	return result.NewRedactor()
}

var (
	_ model.Renderer = (*JSONRenderer)(nil)
	_ model.Renderer = (*TextRenderer)(nil)
	_ model.Renderer = (*PrettyRenderer)(nil)
)
