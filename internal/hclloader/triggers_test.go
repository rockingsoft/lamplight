package hclloader

import (
	"fmt"
	"testing"

	"github.com/hashicorp/hcl/v2/hclparse"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"lamplight/internal/model"
)

func TestParseAllNonHTTPTriggers(t *testing.T) {
	cases := []struct {
		block, body string
		kind        model.TriggerKind
	}{
		{"grpc_request", "protobuf = \"syntax = \\\"proto3\\\";\"\naddress = \"localhost:4317\"\nmethod = \"svc.Call\"\nrequest = \"{}\"", model.TriggerGRPC},
		{"graphql_request", "url = \"https://example.test\"\nquery = \"query { ok }\"", model.TriggerGraphQL},
		{"kafka_request", "broker_urls = [\"localhost:9092\"]\ntopic = \"events\"\nmessage_value = \"hello\"", model.TriggerKafka},
		{"traceid", `id = "0123456789abcdef0123456789abcdef"`, model.TriggerTraceID},
		{"cypress", `id = "0123456789abcdef0123456789abcdef"`, model.TriggerCypress},
		{"playwright", `id = "0123456789abcdef0123456789abcdef"`, model.TriggerPlaywright},
		{"artillery", `id = "0123456789abcdef0123456789abcdef"`, model.TriggerArtillery},
		{"k6", `id = "0123456789abcdef0123456789abcdef"`, model.TriggerK6},
		{"k6", "script = \"load.js\"\nenv = { BASE_URL = \"https://example.test\" }\narguments = { vus = 1 }", model.TriggerK6},
		{"playwright_engine", "target = \"https://example.test\"\nscript = \"async () => {}\"", model.TriggerPlaywrightEngine},
	}
	for _, tc := range cases {
		t.Run(tc.block, func(t *testing.T) {
			source := fmt.Sprintf("step \"example\" {\n%s {\n%s\n}\n}", tc.block, tc.body)
			file, diags := hclparse.NewParser().ParseHCL([]byte(source), "trigger.wick")
			if diags.HasErrors() {
				t.Fatal(diags.Error())
			}
			body := file.Body.(*hclsyntax.Body)
			step, parsed := parseStep(body.Blocks[0].AsHCLBlock())
			if len(parsed) != 0 {
				t.Fatalf("diagnostics: %#v", parsed)
			}
			if step.Trigger.Kind != tc.kind {
				t.Fatalf("kind=%q want=%q", step.Trigger.Kind, tc.kind)
			}
		})
	}
}

func TestK6RequiresExactlyOneSource(t *testing.T) {
	for _, body := range []string{"", "id = \"0123456789abcdef0123456789abcdef\"\nscript = \"load.js\""} {
		source := fmt.Sprintf("step \"example\" {\nk6 {\n%s\n}\n}", body)
		file, diags := hclparse.NewParser().ParseHCL([]byte(source), "trigger.wick")
		if diags.HasErrors() {
			t.Fatal(diags.Error())
		}
		parsed := file.Body.(*hclsyntax.Body)
		_, diagnostics := parseStep(parsed.Blocks[0].AsHCLBlock())
		if len(diagnostics) == 0 || diagnostics[0].Message != "k6 requires exactly one of id or script" {
			t.Fatalf("diagnostics=%#v", diagnostics)
		}
	}
}

func TestK6CloudRunExecutor(t *testing.T) {
	source := `step "load" {
  k6 {
    script = "load.js"
    files  = ["lib"]
    executor "cloud_run" {
      project = "project"
      region  = "southamerica-east1"
      job     = "loadtest"
      bucket  = "bucket"
      tasks   = 4
      timeout = "20m"
    }
  }
}`
	file, diags := hclparse.NewParser().ParseHCL([]byte(source), "trigger.wick")
	if diags.HasErrors() {
		t.Fatal(diags.Error())
	}
	step, diagnostics := parseStep(file.Body.(*hclsyntax.Body).Blocks[0].AsHCLBlock())
	if len(diagnostics) != 0 {
		t.Fatalf("diagnostics=%#v", diagnostics)
	}
	if step.Trigger.Executor == nil || step.Trigger.Executor.Kind != "cloud_run" || len(step.Trigger.Executor.Attributes) != 6 {
		t.Fatalf("executor=%#v", step.Trigger.Executor)
	}
}

func TestK6CloudRunExecutorRequiresScriptAndFilesRequireExecutor(t *testing.T) {
	for _, source := range []string{
		`step "load" {
  k6 {
    id = "0123456789abcdef0123456789abcdef"
    executor "cloud_run" {
      project = "p"
      region = "r"
      job = "j"
      bucket = "b"
      tasks = 1
    }
  }
}`,
		`step "load" {
  k6 {
    script = "load.js"
    files = ["lib"]
  }
}`,
	} {
		file, diags := hclparse.NewParser().ParseHCL([]byte(source), "trigger.wick")
		if diags.HasErrors() {
			t.Fatal(diags.Error())
		}
		_, diagnostics := parseStep(file.Body.(*hclsyntax.Body).Blocks[0].AsHCLBlock())
		if len(diagnostics) == 0 {
			t.Fatalf("expected diagnostics for %s", source)
		}
	}
}
