package tracetestmigrate

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	if len(result.Warnings) != 0 {
		t.Fatalf("warnings = %#v", result.Warnings)
	}
	if !strings.Contains(text, `assertions = {`) || strings.Contains(text, `exactly  = 0`) {
		t.Fatalf("span assertions were not preserved directly:\n%s", text)
	}
	_, diagnostics := hclsyntax.ParseConfig(result.HCL, "test.wick", hcl.Pos{Line: 1, Column: 1})
	if diagnostics.HasErrors() {
		t.Fatalf("generated invalid HCL: %s\n%s", diagnostics.Error(), text)
	}
}

func TestConvertKafkaTest(t *testing.T) {
	source := `type: Test
spec:
  name: Stream import
  trigger:
    type: kafka
    kafka:
      brokerUrls: ["${BROKER}"]
      topic: pokemon
      headers:
        - key: source
          value: test
      messageKey: snorlax-key
      messageValue: '{"id":143}'
  specs:
    - name: consumed
      selector: span[messaging.system="kafka"]
      assertions:
        - attr:messaging.system = "kafka"
`
	result, err := Convert([]byte(source))
	if err != nil {
		t.Fatal(err)
	}
	text := string(result.HCL)
	for _, wanted := range []string{`variable "BROKER"`, `kafka_request {`, `broker_urls = ["${var.BROKER}"]`, `topic         = "pokemon"`, `message_key   = "snorlax-key"`, `message_value = "{\"id\":143}"`, `"source" = "test"`} {
		if !strings.Contains(text, wanted) {
			t.Errorf("output missing %q:\n%s", wanted, text)
		}
	}
	_, diagnostics := hclsyntax.ParseConfig(result.HCL, "test.wick", hcl.Pos{Line: 1, Column: 1})
	if diagnostics.HasErrors() {
		t.Fatalf("generated invalid HCL: %s\n%s", diagnostics.Error(), text)
	}
}

func TestConvertJSONPathAssertions(t *testing.T) {
	for _, test := range []struct {
		assertion string
		wanted    string
	}{
		{`attr:db.result | json_path '$.imageUrl' = "https://example/image.png"`, `tostring(jsondecode(span.attributes["db.result"]).imageUrl) == "https://example/image.png"`},
		{`attr:http.response.body | json_path '$.items[*].imageUrl' contains "sprite.png"`, `contains(jsondecode(span.attributes["http.response.body"]).items[*].imageUrl, "sprite.png")`},
	} {
		source := strings.Replace(exampleYAML, "attr:tracetest.span.duration < 100ms", test.assertion, 1)
		result, err := Convert([]byte(source))
		if err != nil {
			t.Fatalf("assertion %q: %v", test.assertion, err)
		}
		if !strings.Contains(string(result.HCL), test.wanted) {
			t.Fatalf("output missing %q:\n%s", test.wanted, result.HCL)
		}
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

func TestRunIgnoresFilesWithoutTestsAndHandlesMultiDocumentYAML(t *testing.T) {
	input := t.TempDir()
	config := "---\ntype: PollingProfile\nspec:\n  name: Default\n  default: true\n  periodic:\n    timeout: 1m\n---\ntype: DataStore\nspec:\n  name: jaeger\n  type: jaeger\n  jaeger:\n    endpoint: jaeger:16685\n    tls:\n      insecure: true\n"
	if err := os.WriteFile(filepath.Join(input, "provision.yaml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	tests := exampleYAML + "\n---\n" + strings.Replace(exampleYAML, "Pokeshop Import", "Second Test", 1)
	if err := os.WriteFile(filepath.Join(input, "tests.yaml"), []byte(tests), 0o600); err != nil {
		t.Fatal(err)
	}
	output := t.TempDir()
	results, err := Run(input, output, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 || results[0].ImportedTests != 0 || results[0].ImportedDatasources != 1 || results[1].ImportedTests != 2 {
		t.Fatalf("results=%#v", results)
	}
	if len(results[1].Destinations) != 2 {
		t.Fatalf("destinations=%#v", results[1].Destinations)
	}
	configContents, err := os.ReadFile(filepath.Join(output, ".lamplight"))
	if err != nil {
		t.Fatal(err)
	}
	for _, wanted := range []string{`datasource "jaeger"`, `endpoint = "http://jaeger:16686"`, `observation_window = duration("1m0s")`} {
		if !strings.Contains(string(configContents), wanted) {
			t.Fatalf("config missing %q:\n%s", wanted, configContents)
		}
	}
}

func TestRunWithOnlyIgnoredFilesDoesNotCreateProject(t *testing.T) {
	input := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(input, []byte("scheme: http\nendpoint: localhost:11633\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "output")
	results, err := Run(input, output, false)
	if err != nil || len(results) != 1 || results[0].ImportedTests != 0 {
		t.Fatalf("results=%#v err=%v", results, err)
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatalf("output should not exist: %v", err)
	}
}

func TestQueryEndpointConversion(t *testing.T) {
	for _, test := range []struct {
		raw      string
		insecure bool
		want     string
	}{
		{"jaeger:16685", true, "http://jaeger:16686"},
		{"jaeger.example:16685", false, "https://jaeger.example:16686"},
		{"http://localhost:16686", true, "http://localhost:16686"},
	} {
		got, err := queryEndpoint(test.raw, test.insecure, "16685", "16686")
		if err != nil || got != test.want {
			t.Fatalf("endpoint=%q insecure=%t got=%q err=%v", test.raw, test.insecure, got, err)
		}
	}
}

func TestDecodeDataStoresSupportedByBothProducts(t *testing.T) {
	for _, test := range []struct {
		name   string
		source string
		want   []string
	}{
		{"tempo http", "type: DataStore\nspec:\n  type: tempo\n  tempo:\n    http:\n      url: tempo:80\n      headers: { X-Tenant: demo }\n      tls: { insecure: true }\n", []string{`datasource "tempo"`, `endpoint = "http://tempo:80"`, `"X-Tenant" = "demo"`}},
		{"tempo grpc", "type: DataStore\nspec:\n  type: tempo\n  tempo:\n    grpc:\n      endpoint: tempo:9095\n      tls: { insecure: true }\n", []string{`datasource "tempo"`, `endpoint = "http://tempo:3200"`}},
		{"opensearch", "type: DataStore\nspec:\n  type: opensearch\n  opensearch:\n    addresses: [https://search:9200]\n    index: traces\n    username: user\n    password: pass\n    insecureSkipVerify: true\n", []string{`datasource "opensearch"`, `endpoint = "https://search:9200/traces"`, `"Authorization" = "Basic dXNlcjpwYXNz"`, `skip_verify = true`}},
		{"elasticapm", "type: DataStore\nspec:\n  type: elasticapm\n  elasticapm:\n    addresses: [http://elastic:9200]\n    index: traces-apm-default\n", []string{`datasource "elasticapm"`, `endpoint = "http://elastic:9200/traces-apm-default"`}},
		{"signalfx", "type: DataStore\nspec:\n  type: signalfx\n  signalfx:\n    realm: us1\n    token: secret\n", []string{`datasource "signalfx"`, `endpoint = "https://api.us1.signalfx.com"`, `"X-SF-TOKEN" = "secret"`}},
		{"otlp based", "type: DataStore\nspec:\n  type: datadog\n", []string{`datasource "datadog"`, `endpoint = "http://127.0.0.1:4318"`}},
	} {
		t.Run(test.name, func(t *testing.T) {
			resources, err := inspectResources([]byte(test.source))
			if err != nil || len(resources.datasources) != 1 {
				t.Fatalf("resources=%#v err=%v", resources, err)
			}
			config := configContents(&resources.datasources[0])
			for _, wanted := range test.want {
				if !strings.Contains(config, wanted) {
					t.Fatalf("config missing %q:\n%s", wanted, config)
				}
			}
		})
	}
}

func TestWriteConfigUpgradesGeneratedBaselineAndProtectsCustomConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".lamplight")
	if err := os.WriteFile(path, []byte(configContents(nil)), 0o600); err != nil {
		t.Fatal(err)
	}
	datasource := &datasourceImport{Kind: "jaeger", Endpoint: "http://jaeger:16686", ObservationWindow: time.Minute}
	if err := writeConfig(context.Background(), dir, datasource); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(contents), `datasource "jaeger"`) {
		t.Fatalf("contents=%q err=%v", contents, err)
	}
	if err := os.WriteFile(path, []byte("custom"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeConfig(context.Background(), dir, datasource); err == nil || !strings.Contains(err.Error(), "customized") {
		t.Fatalf("error=%v", err)
	}
}
