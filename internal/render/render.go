// Package render turns a completed RunResult into JSON or human-readable
// output. It never changes run semantics or computes a new aggregate result.
package render

import (
	"bytes"
	"fmt"
	"io"
	"strings"

	"github.com/muesli/termenv"
	"lamplight/internal/model"
	"lamplight/internal/result"
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

// NewAutoPrettyRenderer applies the same terminal color detection used by the
// other pretty CLI commands.
func NewAutoPrettyRenderer(writer io.Writer, redactors ...result.Redactor) *PrettyRenderer {
	formatter := NewAutoPrettyFormatter(writer)
	return NewPrettyRenderer(formatter.output.Profile != termenv.Ascii, redactors...)
}

func (r *PrettyRenderer) Render(run model.RunResult) ([]byte, error) {
	var output bytes.Buffer
	formatter := NewPrettyFormatter(&output, r.Color)
	fmt.Fprintf(&output, "%s %s (%s)\n", formatter.StatusMarker(run.Status), formatter.StatusLabel(run.Status), r.Redactor.RedactString(run.RunID))
	fmt.Fprintf(&output, "Started %s · %d ms · %d passed, %d failed, %d errors\n", run.StartedAt.Format("2006-01-02 15:04:05Z07:00"), run.DurationMS, run.Summary.TestsPassed, run.Summary.TestsFailed, run.Summary.Errors)
	for _, test := range run.Tests {
		fmt.Fprintf(&output, "%s %s", formatter.StatusMarker(test.Status), r.Redactor.RedactString(test.Name))
		if len(test.Tags) > 0 {
			fmt.Fprintf(&output, " [%s]", strings.Join(redactStrings(r.Redactor, test.Tags), ", "))
		}
		fmt.Fprintf(&output, " · %d ms\n", test.DurationMS)
		r.writePrettyDiagnostic(&output, formatter, "  ", test.Error)
		for _, step := range test.Steps {
			fmt.Fprintf(&output, "  %s %s · %d ms\n", formatter.StatusMarker(step.Status), r.Redactor.RedactString(step.Name), step.DurationMS)
			r.writePrettyDiagnostic(&output, formatter, "    ", step.Error)
			for _, check := range step.Checks {
				status, reason := prettyCheckState(step.Status, check)
				fmt.Fprintf(&output, "    %s %s", formatter.StatusMarker(status), r.Redactor.RedactString(check.Name))
				if reason != "" {
					fmt.Fprintf(&output, " — %s", r.Redactor.RedactString(reason))
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

func prettyCheckState(stepStatus model.Status, check model.CheckResult) (model.Status, string) {
	if stepStatus != model.StatusCancelled {
		return check.Status, check.Reason
	}
	if check.Status == model.StatusPassed {
		return model.StatusCancelled, "completed before cancellation"
	}
	return model.StatusCancelled, "cancelled"
}

func (r *PrettyRenderer) writePrettyDiagnostic(output *bytes.Buffer, formatter PrettyFormatter, indent string, diagnostic *model.Diagnostic) {
	if diagnostic == nil {
		return
	}
	summary, suggestion := friendlyDiagnostic(diagnostic)
	fmt.Fprintf(output, "%s%s %s\n", indent, formatter.Failure("Error:"), r.Redactor.RedactString(summary))
	fmt.Fprintf(output, "%s%s %s\n", indent, formatter.Muted("Details:"), r.Redactor.RedactString(diagnostic.Message))
	if suggestion != "" {
		fmt.Fprintf(output, "%s%s %s\n", indent, formatter.Accent("Try:"), r.Redactor.RedactString(suggestion))
	}
}

func friendlyDiagnostic(diagnostic *model.Diagnostic) (string, string) {
	suggestion := diagnostic.Suggestion
	message := strings.ToLower(diagnostic.Message)

	switch diagnostic.Code {
	case "datasource_required":
		return "These tests need a trace datasource, but none is configured.", firstNonEmpty(suggestion, "Add a datasource block to .lamplight and run the command again.")
	case "datasource_connection":
		summary := "Lamplight could not connect to the trace datasource."
		if strings.Contains(message, "no such host") || strings.Contains(message, "server misbehaving") {
			return summary, firstNonEmpty(suggestion, "Check the datasource hostname. Docker service names only resolve inside their Docker network; from the host, use a published address such as localhost.")
		}
		if strings.Contains(message, "connection refused") {
			return summary, firstNonEmpty(suggestion, "Make sure the datasource is running and that its configured host and port are reachable from this machine.")
		}
		if strings.Contains(message, "timeout") || strings.Contains(message, "deadline exceeded") {
			return summary, firstNonEmpty(suggestion, "Check network access to the datasource and verify that the configured endpoint responds before retrying.")
		}
		return summary, firstNonEmpty(suggestion, "Verify the datasource endpoint and credentials, then retry the run.")
	case "http_execution":
		summary := "The test's HTTP request could not be completed."
		if strings.Contains(message, "no such host") {
			return summary, firstNonEmpty(suggestion, "Check the request hostname and whether it is resolvable from this machine.")
		}
		if strings.Contains(message, "connection refused") {
			return summary, firstNonEmpty(suggestion, "Make sure the target service is running and listening on the configured host and port.")
		}
		if strings.Contains(message, "timeout") || strings.Contains(message, "deadline exceeded") {
			return summary, firstNonEmpty(suggestion, "Check the target service and network, or increase the HTTP timeout if the response is expected to take longer.")
		}
		return summary, firstNonEmpty(suggestion, "Verify the request URL and target service, then retry the run.")
	case "trigger_execution":
		if strings.Contains(message, "kafka") || strings.Contains(message, "broker") {
			summary := "Lamplight could not connect to a Kafka broker."
			if strings.Contains(message, "connection refused") || strings.Contains(message, "available brokers") {
				return summary, firstNonEmpty(suggestion, "Make sure Kafka is running, then use the broker's host-published address and port. A container port such as 9092 may be published on a different host port.")
			}
			if strings.Contains(message, "no such host") {
				return summary, firstNonEmpty(suggestion, "Check the broker hostname. Docker service names only resolve inside their Docker network; host-run tests need a host-published address.")
			}
			if strings.Contains(message, "timeout") || strings.Contains(message, "deadline exceeded") {
				return summary, firstNonEmpty(suggestion, "Check that Kafka is healthy and reachable, and verify its advertised listeners match the address used by this test.")
			}
			return summary, firstNonEmpty(suggestion, "Verify the broker address, published port, and advertised listeners, then retry the run.")
		}
		return "The test trigger could not be executed.", firstNonEmpty(suggestion, "Check the trigger configuration and make sure its target service is running and reachable.")
	case "trace_not_observed":
		return "No trace reached the datasource before the observation window ended.", firstNonEmpty(suggestion, "Verify that the application exports this request's trace to the configured datasource; if it is only delayed, increase observation_window.")
	default:
		return fmt.Sprintf("Run error [%s].", diagnostic.Code), suggestion
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

// statusMarker and statusLabel remain as narrow compatibility helpers for
// callers inside this package; all styling is delegated to PrettyFormatter.
func (r *PrettyRenderer) statusMarker(status model.Status) string {
	return NewPrettyFormatter(io.Discard, r.Color).StatusMarker(status)
}

func (r *PrettyRenderer) statusLabel(status model.Status) string {
	return NewPrettyFormatter(io.Discard, r.Color).StatusLabel(status)
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
