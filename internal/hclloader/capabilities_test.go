package hclloader

import (
	"fmt"
	"testing"

	"github.com/hashicorp/hcl/v2/hclparse"
	"github.com/hashicorp/hcl/v2/hclsyntax"
)

func TestTriggerCapabilitiesCoverParserWithValidExamples(t *testing.T) {
	want := map[string]bool{
		"http_request": true, "grpc_request": true, "graphql_request": true,
		"kafka_request": true, "traceid": true, "cypress": true,
		"playwright": true, "artillery": true, "k6": true, "playwright_engine": true,
	}
	capabilities := TriggerCapabilities()
	if len(capabilities) != len(want) {
		t.Fatalf("capabilities=%d want=%d", len(capabilities), len(want))
	}
	for _, capability := range capabilities {
		if !want[capability.Block] {
			t.Errorf("unexpected capability %q", capability.Block)
		}
		source := fmt.Sprintf("step \"trigger\" {\n%s\n}\n", capability.Example)
		file, diags := hclparse.NewParser().ParseHCL([]byte(source), "capability.wick")
		if diags.HasErrors() {
			t.Errorf("%s example does not parse: %s", capability.Block, diags.Error())
			continue
		}
		body := file.Body.(*hclsyntax.Body)
		_, parsed := parseStep(body.Blocks[0].AsHCLBlock())
		if len(parsed) != 0 {
			t.Errorf("%s example diagnostics: %#v", capability.Block, parsed)
		}
	}
}
