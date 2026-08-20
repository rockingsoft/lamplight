package model

import (
	"context"
	"time"

	"github.com/hashicorp/hcl/v2"
	"github.com/zclconf/go-cty/cty"
)

type StaticLoader interface {
	Load(configPath string) (*ProjectDefinition, hcl.Diagnostics)
}

type RuntimeResolver interface {
	Resolve(context.Context, *ProjectDefinition, []string, map[string]string) (*Project, []Diagnostic)
}

type HTTPExecutor interface {
	Execute(context.Context, HTTPRequest, HTTPClientConfig, *TestTraceContext) (Response, error)
}

type ExpressionEvaluator interface {
	Evaluate(hcl.Expression, *hcl.EvalContext) (cty.Value, hcl.Diagnostics)
}

type ArtifactStore interface {
	WriteRun(context.Context, RunResult) ([]ArtifactReference, error)
	Finalize(context.Context, RunResult, bool) ([]ArtifactReference, error)
}

type Renderer interface {
	Render(RunResult) ([]byte, error)
}

type Clock interface {
	Now() time.Time
	After(time.Duration) <-chan time.Time
}
