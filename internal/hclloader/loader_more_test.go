package hclloader

import (
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclparse"
	"github.com/zclconf/go-cty/cty"
	"lamplight/internal/config"
	"lamplight/internal/diagnostic"
	"lamplight/internal/model"
)

func loaderExpression(t *testing.T, source string) hcl.Expression {
	t.Helper()
	file, diags := hclparse.NewParser().ParseHCL([]byte("value = "+source+"\n"), "test.hcl")
	if diags.HasErrors() {
		t.Fatalf("parse %q: %s", source, diags.Error())
	}
	content, diags := file.Body.Content(&hcl.BodySchema{Attributes: []hcl.AttributeSchema{{Name: "value"}}})
	if diags.HasErrors() {
		t.Fatal(diags.Error())
	}
	return content.Attributes["value"].Expr
}

func loaderBlock(t *testing.T, source, typeName string, labels []string) *hcl.Block {
	t.Helper()
	file, diags := hclparse.NewParser().ParseHCL([]byte(source), "test.hcl")
	if diags.HasErrors() {
		t.Fatalf("parse block: %s", diags.Error())
	}
	schema := hcl.BodySchema{Blocks: []hcl.BlockHeaderSchema{{Type: typeName, LabelNames: labels}}}
	content, diags := file.Body.Content(&schema)
	if diags.HasErrors() || len(content.Blocks) != 1 {
		t.Fatalf("block diagnostics=%s blocks=%d", diags.Error(), len(content.Blocks))
	}
	return content.Blocks[0]
}

func TestParseDatasourceTable(t *testing.T) {
	tests := []struct {
		name      string
		source    string
		wantNil   bool
		wantDiags bool
	}{
		{name: "unsupported kind", source: `datasource "other" { endpoint = "x" }`, wantNil: true, wantDiags: true},
		{name: "jaeger", source: `datasource "jaeger" { endpoint = "http://localhost:16686" }`},
		{name: "otlp provider", source: `datasource "datadog" { endpoint = "http://localhost:4318" }`},
		{name: "missing endpoint", source: `datasource "tempo" {}`, wantNil: true, wantDiags: true},
		{name: "complete", source: "datasource \"tempo\" {\n  endpoint = var.ENDPOINT\n  headers = { X = \"one\" }\n  observation_window = duration(\"4s\")\n  settle_window = duration(\"1s\")\n  polling_interval = duration(\"250ms\")\n  auth { bearer_token = var.TOKEN }\n  tls { skip_verify = true }\n}\n", wantDiags: true},
		{name: "duplicate auth tls", source: "datasource \"tempo\" {\n  endpoint = \"x\"\n  auth { bearer_token = \"one\" }\n  auth { bearer_token = \"two\" }\n  tls { skip_verify = false }\n  tls { skip_verify = false }\n}\n", wantDiags: true},
		{name: "invalid windows", source: "datasource \"tempo\" {\n  endpoint = \"x\"\n  observation_window = 0\n  settle_window = \"bad\"\n  polling_interval = 0\n}\n", wantDiags: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, diags := parseDatasource(loaderBlock(t, tt.source, "datasource", []string{"kind"}))
			if (got == nil) != tt.wantNil || (len(diags) > 0) != tt.wantDiags {
				t.Fatalf("datasource=%#v diagnostics=%#v", got, diags)
			}
			if tt.name == "complete" && (got.ObservationWindow != 4*time.Second || got.SettleWindow != time.Second || got.PollingInterval != 250*time.Millisecond || !got.TLSSkipVerify || got.BearerToken == nil || len(got.Headers) != 1) {
				t.Fatalf("complete datasource=%#v", got)
			}
			if tt.name == "jaeger" && got.PollingInterval != 500*time.Millisecond {
				t.Fatalf("default polling interval=%s", got.PollingInterval)
			}
		})
	}
}

func TestParsePrometheusMetricsSourceAndCheck(t *testing.T) {
	source, diags := parseMetricsSource(loaderBlock(t, "metrics \"prometheus\" {\n  endpoint = var.METRICS_URL\n  headers = { \"X-Tenant\" = \"demo\" }\n  observation_window = duration(\"5s\")\n  settle_window = duration(\"1s\")\n  polling_interval = duration(\"250ms\")\n  auth { bearer_token = var.TOKEN }\n}\n", "metrics", []string{"kind"}))
	if len(diags) != 0 || source == nil || source.Kind != "prometheus" || source.ObservationWindow != 5*time.Second || source.PollingInterval != 250*time.Millisecond || source.BearerToken == nil {
		t.Fatalf("source=%#v diagnostics=%#v", source, diags)
	}

	file, hclDiags := hclparse.NewParser().ParseHCL([]byte("test \"metrics\" {\n  step \"create\" {\n    http_request {\n      method = \"POST\"\n      url = \"http://example.test/orders\"\n    }\n    check \"order metric\" {\n      metrics {\n        query = \"sum by (result) (orders_total)\"\n        metric_assertions = { incremented = metric.delta == 1 }\n        exactly = 1\n      }\n    }\n  }\n}\n"), "metrics.wick")
	if hclDiags.HasErrors() {
		t.Fatal(hclDiags.Error())
	}
	definition := &model.ProjectDefinition{Variables: map[string]model.VariableDefinition{}, Tests: map[string]model.TestDefinition{}}
	parsed := parseDefinitions(file, definition)
	check := definition.Tests["metrics"].Steps[0].Checks[0]
	if len(parsed) != 0 || check.Metrics == nil || check.Metrics.Query == nil || check.Metrics.Rule.Kind != "exactly" || len(check.Metrics.Assertions) != 1 {
		t.Fatalf("check=%#v diagnostics=%#v", check, parsed)
	}
	_, missingQuery := parseMetricsCheck(loaderBlock(t, "metrics { exactly = 1 }", "metrics", nil))
	if len(missingQuery) == 0 {
		t.Fatal("metrics check without PromQL query was accepted")
	}
}

func TestLoadProjectWithPrometheusMetricCheck(t *testing.T) {
	directory := t.TempDir()
	base := filepath.Join(directory, "tests")
	if err := os.Mkdir(base, 0o700); err != nil {
		t.Fatal(err)
	}
	root := "project { base_dir = \"./tests\" }\nvariable \"METRICS_URL\" { default = \"http://127.0.0.1:9090/metrics\" }\nmetrics \"prometheus_scrape\" { endpoint = var.METRICS_URL }\n"
	if err := os.WriteFile(filepath.Join(directory, ".lamplight"), []byte(root), 0o600); err != nil {
		t.Fatal(err)
	}
	test := "test \"operation\" {\n  step \"run\" {\n    http_request {\n      method = \"POST\"\n      url = \"http://example.test\"\n    }\n    check \"metric\" {\n      metrics {\n        query = \"operations_total\"\n        metric_assertions = { incremented = metric.delta == 1 }\n        exactly = 1\n      }\n    }\n  }\n}\n"
	if err := os.WriteFile(filepath.Join(base, "operation.wick"), []byte(test), 0o600); err != nil {
		t.Fatal(err)
	}
	definition, diags := (Loader{}).LoadProject(config.Options{ConfigPath: filepath.Join(directory, ".lamplight")})
	if len(diags) != 0 || definition.Metrics == nil || definition.Metrics.Kind != "prometheus_scrape" || definition.Tests["operation"].Steps[0].Checks[0].Metrics == nil {
		t.Fatalf("definition=%#v diagnostics=%#v", definition, diags)
	}
}

func TestLoadOTLPDatasourceWithPromQLMetricCheck(t *testing.T) {
	directory := t.TempDir()
	base := filepath.Join(directory, "tests")
	if err := os.Mkdir(base, 0o700); err != nil {
		t.Fatal(err)
	}
	root := "project { base_dir = \"./tests\" }\ndatasource \"otlp\" { endpoint = \"http://127.0.0.1:4318\" }\n"
	if err := os.WriteFile(filepath.Join(directory, ".lamplight"), []byte(root), 0o600); err != nil {
		t.Fatal(err)
	}
	test := "test \"operation\" {\n  step \"run\" {\n    http_request {\n      method = \"POST\"\n      url = \"http://example.test\"\n    }\n    check \"metric\" {\n      metrics {\n        query = \"http_server_request_duration_seconds_count\"\n        exactly = 1\n      }\n    }\n  }\n}\n"
	if err := os.WriteFile(filepath.Join(base, "operation.wick"), []byte(test), 0o600); err != nil {
		t.Fatal(err)
	}
	definition, diags := (Loader{}).LoadProject(config.Options{ConfigPath: filepath.Join(directory, ".lamplight")})
	if len(diags) != 0 || definition.Datasource == nil || definition.Datasource.Kind != "otlp" || definition.Metrics != nil || definition.Tests["operation"].Steps[0].Checks[0].Metrics == nil {
		t.Fatalf("definition=%#v diagnostics=%#v", definition, diags)
	}
}

func TestParseDefinitionsTable(t *testing.T) {
	tests := []struct {
		name      string
		source    string
		wantDiags bool
	}{
		{name: "complete", source: "variable \"PORT\" {\n  type = int\n  default = 80\n  sensitive = false\n}\nvariable \"WAIT\" {\n  type = duration\n  default = duration(\"1s\")\n}\ntest \"demo\" {\n  tags = [\"z\", \"a\"]\n  outputs = { result = response.body }\n  step \"first\" {\n    http_request {\n      method = \"POST\"\n      url = \"http://example.test\"\n      headers = { X = \"one\" }\n      body = \"body\"\n    }\n    outputs = { id = response.json.id }\n    check \"response\" { response = { ok = response.status_code == 200 } }\n    check \"spans\" {\n      spans {\n        matching = \"span.name == \\\"demo\\\"\"\n        span_assertions = { name = span.name == \"demo\" }\n        exactly = 1\n        observation_window = duration(\"1s\")\n      }\n    }\n  }\n}\n", wantDiags: false},
		{name: "invalid definitions", source: "variable \"bad-name\" {\n  type = bool\n  default = 1\n  sensitive = \"yes\"\n}\ntest \"empty\" {}\n", wantDiags: true},
		{name: "invalid checks", source: "test \"bad\" {\n  step \"s\" {\n    check \"empty\" { response = {} }\n    check \"quantity\" {\n      spans {\n        matching = \"x\"\n        at_least = -1\n        at_most = 1\n      }\n    }\n  }\n}\n", wantDiags: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			file, hclDiags := hclparse.NewParser().ParseHCL([]byte(tt.source), "definitions.hcl")
			if hclDiags.HasErrors() {
				t.Fatalf("parse: %s", hclDiags.Error())
			}
			definition := &model.ProjectDefinition{Variables: map[string]model.VariableDefinition{}, Tests: map[string]model.TestDefinition{}}
			diags := parseDefinitions(file, definition)
			if (len(diags) > 0) != tt.wantDiags {
				t.Fatalf("definition=%#v diagnostics=%#v", definition, diags)
			}
			if tt.name == "complete" && (len(definition.Variables) != 2 || len(definition.Tests) != 1 || len(definition.Tests["demo"].Tags) != 2) {
				t.Fatalf("definition=%#v", definition)
			}
		})
	}
	file, hclDiags := hclparse.NewParser().ParseHCL([]byte("variable \"PORT\" {}\ntest \"demo\" {\n  step \"s\" {\n    http_request {\n      method = \"GET\"\n      url = \"x\"\n    }\n  }\n}\n"), "duplicate.hcl")
	if hclDiags.HasErrors() {
		t.Fatal(hclDiags.Error())
	}
	definition := &model.ProjectDefinition{Variables: map[string]model.VariableDefinition{}, Tests: map[string]model.TestDefinition{}}
	parseDefinitions(file, definition)
	if len(parseDefinitions(file, definition)) == 0 {
		t.Fatal("duplicate definitions produced no diagnostics")
	}
}

func TestLoaderExpressionHelpers(t *testing.T) {
	if got, ds := typeName(loaderExpression(t, "string")); got != "string" || len(ds) != 0 {
		t.Fatalf("typeName traversal=%q %#v", got, ds)
	}
	if got, ds := typeName(loaderExpression(t, `"duration"`)); got != "duration" || len(ds) != 0 {
		t.Fatalf("typeName string=%q %#v", got, ds)
	}
	if _, ds := typeName(loaderExpression(t, "1")); len(ds) == 0 {
		t.Fatal("typeName accepted number")
	}
	for _, test := range []struct {
		source string
		want   bool
	}{{"true", true}, {"false", false}} {
		if got, ds := boolExpression(loaderExpression(t, test.source)); len(ds) != 0 || got != test.want {
			t.Fatalf("boolExpression=%v %#v", got, ds)
		}
	}
	if _, ds := boolExpression(loaderExpression(t, `"true"`)); len(ds) == 0 {
		t.Fatal("boolExpression accepted string")
	}
	for _, test := range []struct {
		source string
		want   int64
	}{{"4", 4}, {"-1", -1}} {
		if got, ds := intExpression(loaderExpression(t, test.source)); len(ds) != 0 || got != test.want {
			t.Fatalf("intExpression=%d %#v", got, ds)
		}
	}
	for _, source := range []string{"1.5", `"1"`} {
		if _, ds := intExpression(loaderExpression(t, source)); len(ds) == 0 {
			t.Fatalf("intExpression accepted %s", source)
		}
	}
	if got, ds := durationExpression(loaderExpression(t, `duration("2s")`)); len(ds) != 0 || got != 2*time.Second {
		t.Fatalf("durationExpression=%v %#v", got, ds)
	}
	for _, source := range []string{`duration("bad")`, `true`, "1.5"} {
		if _, ds := durationExpression(loaderExpression(t, source)); len(ds) == 0 {
			t.Fatalf("durationExpression accepted %s", source)
		}
	}
	if got, ds := tagsExpression(loaderExpression(t, `["z", "a"]`)); len(ds) != 0 || got[0] != "a" {
		t.Fatalf("tagsExpression=%#v %#v", got, ds)
	}
	for _, source := range []string{`"tag"`, `["a", 1]`, `["a", "a"]`} {
		if _, ds := tagsExpression(loaderExpression(t, source)); len(ds) == 0 {
			t.Fatalf("tagsExpression accepted %s", source)
		}
	}
}

func TestLoaderMapsReferencesAndDiagnostics(t *testing.T) {
	for _, source := range []string{`{"a": true}`, `{"a": true, "a": false}`, `{1: true}`, `[]`} {
		_, _ = expressionMap(loaderExpression(t, source))
	}
	if !validDefault("string", cty.StringVal("x")) || !validDefault("int", cty.NumberIntVal(1)) || !validDefault("duration", cty.NumberIntVal(1)) || validDefault("string", cty.NumberIntVal(1)) || validDefault("int", cty.NumberVal(bigFloat(1.5))) || validDefault("other", cty.StringVal("x")) {
		t.Fatal("validDefault table failed")
	}
	if len(validateIdentifiers("output", map[string]hcl.Expression{"good_name": loaderExpression(t, "true"), "bad.name": loaderExpression(t, "true")})) != 1 {
		t.Fatal("validateIdentifiers missed invalid name")
	}
	prior := map[string]model.StepDefinition{"login": {Name: "login", Outputs: map[string]hcl.Expression{"id": loaderExpression(t, `"id"`)}}}
	for _, source := range []string{"steps", "steps.login.foo", "steps.unknown", "steps.login.outputs.missing", "steps.login.outputs.id"} {
		_ = validateStepTraversal(loaderExpression(t, source).Variables()[0], prior)
	}
	definition := &model.ProjectDefinition{Variables: map[string]model.VariableDefinition{}, Tests: map[string]model.TestDefinition{"test": {Steps: []model.StepDefinition{{Name: "s", HTTP: model.HTTPRequestDefinition{Method: loaderExpression(t, "unknown")}}}}}}
	if len(validateReferences(definition)) == 0 {
		t.Fatal("validateReferences accepted an unknown expression root")
	}
	if len(checkExpressions(model.CheckDefinition{Response: map[string]hcl.Expression{"ok": loaderExpression(t, "true")}})) != 1 || len(checkExpressions(model.CheckDefinition{Spans: &model.SpanCheckDefinition{Matching: loaderExpression(t, "true"), Assertions: map[string]hcl.Expression{"name": loaderExpression(t, "true")}}})) != 2 {
		t.Fatal("checkExpressions did not cover response and span checks")
	}
	converted := toHCLDiagnostics([]model.Diagnostic{diagnostic.Error(diagnostic.CodeConfig, "error", hcl.Range{Filename: "x"}, ""), diagnostic.Warning(diagnostic.CodeConfig, "warning", hcl.Range{Filename: "x"}, "")})
	if len(converted) != 2 || converted[0].Severity != hcl.DiagError || converted[1].Severity != hcl.DiagWarning {
		t.Fatalf("toHCLDiagnostics=%#v", converted)
	}
}

func bigFloat(value float64) *big.Float { return big.NewFloat(value) }
