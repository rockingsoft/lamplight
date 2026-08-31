// Package hclloader turns project HCL into the frozen static model.
package hclloader

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclparse"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"
	"lamplight/internal/config"
	"lamplight/internal/diagnostic"
	"lamplight/internal/discovery"
	"lamplight/internal/expr"
	"lamplight/internal/model"
)

type Loader struct{ Options config.Options }

// Load implements model.StaticLoader. An empty configPath uses normal upward
// config discovery; a supplied path has the same semantics as --config.
func (loader Loader) Load(configPath string) (*model.ProjectDefinition, hcl.Diagnostics) {
	options := loader.Options
	if configPath != "" {
		options.ConfigPath = configPath
	}
	definition, diags := loader.LoadProject(options)
	return definition, toHCLDiagnostics(diags)
}

func (loader Loader) LoadProject(options config.Options) (*model.ProjectDefinition, []model.Diagnostic) {
	root, diags := config.Load(options)
	if root == nil {
		return nil, diags
	}
	definition := &model.ProjectDefinition{ConfigPath: root.ConfigPath, BaseDir: root.BaseDir, Output: root.Output, HTTPClient: root.HTTPClient, HTTPProxy: root.ProxyExpr, Variables: map[string]model.VariableDefinition{}, Tests: map[string]model.TestDefinition{}, DefaultTarget: root.DefaultTarget, Targets: map[string]model.TargetDefinition{}}
	if root.DatasourceRaw != nil {
		datasource, ds := parseDatasource(root.DatasourceRaw)
		diags = append(diags, ds...)
		definition.Datasource = datasource
	}
	if root.MetricsRaw != nil {
		metrics, ds := parseMetricsSource(root.MetricsRaw)
		diags = append(diags, ds...)
		definition.Metrics = metrics
	}
	if root.InstrumentationRaw != nil {
		instrumentation, ds := parseInstrumentation(root.InstrumentationRaw)
		diags = append(diags, ds...)
		definition.Instrumentation = instrumentation
	}
	for _, block := range root.DefinitionRaw {
		switch block.Type {
		case "variable":
			variable, ds := parseVariable(block)
			diags = append(diags, ds...)
			if variable.Name != "" {
				if prior, exists := definition.Variables[variable.Name]; exists {
					diags = append(diags, diagnostic.Error(diagnostic.CodeDuplicate, diagnostic.ReferenceMessage("variable", variable.Name, prior.Range), block.DefRange, "rename one variable"))
				} else {
					definition.Variables[variable.Name] = variable
				}
			}
		case "target":
			target, ds := parseTarget(block)
			diags = append(diags, ds...)
			if target.Name != "" {
				if prior, exists := definition.Targets[target.Name]; exists {
					diags = append(diags, diagnostic.Error(diagnostic.CodeDuplicate, diagnostic.ReferenceMessage("target", target.Name, prior.Range), block.DefRange, "rename one target"))
				} else {
					definition.Targets[target.Name] = target
				}
			}
		}
	}
	files, err := discovery.Discover(root.BaseDir)
	if err != nil {
		return definition, append(diags, model.Diagnostic{Severity: diagnostic.SeverityError, Code: diagnostic.CodePath, Message: err.Error(), File: root.BaseDir})
	}
	for _, fileName := range files {
		file, hclDiags := hclparse.NewParser().ParseHCLFile(fileName)
		if hclDiags.HasErrors() {
			diags = append(diags, diagnostic.FromHCL(hclDiags, diagnostic.CodeParse)...)
			continue
		}
		fileDiags := parseDefinitions(file, definition)
		diags = append(diags, fileDiags...)
	}
	if definition.DefaultTarget != "" {
		if _, exists := definition.Targets[definition.DefaultTarget]; !exists && definition.DefaultTarget != "local" {
			diags = append(diags, diagnostic.Error(diagnostic.CodeReference, fmt.Sprintf("default target %q is not declared", definition.DefaultTarget), root.ProjectRange, "declare the target or use local"))
		}
	}
	for _, target := range definition.Targets {
		for name, expression := range target.Variables {
			variable, exists := definition.Variables[name]
			if !exists {
				diags = append(diags, diagnostic.Error(diagnostic.CodeReference, fmt.Sprintf("target %q sets undefined variable %q", target.Name, name), expression.Range(), "declare the variable"))
				continue
			}
			if variable.Sensitive {
				diags = append(diags, diagnostic.Error(diagnostic.CodeSensitivity, fmt.Sprintf("target %q cannot set sensitive variable %q", target.Name, name), expression.Range(), "supply it with LAMPLIGHT_VAR_"+name))
				continue
			}
			value, valueDiags := expr.Evaluate(expression, &hcl.EvalContext{Functions: expr.Functions()})
			if valueDiags.HasErrors() {
				diags = append(diags, diagnostic.FromHCL(valueDiags, diagnostic.CodeExpression)...)
				continue
			}
			if !targetValueMatches(variable.Type, value) {
				diags = append(diags, diagnostic.Error(diagnostic.CodeSchema, fmt.Sprintf("target %q value for variable %q does not match type %s", target.Name, name, variable.Type), expression.Range(), "provide a value matching the variable type"))
			}
		}
	}
	diags = append(diags, validateReferences(definition)...)
	if definition.Instrumentation != nil && (definition.Datasource == nil || definition.Datasource.Kind != "otlp") {
		diags = append(diags, diagnostic.Error(diagnostic.CodeConfig, "OBI instrumentation requires the embedded OTLP datasource", root.InstrumentationRaw.DefRange, "add datasource \"otlp\" with a local HTTP endpoint"))
	}
	return definition, diags
}

func parseInstrumentation(block *hcl.Block) (*model.InstrumentationDefinition, []model.Diagnostic) {
	if len(block.Labels) != 1 || block.Labels[0] != "obi" {
		return nil, []model.Diagnostic{diagnostic.Error(diagnostic.CodeSchema, fmt.Sprintf("unsupported instrumentation %q", strings.Join(block.Labels, " ")), block.DefRange, "use instrumentation \"obi\"")}
	}
	content, hclDiags := block.Body.Content(&hcl.BodySchema{Attributes: []hcl.AttributeSchema{{Name: "open_ports", Required: true}, {Name: "image"}, {Name: "context_propagation"}}})
	diags := diagnostic.FromHCL(hclDiags, diagnostic.CodeSchema)
	result := &model.InstrumentationDefinition{Kind: "obi", Image: "docker.io/otel/ebpf-instrument:v0.11.0", ContextPropagation: "all", Range: model.Range(block.DefRange)}
	if attr, ok := content.Attributes["image"]; ok {
		result.Image, diags = appendLiteralString(result.Image, diags, attr.Expr)
	}
	if attr, ok := content.Attributes["context_propagation"]; ok {
		result.ContextPropagation, diags = appendLiteralString(result.ContextPropagation, diags, attr.Expr)
		if result.ContextPropagation != "all" && result.ContextPropagation != "headers" && result.ContextPropagation != "disabled" {
			diags = append(diags, diagnostic.Error(diagnostic.CodeSchema, "instrumentation.context_propagation must be all, headers, or disabled", attr.Expr.Range(), "use all for end-to-end Lamplight correlation"))
		}
	}
	if attr, ok := content.Attributes["open_ports"]; ok {
		value, ds := attr.Expr.Value(nil)
		collection := false
		if !ds.HasErrors() && value != cty.NilVal {
			collection = value.Type().IsTupleType() || value.Type().IsListType() || value.Type().IsSetType()
		}
		if ds.HasErrors() || value == cty.NilVal || !value.IsKnown() || value.IsNull() || !collection {
			diags = append(diags, diagnostic.Error(diagnostic.CodeSchema, "instrumentation.open_ports must be a literal list of ports", attr.Expr.Range(), "use open_ports = [8080]"))
		} else {
			it := value.ElementIterator()
			seen := map[int]bool{}
			for it.Next() {
				_, item := it.Element()
				if item.Type() != cty.Number {
					diags = append(diags, diagnostic.Error(diagnostic.CodeSchema, "instrumentation.open_ports entries must be integers", attr.Expr.Range(), "use ports from 1 through 65535"))
					continue
				}
				n, accuracy := item.AsBigFloat().Int64()
				if accuracy != 0 || n < 1 || n > 65535 {
					diags = append(diags, diagnostic.Error(diagnostic.CodeSchema, "instrumentation.open_ports entries must be valid ports", attr.Expr.Range(), "use ports from 1 through 65535"))
					continue
				}
				if !seen[int(n)] {
					result.OpenPorts = append(result.OpenPorts, int(n))
					seen[int(n)] = true
				}
			}
		}
	}
	if len(result.OpenPorts) == 0 {
		diags = append(diags, diagnostic.Error(diagnostic.CodeSchema, "instrumentation.open_ports must not be empty", block.DefRange, "list at least one application port"))
	}
	return result, diags
}

func targetValueMatches(typeName string, value cty.Value) bool {
	if !value.IsKnown() || value.IsNull() {
		return false
	}
	if typeName == "" || typeName == "string" {
		return value.Type() == cty.String
	}
	if typeName == "int" || typeName == "duration" {
		if value.Type() != cty.Number {
			return false
		}
		_, accuracy := value.AsBigFloat().Int64()
		return accuracy == 0
	}
	return false
}

func parseTarget(block *hcl.Block) (model.TargetDefinition, []model.Diagnostic) {
	target := model.TargetDefinition{Variables: map[string]hcl.Expression{}, Range: model.Range(block.DefRange)}
	if len(block.Labels) != 1 || block.Labels[0] == "" {
		return target, []model.Diagnostic{diagnostic.Error(diagnostic.CodeSchema, "target requires one name label", block.DefRange, "add a target name")}
	}
	target.Name = block.Labels[0]
	content, hclDiags := block.Body.Content(&hcl.BodySchema{
		Attributes: []hcl.AttributeSchema{{Name: "runtime", Required: true}, {Name: "variables"}},
		Blocks:     []hcl.BlockHeaderSchema{{Type: "docker_compose"}, {Type: "kubernetes"}},
	})
	diags := diagnostic.FromHCL(hclDiags, diagnostic.CodeSchema)
	if attr, ok := content.Attributes["runtime"]; ok {
		value, ds := attr.Expr.Value(nil)
		if ds.HasErrors() || !value.IsKnown() || value.IsNull() || value.Type() != cty.String {
			diags = append(diags, diagnostic.Error(diagnostic.CodeSchema, "target.runtime must be a string literal", attr.Expr.Range(), "use local, docker_compose, or kubernetes"))
		} else {
			target.Runtime = value.AsString()
		}
	}
	if target.Runtime != "local" && target.Runtime != "docker_compose" && target.Runtime != "kubernetes" {
		diags = append(diags, diagnostic.Error(diagnostic.CodeSchema, fmt.Sprintf("unsupported target runtime %q", target.Runtime), block.DefRange, "use local, docker_compose, or kubernetes"))
	}
	if attr, ok := content.Attributes["variables"]; ok {
		values, ds := expressionMap(attr.Expr)
		target.Variables = values
		diags = append(diags, ds...)
	}
	compose := blocksOfType(content.Blocks, "docker_compose")
	kubernetes := blocksOfType(content.Blocks, "kubernetes")
	if len(compose) > 1 || len(kubernetes) > 1 {
		diags = append(diags, diagnostic.Error(diagnostic.CodeSchema, "target runtime configuration blocks may appear at most once", block.DefRange, "keep one runtime configuration block"))
	}
	if len(compose) > 0 {
		body, bodyDiags := compose[0].Body.Content(&hcl.BodySchema{Attributes: []hcl.AttributeSchema{{Name: "project"}, {Name: "services"}}})
		diags = append(diags, diagnostic.FromHCL(bodyDiags, diagnostic.CodeSchema)...)
		if attr, ok := body.Attributes["project"]; ok {
			target.Compose.Project, diags = appendLiteralString(target.Compose.Project, diags, attr.Expr)
		}
		if attr, ok := body.Attributes["services"]; ok {
			services, valueDiags := tagsExpression(attr.Expr)
			target.Compose.Services = services
			diags = append(diags, valueDiags...)
		}
	}
	if len(kubernetes) > 0 {
		body, ds := kubernetes[0].Body.Content(&hcl.BodySchema{Attributes: []hcl.AttributeSchema{{Name: "context"}, {Name: "namespace"}, {Name: "service_account"}}})
		diags = append(diags, diagnostic.FromHCL(ds, diagnostic.CodeSchema)...)
		for name, dest := range map[string]*string{"context": &target.Kubernetes.Context, "namespace": &target.Kubernetes.Namespace, "service_account": &target.Kubernetes.ServiceAccount} {
			if attr, ok := body.Attributes[name]; ok {
				*dest, diags = appendLiteralString(*dest, diags, attr.Expr)
			}
		}
	}
	if target.Runtime == "docker_compose" && len(kubernetes) > 0 || target.Runtime == "kubernetes" && len(compose) > 0 {
		diags = append(diags, diagnostic.Error(diagnostic.CodeSchema, "target runtime block does not match target.runtime", block.DefRange, "use the matching runtime block"))
	}
	return target, diags
}

func appendLiteralString(current string, diags []model.Diagnostic, expression hcl.Expression) (string, []model.Diagnostic) {
	value, ds := expression.Value(nil)
	if ds.HasErrors() || !value.IsKnown() || value.IsNull() || value.Type() != cty.String {
		return current, append(diags, diagnostic.Error(diagnostic.CodeSchema, "value must be a string literal", expression.Range(), "use a quoted string"))
	}
	return value.AsString(), diags
}

func parseDatasource(block *hcl.Block) (*model.DatasourceDefinition, []model.Diagnostic) {
	var diags []model.Diagnostic
	if len(block.Labels) != 1 || !model.IsSupportedDatasourceKind(block.Labels[0]) {
		return nil, []model.Diagnostic{diagnostic.Error(diagnostic.CodeSchema, fmt.Sprintf("unsupported datasource %q", strings.Join(block.Labels, " ")), block.DefRange, "use one of: "+strings.Join(model.SupportedDatasourceKinds, ", "))}
	}
	content, hclDiags := block.Body.Content(&hcl.BodySchema{Attributes: []hcl.AttributeSchema{{Name: "endpoint", Required: true}, {Name: "observation_window"}, {Name: "settle_window"}, {Name: "polling_interval"}, {Name: "headers"}}, Blocks: []hcl.BlockHeaderSchema{{Type: "auth"}, {Type: "tls"}}})
	diags = append(diags, diagnostic.FromHCL(hclDiags, diagnostic.CodeSchema)...)
	if _, ok := content.Attributes["endpoint"]; !ok {
		return nil, diags
	}
	datasource := &model.DatasourceDefinition{Kind: block.Labels[0], Endpoint: content.Attributes["endpoint"].Expr, Headers: map[string]hcl.Expression{}, ObservationWindow: 30 * time.Second, SettleWindow: 2 * time.Second, PollingInterval: 500 * time.Millisecond}
	if attr, ok := content.Attributes["headers"]; ok {
		values, ds := expressionMap(attr.Expr)
		datasource.Headers = values
		diags = append(diags, ds...)
	}
	if attr, ok := content.Attributes["observation_window"]; ok {
		value, ds := durationExpression(attr.Expr)
		diags = append(diags, ds...)
		if len(ds) == 0 && value > 0 {
			datasource.ObservationWindow = value
		} else if len(ds) == 0 {
			diags = append(diags, diagnostic.Error(diagnostic.CodeSchema, "datasource.observation_window must be positive", attr.Expr.Range(), "use duration(\"30s\")"))
		}
	}
	if attr, ok := content.Attributes["settle_window"]; ok {
		value, ds := durationExpression(attr.Expr)
		diags = append(diags, ds...)
		if len(ds) == 0 && value > 0 {
			datasource.SettleWindow = value
		} else if len(ds) == 0 {
			diags = append(diags, diagnostic.Error(diagnostic.CodeSchema, "datasource.settle_window must be positive", attr.Expr.Range(), "use duration(\"2s\")"))
		}
	}
	if attr, ok := content.Attributes["polling_interval"]; ok {
		value, ds := durationExpression(attr.Expr)
		diags = append(diags, ds...)
		if len(ds) == 0 && value > 0 {
			datasource.PollingInterval = value
		} else if len(ds) == 0 {
			diags = append(diags, diagnostic.Error(diagnostic.CodeSchema, "datasource.polling_interval must be positive", attr.Expr.Range(), "use duration(\"500ms\")"))
		}
	}
	if len(blocksOfType(content.Blocks, "auth")) > 1 {
		diags = append(diags, diagnostic.Error(diagnostic.CodeSchema, "datasource allows at most one auth block", blocksOfType(content.Blocks, "auth")[1].DefRange, "merge authentication settings"))
	}
	if auth := blocksOfType(content.Blocks, "auth"); len(auth) > 0 {
		body, ds := auth[0].Body.Content(&hcl.BodySchema{Attributes: []hcl.AttributeSchema{{Name: "bearer_token", Required: true}}})
		diags = append(diags, diagnostic.FromHCL(ds, diagnostic.CodeSchema)...)
		if attr, ok := body.Attributes["bearer_token"]; ok {
			datasource.BearerToken = attr.Expr
		}
	}
	if len(blocksOfType(content.Blocks, "tls")) > 1 {
		diags = append(diags, diagnostic.Error(diagnostic.CodeSchema, "datasource allows at most one tls block", blocksOfType(content.Blocks, "tls")[1].DefRange, "merge TLS settings"))
	}
	if tls := blocksOfType(content.Blocks, "tls"); len(tls) > 0 {
		body, ds := tls[0].Body.Content(&hcl.BodySchema{Attributes: []hcl.AttributeSchema{{Name: "skip_verify"}}})
		diags = append(diags, diagnostic.FromHCL(ds, diagnostic.CodeSchema)...)
		if attr, ok := body.Attributes["skip_verify"]; ok {
			value, ds := boolExpression(attr.Expr)
			diags = append(diags, ds...)
			if len(ds) == 0 {
				datasource.TLSSkipVerify = value
				if value {
					diags = append(diags, diagnostic.Warning(diagnostic.CodeConfig, "datasource.tls.skip_verify disables TLS certificate verification", attr.Expr.Range(), "keep TLS verification enabled outside local development"))
				}
			}
		}
	}
	return datasource, diags
}

func parseMetricsSource(block *hcl.Block) (*model.MetricsDefinition, []model.Diagnostic) {
	if len(block.Labels) != 1 || block.Labels[0] != "prometheus" && block.Labels[0] != "prometheus_scrape" {
		return nil, []model.Diagnostic{diagnostic.Error(diagnostic.CodeSchema, fmt.Sprintf("unsupported metrics source %q", strings.Join(block.Labels, " ")), block.DefRange, "use prometheus or prometheus_scrape; datasource \"otlp\" receives metrics directly")}
	}
	content, hclDiags := block.Body.Content(&hcl.BodySchema{Attributes: []hcl.AttributeSchema{{Name: "endpoint", Required: true}, {Name: "observation_window"}, {Name: "settle_window"}, {Name: "polling_interval"}, {Name: "headers"}}, Blocks: []hcl.BlockHeaderSchema{{Type: "auth"}, {Type: "tls"}}})
	diags := diagnostic.FromHCL(hclDiags, diagnostic.CodeSchema)
	endpoint, ok := content.Attributes["endpoint"]
	if !ok {
		return nil, diags
	}
	result := &model.MetricsDefinition{Kind: block.Labels[0], Endpoint: endpoint.Expr, Headers: map[string]hcl.Expression{}, ObservationWindow: 10 * time.Second, SettleWindow: 2 * time.Second, PollingInterval: 500 * time.Millisecond}
	if attr, exists := content.Attributes["headers"]; exists {
		var ds []model.Diagnostic
		result.Headers, ds = expressionMap(attr.Expr)
		diags = append(diags, ds...)
	}
	for name, target := range map[string]*time.Duration{"observation_window": &result.ObservationWindow, "settle_window": &result.SettleWindow, "polling_interval": &result.PollingInterval} {
		if attr, exists := content.Attributes[name]; exists {
			value, ds := durationExpression(attr.Expr)
			diags = append(diags, ds...)
			if len(ds) == 0 && value > 0 {
				*target = value
			} else if len(ds) == 0 {
				diags = append(diags, diagnostic.Error(diagnostic.CodeSchema, "metrics."+name+" must be positive", attr.Expr.Range(), "use a positive duration"))
			}
		}
	}
	auth := blocksOfType(content.Blocks, "auth")
	if len(auth) > 1 {
		diags = append(diags, diagnostic.Error(diagnostic.CodeSchema, "metrics allows at most one auth block", auth[1].DefRange, "merge auth settings"))
	}
	if len(auth) > 0 {
		body, ds := auth[0].Body.Content(&hcl.BodySchema{Attributes: []hcl.AttributeSchema{{Name: "bearer_token", Required: true}}})
		diags = append(diags, diagnostic.FromHCL(ds, diagnostic.CodeSchema)...)
		if attr, exists := body.Attributes["bearer_token"]; exists {
			result.BearerToken = attr.Expr
		}
	}
	tlsBlocks := blocksOfType(content.Blocks, "tls")
	if len(tlsBlocks) > 1 {
		diags = append(diags, diagnostic.Error(diagnostic.CodeSchema, "metrics allows at most one tls block", tlsBlocks[1].DefRange, "merge TLS settings"))
	}
	if len(tlsBlocks) > 0 {
		body, ds := tlsBlocks[0].Body.Content(&hcl.BodySchema{Attributes: []hcl.AttributeSchema{{Name: "skip_verify"}}})
		diags = append(diags, diagnostic.FromHCL(ds, diagnostic.CodeSchema)...)
		if attr, exists := body.Attributes["skip_verify"]; exists {
			value, valueDiags := attr.Expr.Value(nil)
			if valueDiags.HasErrors() || !value.IsKnown() || value.IsNull() || value.Type() != cty.Bool {
				diags = append(diags, diagnostic.Error(diagnostic.CodeSchema, "metrics.tls.skip_verify must be a boolean literal", attr.Expr.Range(), "use true or false"))
			} else {
				result.TLSSkipVerify = value.True()
			}
		}
	}
	return result, diags
}

func parseDefinitions(file *hcl.File, definition *model.ProjectDefinition) []model.Diagnostic {
	content, hclDiags := file.Body.Content(&hcl.BodySchema{Blocks: []hcl.BlockHeaderSchema{{Type: "variable", LabelNames: []string{"name"}}, {Type: "test", LabelNames: []string{"name"}}}})
	diags := diagnostic.FromHCL(hclDiags, diagnostic.CodeSchema)
	for _, block := range content.Blocks {
		switch block.Type {
		case "variable":
			variable, ds := parseVariable(block)
			diags = append(diags, ds...)
			if variable.Name == "" {
				continue
			}
			if prior, exists := definition.Variables[variable.Name]; exists {
				diags = append(diags, diagnostic.Error(diagnostic.CodeDuplicate, diagnostic.ReferenceMessage("variable", variable.Name, prior.Range), block.DefRange, "rename one variable"))
				continue
			}
			definition.Variables[variable.Name] = variable
		case "test":
			test, ds := parseTest(block, file.Bytes)
			diags = append(diags, ds...)
			if test.Name == "" {
				continue
			}
			if prior, exists := definition.Tests[test.Name]; exists {
				diags = append(diags, diagnostic.Error(diagnostic.CodeDuplicate, diagnostic.ReferenceMessage("test", test.Name, prior.Range), block.DefRange, "rename one test"))
				continue
			}
			definition.Tests[test.Name] = test
		}
	}
	return diags
}

func parseVariable(block *hcl.Block) (model.VariableDefinition, []model.Diagnostic) {
	variable := model.VariableDefinition{Type: "string", Range: model.Range(block.DefRange)}
	var diags []model.Diagnostic
	if len(block.Labels) != 1 {
		return variable, []model.Diagnostic{diagnostic.Error(diagnostic.CodeSchema, "variable requires one name label", block.DefRange, "add a variable name")}
	}
	variable.Name = block.Labels[0]
	if !hclsyntax.ValidIdentifier(variable.Name) {
		diags = append(diags, diagnostic.Error(diagnostic.CodeSchema, fmt.Sprintf("variable name %q must be a valid HCL identifier", variable.Name), block.DefRange, "use letters, digits, underscores, and hyphens allowed by HCL"))
	}
	content, hclDiags := block.Body.Content(&hcl.BodySchema{Attributes: []hcl.AttributeSchema{{Name: "type"}, {Name: "default"}, {Name: "sensitive"}}})
	diags = append(diags, diagnostic.FromHCL(hclDiags, diagnostic.CodeSchema)...)
	if attr, ok := content.Attributes["type"]; ok {
		typeName, ds := typeName(attr.Expr)
		diags = append(diags, ds...)
		if typeName != "" {
			variable.Type = typeName
		}
	}
	if variable.Type != "string" && variable.Type != "int" && variable.Type != "duration" {
		diags = append(diags, diagnostic.Error(diagnostic.CodeSchema, fmt.Sprintf("variable %q has unsupported type %q", variable.Name, variable.Type), block.DefRange, "use string, int, or duration"))
	}
	if attr, ok := content.Attributes["sensitive"]; ok {
		value, ds := boolExpression(attr.Expr)
		diags = append(diags, ds...)
		if len(ds) == 0 {
			variable.Sensitive = value
		}
	}
	if attr, ok := content.Attributes["default"]; ok {
		value, hclDs := expr.Evaluate(attr.Expr, &hcl.EvalContext{Functions: expr.Functions()})
		if hclDs.HasErrors() {
			diags = append(diags, diagnostic.FromHCL(hclDs, diagnostic.CodeExpression)...)
		} else {
			variable.Default, variable.HasDefault = value, true
			if !validDefault(variable.Type, value) {
				diags = append(diags, diagnostic.Error(diagnostic.CodeSchema, fmt.Sprintf("default for variable %q does not match type %s", variable.Name, variable.Type), attr.Expr.Range(), "change the default or variable type"))
			}
		}
	}
	return variable, diags
}

func parseTest(block *hcl.Block, _ []byte) (model.TestDefinition, []model.Diagnostic) {
	test := model.TestDefinition{File: block.DefRange.Filename, Range: model.Range(block.DefRange), Outputs: map[string]hcl.Expression{}}
	var diags []model.Diagnostic
	if len(block.Labels) != 1 {
		return test, []model.Diagnostic{diagnostic.Error(diagnostic.CodeSchema, "test requires one name label", block.DefRange, "add a test name")}
	}
	test.Name = block.Labels[0]
	content, hclDiags := block.Body.Content(&hcl.BodySchema{Attributes: []hcl.AttributeSchema{{Name: "tags"}, {Name: "outputs"}}, Blocks: []hcl.BlockHeaderSchema{{Type: "step", LabelNames: []string{"name"}}}})
	diags = append(diags, diagnostic.FromHCL(hclDiags, diagnostic.CodeSchema)...)
	if attr, ok := content.Attributes["tags"]; ok {
		tags, ds := tagsExpression(attr.Expr)
		test.Tags = tags
		diags = append(diags, ds...)
	}
	if attr, ok := content.Attributes["outputs"]; ok {
		outputs, ds := expressionMap(attr.Expr)
		test.Outputs = outputs
		diags = append(diags, ds...)
		diags = append(diags, validateIdentifiers("test output", outputs)...)
	}
	if len(content.Blocks) == 0 {
		diags = append(diags, diagnostic.Error(diagnostic.CodeSchema, fmt.Sprintf("test %q requires at least one step", test.Name), block.DefRange, "add a step block"))
	}
	steps := map[string]model.StepDefinition{}
	for _, child := range content.Blocks {
		step, ds := parseStep(child)
		diags = append(diags, ds...)
		if step.Name == "" {
			continue
		}
		if prior, exists := steps[step.Name]; exists {
			diags = append(diags, diagnostic.Error(diagnostic.CodeDuplicate, diagnostic.ReferenceMessage("step", step.Name, prior.Range), child.DefRange, "rename one step"))
			continue
		}
		steps[step.Name] = step
		test.Steps = append(test.Steps, step)
	}
	return test, diags
}

func parseStep(block *hcl.Block) (model.StepDefinition, []model.Diagnostic) {
	step := model.StepDefinition{Range: model.Range(block.DefRange), Outputs: map[string]hcl.Expression{}}
	var diags []model.Diagnostic
	if len(block.Labels) != 1 {
		return step, []model.Diagnostic{diagnostic.Error(diagnostic.CodeSchema, "step requires one name label", block.DefRange, "add a step name")}
	}
	step.Name = block.Labels[0]
	if !hclsyntax.ValidIdentifier(step.Name) {
		diags = append(diags, diagnostic.Error(diagnostic.CodeSchema, fmt.Sprintf("step name %q must be a valid HCL identifier", step.Name), block.DefRange, "use a valid HCL identifier"))
	}
	triggerBlocks := []hcl.BlockHeaderSchema{{Type: "check", LabelNames: []string{"name"}}}
	for _, capability := range TriggerCapabilities() {
		triggerBlocks = append(triggerBlocks, hcl.BlockHeaderSchema{Type: capability.Block})
	}
	content, hclDiags := block.Body.Content(&hcl.BodySchema{Attributes: []hcl.AttributeSchema{{Name: "outputs"}}, Blocks: triggerBlocks})
	diags = append(diags, diagnostic.FromHCL(hclDiags, diagnostic.CodeSchema)...)
	var triggers []*hcl.Block
	for _, child := range content.Blocks {
		if child.Type != "check" {
			triggers = append(triggers, child)
		}
	}
	if len(triggers) != 1 {
		diags = append(diags, diagnostic.Error(diagnostic.CodeSchema, fmt.Sprintf("step %q requires exactly one trigger block", step.Name), block.DefRange, "add one request or traceid trigger block"))
	} else if triggers[0].Type == "http_request" {
		request, ds := parseHTTP(triggers[0])
		step.HTTP = request
		step.Trigger.Kind = model.TriggerHTTP
		diags = append(diags, ds...)
	} else {
		trigger, ds := parseTrigger(triggers[0])
		step.Trigger = trigger
		diags = append(diags, ds...)
	}
	if attr, ok := content.Attributes["outputs"]; ok {
		outputs, ds := expressionMap(attr.Expr)
		step.Outputs = outputs
		diags = append(diags, ds...)
		diags = append(diags, validateIdentifiers("step output", outputs)...)
	}
	for _, child := range blocksOfType(content.Blocks, "check") {
		check, ds := parseCheck(child)
		step.Checks = append(step.Checks, check)
		diags = append(diags, ds...)
	}
	return step, diags
}

func parseTrigger(block *hcl.Block) (model.TriggerDefinition, []model.Diagnostic) {
	capability, exists := triggerCapability(block.Type)
	if !exists || block.Type == "http_request" {
		return model.TriggerDefinition{}, []model.Diagnostic{diagnostic.Error(diagnostic.CodeSchema, fmt.Sprintf("unsupported trigger block %q", block.Type), block.DefRange, "use a trigger returned by lamplight_get_capabilities")}
	}
	schema := &hcl.BodySchema{}
	for _, attribute := range capability.Attributes {
		schema.Attributes = append(schema.Attributes, hcl.AttributeSchema{Name: attribute.Name, Required: attribute.Required})
	}
	if block.Type == "k6" {
		schema.Blocks = append(schema.Blocks, hcl.BlockHeaderSchema{Type: "executor", LabelNames: []string{"kind"}})
	}
	content, hclDiags := block.Body.Content(schema)
	definition := model.TriggerDefinition{Kind: capability.Kind, Attributes: map[string]hcl.Expression{}}
	for name, attribute := range content.Attributes {
		definition.Attributes[name] = attribute.Expr
	}
	diags := diagnostic.FromHCL(hclDiags, diagnostic.CodeSchema)
	if block.Type == "k6" {
		_, hasID := content.Attributes["id"]
		_, hasScript := content.Attributes["script"]
		if hasID == hasScript {
			diags = append(diags, diagnostic.Error(diagnostic.CodeSchema, "k6 requires exactly one of id or script", block.DefRange, "use script to execute k6 or id to attach an existing trace"))
		}
		executors := blocksOfType(content.Blocks, "executor")
		if len(executors) > 1 {
			diags = append(diags, diagnostic.Error(diagnostic.CodeSchema, "k6 executor may appear at most once", block.DefRange, "keep one executor block"))
		} else if len(executors) == 1 {
			if hasID {
				diags = append(diags, diagnostic.Error(diagnostic.CodeSchema, "k6 executor requires script execution", executors[0].DefRange, "remove id and configure script"))
			}
			executor := executors[0]
			if len(executor.Labels) != 1 || executor.Labels[0] != "cloud_run" {
				diags = append(diags, diagnostic.Error(diagnostic.CodeSchema, "k6 executor must be cloud_run", executor.DefRange, "use executor \"cloud_run\""))
			} else {
				attributes := []hcl.AttributeSchema{{Name: "project", Required: true}, {Name: "region", Required: true}, {Name: "job", Required: true}, {Name: "bucket", Required: true}, {Name: "tasks", Required: true}, {Name: "job_env"}, {Name: "timeout"}, {Name: "start_delay"}}
				body, bodyDiags := executor.Body.Content(&hcl.BodySchema{Attributes: attributes})
				diags = append(diags, diagnostic.FromHCL(bodyDiags, diagnostic.CodeSchema)...)
				definition.Executor = &model.TriggerExecutorDefinition{Kind: "cloud_run", Attributes: map[string]hcl.Expression{}, Range: model.Range(executor.DefRange)}
				for name, attribute := range body.Attributes {
					definition.Executor.Attributes[name] = attribute.Expr
				}
			}
		}
		if _, hasFiles := content.Attributes["files"]; hasFiles && len(executors) == 0 {
			diags = append(diags, diagnostic.Error(diagnostic.CodeSchema, "k6.files requires a remote executor", content.Attributes["files"].Range, "add executor \"cloud_run\" or remove files"))
		}
	}
	return definition, diags
}

func parseHTTP(block *hcl.Block) (model.HTTPRequestDefinition, []model.Diagnostic) {
	request := model.HTTPRequestDefinition{Headers: map[string]hcl.Expression{}}
	capability, _ := triggerCapability("http_request")
	schema := &hcl.BodySchema{}
	for _, attribute := range capability.Attributes {
		schema.Attributes = append(schema.Attributes, hcl.AttributeSchema{Name: attribute.Name, Required: attribute.Required})
	}
	content, hclDiags := block.Body.Content(schema)
	diags := diagnostic.FromHCL(hclDiags, diagnostic.CodeSchema)
	if attr, ok := content.Attributes["method"]; ok {
		request.Method = attr.Expr
	}
	if attr, ok := content.Attributes["url"]; ok {
		request.URL = attr.Expr
	}
	if attr, ok := content.Attributes["body"]; ok {
		request.Body = attr.Expr
	}
	if attr, ok := content.Attributes["headers"]; ok {
		values, ds := expressionMap(attr.Expr)
		request.Headers = values
		diags = append(diags, ds...)
	}
	return request, diags
}

func parseCheck(block *hcl.Block) (model.CheckDefinition, []model.Diagnostic) {
	check := model.CheckDefinition{Range: model.Range(block.DefRange), Response: map[string]hcl.Expression{}}
	var diags []model.Diagnostic
	if len(block.Labels) != 1 {
		return check, []model.Diagnostic{diagnostic.Error(diagnostic.CodeSchema, "check requires one name label", block.DefRange, "add a check name")}
	}
	check.Name = block.Labels[0]
	content, hclDiags := block.Body.Content(&hcl.BodySchema{Attributes: []hcl.AttributeSchema{{Name: "response"}}, Blocks: []hcl.BlockHeaderSchema{{Type: "spans"}, {Type: "metrics"}}})
	diags = append(diags, diagnostic.FromHCL(hclDiags, diagnostic.CodeSchema)...)
	if attr, ok := content.Attributes["response"]; ok {
		response, ds := expressionMap(attr.Expr)
		check.Response = response
		diags = append(diags, ds...)
		if len(response) == 0 {
			diags = append(diags, diagnostic.Error(diagnostic.CodeSchema, "check.response must not be empty", attr.Expr.Range(), "add a named boolean expression"))
		}
	}
	spans := blocksOfType(content.Blocks, "spans")
	if len(spans) > 1 {
		diags = append(diags, diagnostic.Error(diagnostic.CodeSchema, "check allows at most one spans block", spans[1].DefRange, "merge span settings"))
	}
	if len(spans) > 0 {
		value, ds := parseSpans(spans[0])
		check.Spans = value
		diags = append(diags, ds...)
	}
	metrics := blocksOfType(content.Blocks, "metrics")
	if len(metrics) > 1 {
		diags = append(diags, diagnostic.Error(diagnostic.CodeSchema, "check allows at most one metrics block", metrics[1].DefRange, "merge metric settings"))
	}
	if len(metrics) > 0 {
		value, ds := parseMetricsCheck(metrics[0])
		check.Metrics = value
		diags = append(diags, ds...)
	}
	if len(check.Response) == 0 && check.Spans == nil && check.Metrics == nil {
		diags = append(diags, diagnostic.Error(diagnostic.CodeSchema, "check must contain response, spans, metrics, or a combination", block.DefRange, "add a response map, spans block, or metrics block"))
	}
	return check, diags
}

func parseMetricsCheck(block *hcl.Block) (*model.MetricCheckDefinition, []model.Diagnostic) {
	result := &model.MetricCheckDefinition{Assertions: map[string]hcl.Expression{}}
	content, hclDiags := block.Body.Content(&hcl.BodySchema{Attributes: []hcl.AttributeSchema{{Name: "query", Required: true}, {Name: "assertions"}, {Name: "at_least"}, {Name: "at_most"}, {Name: "exactly"}, {Name: "observation_window"}}})
	diags := diagnostic.FromHCL(hclDiags, diagnostic.CodeSchema)
	if attr, ok := content.Attributes["query"]; ok {
		result.Query = attr.Expr
	}
	if attr, ok := content.Attributes["assertions"]; ok {
		var ds []model.Diagnostic
		result.Assertions, ds = expressionMap(attr.Expr)
		diags = append(diags, ds...)
	}
	quantityCount := 0
	for _, name := range []string{"at_least", "at_most", "exactly"} {
		if attr, ok := content.Attributes[name]; ok {
			quantityCount++
			value, ds := intExpression(attr.Expr)
			diags = append(diags, ds...)
			if len(ds) == 0 && value >= 0 {
				result.Rule = model.QuantityRule{Kind: name, Value: int(value)}
			} else if len(ds) == 0 {
				diags = append(diags, diagnostic.Error(diagnostic.CodeSchema, "metric quantity must be non-negative", attr.Expr.Range(), "use zero or a positive integer"))
			}
		}
	}
	if quantityCount != 1 {
		diags = append(diags, diagnostic.Error(diagnostic.CodeSchema, "metrics requires exactly one of at_least, at_most, or exactly", block.DefRange, "choose one quantity rule"))
	}
	if attr, ok := content.Attributes["observation_window"]; ok {
		value, ds := durationExpression(attr.Expr)
		diags = append(diags, ds...)
		if len(ds) == 0 && value > 0 {
			result.ObservationWindow = value
		} else if len(ds) == 0 {
			diags = append(diags, diagnostic.Error(diagnostic.CodeSchema, "metrics.observation_window must be positive", attr.Expr.Range(), "use a positive duration"))
		}
	}
	return result, diags
}

func parseSpans(block *hcl.Block) (*model.SpanCheckDefinition, []model.Diagnostic) {
	spans := &model.SpanCheckDefinition{Assertions: map[string]hcl.Expression{}}
	content, hclDiags := block.Body.Content(&hcl.BodySchema{Attributes: []hcl.AttributeSchema{{Name: "matching", Required: true}, {Name: "assertions"}, {Name: "span_assertions"}, {Name: "at_least"}, {Name: "at_most"}, {Name: "exactly"}, {Name: "observation_window"}}})
	diags := diagnostic.FromHCL(hclDiags, diagnostic.CodeSchema)
	if attr, ok := content.Attributes["matching"]; ok {
		spans.Matching = attr.Expr
	}
	assertionsAttr, hasAssertions := content.Attributes["assertions"]
	legacyAssertionsAttr, hasLegacyAssertions := content.Attributes["span_assertions"]
	if hasAssertions && hasLegacyAssertions {
		diags = append(diags, diagnostic.Error(diagnostic.CodeSchema, "spans cannot define both assertions and span_assertions", legacyAssertionsAttr.Expr.Range(), "keep assertions; span_assertions is a legacy alias"))
	}
	if hasAssertions {
		assertions, ds := expressionMap(assertionsAttr.Expr)
		spans.Assertions = assertions
		diags = append(diags, ds...)
	} else if hasLegacyAssertions {
		// span_assertions shipped before the contextual assertions spelling.
		// Keep accepting it so existing projects remain loadable.
		assertions, ds := expressionMap(legacyAssertionsAttr.Expr)
		spans.Assertions = assertions
		diags = append(diags, ds...)
	}
	quantityCount := 0
	for _, rule := range []string{"at_least", "at_most", "exactly"} {
		if attr, ok := content.Attributes[rule]; ok {
			quantityCount++
			value, ds := intExpression(attr.Expr)
			diags = append(diags, ds...)
			if len(ds) == 0 {
				if value < 0 {
					diags = append(diags, diagnostic.Error(diagnostic.CodeSchema, "span quantity must be non-negative", attr.Expr.Range(), "use zero or a positive integer"))
				} else {
					spans.Rule = model.QuantityRule{Kind: rule, Value: int(value)}
				}
			}
		}
	}
	if quantityCount != 1 {
		diags = append(diags, diagnostic.Error(diagnostic.CodeSchema, "spans requires exactly one of at_least, at_most, or exactly", block.DefRange, "choose one quantity rule"))
	}
	if attr, ok := content.Attributes["observation_window"]; ok {
		value, ds := durationExpression(attr.Expr)
		diags = append(diags, ds...)
		if len(ds) == 0 {
			if value <= 0 {
				diags = append(diags, diagnostic.Error(diagnostic.CodeSchema, "spans.observation_window must be positive", attr.Expr.Range(), "use duration(\"30s\")"))
			} else {
				spans.ObservationWindow = value
			}
		}
	}
	return spans, diags
}

func expressionMap(expression hcl.Expression) (map[string]hcl.Expression, []model.Diagnostic) {
	pairs, hclDiags := hcl.ExprMap(expression)
	if hclDiags.HasErrors() {
		return nil, diagnostic.FromHCL(hclDiags, diagnostic.CodeSchema)
	}
	result := make(map[string]hcl.Expression, len(pairs))
	var diags []model.Diagnostic
	for _, pair := range pairs {
		value, ds := pair.Key.Value(nil)
		if ds.HasErrors() || !value.IsKnown() || value.Type() != cty.String {
			if ds.HasErrors() {
				diags = append(diags, diagnostic.FromHCL(ds, diagnostic.CodeSchema)...)
			} else {
				diags = append(diags, diagnostic.Error(diagnostic.CodeSchema, "map keys must be known strings", pair.Key.Range(), "use a quoted key"))
			}
			continue
		}
		name := value.AsString()
		if _, exists := result[name]; exists {
			diags = append(diags, diagnostic.Error(diagnostic.CodeDuplicate, fmt.Sprintf("duplicate map key %q", name), pair.Key.Range(), "use unique map keys"))
			continue
		}
		result[name] = pair.Value
	}
	return result, diags
}

func typeName(expression hcl.Expression) (string, []model.Diagnostic) {
	if traversal, ds := hcl.AbsTraversalForExpr(expression); !ds.HasErrors() && len(traversal) == 1 {
		return traversal.RootName(), nil
	}
	value, ds := expression.Value(nil)
	if ds.HasErrors() || !value.IsKnown() || value.Type() != cty.String {
		if ds.HasErrors() {
			return "", diagnostic.FromHCL(ds, diagnostic.CodeSchema)
		}
		return "", []model.Diagnostic{diagnostic.Error(diagnostic.CodeSchema, "variable.type must be string, int, or duration", expression.Range(), "use a supported type name")}
	}
	return value.AsString(), nil
}
func boolExpression(expression hcl.Expression) (bool, []model.Diagnostic) {
	value, ds := expression.Value(nil)
	if ds.HasErrors() || !value.IsKnown() || value.Type() != cty.Bool {
		if ds.HasErrors() {
			return false, diagnostic.FromHCL(ds, diagnostic.CodeSchema)
		}
		return false, []model.Diagnostic{diagnostic.Error(diagnostic.CodeSchema, "value must be a boolean literal", expression.Range(), "use true or false")}
	}
	return value.True(), nil
}
func intExpression(expression hcl.Expression) (int64, []model.Diagnostic) {
	value, ds := expression.Value(nil)
	if ds.HasErrors() || !value.IsKnown() || value.Type() != cty.Number {
		if ds.HasErrors() {
			return 0, diagnostic.FromHCL(ds, diagnostic.CodeSchema)
		}
		return 0, []model.Diagnostic{diagnostic.Error(diagnostic.CodeSchema, "value must be an integer literal", expression.Range(), "use an integer")}
	}
	number, accuracy := value.AsBigFloat().Int64()
	if accuracy != 0 {
		return 0, []model.Diagnostic{diagnostic.Error(diagnostic.CodeSchema, "value must be an integer", expression.Range(), "use an integer")}
	}
	return number, nil
}
func durationExpression(expression hcl.Expression) (time.Duration, []model.Diagnostic) {
	value, ds := expr.Evaluate(expression, &hcl.EvalContext{Functions: expr.Functions()})
	if ds.HasErrors() {
		return 0, diagnostic.FromHCL(ds, diagnostic.CodeExpression)
	}
	if !value.IsKnown() || value.Type() != cty.Number {
		return 0, []model.Diagnostic{diagnostic.Error(diagnostic.CodeSchema, "value must be duration(\"…\")", expression.Range(), "use a duration expression")}
	}
	number, accuracy := value.AsBigFloat().Int64()
	if accuracy != 0 {
		return 0, []model.Diagnostic{diagnostic.Error(diagnostic.CodeSchema, "duration is out of range", expression.Range(), "use a valid duration")}
	}
	return time.Duration(number), nil
}
func tagsExpression(expression hcl.Expression) ([]string, []model.Diagnostic) {
	value, ds := expression.Value(nil)
	if ds.HasErrors() || !value.IsKnown() || !(value.Type().IsTupleType() || value.Type().IsListType() || value.Type().IsSetType()) {
		if ds.HasErrors() {
			return nil, diagnostic.FromHCL(ds, diagnostic.CodeSchema)
		}
		return nil, []model.Diagnostic{diagnostic.Error(diagnostic.CodeSchema, "tags must be a literal list of strings", expression.Range(), "use tags = [\"smoke\"]")}
	}
	var tags []string
	iterator := value.ElementIterator()
	for iterator.Next() {
		_, item := iterator.Element()
		if item.Type() != cty.String {
			return nil, []model.Diagnostic{diagnostic.Error(diagnostic.CodeSchema, "tags must contain only strings", expression.Range(), "use string tags")}
		}
		tags = append(tags, item.AsString())
	}
	sort.Strings(tags)
	for i := 1; i < len(tags); i++ {
		if tags[i] == tags[i-1] {
			return nil, []model.Diagnostic{diagnostic.Error(diagnostic.CodeDuplicate, fmt.Sprintf("duplicate tag %q", tags[i]), expression.Range(), "use each tag once")}
		}
	}
	return tags, nil
}
func validDefault(typeName string, value cty.Value) bool {
	if !value.IsKnown() || value.IsNull() {
		return false
	}
	switch typeName {
	case "string":
		return value.Type() == cty.String
	case "int", "duration":
		if value.Type() != cty.Number {
			return false
		}
		_, accuracy := value.AsBigFloat().Int64()
		return accuracy == 0
	}
	return false
}
func validateIdentifiers(kind string, expressions map[string]hcl.Expression) []model.Diagnostic {
	var diags []model.Diagnostic
	for name, expression := range expressions {
		if !hclsyntax.ValidIdentifier(name) {
			diags = append(diags, diagnostic.Error(diagnostic.CodeSchema, fmt.Sprintf("%s name %q must be a valid HCL identifier", kind, name), expression.Range(), "use a valid HCL identifier"))
		}
	}
	return diags
}

func validateReferences(definition *model.ProjectDefinition) []model.Diagnostic {
	var diags []model.Diagnostic
	for _, test := range definition.Tests {
		seen := map[string]model.StepDefinition{}
		for _, step := range test.Steps {
			for _, expression := range append([]hcl.Expression{step.HTTP.Method, step.HTTP.URL, step.HTTP.Body}, mapExpressions(step.HTTP.Headers)...) {
				diags = append(diags, validateExpression(expression, definition.Variables, seen, "var", "steps")...)
			}
			for _, expression := range mapExpressions(step.Trigger.Attributes) {
				diags = append(diags, validateExpression(expression, definition.Variables, seen, "var", "steps")...)
			}
			if step.Trigger.Executor != nil {
				for _, expression := range mapExpressions(step.Trigger.Executor.Attributes) {
					diags = append(diags, validateExpression(expression, definition.Variables, seen, "var", "steps")...)
				}
			}
			for _, expression := range mapExpressions(step.Outputs) {
				diags = append(diags, validateExpression(expression, definition.Variables, seen, "response", "var", "steps")...)
			}
			for _, check := range step.Checks {
				for _, expression := range mapExpressions(check.Response) {
					diags = append(diags, validateExpression(expression, definition.Variables, seen, "response", "var", "steps")...)
				}
				if check.Spans != nil {
					for _, expression := range append([]hcl.Expression{check.Spans.Matching}, mapExpressions(check.Spans.Assertions)...) {
						diags = append(diags, validateExpression(expression, definition.Variables, seen, "span", "resource", "response", "var", "steps")...)
					}
				}
				if check.Metrics != nil {
					diags = append(diags, validateExpression(check.Metrics.Query, definition.Variables, seen, "var", "steps")...)
					for _, expression := range mapExpressions(check.Metrics.Assertions) {
						diags = append(diags, validateExpression(expression, definition.Variables, seen, "metric", "response", "var", "steps")...)
					}
				}
			}
			seen[step.Name] = step
		}
		for _, expression := range mapExpressions(test.Outputs) {
			diags = append(diags, validateExpression(expression, definition.Variables, seen, "var", "steps")...)
		}
	}
	if definition.Datasource != nil {
		for _, expression := range append([]hcl.Expression{definition.Datasource.Endpoint, definition.Datasource.BearerToken}, mapExpressions(definition.Datasource.Headers)...) {
			if expression != nil {
				diags = append(diags, validateExpression(expression, definition.Variables, nil, "var")...)
			}
		}
	}
	if definition.Metrics != nil {
		for _, expression := range append([]hcl.Expression{definition.Metrics.Endpoint, definition.Metrics.BearerToken}, mapExpressions(definition.Metrics.Headers)...) {
			if expression != nil {
				diags = append(diags, validateExpression(expression, definition.Variables, nil, "var")...)
			}
		}
	}
	return diags
}

func validateExpression(expression hcl.Expression, variables map[string]model.VariableDefinition, prior map[string]model.StepDefinition, roots ...string) []model.Diagnostic {
	if expression == nil {
		return nil
	}
	var diags []model.Diagnostic
	allowed := map[string]bool{}
	for _, root := range roots {
		allowed[root] = true
	}
	for _, traversal := range expression.Variables() {
		root := traversal.RootName()
		if !allowed[root] {
			diags = append(diags, diagnostic.Error(diagnostic.CodeReference, fmt.Sprintf("%q is not available in this expression", root), traversal.SourceRange(), "use only the documented expression context"))
			continue
		}
		if root == "var" {
			if len(traversal) < 2 {
				diags = append(diags, diagnostic.Error(diagnostic.CodeReference, "var must be followed by a variable name", traversal.SourceRange(), "use var.NAME"))
				continue
			}
			attr, ok := traversal[1].(hcl.TraverseAttr)
			if !ok || !hclsyntax.ValidIdentifier(attr.Name) {
				diags = append(diags, diagnostic.Error(diagnostic.CodeReference, "variable reference must use var.NAME", traversal.SourceRange(), "use a declared identifier"))
				continue
			}
			if _, found := variables[attr.Name]; !found {
				diags = append(diags, diagnostic.Error(diagnostic.CodeReference, fmt.Sprintf("variable %q is not declared", attr.Name), traversal.SourceRange(), "declare the variable or correct the reference"))
			}
		}
		if root == "steps" {
			diags = append(diags, validateStepTraversal(traversal, prior)...)
		}
	}
	if hclDiags := expr.Validate(expression, roots...); hclDiags.HasErrors() {
		diags = append(diags, diagnostic.FromHCL(hclDiags, diagnostic.CodeExpression)...)
	}
	return diags
}

func validateStepTraversal(traversal hcl.Traversal, prior map[string]model.StepDefinition) []model.Diagnostic {
	if len(traversal) < 2 {
		return []model.Diagnostic{diagnostic.Error(diagnostic.CodeReference, "steps must be followed by a prior step name", traversal.SourceRange(), "use steps.NAME.outputs.VALUE")}
	}
	stepName, ok := traversal[1].(hcl.TraverseAttr)
	if !ok || !hclsyntax.ValidIdentifier(stepName.Name) {
		return []model.Diagnostic{diagnostic.Error(diagnostic.CodeReference, "step reference must use steps.NAME", traversal.SourceRange(), "use a valid step identifier")}
	}
	step, exists := prior[stepName.Name]
	if !exists {
		return []model.Diagnostic{diagnostic.Error(diagnostic.CodeReference, fmt.Sprintf("step %q is not defined before this reference", stepName.Name), traversal.SourceRange(), "reference an earlier step")}
	}
	if len(traversal) >= 4 {
		outputName, ok := traversal[3].(hcl.TraverseAttr)
		if !ok || traversal[2].(hcl.TraverseAttr).Name != "outputs" || !hclsyntax.ValidIdentifier(outputName.Name) {
			return []model.Diagnostic{diagnostic.Error(diagnostic.CodeReference, "step output reference must use steps.NAME.outputs.VALUE", traversal.SourceRange(), "use a declared output identifier")}
		}
		if _, exists := step.Outputs[outputName.Name]; !exists {
			return []model.Diagnostic{diagnostic.Error(diagnostic.CodeReference, fmt.Sprintf("output %q does not exist on step %q", outputName.Name, stepName.Name), traversal.SourceRange(), "reference a declared output")}
		}
	}
	return nil
}

func mapExpressions(expressions map[string]hcl.Expression) []hcl.Expression {
	result := make([]hcl.Expression, 0, len(expressions))
	for _, expression := range expressions {
		result = append(result, expression)
	}
	return result
}

func checkExpressions(check model.CheckDefinition) []hcl.Expression {
	result := mapExpressions(check.Response)
	if check.Spans != nil {
		result = append(result, check.Spans.Matching)
		result = append(result, mapExpressions(check.Spans.Assertions)...)
	}
	if check.Metrics != nil {
		result = append(result, check.Metrics.Query)
		result = append(result, mapExpressions(check.Metrics.Assertions)...)
	}
	return result
}

func blocksOfType(blocks hcl.Blocks, typeName string) hcl.Blocks {
	result := make(hcl.Blocks, 0)
	for _, block := range blocks {
		if block.Type == typeName {
			result = append(result, block)
		}
	}
	return result
}

func toHCLDiagnostics(diags []model.Diagnostic) hcl.Diagnostics {
	result := make(hcl.Diagnostics, 0, len(diags))
	for _, d := range diags {
		severity := hcl.DiagError
		if d.Severity == diagnostic.SeverityWarning {
			severity = hcl.DiagWarning
		}
		r := hcl.Range{Filename: d.Range.File, Start: hcl.Pos{Line: d.Range.StartLine, Column: d.Range.StartColumn}, End: hcl.Pos{Line: d.Range.EndLine, Column: d.Range.EndColumn}}
		result = append(result, &hcl.Diagnostic{Severity: severity, Summary: d.Code, Detail: d.Message, Subject: &r})
	}
	return result
}

var _ = filepath.Separator
var _ = strings.Builder{}
