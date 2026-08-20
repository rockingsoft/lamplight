package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"
	"lamplight/internal/model"
)

func cliExpr(t *testing.T, source string) hcl.Expression {
	t.Helper()
	expression, diagnostics := hclsyntax.ParseExpression([]byte(source), "cli-test.hcl", hcl.Pos{Line: 1, Column: 1})
	if diagnostics.HasErrors() {
		t.Fatal(diagnostics.Error())
	}
	return expression
}

func TestMainUsageAndFlagErrors(t *testing.T) {
	for _, args := range [][]string{nil, {"unknown"}, {"list"}, {"list", "nope"}, {"validate", "--unknown"}, {"run", "--unknown"}, {"init", "--unknown"}, {"run", "one", "two"}} {
		var out, stderr bytes.Buffer
		if code := Main(context.Background(), args, IO{Out: &out, Err: &stderr}); code != 1 {
			t.Fatalf("args=%v code=%d out=%q err=%q", args, code, out.String(), stderr.String())
		}
		if stderr.Len() == 0 {
			t.Fatalf("args=%v produced no diagnostic", args)
		}
	}
}

func TestValidateAndListReportLoadErrors(t *testing.T) {
	dir := t.TempDir()
	for _, args := range [][]string{{"validate", "-w", dir}, {"list", "tests", "-w", dir}} {
		var out, stderr bytes.Buffer
		if code := Main(context.Background(), args, IO{Out: &out, Err: &stderr}); code != 1 || stderr.Len() == 0 {
			t.Fatalf("args=%v code=%d stderr=%q", args, code, stderr.String())
		}
	}
}

func TestVarsFlagAndSmallCLIHelpers(t *testing.T) {
	values := varsFlag{}
	if err := values.Set("NAME=value=with-equals"); err != nil || values["NAME"] != "value=with-equals" {
		t.Fatalf("values=%#v err=%v", values, err)
	}
	for _, raw := range []string{"missing-equals", "=empty", "NAME=duplicate"} {
		if err := values.Set(raw); err == nil {
			t.Fatalf("Set(%q) unexpectedly succeeded", raw)
		}
	}
	if values.String() != "" {
		t.Fatal("varsFlag String should be empty")
	}
	if value, err := evalString(cliExpr(t, `"ok"`), nil); err != nil || value != "ok" {
		t.Fatalf("evalString=%q err=%v", value, err)
	}
	if _, err := evalString(cliExpr(t, `1`), nil); err == nil {
		t.Fatal("expected non-string evaluation error")
	}
	if _, err := evalString(cliExpr(t, `var.MISSING`), nil); err == nil {
		t.Fatal("expected unknown variable error")
	}

	definition := &model.ProjectDefinition{
		HTTPProxy: cliExpr(t, `"proxy"`),
		Datasource: &model.DatasourceDefinition{
			Endpoint: cliExpr(t, `"http://localhost:3200"`), BearerToken: cliExpr(t, `"token"`),
			Headers: map[string]hcl.Expression{"X": cliExpr(t, `"header"`)},
		},
		Tests: map[string]model.TestDefinition{
			"test": {
				Name: "test", Outputs: map[string]hcl.Expression{"test": cliExpr(t, `var.VALUE`)},
				Steps: []model.StepDefinition{{
					Name:    "step",
					HTTP:    model.HTTPRequestDefinition{Method: cliExpr(t, `"GET"`), URL: cliExpr(t, `"url"`), Body: cliExpr(t, `"body"`), Headers: map[string]hcl.Expression{"X": cliExpr(t, `"value"`)}},
					Outputs: map[string]hcl.Expression{"step": cliExpr(t, `var.VALUE`)},
					Checks:  []model.CheckDefinition{{Response: map[string]hcl.Expression{"response": cliExpr(t, `true`)}, Spans: &model.SpanCheckDefinition{Matching: cliExpr(t, `true`), Assertions: map[string]hcl.Expression{"span": cliExpr(t, `true`)}}}},
				}},
			},
		},
	}
	expressions := collectExpressions(definition, []model.TestDefinition{definition.Tests["test"]})
	if len(expressions) != 13 {
		t.Fatalf("collected %d expressions", len(expressions))
	}
	valuesForRedaction := map[string]model.SensitiveValue{"VALUE": {Value: cty.StringVal("secret"), Sensitive: true}, "NUMBER": {Value: cty.NumberIntVal(1)}}
	if got := sensitiveStrings(valuesForRedaction); len(got) != 1 || got[0] != "secret" {
		t.Fatalf("sensitive=%#v", got)
	}
	if !testNeedsDatasource(definition.Tests["test"]) || testNeedsDatasource(model.TestDefinition{}) {
		t.Fatal("unexpected datasource requirement")
	}
}

func TestTempoStoreAndDiagnostics(t *testing.T) {
	values := map[string]model.SensitiveValue{"ENDPOINT": {Value: cty.StringVal("http://localhost:3200")}, "TOKEN": {Value: cty.StringVal("token")}}
	definition := &model.DatasourceDefinition{Endpoint: cliExpr(t, `var.ENDPOINT`), Headers: map[string]hcl.Expression{"X": cliExpr(t, `"header"`)}, BearerToken: cliExpr(t, `var.TOKEN`)}
	if store, err := tempoStore(definition, values); err != nil || store == nil {
		t.Fatalf("store=%#v err=%v", store, err)
	}
	if _, err := tempoStore(&model.DatasourceDefinition{Endpoint: cliExpr(t, `"not-a-url"`)}, nil); err == nil {
		t.Fatal("expected invalid endpoint error")
	}
	if _, err := tempoStore(&model.DatasourceDefinition{Endpoint: cliExpr(t, `"http://localhost:3200"`), Headers: map[string]hcl.Expression{"X": cliExpr(t, `var.MISSING`)}}, nil); err == nil {
		t.Fatal("expected invalid header error")
	}
	if _, err := tempoStore(&model.DatasourceDefinition{Endpoint: cliExpr(t, `"http://localhost:3200"`), BearerToken: cliExpr(t, `var.MISSING`)}, nil); err == nil {
		t.Fatal("expected invalid bearer token error")
	}

	var output bytes.Buffer
	if !printDiagnostics(&output, []model.Diagnostic{{Severity: "warning", Code: "warn", Message: "be careful"}, {Severity: "error", Code: "bad", Message: "failed", File: "test.hcl", Range: model.SourceRange{StartLine: 2, StartColumn: 3}}}) {
		t.Fatal("expected error diagnostic")
	}
	if !strings.Contains(output.String(), "test.hcl:2:3") || !strings.Contains(output.String(), "warning[warn]") {
		t.Fatalf("diagnostics=%q", output.String())
	}
	var usageOutput bytes.Buffer
	usage(&usageOutput)
	if !strings.Contains(usageOutput.String(), "usage: lamplight") {
		t.Fatalf("usage=%q", usageOutput.String())
	}
}

func TestRunReportsSelectionAndVariableErrors(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "tests")
	if err := os.Mkdir(base, 0o755); err != nil {
		t.Fatal(err)
	}
	config := []byte("project { base_dir = \"tests\" output = \"json\" }\n")
	if err := os.WriteFile(filepath.Join(dir, ".lamplight"), config, 0o600); err != nil {
		t.Fatal(err)
	}
	testFile := []byte("variable \"REQUIRED\" { type = string }\ntest \"health\" { step \"get\" { http_request { method = \"GET\" url = \"http://127.0.0.1:1\" } } }\n")
	if err := os.WriteFile(filepath.Join(base, "test.wick"), testFile, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"run", "-w", dir, "missing"}, {"run", "-w", dir, "health"}} {
		var out, stderr bytes.Buffer
		if code := Main(context.Background(), args, IO{Out: &out, Err: &stderr}); code != 1 || stderr.Len() == 0 {
			t.Fatalf("args=%v code=%d stderr=%q", args, code, stderr.String())
		}
	}
}
