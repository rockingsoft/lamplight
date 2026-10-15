package hclloader

import (
	"strings"
	"testing"

	"github.com/hashicorp/hcl/v2/hclparse"
	"github.com/hashicorp/hcl/v2/hclsyntax"
)

func TestRequestHookParsesBesideRequestTrigger(t *testing.T) {
	source := `step "send" {
  http_request {
    method = "POST"
    url = "http://example.test"
  }
  request_hook {
    command = ["bash", "hook.sh"]
    env = { TOKEN = var.TOKEN }
  }
}`
	file, diags := hclparse.NewParser().ParseHCL([]byte(source), "hook.wick")
	if diags.HasErrors() {
		t.Fatal(diags)
	}
	step, parsed := parseStep(file.Body.(*hclsyntax.Body).Blocks[0].AsHCLBlock())
	if len(parsed) != 0 || step.RequestHook == nil || step.RequestHook.Command == nil || step.RequestHook.Env["TOKEN"] == nil {
		t.Fatalf("step=%#v diagnostics=%#v", step, parsed)
	}
}

func TestRequestHookRejectsTraceAttachment(t *testing.T) {
	source := `step "attach" {
  traceid { id = "0123456789abcdef0123456789abcdef" }
  request_hook { command = ["cat"] }
}`
	file, diags := hclparse.NewParser().ParseHCL([]byte(source), "hook.wick")
	if diags.HasErrors() {
		t.Fatal(diags)
	}
	_, parsed := parseStep(file.Body.(*hclsyntax.Body).Blocks[0].AsHCLBlock())
	if len(parsed) == 0 || !strings.Contains(parsed[0].Message, "requires an HTTP, GraphQL, gRPC, or Kafka") {
		t.Fatalf("diagnostics=%#v", parsed)
	}
}
