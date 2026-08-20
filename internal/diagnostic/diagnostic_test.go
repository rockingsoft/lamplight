package diagnostic

import (
	"testing"

	"github.com/hashicorp/hcl/v2"
	"tracetest/internal/model"
)

func TestRedact(t *testing.T) {
	tests := []struct {
		name      string
		message   string
		secrets   []string
		want      string
		wantFound bool
	}{
		{name: "replaces all occurrences", message: "secret secret", secrets: []string{"secret"}, want: "[REDACTED] [REDACTED]", wantFound: true},
		{name: "ignores empty and absent values", message: "ordinary", secrets: []string{"", "missing"}, want: "ordinary"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, found := Redact(tt.message, tt.secrets)
			if got != tt.want || found != tt.wantFound {
				t.Fatalf("Redact() = %q, %v; want %q, %v", got, found, tt.want, tt.wantFound)
			}
		})
	}
}

func TestConstructorsAndRedacted(t *testing.T) {
	r := hcl.Range{Filename: "test.hcl", Start: hcl.Pos{Line: 2, Column: 3}, End: hcl.Pos{Line: 2, Column: 9}}
	if got := New(SeverityInfo, CodeConfig, "message", r, "suggestion"); got.Severity != SeverityInfo || got.Code != CodeConfig || got.File != "test.hcl" || got.Range.StartLine != 2 || got.Suggestion != "suggestion" {
		t.Fatalf("New() = %#v", got)
	}
	if Error(CodeParse, "error", r, "fix").Severity != SeverityError || Warning(CodeConfig, "warning", r, "fix").Severity != SeverityWarning {
		t.Fatal("constructors returned the wrong severity")
	}
	got := Redacted(model.Diagnostic{Message: "token=secret", Suggestion: "replace secret"}, []string{"secret"})
	if got.Message != "token=[REDACTED]" || got.Suggestion != "replace [REDACTED]" || !got.SensitiveRedacted {
		t.Fatalf("Redacted() = %#v", got)
	}
	unchanged := Redacted(model.Diagnostic{Message: "safe", Suggestion: "safe"}, nil)
	if unchanged.SensitiveRedacted || unchanged.Message != "safe" || unchanged.Suggestion != "safe" {
		t.Fatalf("Redacted() changed an insensitive diagnostic: %#v", unchanged)
	}
}

func TestFromHCL(t *testing.T) {
	r := hcl.Range{Filename: "source.hcl", Start: hcl.Pos{Line: 4, Column: 1}, End: hcl.Pos{Line: 4, Column: 5}}
	diags := hcl.Diagnostics{
		&hcl.Diagnostic{Severity: hcl.DiagError, Summary: "error summary", Detail: "error detail", Subject: &r},
		&hcl.Diagnostic{Severity: hcl.DiagWarning, Summary: "warning summary"},
	}
	got := FromHCL(diags, CodeSchema)
	if len(got) != 2 || got[0].Severity != SeverityError || got[0].Message != "error summary: error detail" || got[0].File != "source.hcl" || got[1].Severity != SeverityWarning || got[1].Message != "warning summary" || got[1].File != "" {
		t.Fatalf("FromHCL() = %#v", got)
	}
	if len(FromHCL(nil, CodeSchema)) != 0 {
		t.Fatal("FromHCL(nil) returned diagnostics")
	}
}

func TestReferenceMessage(t *testing.T) {
	if got := ReferenceMessage("test", "checkout", model.SourceRange{}); got != `unknown test "checkout"` {
		t.Fatalf("ReferenceMessage() = %q", got)
	}
	declaration := model.SourceRange{File: "tests.hcl", StartLine: 7}
	if got := ReferenceMessage("variable", "BASE_URL", declaration); got != `invalid reference to variable "BASE_URL" (declared at tests.hcl:7)` {
		t.Fatalf("ReferenceMessage() = %q", got)
	}
}
