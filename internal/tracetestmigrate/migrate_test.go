package tracetestmigrate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"lamplight/internal/config"
	"lamplight/internal/hclloader"
)

const exampleYAML = `type: Test
spec:
  id: legacy-id
  name: Pokeshop Import
  tags: [smoke]
  trigger:
    type: http
    httpRequest:
      url: ${BASE_URL}/pokemon/import
      method: POST
      headers:
        - key: Content-Type
          value: application/json
        - key: Authorization
          value: Bearer ${TOKEN}
      body: '{"id":52}'
  specs:
    - name: request succeeded
      selector: span[tracetest.span.type="http" service.name="api"]
      assertions:
        - tracetest.response.status = 200
        - attr:tracetest.selected_spans.count = 1
        - attr:tracetest.span.duration < 100ms
`

func TestConvertHTTPTest(t *testing.T) {
	result, err := Convert([]byte(exampleYAML))
	if err != nil {
		t.Fatal(err)
	}
	text := string(result.HCL)
	for _, wanted := range []string{
		`variable "BASE_URL"`, `variable "TOKEN"`, `test "Pokeshop Import"`,
		`url    = "${var.BASE_URL}/pokemon/import"`, `response.status_code == 200`,
		`span.attributes["tracetest.span.type"] == "http"`, `resource["service.name"] == "api"`,
		`span.duration < duration("100ms")`, `exactly  = 1`,
	} {
		if !strings.Contains(text, wanted) {
			t.Errorf("output missing %q:\n%s", wanted, text)
		}
	}
	if len(result.Warnings) != 1 {
		t.Fatalf("warnings = %#v", result.Warnings)
	}
	_, diagnostics := hclsyntax.ParseConfig(result.HCL, "test.wick", hcl.Pos{Line: 1, Column: 1})
	if diagnostics.HasErrors() {
		t.Fatalf("generated invalid HCL: %s\n%s", diagnostics.Error(), text)
	}
}

func TestConvertRejectsLossyUnsupportedInputs(t *testing.T) {
	for _, test := range []struct{ name, yaml, want string }{
		{name: "resource", yaml: "type: VariableSet\nspec: {}\n", want: "resource type"},
		{name: "trigger", yaml: strings.Replace(exampleYAML, "type: http", "type: grpc", 1), want: "trigger type"},
		{name: "selector chain", yaml: strings.Replace(exampleYAML, `span[tracetest.span.type="http" service.name="api"]`, `span[name="one"] span[name="two"]`, 1), want: "single span selector"},
		{name: "unsupported function", yaml: strings.Replace(exampleYAML, "attr:tracetest.span.duration < 100ms", "attr:name contains 'api'", 1), want: "not supported"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := Convert([]byte(test.yaml))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestRunCreatesProjectAndProtectsExistingFiles(t *testing.T) {
	input := filepath.Join(t.TempDir(), "test.yaml")
	if err := os.WriteFile(input, []byte(exampleYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	output := t.TempDir()
	results, err := Run(input, output, false)
	if err != nil || len(results) != 1 {
		t.Fatalf("results=%#v err=%v", results, err)
	}
	for _, path := range []string{filepath.Join(output, ".lamplight"), filepath.Join(output, "lamplight", "pokeshop-import.wick")} {
		if _, err := os.Stat(path); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Run(input, output, false); err == nil || !strings.Contains(err.Error(), "refusing to overwrite") {
		t.Fatalf("overwrite error = %v", err)
	}
	if _, err := Run(input, output, true); err != nil {
		t.Fatal(err)
	}
	definition, diagnostics := (hclloader.Loader{}).LoadProject(config.Options{WorkingDir: output})
	if definition == nil || len(definition.Tests) != 1 {
		t.Fatalf("loaded definition=%#v diagnostics=%#v", definition, diagnostics)
	}
	for _, diagnostic := range diagnostics {
		if diagnostic.Severity == "error" {
			t.Fatalf("generated project does not validate: %#v", diagnostics)
		}
	}
}
