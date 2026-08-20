package cli

import (
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/mattn/go-isatty"
	"golang.org/x/term"
	"lamplight/internal/engine"
	"lamplight/internal/model"
	"lamplight/internal/render"
	"lamplight/internal/result"
)

type runProgress struct {
	writer    io.Writer
	formatter render.PrettyFormatter
	redactor  result.Redactor
	terminal  bool
	width     int
	mu        sync.Mutex
	spinner   *progressSpinner
	lineCount int
	testLine  int
	stepLine  int
	dsLine    int
}

type progressSpinner struct {
	indent string
	text   string
	stop   chan struct{}
	done   chan struct{}
}

func newRunProgress(writer io.Writer, redactor result.Redactor) *runProgress {
	file, isFile := writer.(interface{ Fd() uintptr })
	terminal := isFile && (isatty.IsTerminal(file.Fd()) || isatty.IsCygwinTerminal(file.Fd()))
	width := 80
	if terminal {
		if columns, _, err := term.GetSize(int(file.Fd())); err == nil && columns > 0 {
			width = columns
		}
	}
	return &runProgress{writer: writer, formatter: render.NewAutoPrettyFormatter(writer), redactor: redactor, terminal: terminal, width: width, testLine: -1, stepLine: -1, dsLine: -1}
}

func (p *runProgress) Report(event engine.ProgressEvent) {
	switch event.Kind {
	case engine.ProgressRunStarted:
		p.writeLine("%s Running %d %s...", p.formatter.Accent("→"), event.TestsTotal, plural(event.TestsTotal, "test", "tests"))
	case engine.ProgressDatasourceStarted:
		p.dsLine = p.writeLine("%s Checking trace datasource connection...", p.formatter.Accent("→"))
	case engine.ProgressDatasourceCompleted:
		if event.Status == model.StatusPassed {
			p.replaceLine(p.dsLine, "%s Trace datasource connected", p.formatter.Success("✓"))
		} else {
			p.replaceLine(p.dsLine, "%s Trace datasource connection failed", p.formatter.Failure("!"))
		}
	case engine.ProgressTestStarted:
		p.testLine = p.writeLine("%s Test: %s", p.formatter.Accent("→"), p.redactor.RedactString(event.TestName))
	case engine.ProgressTestCompleted:
		p.replaceLine(p.testLine, "%s Test: %s %s", p.formatter.StatusMarker(event.Status), p.redactor.RedactString(event.TestName), p.formatter.Muted(fmt.Sprintf("· %d ms", event.DurationMS)))
	case engine.ProgressStepStarted:
		p.stepLine = p.writeLine("  %s Step: %s", p.formatter.Accent("→"), p.redactor.RedactString(event.StepName))
	case engine.ProgressStepCompleted:
		p.finishSpinner(p.formatter.StatusMarker(event.Status), p.currentSpinnerText())
		p.replaceLine(p.stepLine, "  %s Step: %s %s", p.formatter.StatusMarker(event.Status), p.redactor.RedactString(event.StepName), p.formatter.Muted(fmt.Sprintf("· %d ms", event.DurationMS)))
	case engine.ProgressTriggerStarted:
		p.startSpinner("    ", fmt.Sprintf("Running %s trigger...", triggerLabel(event.Trigger)))
	case engine.ProgressTriggerCompleted:
		text := fmt.Sprintf("%s trigger %s", strings.ToUpper(triggerLabel(event.Trigger)), statusWord(event.Status))
		if event.StatusCode > 0 {
			text += fmt.Sprintf(" · HTTP %d", event.StatusCode)
		}
		text += fmt.Sprintf(" · %d ms", event.DurationMS)
		p.finishSpinner(p.formatter.StatusMarker(event.Status), text)
	case engine.ProgressTracePolling:
		p.startSpinner("    ", fmt.Sprintf("Polling trace spans (up to %s)...", event.ObservationWindow))
	case engine.ProgressTraceObserved:
		text := p.traceProgressText(event)
		if status, terminal := terminalCheckStatus(event.Checks); terminal {
			p.finishSpinner(p.formatter.StatusMarker(status), text)
			p.writeFailedChecks(event.Checks)
		} else {
			p.updateSpinner(text)
		}
	}
}

func (p *runProgress) writeFailedChecks(checks []engine.ProgressCheck) {
	for _, check := range checks {
		if check.Status != model.StatusFailed && check.Status != model.StatusError {
			continue
		}
		detail := friendlyProgressReason(check)
		p.writeLine("      %s %s", p.formatter.StatusMarker(check.Status), p.redactor.RedactString(check.Name))
		if detail != "" {
			p.writeLine("        %s", p.redactor.RedactString(detail))
		}
	}
}

func friendlyProgressReason(check engine.ProgressCheck) string {
	switch check.Reason {
	case "span_assertion_failed":
		return "A matching span did not satisfy the assertion."
	case "count_not_satisfied":
		return fmt.Sprintf("Expected %s %d matching spans; found %d.", quantityWords(check.Rule.Kind), check.Rule.Value, check.MatchCount)
	case "trace_not_observed":
		return "No matching trace was observed before the timeout."
	case "":
		return ""
	default:
		return strings.ReplaceAll(check.Reason, "_", " ") + "."
	}
}

func quantityWords(kind string) string {
	switch kind {
	case "at_least":
		return "at least"
	case "at_most":
		return "at most"
	case "exactly":
		return "exactly"
	default:
		return kind
	}
}

func (p *runProgress) writeLine(format string, args ...any) int {
	line := p.lineCount
	writef(p.writer, format+"\n", args...)
	p.lineCount++
	return line
}

func (p *runProgress) replaceLine(line int, format string, args ...any) {
	if !p.terminal || line < 0 || line >= p.lineCount {
		p.writeLine(format, args...)
		return
	}
	distance := p.lineCount - line
	value := fmt.Sprintf(format, args...)
	p.mu.Lock()
	_, _ = fmt.Fprintf(p.writer, "\x1b[%dA\r\x1b[2K%s\x1b[%dB\r", distance, value, distance)
	p.mu.Unlock()
}

func (p *runProgress) traceProgressText(event engine.ProgressEvent) string {
	if event.RetryError != "" {
		return fmt.Sprintf("Polling trace · attempt %d · retrying: %s", event.Attempt, p.redactor.RedactString(event.RetryError))
	}
	matches, completed := 0, 0
	for _, check := range event.Checks {
		matches += check.MatchCount
		if check.Status != "" {
			completed++
		}
	}
	label := "Waiting for trace"
	if _, terminal := terminalCheckStatus(event.Checks); terminal {
		label = "Trace checks"
	}
	return fmt.Sprintf("%s · %d/%d ready · %d %s received · attempt %d", label, completed, len(event.Checks), event.SpanCount, plural(event.SpanCount, "span", "spans"), event.Attempt)
}

func terminalCheckStatus(checks []engine.ProgressCheck) (model.Status, bool) {
	if len(checks) == 0 {
		return "", false
	}
	status := model.StatusPassed
	for _, check := range checks {
		if check.Status == "" {
			return "", false
		}
		if check.Status != model.StatusPassed {
			status = check.Status
		}
	}
	return status, true
}

func statusWord(status model.Status) string {
	if status == model.StatusPassed {
		return "succeeded"
	}
	return "failed"
}

func (p *runProgress) startSpinner(indent, value string) {
	p.finishSpinner("", "")
	spinner := &progressSpinner{indent: indent, text: value, stop: make(chan struct{}), done: make(chan struct{})}
	p.mu.Lock()
	p.spinner = spinner
	p.mu.Unlock()
	if !p.terminal {
		writef(p.writer, "%s%s %s\n", indent, p.formatter.Accent("→"), value)
		return
	}
	go p.animate(spinner)
}

func (p *runProgress) animate(spinner *progressSpinner) {
	defer close(spinner.done)
	frames := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
	ticker := time.NewTicker(80 * time.Millisecond)
	defer ticker.Stop()
	frame := 0
	for {
		p.mu.Lock()
		text := p.fitSpinnerText(spinner.indent, spinner.text)
		_, _ = fmt.Fprintf(p.writer, "\r\x1b[2K%s%s %s", spinner.indent, p.formatter.Accent(frames[frame%len(frames)]), text)
		p.mu.Unlock()
		frame++
		select {
		case <-spinner.stop:
			return
		case <-ticker.C:
		}
	}
}

func (p *runProgress) fitSpinnerText(indent, value string) string {
	available := p.width - len([]rune(indent)) - 2
	if available < 8 {
		available = 8
	}
	runes := []rune(value)
	if len(runes) <= available {
		return value
	}
	return string(runes[:available-1]) + "…"
}

func (p *runProgress) updateSpinner(value string) {
	p.mu.Lock()
	spinner := p.spinner
	if spinner != nil {
		spinner.text = value
	}
	p.mu.Unlock()
	if spinner != nil && !p.terminal {
		writef(p.writer, "%s%s %s\n", spinner.indent, p.formatter.Accent("→"), value)
	}
}

func (p *runProgress) currentSpinnerText() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.spinner == nil {
		return ""
	}
	return p.spinner.text
}

func (p *runProgress) finishSpinner(marker, value string) {
	p.mu.Lock()
	spinner := p.spinner
	if spinner == nil {
		p.mu.Unlock()
		return
	}
	p.spinner = nil
	close(spinner.stop)
	p.mu.Unlock()
	if p.terminal {
		<-spinner.done
		p.mu.Lock()
		_, _ = fmt.Fprint(p.writer, "\r\x1b[2K")
		p.mu.Unlock()
	}
	if value != "" {
		p.writeLine("%s%s %s", spinner.indent, marker, value)
	}
}

func triggerLabel(kind model.TriggerKind) string {
	if kind == "" || kind == model.TriggerHTTP {
		return "http"
	}
	return strings.ReplaceAll(string(kind), "_", " ")
}

func plural(count int, singular, plural string) string {
	if count == 1 {
		return singular
	}
	return plural
}
