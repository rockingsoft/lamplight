package cli

import (
	"fmt"
	"io"

	"lamplight/internal/engine"
	"lamplight/internal/model"
	"lamplight/internal/render"
	"lamplight/internal/result"
)

// ciRunProgress follows the Playwright-style line reporter: completed tests
// produce one stable line and failures are explained immediately below it.
type ciRunProgress struct {
	writer   io.Writer
	redactor result.Redactor
	format   render.PrettyFormatter
	index    int
	checks   []engine.ProgressCheck
}

func newCIRunProgress(writer io.Writer, redactor result.Redactor) *ciRunProgress {
	return &ciRunProgress{writer: writer, redactor: redactor, format: render.NewPrettyFormatter(writer, false)}
}

func (p *ciRunProgress) Report(event engine.ProgressEvent) {
	switch event.Kind {
	case engine.ProgressRunStarted:
		writef(p.writer, "Running %d %s\n\n", event.TestsTotal, plural(event.TestsTotal, "test", "tests"))
	case engine.ProgressTestStarted:
		p.checks = nil
	case engine.ProgressTraceObserved:
		if _, terminal := terminalCheckStatus(event.Checks); terminal {
			p.checks = append([]engine.ProgressCheck(nil), event.Checks...)
		}
	case engine.ProgressTestCompleted:
		p.index++
		writef(p.writer, "  %s %d %s %s\n", p.format.StatusMarker(event.Status), p.index, p.redactor.RedactString(event.TestName), p.format.Muted("("+prettyProgressDuration(event.DurationMS)+")"))
		p.writeFailures()
	}
}

func (p *ciRunProgress) writeFailures() {
	for _, check := range p.checks {
		if check.Status != model.StatusFailed && check.Status != model.StatusError {
			continue
		}
		writef(p.writer, "      %s\n", p.redactor.RedactString(check.Name))
		if detail := friendlyProgressReason(check); detail != "" {
			writef(p.writer, "        %s\n", p.redactor.RedactString(detail))
		}
	}
}

func prettyProgressDuration(milliseconds int64) string {
	return fmt.Sprintf("%.1fs", float64(milliseconds)/1000)
}
