// Package config resolves the project configuration before test discovery.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclparse"
	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/function"
	"lamplight/internal/diagnostic"
	"lamplight/internal/model"
)

const ConfigFilename = ".lamplight"

type Options struct {
	ConfigPath string
	WorkingDir string
}

type Paths struct {
	ConfigPath string
	ConfigDir  string
}

// Resolve applies --config and --working-dir semantics without reading HCL.
func Resolve(options Options) (Paths, []model.Diagnostic) {
	workingDir := options.WorkingDir
	if workingDir == "" {
		var err error
		workingDir, err = os.Getwd()
		if err != nil {
			return Paths{}, []model.Diagnostic{{Severity: diagnostic.SeverityError, Code: diagnostic.CodePath, Message: err.Error()}}
		}
	}
	workingDir, err := filepath.Abs(workingDir)
	if err != nil {
		return Paths{}, []model.Diagnostic{{Severity: diagnostic.SeverityError, Code: diagnostic.CodePath, Message: err.Error()}}
	}
	if info, err := os.Stat(workingDir); err != nil || !info.IsDir() {
		return Paths{}, []model.Diagnostic{{Severity: diagnostic.SeverityError, Code: diagnostic.CodePath, Message: fmt.Sprintf("working directory %q does not exist or is not a directory", workingDir)}}
	}

	configPath := options.ConfigPath
	if configPath != "" {
		if !filepath.IsAbs(configPath) {
			configPath = filepath.Join(workingDir, configPath)
		}
	} else {
		for current := workingDir; ; current = filepath.Dir(current) {
			candidate := filepath.Join(current, ConfigFilename)
			if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() {
				configPath = candidate
				break
			}
			parent := filepath.Dir(current)
			if parent == current {
				break
			}
		}
	}
	if configPath == "" {
		return Paths{}, []model.Diagnostic{{Severity: diagnostic.SeverityError, Code: diagnostic.CodePath, Message: fmt.Sprintf("could not find %s from %q", ConfigFilename, workingDir)}}
	}
	realPath, err := filepath.EvalSymlinks(configPath)
	if err != nil {
		return Paths{}, []model.Diagnostic{{Severity: diagnostic.SeverityError, Code: diagnostic.CodePath, Message: fmt.Sprintf("configuration %q: %v", configPath, err)}}
	}
	info, err := os.Stat(realPath)
	if err != nil || !info.Mode().IsRegular() {
		return Paths{}, []model.Diagnostic{{Severity: diagnostic.SeverityError, Code: diagnostic.CodePath, Message: fmt.Sprintf("configuration %q is not a regular file", configPath)}}
	}
	return Paths{ConfigPath: realPath, ConfigDir: filepath.Dir(realPath)}, nil
}

type Config struct {
	Paths
	BaseDir            string
	HTTPClient         model.HTTPClientConfig
	ProxyExpr          hcl.Expression
	ConfigFile         *hcl.File
	ProjectRange       hcl.Range
	DatasourceRaw      *hcl.Block
	MetricsRaw         *hcl.Block
	InstrumentationRaw *hcl.Block
	DefinitionRaw      hcl.Blocks
	DefaultTarget      string
}

// Load parses the root configuration and resolves its filesystem-only values.
func Load(options Options) (*Config, []model.Diagnostic) {
	paths, diags := Resolve(options)
	if len(diags) > 0 {
		return nil, diags
	}
	parser := hclparse.NewParser()
	file, hclDiags := parser.ParseHCLFile(paths.ConfigPath)
	if hclDiags.HasErrors() {
		return nil, diagnostic.FromHCL(hclDiags, diagnostic.CodeParse)
	}
	content, hclDiags := file.Body.Content(&hcl.BodySchema{Blocks: []hcl.BlockHeaderSchema{{Type: "project"}, {Type: "datasource", LabelNames: []string{"kind"}}, {Type: "metrics", LabelNames: []string{"kind"}}, {Type: "instrumentation", LabelNames: []string{"kind"}}, {Type: "variable", LabelNames: []string{"name"}}, {Type: "target", LabelNames: []string{"name"}}}})
	diags = append(diags, diagnostic.FromHCL(hclDiags, diagnostic.CodeSchema)...)
	var project *hcl.Block
	for _, block := range content.Blocks {
		switch block.Type {
		case "project":
			if project != nil {
				diags = append(diags, diagnostic.Error(diagnostic.CodeConfig, "exactly one project block is required", block.DefRange, "remove the extra project block"))
			} else {
				project = block
			}
		}
	}
	if project == nil {
		diags = append(diags, diagnostic.Error(diagnostic.CodeConfig, "exactly one project block is required", hcl.Range{Filename: paths.ConfigPath}, "add a project block"))
		return nil, diags
	}
	projectContent, projectDiags := project.Body.Content(&hcl.BodySchema{
		Attributes: []hcl.AttributeSchema{{Name: "base_dir", Required: true}, {Name: "default_target"}},
		Blocks:     []hcl.BlockHeaderSchema{{Type: "http_client"}},
	})
	diags = append(diags, diagnostic.FromHCL(projectDiags, diagnostic.CodeSchema)...)
	baseAttribute, hasBaseDir := projectContent.Attributes["base_dir"]
	if !hasBaseDir {
		return nil, diags
	}
	baseExpr := baseAttribute.Expr
	if len(baseExpr.Variables()) != 0 {
		diags = append(diags, diagnostic.Error(diagnostic.CodeConfig, "project.base_dir cannot depend on runtime values", baseExpr.Range(), "use a literal path"))
	}
	base, valueDiags := literalString(baseExpr)
	diags = append(diags, valueDiags...)
	if base != "" {
		if !filepath.IsAbs(base) {
			base = filepath.Join(paths.ConfigDir, base)
		}
		base, _ = filepath.Abs(base)
		if info, err := os.Stat(base); err != nil || !info.IsDir() {
			diags = append(diags, diagnostic.Error(diagnostic.CodePath, fmt.Sprintf("project.base_dir %q does not exist or is not a directory", base), baseExpr.Range(), "create the directory or correct base_dir"))
		}
	}
	defaultTarget := ""
	if attr, ok := projectContent.Attributes["default_target"]; ok {
		defaultTarget, valueDiags = literalString(attr.Expr)
		diags = append(diags, valueDiags...)
	}
	httpConfig, proxyExpr, httpDiags := parseHTTPClient(projectContent.Blocks, project.DefRange)
	diags = append(diags, httpDiags...)
	var datasource *hcl.Block
	var metrics *hcl.Block
	var instrumentation *hcl.Block
	for _, block := range content.Blocks {
		switch block.Type {
		case "datasource":
			if datasource != nil {
				diags = append(diags, diagnostic.Error(diagnostic.CodeConfig, "at most one datasource block is allowed", block.DefRange, "remove the extra datasource"))
			} else {
				datasource = block
			}
		case "metrics":
			if metrics != nil {
				diags = append(diags, diagnostic.Error(diagnostic.CodeConfig, "at most one metrics block is allowed", block.DefRange, "remove the extra metrics block"))
			} else {
				metrics = block
			}
		case "instrumentation":
			if instrumentation != nil {
				diags = append(diags, diagnostic.Error(diagnostic.CodeConfig, "at most one instrumentation block is allowed", block.DefRange, "remove the extra instrumentation block"))
			} else {
				instrumentation = block
			}
		}
	}
	var definitions hcl.Blocks
	for _, block := range content.Blocks {
		if block.Type == "variable" || block.Type == "target" {
			definitions = append(definitions, block)
		}
	}
	return &Config{Paths: paths, BaseDir: base, HTTPClient: httpConfig, ProxyExpr: proxyExpr, ConfigFile: file, ProjectRange: project.DefRange, DatasourceRaw: datasource, MetricsRaw: metrics, InstrumentationRaw: instrumentation, DefinitionRaw: definitions, DefaultTarget: defaultTarget}, diags
}

func literalString(expression hcl.Expression) (string, []model.Diagnostic) {
	value, hclDiags := expression.Value(nil)
	if hclDiags.HasErrors() {
		return "", diagnostic.FromHCL(hclDiags, diagnostic.CodeConfig)
	}
	if !value.IsKnown() || value.IsNull() || value.Type() != cty.String {
		return "", []model.Diagnostic{diagnostic.Error(diagnostic.CodeConfig, "value must be a known string literal", expression.Range(), "use a quoted string")}
	}
	return value.AsString(), nil
}

func parseHTTPClient(blocks hcl.Blocks, fallback hcl.Range) (model.HTTPClientConfig, hcl.Expression, []model.Diagnostic) {
	config := model.DefaultHTTPClientConfig()
	var proxy hcl.Expression
	var diags []model.Diagnostic
	if len(blocks) == 0 {
		return config, proxy, diags
	}
	if len(blocks) > 1 {
		for _, block := range blocks[1:] {
			diags = append(diags, diagnostic.Error(diagnostic.CodeConfig, "project allows at most one http_client block", block.DefRange, "merge the settings into one block"))
		}
	}
	content, hclDiags := blocks[0].Body.Content(&hcl.BodySchema{Attributes: []hcl.AttributeSchema{{Name: "timeout"}, {Name: "follow_redirects"}, {Name: "max_request_body_bytes"}, {Name: "max_response_body_bytes"}, {Name: "proxy"}, {Name: "tls_skip_verify"}}})
	diags = append(diags, diagnostic.FromHCL(hclDiags, diagnostic.CodeSchema)...)
	if attr, ok := content.Attributes["timeout"]; ok {
		value, ds := durationLiteral(attr.Expr)
		diags = append(diags, ds...)
		if value > 0 {
			config.Timeout = value
		} else if len(ds) == 0 {
			diags = append(diags, diagnostic.Error(diagnostic.CodeConfig, "http_client.timeout must be positive", attr.Expr.Range(), "use duration(\"30s\") or another positive duration"))
		}
	}
	if attr, ok := content.Attributes["follow_redirects"]; ok {
		value, ds := boolLiteral(attr.Expr)
		diags = append(diags, ds...)
		if len(ds) == 0 {
			config.FollowRedirects = value
		}
	}
	if attr, ok := content.Attributes["tls_skip_verify"]; ok {
		value, ds := boolLiteral(attr.Expr)
		diags = append(diags, ds...)
		if len(ds) == 0 {
			config.TLSSkipVerify = value
			if value {
				diags = append(diags, diagnostic.Warning(diagnostic.CodeConfig, "http_client.tls_skip_verify disables TLS certificate verification", attr.Expr.Range(), "keep TLS verification enabled outside local development"))
			}
		}
	}
	for _, setting := range []struct {
		name   string
		target *int64
	}{{"max_request_body_bytes", &config.MaxRequestBodyBytes}, {"max_response_body_bytes", &config.MaxResponseBodyBytes}} {
		if attr, ok := content.Attributes[setting.name]; ok {
			value, ds := intLiteral(attr.Expr)
			diags = append(diags, ds...)
			if len(ds) == 0 {
				if value <= 0 {
					diags = append(diags, diagnostic.Error(diagnostic.CodeConfig, setting.name+" must be positive", attr.Expr.Range(), "use a positive integer"))
				} else {
					*setting.target = value
				}
			}
		}
	}
	if attr, ok := content.Attributes["proxy"]; ok {
		proxy = attr.Expr
		if len(attr.Expr.Variables()) == 0 {
			value, ds := literalString(attr.Expr)
			diags = append(diags, ds...)
			if len(ds) == 0 {
				config.Proxy = value
			}
		}
	}
	return config, proxy, diags
}

func boolLiteral(expression hcl.Expression) (bool, []model.Diagnostic) {
	value, ds := expression.Value(nil)
	if ds.HasErrors() || !value.IsKnown() || value.Type() != cty.Bool {
		if ds.HasErrors() {
			return false, diagnostic.FromHCL(ds, diagnostic.CodeConfig)
		}
		return false, []model.Diagnostic{diagnostic.Error(diagnostic.CodeConfig, "value must be a boolean literal", expression.Range(), "use true or false")}
	}
	return value.True(), nil
}
func intLiteral(expression hcl.Expression) (int64, []model.Diagnostic) {
	value, ds := expression.Value(nil)
	if ds.HasErrors() || !value.IsKnown() || value.Type() != cty.Number {
		if ds.HasErrors() {
			return 0, diagnostic.FromHCL(ds, diagnostic.CodeConfig)
		}
		return 0, []model.Diagnostic{diagnostic.Error(diagnostic.CodeConfig, "value must be an integer literal", expression.Range(), "use an integer")}
	}
	i, acc := value.AsBigFloat().Int64()
	if acc != 0 {
		return 0, []model.Diagnostic{diagnostic.Error(diagnostic.CodeConfig, "value must be an integer", expression.Range(), "use an integer")}
	}
	return i, nil
}
func durationLiteral(expression hcl.Expression) (time.Duration, []model.Diagnostic) {
	value, ds := expression.Value(&hcl.EvalContext{Functions: map[string]function.Function{"duration": durationFunction()}})
	if ds.HasErrors() {
		return 0, diagnostic.FromHCL(ds, diagnostic.CodeConfig)
	}
	if !value.IsKnown() || value.Type() != cty.Number {
		return 0, []model.Diagnostic{diagnostic.Error(diagnostic.CodeConfig, "value must be duration(\"…\")", expression.Range(), "use duration(\"30s\")")}
	}
	i, acc := value.AsBigFloat().Int64()
	if acc != 0 {
		return 0, []model.Diagnostic{diagnostic.Error(diagnostic.CodeConfig, "duration is out of range", expression.Range(), "use a valid Go duration")}
	}
	return time.Duration(i), nil
}

// Kept private so config has no dependency on expression evaluation.
func durationFunction() function.Function {
	return function.New(&function.Spec{Params: []function.Parameter{{Name: "value", Type: cty.String}}, Type: function.StaticReturnType(cty.Number), Impl: func(args []cty.Value, _ cty.Type) (cty.Value, error) {
		d, err := time.ParseDuration(args[0].AsString())
		if err != nil {
			return cty.NilVal, err
		}
		return cty.NumberIntVal(int64(d)), nil
	}})
}
