package hclloader

import (
	"sort"

	"lamplight/internal/model"
)

// AttributeCapability describes one public trigger attribute for MCP clients,
// documentation generators, and the HCL parser.
type AttributeCapability struct {
	Name        string `json:"name"`
	Type        string `json:"type"`
	Required    bool   `json:"required"`
	Sensitive   bool   `json:"sensitive,omitempty"`
	Description string `json:"description"`
}

// TriggerCapability is the canonical inventory of trigger blocks accepted by
// the current binary. Example is a complete, statically valid trigger block.
type TriggerCapability struct {
	Block            string                `json:"block"`
	Kind             model.TriggerKind     `json:"kind"`
	Execution        string                `json:"execution"`
	TracePropagation string                `json:"trace_propagation"`
	Description      string                `json:"description"`
	Attributes       []AttributeCapability `json:"attributes"`
	ExactlyOneOf     []string              `json:"exactly_one_of,omitempty"`
	Example          string                `json:"example"`
}

var triggerCapabilities = map[string]TriggerCapability{
	"http_request": {
		Block: "http_request", Kind: model.TriggerHTTP, Execution: "native", TracePropagation: "automatic_w3c",
		Description: "Execute an HTTP or HTTPS request.",
		Attributes: []AttributeCapability{
			{Name: "method", Type: "string", Required: true, Description: "HTTP method."},
			{Name: "url", Type: "string", Required: true, Description: "Absolute HTTP or HTTPS URL."},
			{Name: "headers", Type: "map(string)", Description: "Request headers; Lamplight owns traceparent and tracestate."},
			{Name: "body", Type: "string", Description: "Optional request body."},
		},
		Example: "http_request {\n  method = \"GET\"\n  url    = \"https://example.test/health\"\n}",
	},
	"grpc_request": {
		Block: "grpc_request", Kind: model.TriggerGRPC, Execution: "native", TracePropagation: "automatic_w3c_metadata",
		Description: "Invoke a unary gRPC method using an inline protobuf descriptor.",
		Attributes: []AttributeCapability{
			{Name: "protobuf", Type: "string", Required: true, Description: "Inline protobuf source."},
			{Name: "address", Type: "string", Required: true, Description: "gRPC server address."},
			{Name: "method", Type: "string", Required: true, Description: "Fully qualified service method."},
			{Name: "request", Type: "string", Required: true, Description: "JSON-encoded request message."},
			{Name: "metadata", Type: "map(string)", Description: "Additional gRPC metadata."},
		},
		Example: "grpc_request {\n  protobuf = \"syntax = \\\"proto3\\\"; package demo; service Health { rpc Check (Request) returns (Reply); } message Request {} message Reply { bool ok = 1; }\"\n  address  = \"localhost:50051\"\n  method   = \"demo.Health/Check\"\n  request  = \"{}\"\n}",
	},
	"graphql_request": {
		Block: "graphql_request", Kind: model.TriggerGraphQL, Execution: "native", TracePropagation: "automatic_w3c",
		Description: "Execute a GraphQL operation over HTTP POST.",
		Attributes: []AttributeCapability{
			{Name: "url", Type: "string", Required: true, Description: "GraphQL endpoint."},
			{Name: "query", Type: "string", Required: true, Description: "GraphQL query or mutation."},
			{Name: "variables", Type: "object", Description: "GraphQL variables."},
			{Name: "operation_name", Type: "string", Description: "GraphQL operation name."},
			{Name: "headers", Type: "map(string)", Description: "Additional HTTP headers."},
		},
		Example: "graphql_request {\n  url   = \"https://example.test/graphql\"\n  query = \"query Health { health }\"\n}",
	},
	"kafka_request": {
		Block: "kafka_request", Kind: model.TriggerKafka, Execution: "native", TracePropagation: "automatic_w3c_headers",
		Description: "Publish one Kafka record.",
		Attributes: []AttributeCapability{
			{Name: "broker_urls", Type: "list(string)", Required: true, Description: "Kafka bootstrap brokers."},
			{Name: "topic", Type: "string", Required: true, Description: "Destination topic."},
			{Name: "message_value", Type: "string", Required: true, Description: "Record value."},
			{Name: "message_key", Type: "string", Description: "Optional record key."},
			{Name: "headers", Type: "map(string)", Description: "Record headers."},
			{Name: "username", Type: "string", Sensitive: true, Description: "SASL username."},
			{Name: "password", Type: "string", Sensitive: true, Description: "SASL password."},
			{Name: "tls", Type: "bool", Description: "Enable TLS."},
		},
		Example: "kafka_request {\n  broker_urls  = [\"localhost:9092\"]\n  topic        = \"events\"\n  message_value = jsonencode({ type = \"health\" })\n}",
	},
	"traceid":    traceIDCapability("traceid", model.TriggerTraceID, "Attach an existing trace ID without executing an external trigger."),
	"cypress":    traceIDCapability("cypress", model.TriggerCypress, "Attach a trace ID produced by Cypress."),
	"playwright": traceIDCapability("playwright", model.TriggerPlaywright, "Attach a trace ID produced by Playwright."),
	"artillery":  traceIDCapability("artillery", model.TriggerArtillery, "Attach a trace ID produced by Artillery."),
	"k6": {
		Block: "k6", Kind: model.TriggerK6, Execution: "local_process_or_trace_attachment", TracePropagation: "w3c_environment",
		Description: "Run a local k6 script or attach an existing k6 trace ID.", ExactlyOneOf: []string{"id", "script"},
		Attributes: []AttributeCapability{
			{Name: "id", Type: "trace_id", Description: "Existing 32-character trace ID; mutually exclusive with script."},
			{Name: "script", Type: "project_relative_file", Description: "k6 JavaScript file; mutually exclusive with id."},
			{Name: "env", Type: "map(string)", Sensitive: true, Description: "Environment values exposed through __ENV."},
			{Name: "arguments", Type: "map(string|number|bool|list)", Description: "k6 flags without leading dashes."},
		},
		Example: "k6 {\n  script = \"k6/load.js\"\n  env = {\n    BASE_URL = \"https://example.test\"\n  }\n  arguments = {\n    vus        = 1\n    iterations = 1\n  }\n}",
	},
	"playwright_engine": {
		Block: "playwright_engine", Kind: model.TriggerPlaywrightEngine, Execution: "npx_process", TracePropagation: "automatic_w3c",
		Description: "Execute an inline Playwright Engine script through npx.",
		Attributes: []AttributeCapability{
			{Name: "target", Type: "string", Required: true, Description: "Target URL."},
			{Name: "script", Type: "string", Required: true, Description: "Inline Playwright Engine JavaScript."},
			{Name: "method", Type: "string", Description: "HTTP method; defaults to GET."},
		},
		Example: "playwright_engine {\n  target = \"https://example.test\"\n  method = \"GET\"\n  script = \"async () => {}\"\n}",
	},
}

func traceIDCapability(block string, kind model.TriggerKind, description string) TriggerCapability {
	return TriggerCapability{
		Block: block, Kind: kind, Execution: "trace_attachment", TracePropagation: "external",
		Description: description,
		Attributes:  []AttributeCapability{{Name: "id", Type: "trace_id", Required: true, Description: "Existing 32-character hexadecimal trace ID."}},
		Example:     block + " {\n  id = \"0123456789abcdef0123456789abcdef\"\n}",
	}
}

// TriggerCapabilities returns a stable, sorted copy of the public inventory.
func TriggerCapabilities() []TriggerCapability {
	blocks := make([]string, 0, len(triggerCapabilities))
	for block := range triggerCapabilities {
		blocks = append(blocks, block)
	}
	sort.Strings(blocks)
	capabilities := make([]TriggerCapability, 0, len(blocks))
	for _, block := range blocks {
		capability := triggerCapabilities[block]
		capability.Attributes = append([]AttributeCapability(nil), capability.Attributes...)
		capability.ExactlyOneOf = append([]string(nil), capability.ExactlyOneOf...)
		capabilities = append(capabilities, capability)
	}
	return capabilities
}

func triggerCapability(block string) (TriggerCapability, bool) {
	capability, ok := triggerCapabilities[block]
	return capability, ok
}
