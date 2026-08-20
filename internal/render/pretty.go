package render

import (
	"io"
	"strings"

	"github.com/muesli/termenv"
	"lamplight/internal/model"
)

// PrettyFormatter owns the visual language shared by every human-oriented
// command output. Keeping these styles here prevents commands from inventing
// subtly different markers and colors.
type PrettyFormatter struct {
	output *termenv.Output
}

// NewPrettyFormatter creates a formatter for writer. When color is false it
// always returns ANSI-free strings, which keeps redirected output stable.
func NewPrettyFormatter(writer io.Writer, color bool) PrettyFormatter {
	output := termenv.NewOutput(writer)
	if color {
		output.Profile = termenv.ANSI
	} else {
		output.Profile = termenv.Ascii
	}
	return PrettyFormatter{output: output}
}

// NewAutoPrettyFormatter lets termenv select color support from the writer and
// environment. It is intended for CLI commands that write directly to stdout.
func NewAutoPrettyFormatter(writer io.Writer) PrettyFormatter {
	return PrettyFormatter{output: termenv.NewOutput(writer)}
}

func (f PrettyFormatter) Success(value string) string {
	return f.output.String(value).Foreground(f.output.Color("2")).Bold().String()
}

func (f PrettyFormatter) Failure(value string) string {
	return f.output.String(value).Foreground(f.output.Color("1")).Bold().String()
}

func (f PrettyFormatter) Warning(value string) string {
	return f.output.String(value).Foreground(f.output.Color("3")).Bold().String()
}

func (f PrettyFormatter) Accent(value string) string {
	return f.output.String(value).Foreground(f.output.Color("6")).Bold().String()
}

func (f PrettyFormatter) Muted(value string) string {
	return f.output.String(value).Foreground(f.output.Color("8")).String()
}

func (f PrettyFormatter) StatusMarker(status model.Status) string {
	switch status {
	case model.StatusPassed:
		return f.Success("✓")
	case model.StatusFailed, model.StatusError:
		return f.Failure(map[model.Status]string{model.StatusFailed: "✗", model.StatusError: "!"}[status])
	case model.StatusCancelled:
		return f.Warning("■")
	case model.StatusSkipped:
		return f.Muted("-")
	default:
		return f.Warning("?")
	}
}

func (f PrettyFormatter) StatusLabel(status model.Status) string {
	label := strings.ToUpper(string(status))
	switch status {
	case model.StatusPassed:
		return f.Success(label)
	case model.StatusFailed, model.StatusError:
		return f.Failure(label)
	case model.StatusCancelled:
		return f.Warning(label)
	default:
		return f.Muted(label)
	}
}
