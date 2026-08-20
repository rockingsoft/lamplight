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
	return &runProgress{writer: writer, formatter: render.NewAutoPrettyFormatter(writer), redactor: redactor, terminal: terminal, width: width}
}

func (p *runProgress) Report(event engine.ProgressEvent) {
	switch event.Kind {
	case engine.ProgressRunStarted:
		writef(p.writer, "%s Running %d %s...\n", p.formatter.Accent("→"), event.TestsTotal, plural(event.TestsTotal, "test", "tests"))
	case engine.ProgressDatasourceStarted:
		writef(p.writer, "%s Checking trace datasource connection...\n", p.formatter.Accent("→"))
	case engine.ProgressDatasourceCompleted:
		if event.Status == model.StatusPassed {
			writef(p.writer, "%s Trace datasource connected\n", p.formatter.Success("✓"))
		} else {
			writef(p.writer, "%s Trace datasource connection failed\n", p.formatter.Failure("!"))
		}
	case engine.ProgressTestStarted:
		writef(p.writer, "%s Test: %s\n", p.formatter.Accent("→"), p.redactor.RedactString(event.TestName))
	case engine.ProgressTestCompleted:
		writef(p.writer, "%s Test: %s %s\n", p.formatter.StatusMarker(event.Status), p.redactor.RedactString(event.TestName), p.formatter.Muted(fmt.Sprintf("· %d ms", event.DurationMS)))
	case engine.ProgressStepStarted:
		writef(p.writer, "  %s Step: %s\n", p.formatter.Accent("→"), p.redactor.RedactString(event.StepName))
	case engine.ProgressStepCompleted:
		p.finishSpinner(p.formatter.StatusMarker(event.Status), p.currentSpinnerText())
		writef(p.writer, "  %s Step: %s %s\n", p.formatter.StatusMarker(event.Status), p.redactor.RedactString(event.StepName), p.formatter.Muted(fmt.Sprintf("· %d ms", event.DurationMS)))
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
		} else {
			p.updateSpinner(text)
		}
	}
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
	return fmt.Sprintf("Polling trace · attempt %d · %d %s received · %d %s matching · %d/%d checks ready", event.Attempt, event.SpanCount, plural(event.SpanCount, "span", "spans"), matches, plural(matches, "span", "spans"), completed, len(event.Checks))
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
		writef(p.writer, "%s%s %s\n", spinner.indent, marker, value)
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
