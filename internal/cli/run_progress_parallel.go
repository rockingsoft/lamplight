package cli

import (
	"io"
	"sync"

	"lamplight/internal/engine"
	"lamplight/internal/render"
	"lamplight/internal/result"
)

// Concurrent steps can interleave, so report complete tests rather than
// attributing a shared step or trace line to the wrong test.
type parallelRunProgress struct {
	writer   io.Writer
	redactor result.Redactor
	format   render.PrettyFormatter
	mu       sync.Mutex
	finished int
	total    int
}

func newParallelRunProgress(writer io.Writer, redactor result.Redactor) *parallelRunProgress {
	return &parallelRunProgress{writer: writer, redactor: redactor, format: render.NewPrettyFormatter(writer, false)}
}

func (p *parallelRunProgress) Report(event engine.ProgressEvent) {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch event.Kind {
	case engine.ProgressRunStarted:
		p.total = event.TestsTotal
		writef(p.writer, "Running %d %s concurrently\n\n", event.TestsTotal, plural(event.TestsTotal, "test", "tests"))
	case engine.ProgressTestCompleted:
		p.finished++
		writef(p.writer, "  %s %d/%d %s %s\n", p.format.StatusMarker(event.Status), p.finished, p.total, p.redactor.RedactString(event.TestName), p.format.Muted("("+prettyProgressDuration(event.DurationMS)+")"))
	}
}
