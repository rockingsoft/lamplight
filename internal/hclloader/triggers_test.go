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
