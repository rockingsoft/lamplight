package hclloader

import (
	"path/filepath"

	"github.com/hashicorp/hcl/v2/hclparse"
	"lamplight/internal/config"
	"lamplight/internal/diagnostic"
	"lamplight/internal/model"
)

// ValidateTestContent validates a prospective .wick file against the rest of
// the current project without writing or replacing any file.
func (loader Loader) ValidateTestContent(options config.Options, path string, content []byte) (*model.ProjectDefinition, []model.Diagnostic) {
	definition, existing := loader.LoadProject(options)
	if definition == nil {
		return nil, existing
	}
	path, _ = filepath.Abs(path)
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	path = filepath.Clean(path)
	diags := make([]model.Diagnostic, 0, len(existing))
	for _, item := range existing {
		if diagnosticFile(item) != path {
			diags = append(diags, item)
		}
	}

	prospective := *definition
	prospective.Variables = make(map[string]model.VariableDefinition, len(definition.Variables))
	for name, variable := range definition.Variables {
		if filepath.Clean(variable.Range.File) != path {
			prospective.Variables[name] = variable
		}
	}
	prospective.Tests = make(map[string]model.TestDefinition, len(definition.Tests))
	for name, test := range definition.Tests {
		if filepath.Clean(test.File) != path {
			prospective.Tests[name] = test
		}
	}

	file, hclDiags := hclparse.NewParser().ParseHCL(content, path)
	if hclDiags.HasErrors() {
		return &prospective, append(diags, diagnostic.FromHCL(hclDiags, diagnostic.CodeParse)...)
	}
	diags = append(diags, parseDefinitions(file, &prospective)...)
	diags = append(diags, validateReferences(&prospective)...)
	return &prospective, diags
}

func diagnosticFile(item model.Diagnostic) string {
	path := item.File
	if path == "" {
		path = item.Range.File
	}
	if path == "" {
		return ""
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return filepath.Clean(path)
	}
	return filepath.Clean(absolute)
}
