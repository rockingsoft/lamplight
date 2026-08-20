// Package diagnostic provides stable, source-aware diagnostics for loading.
package diagnostic

import (
	"fmt"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"lamplight/internal/model"
)

const (
	SeverityError   = "error"
	SeverityWarning = "warning"
	SeverityInfo    = "info"
)

const (
	CodeParse       = "parse_error"
	CodeSchema      = "schema_error"
	CodeDuplicate   = "duplicate_definition"
	CodeReference   = "invalid_reference"
	CodeConfig      = "invalid_config"
	CodePath        = "invalid_path"
	CodeVariable    = "invalid_variable"
	CodeExpression  = "invalid_expression"
	CodeSensitivity = "sensitive_value"
)

func New(severity, code, message string, r hcl.Range, suggestion string) model.Diagnostic {
	return model.Diagnostic{Severity: severity, Code: code, Message: message, File: r.Filename, Range: model.Range(r), Suggestion: suggestion}
}

func Error(code, message string, r hcl.Range, suggestion string) model.Diagnostic {
	return New(SeverityError, code, message, r, suggestion)
}

func Warning(code, message string, r hcl.Range, suggestion string) model.Diagnostic {
	return New(SeverityWarning, code, message, r, suggestion)
}

// FromHCL translates parser and evaluator diagnostics without discarding their
// precise source locations.
func FromHCL(ds hcl.Diagnostics, code string) []model.Diagnostic {
	result := make([]model.Diagnostic, 0, len(ds))
	for _, d := range ds {
		severity := SeverityError
		if d.Severity == hcl.DiagWarning {
			severity = SeverityWarning
		}
		message := d.Summary
		if d.Detail != "" {
			message += ": " + d.Detail
		}
		var r hcl.Range
		if d.Subject != nil {
			r = *d.Subject
		}
		result = append(result, New(severity, code, message, r, ""))
	}
	return result
}

// Redact replaces every supplied secret. It intentionally does not attempt
// heuristics, so unrelated user-facing text remains useful.
func Redact(message string, sensitiveValues []string) (string, bool) {
	redacted := false
	for _, value := range sensitiveValues {
		if value == "" || !strings.Contains(message, value) {
			continue
		}
		message = strings.ReplaceAll(message, value, "[REDACTED]")
		redacted = true
	}
	return message, redacted
}

func Redacted(d model.Diagnostic, sensitiveValues []string) model.Diagnostic {
	d.Message, d.SensitiveRedacted = Redact(d.Message, sensitiveValues)
	d.Suggestion, _ = Redact(d.Suggestion, sensitiveValues)
	return d
}

func ReferenceMessage(kind, name string, declaration model.SourceRange) string {
	if declaration.File == "" {
		return fmt.Sprintf("unknown %s %q", kind, name)
	}
	return fmt.Sprintf("invalid reference to %s %q (declared at %s:%d)", kind, name, declaration.File, declaration.StartLine)
}
