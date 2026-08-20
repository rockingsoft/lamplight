// Package formatcmd implements Lamplight's opinionated source formatter.
package formatcmd

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"lamplight/internal/discovery"
)

// LineWidth is deliberately fixed: like gofmt, Lamplight formatting has no
// project-specific style options.
const LineWidth = 100

// InlineCallWidth keeps deeply nested calls readable even when the complete
// attribute happens to fit just below LineWidth.
const InlineCallWidth = 50

// Run formats all .wick files below workingDir, or the supplied files and
// directories when paths is non-empty. It validates every result before
// writing any file so a syntax error cannot leave a project half-formatted.
func Run(workingDir string, paths []string) (int, error) {
	files, err := resolveFiles(workingDir, paths)
	if err != nil {
		return 0, err
	}
	type change struct {
		path string
		mode os.FileMode
		data []byte
	}
	changes := make([]change, 0, len(files))
	for _, path := range files {
		info, err := os.Lstat(path)
		if err != nil {
			return 0, err
		}
		if !info.Mode().IsRegular() {
			return 0, fmt.Errorf("%s is not a regular file", path)
		}
		source, err := os.ReadFile(path)
		if err != nil {
			return 0, err
		}
		formatted, err := Source(source, path)
		if err != nil {
			return 0, err
		}
		if !bytes.Equal(source, formatted) {
			changes = append(changes, change{path: path, mode: info.Mode().Perm(), data: formatted})
		}
	}
	for _, item := range changes {
		if err := os.WriteFile(item.path, item.data, item.mode); err != nil {
			return 0, err
		}
	}
	return len(changes), nil
}

// Source returns the canonical representation of one Lamplight definition.
func Source(source []byte, filename string) ([]byte, error) {
	formatted := hclwrite.Format(source)
	file, diags := hclsyntax.ParseConfig(formatted, filename, hcl.Pos{Line: 1, Column: 1})
	if diags.HasErrors() {
		return nil, fmt.Errorf("format %s: %s", filename, diags.Error())
	}

	var candidates []hcl.Range
	body, ok := file.Body.(*hclsyntax.Body)
	if !ok {
		return nil, fmt.Errorf("format %s: unsupported HCL body", filename)
	}
	_ = hclsyntax.VisitAll(body, func(node hclsyntax.Node) hcl.Diagnostics {
		expression, ok := node.(*hclsyntax.BinaryOpExpr)
		if !ok || expression.Op != hclsyntax.OpLogicalAnd {
			return nil
		}
		rng := expression.Range()
		if rng.Start.Line == rng.End.Line && lineLength(formatted, rng.Start.Byte) > LineWidth {
			candidates = append(candidates, rng)
		}
		return nil
	})
	ranges := outermost(candidates)
	for index := len(ranges) - 1; index >= 0; index-- {
		rng := ranges[index]
		expression, parseDiags := hclsyntax.ParseExpression(formatted[rng.Start.Byte:rng.End.Byte], filename, rng.Start)
		if parseDiags.HasErrors() {
			return nil, fmt.Errorf("format %s: %s", filename, parseDiags.Error())
		}
		parts := flattenAnd(expression, formatted)
		indent := strings.Repeat(" ", rng.Start.Column-1)
		continuation := indent + "  "
		replacement := "(\n" + continuation + strings.Join(parts, " &&\n"+continuation) + "\n" + indent + ")"
		formatted = append(append(append([]byte{}, formatted[:rng.Start.Byte]...), replacement...), formatted[rng.End.Byte:]...)
	}
	formatted = hclwrite.Format(formatted)
	formatted, err := wrapLongFunctionCalls(formatted, filename)
	if err != nil {
		return nil, err
	}
	result := hclwrite.Format(formatted)
	if _, diags := hclsyntax.ParseConfig(result, filename, hcl.Pos{Line: 1, Column: 1}); diags.HasErrors() {
		return nil, fmt.Errorf("format %s: %s", filename, diags.Error())
	}
	return result, nil
}

func wrapLongFunctionCalls(source []byte, filename string) ([]byte, error) {
	file, diags := hclsyntax.ParseConfig(source, filename, hcl.Pos{Line: 1, Column: 1})
	if diags.HasErrors() {
		return nil, fmt.Errorf("format %s: %s", filename, diags.Error())
	}
	body, ok := file.Body.(*hclsyntax.Body)
	if !ok {
		return nil, fmt.Errorf("format %s: unsupported HCL body", filename)
	}
	var calls []*hclsyntax.FunctionCallExpr
	_ = hclsyntax.VisitAll(body, func(node hclsyntax.Node) hcl.Diagnostics {
		call, ok := node.(*hclsyntax.FunctionCallExpr)
		if !ok {
			return nil
		}
		rng := call.Range()
		lineStart := bytes.LastIndexByte(source[:rng.Start.Byte], '\n') + 1
		prefix := source[lineStart:rng.Start.Byte]
		if rng.Start.Line == rng.End.Line && rng.End.Byte-rng.Start.Byte > InlineCallWidth && bytes.Contains(prefix, []byte(" = ")) {
			calls = append(calls, call)
		}
		return nil
	})
	ranges := make([]hcl.Range, len(calls))
	byStart := make(map[int]*hclsyntax.FunctionCallExpr, len(calls))
	for index, call := range calls {
		ranges[index] = call.Range()
		byStart[call.Range().Start.Byte] = call
	}
	ranges = outermost(ranges)
	for index := len(ranges) - 1; index >= 0; index-- {
		rng := ranges[index]
		call := byStart[rng.Start.Byte]
		indent := lineIndent(source, rng.Start.Byte)
		continuation := indent + "  "
		arguments := make([]string, 0, len(call.Args))
		for argumentIndex, argument := range call.Args {
			argumentRange := argument.Range()
			text := strings.TrimSpace(string(source[argumentRange.Start.Byte:argumentRange.End.Byte]))
			if call.ExpandFinal && argumentIndex == len(call.Args)-1 {
				text += "..."
			}
			arguments = append(arguments, text)
		}
		replacement := call.Name + "(\n" + continuation + strings.Join(arguments, ",\n"+continuation) + "\n" + indent + ")"
		source = append(append(append([]byte{}, source[:rng.Start.Byte]...), replacement...), source[rng.End.Byte:]...)
	}
	return source, nil
}

func flattenAnd(expression hclsyntax.Expression, source []byte) []string {
	binary, ok := expression.(*hclsyntax.BinaryOpExpr)
	if !ok || binary.Op != hclsyntax.OpLogicalAnd {
		rng := expression.Range()
		return []string{strings.TrimSpace(string(source[rng.Start.Byte:rng.End.Byte]))}
	}
	return append(flattenAnd(binary.LHS, source), flattenAnd(binary.RHS, source)...)
}

func outermost(ranges []hcl.Range) []hcl.Range {
	sort.Slice(ranges, func(i, j int) bool {
		if ranges[i].Start.Byte == ranges[j].Start.Byte {
			return ranges[i].End.Byte > ranges[j].End.Byte
		}
		return ranges[i].Start.Byte < ranges[j].Start.Byte
	})
	result := make([]hcl.Range, 0, len(ranges))
	for _, rng := range ranges {
		if len(result) > 0 && rng.End.Byte <= result[len(result)-1].End.Byte {
			continue
		}
		result = append(result, rng)
	}
	return result
}

func lineLength(source []byte, offset int) int {
	start := bytes.LastIndexByte(source[:offset], '\n') + 1
	end := bytes.IndexByte(source[offset:], '\n')
	if end < 0 {
		end = len(source)
	} else {
		end += offset
	}
	return end - start
}

func lineIndent(source []byte, offset int) string {
	start := bytes.LastIndexByte(source[:offset], '\n') + 1
	end := start
	for end < len(source) && (source[end] == ' ' || source[end] == '\t') {
		end++
	}
	return string(source[start:end])
}

func resolveFiles(workingDir string, paths []string) ([]string, error) {
	if workingDir == "" {
		workingDir = "."
	}
	if len(paths) == 0 {
		return discovery.Discover(workingDir)
	}
	seen := map[string]struct{}{}
	for _, input := range paths {
		path := input
		if !filepath.IsAbs(path) {
			path = filepath.Join(workingDir, path)
		}
		info, err := os.Lstat(path)
		if err != nil {
			return nil, err
		}
		if info.IsDir() {
			files, err := discovery.Discover(path)
			if err != nil {
				return nil, err
			}
			for _, file := range files {
				seen[file] = struct{}{}
			}
			continue
		}
		if filepath.Ext(path) != discovery.DefinitionSuffix {
			return nil, fmt.Errorf("%s is not a %s file", path, discovery.DefinitionSuffix)
		}
		absolute, err := filepath.Abs(path)
		if err != nil {
			return nil, err
		}
		seen[absolute] = struct{}{}
	}
	files := make([]string, 0, len(seen))
	for file := range seen {
		files = append(files, file)
	}
	sort.Strings(files)
	return files, nil
}
