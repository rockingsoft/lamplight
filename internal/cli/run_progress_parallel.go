package cli

import (
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/mattn/go-isatty"
	"golang.org/x/term"

	"lamplight/internal/engine"
	"lamplight/internal/model"
	"lamplight/internal/render"
	"lamplight/internal/result"
)

type testProgressRow struct {
	name     string
	step     string
	status   model.Status
	duration int64
}

// One reporter handles every worker count. Terminal rows are redrawn in place;
// redirected output is reserved for the final run summary.
type parallelRunProgress struct {
	writer   io.Writer
	redactor result.Redactor
	format   render.PrettyFormatter
	terminal bool
	width    int
	mu       sync.Mutex
	finished int
	total    int
	rows     []testProgressRow
	byName   map[string]int
	frame    int
	stop     chan struct{}
	running  bool
}

func newParallelRunProgress(writer io.Writer, redactor result.Redactor) *parallelRunProgress {
	file, isFile := writer.(interface{ Fd() uintptr })
	terminal := isFile && (isatty.IsTerminal(file.Fd()) || isatty.IsCygwinTerminal(file.Fd()))
	width := 80
	if terminal {
		if columns, _, err := term.GetSize(int(file.Fd())); err == nil && columns > 0 {
			width = columns
		}
	}
	format := render.NewPrettyFormatter(writer, false)
	if terminal {
		format = render.NewAutoPrettyFormatter(writer)
	}
	return &parallelRunProgress{writer: writer, redactor: redactor, format: format, terminal: terminal, width: width, byName: make(map[string]int)}
}

func (p *parallelRunProgress) Report(event engine.ProgressEvent) {
	if !p.terminal {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	switch event.Kind {
	case engine.ProgressRunStarted:
		p.total = event.TestsTotal
		p.rows = make([]testProgressRow, event.TestsTotal)
		writef(p.writer, "Running %d %s\n\n", event.TestsTotal, plural(event.TestsTotal, "test", "tests"))
		if p.terminal {
			for range p.rows {
				writef(p.writer, "\n")
			}
			p.stop = make(chan struct{})
			p.running = true
			go p.animate(p.stop)
		}
	case engine.ProgressTestStarted:
		index := len(p.byName)
		if index < len(p.rows) {
			p.byName[event.TestName] = index
			p.rows[index].name = p.redactor.RedactString(event.TestName)
		}
	case engine.ProgressStepStarted:
		if index, ok := p.byName[event.TestName]; ok {
			p.rows[index].step = p.redactor.RedactString(event.StepName)
		}
	case engine.ProgressStepCompleted:
	case engine.ProgressTestCompleted:
		p.finished++
		if index, ok := p.byName[event.TestName]; ok {
			p.rows[index].status = event.Status
			p.rows[index].duration = event.DurationMS
		}
	case engine.ProgressRunCompleted:
		p.running = false
		if p.stop != nil {
			close(p.stop)
			p.stop = nil
		}
	}
	if p.terminal {
		p.draw()
	}
}

func (p *parallelRunProgress) animate(stop <-chan struct{}) {
	ticker := time.NewTicker(80 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			p.mu.Lock()
			if !p.running {
				p.mu.Unlock()
				return
			}
			p.frame++
			p.draw()
			p.mu.Unlock()
		}
	}
}

func (p *parallelRunProgress) draw() {
	if len(p.rows) == 0 {
		return
	}
	frames := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
	writef(p.writer, "\x1b[%dA", len(p.rows))
	for _, row := range p.rows {
		marker := p.format.Muted("·")
		label := "waiting"
		if row.name != "" {
			marker = p.format.Accent(frames[p.frame%len(frames)])
			label = row.name
			if row.step != "" {
				label += " · " + row.step
			}
		}
		if row.status != "" {
			marker = p.format.StatusMarker(row.status)
			label = fmt.Sprintf("%s (%s)", row.name, prettyProgressDuration(row.duration))
		}
		limit := max(8, p.width-4)
		if runes := []rune(label); len(runes) > limit {
			label = string(runes[:limit-1]) + "…"
		}
		writef(p.writer, "\r\x1b[2K  %s %s\n", marker, label)
	}
}
