package mcpserver

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"lamplight/internal/model"
)

type ObserveTraceRequest struct {
	TraceID   string            `json:"trace_id" jsonschema:"32-character hexadecimal trace ID"`
	Target    string            `json:"target,omitempty" jsonschema:"named project target; omit to use project.default_target or local"`
	Variables map[string]string `json:"variables,omitempty" jsonschema:"runtime variable values; prefer MCP process environment for secrets"`
}

type TraceEvidence struct {
	TraceID     string       `json:"trace_id"`
	Found       bool         `json:"found"`
	Valid       bool         `json:"valid"`
	Partial     bool         `json:"partial"`
	Complete    bool         `json:"complete"`
	Spans       []model.Span `json:"spans,omitempty"`
	SpanCount   int          `json:"span_count"`
	Truncated   bool         `json:"truncated,omitempty"`
	Fingerprint string       `json:"fingerprint,omitempty"`
}

func (s *service) observeTrace(ctx context.Context, _ *mcp.CallToolRequest, in ObserveTraceRequest) (*mcp.CallToolResult, TraceEvidence, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.options.ObserveTrace == nil {
		return nil, TraceEvidence{}, fmt.Errorf("MCP server has no trace observer configured")
	}
	out, err := s.options.ObserveTrace(ctx, in)
	if err != nil {
		return toolError(err.Error(), out), out, nil
	}
	return nil, out, nil
}
