package cli

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/zclconf/go-cty/cty"
	"lamplight/internal/artifact"
	"lamplight/internal/config"
	"lamplight/internal/datasource"
	"lamplight/internal/engine"
	"lamplight/internal/expr"
	"lamplight/internal/hclloader"
	"lamplight/internal/httpstep"
	"lamplight/internal/initcmd"
	"lamplight/internal/mcpserver"
	"lamplight/internal/model"
	"lamplight/internal/render"
	"lamplight/internal/result"
	"lamplight/internal/runtimevars"
	"lamplight/internal/selection"
	"lamplight/internal/tracecontext"
	"lamplight/internal/tracetestmigrate"
	triggerexecutor "lamplight/internal/trigger"
)

type IO struct {
	In       io.Reader
	Out, Err io.Writer
}

func Main(ctx context.Context, args []string, streams IO) int {
	if streams.Out == nil {
		streams.Out = os.Stdout
	}
	if streams.Err == nil {
		streams.Err = os.Stderr
	}
	if handled, exitCode := handleHelp(args, streams); handled {
		return exitCode
	}
	if len(args) == 0 {
		usage(streams.Err)
		return 1
	}
	switch args[0] {
	case "init":
		fs := flag.NewFlagSet("init", flag.ContinueOnError)
		fs.SetOutput(streams.Err)
		working := fs.String("working-dir", "", "working directory")
		fs.StringVar(working, "w", "", "working directory")
		if fs.Parse(args[1:]) != nil {
			return 1
		}
		dir := *working
		if dir == "" {
			dir = "."
		}
		if err := initcmd.Run(dir); err != nil {
			fmt.Fprintln(streams.Err, "error:", err)
			return 1
		}
		return 0
	case "validate":
		return validate(args[1:], streams)
	case "list":
		if len(args) < 2 || args[1] != "tests" {
			usage(streams.Err)
			return 1
		}
		return listTests(args[2:], streams)
	case "run":
		return run(ctx, args[1:], streams)
	case "mcp":
		return runMCP(ctx, args[1:], streams)
	case "migrate":
		return migrate(args[1:], streams)
	default:
		usage(streams.Err)
		return 1
	}
}

func migrate(args []string, streams IO) int {
	if len(args) == 0 || args[0] != "tracetest" {
		fmt.Fprintln(streams.Err, "error: migrate requires the source format: tracetest")
		return 1
	}
	fs := flag.NewFlagSet("migrate tracetest", flag.ContinueOnError)
	fs.SetOutput(streams.Err)
	outputDir := fs.String("output-dir", ".", "Lamplight project output directory")
	force := fs.Bool("force", false, "overwrite generated .wick files")
	if fs.Parse(args[1:]) != nil {
		return 1
	}
	if len(fs.Args()) != 1 {
		fmt.Fprintln(streams.Err, "error: migrate tracetest requires one YAML file or directory")
		return 1
	}
	results, err := tracetestmigrate.Run(fs.Args()[0], *outputDir, *force)
	if err != nil {
		fmt.Fprintln(streams.Err, "error:", err)
		return 1
	}
	for _, result := range results {
		fmt.Fprintf(streams.Out, "%s -> %s\n", result.Source, result.Destination)
		for _, warning := range result.Warnings {
			fmt.Fprintln(streams.Err, "warning:", warning)
		}
	}
	return 0
}

func runMCP(ctx context.Context, args []string, streams IO) int {
	fs, common := commonFlags("mcp", streams.Err)
	if fs.Parse(args) != nil {
		return 1
	}
	if len(fs.Args()) != 0 {
		fmt.Fprintln(streams.Err, "error: mcp accepts no positional arguments")
		return 1
	}
	server := mcpserver.New(mcpserver.Options{
		ConfigPath: common.config,
		WorkingDir: common.working,
		RunCLI: func(runCtx context.Context, runArgs []string) (int, []byte, []byte) {
			var stdout, stderr bytes.Buffer
			exitCode := Main(runCtx, runArgs, IO{Out: &stdout, Err: &stderr})
			return exitCode, stdout.Bytes(), stderr.Bytes()
		},
	})
	if err := server.Run(ctx, &mcp.StdioTransport{}); err != nil {
		fmt.Fprintln(streams.Err, "error: mcp:", err)
		return 1
	}
	return 0
}

type common struct{ config, working string }

func commonFlags(name string, stderr io.Writer) (*flag.FlagSet, *common) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	c := &common{}
	fs.StringVar(&c.config, "config", "", "config path")
	fs.StringVar(&c.config, "c", "", "config path")
	fs.StringVar(&c.working, "working-dir", "", "working directory")
	fs.StringVar(&c.working, "w", "", "working directory")
	return fs, c
}

func load(c *common, streams IO) (*model.ProjectDefinition, bool) {
	def, diags := hclloader.Loader{}.LoadProject(config.Options{ConfigPath: c.config, WorkingDir: c.working})
	hasErrors := printDiagnostics(streams.Err, diags)
	return def, !hasErrors && def != nil
}

func validate(args []string, streams IO) int {
	fs, c := commonFlags("validate", streams.Err)
	if fs.Parse(args) != nil {
		return 1
	}
	def, ok := load(c, streams)
	if !ok {
		return 1
	}
	fmt.Fprintf(streams.Out, "Valid: %d tests, %d variables\n", len(def.Tests), len(def.Variables))
	return 0
}

func listTests(args []string, streams IO) int {
	fs, c := commonFlags("list tests", streams.Err)
	if fs.Parse(args) != nil {
		return 1
	}
	def, ok := load(c, streams)
	if !ok {
		return 1
	}
	names := make([]string, 0, len(def.Tests))
	for n := range def.Tests {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		t := def.Tests[name]
		fmt.Fprintf(streams.Out, "%s\ttags=%s\tfile=%s\trequires_datasource=%t\n", t.Name, strings.Join(t.Tags, ","), t.File, testNeedsDatasource(t))
	}
	return 0
}

type varsFlag map[string]string

func (v *varsFlag) String() string { return "" }
func (v *varsFlag) Set(raw string) error {
	name, value, ok := strings.Cut(raw, "=")
	if !ok || name == "" {
		return fmt.Errorf("--var requires NAME=VALUE")
	}
	if _, exists := (*v)[name]; exists {
		return fmt.Errorf("duplicate --var %s", name)
	}
	(*v)[name] = value
	return nil
}

func run(ctx context.Context, args []string, streams IO) int {
	fs, c := commonFlags("run", streams.Err)
	vars := varsFlag{}
	tag := fs.String("tag", "", "select tag")
	output := fs.String("output", "", "output format")
	keep := fs.Bool("keep-artifacts", false, "keep successful artifacts")
	artifactsDir := fs.String("artifacts-dir", "", "artifact parent directory")
	fs.Var(&vars, "var", "NAME=VALUE")
	if fs.Parse(normalizeRunArgs(args)) != nil {
		return 1
	}
	remaining := fs.Args()
	if len(remaining) > 1 {
		fmt.Fprintln(streams.Err, "error: run accepts at most one test name")
		return 1
	}
	name := ""
	if len(remaining) == 1 {
		name = remaining[0]
	}
	def, ok := load(c, streams)
	if !ok {
		return 1
	}
	tests, err := selection.Select(def, selection.Selector{Name: name, Tag: *tag})
	if err != nil {
		fmt.Fprintln(streams.Err, "error:", err)
		return 1
	}
	expressions := collectExpressions(def, tests)
	values, diags := runtimevars.Resolve(def.Variables, runtimevars.Input{Vars: vars}, expressions...)
	if printDiagnostics(streams.Err, diags) {
		return 1
	}
	runtimeProject := &model.Project{Definition: def, Variables: values, Tests: tests, HTTPClient: def.HTTPClient}
	if def.HTTPProxy != nil {
		value, err := evalString(def.HTTPProxy, values)
		if err != nil {
			fmt.Fprintln(streams.Err, "error: proxy:", err)
			return 1
		}
		runtimeProject.HTTPClient.Proxy = value
	}
	if def.Datasource != nil {
		store, err := datasourceStore(def.Datasource, values)
		if err != nil {
			fmt.Fprintln(streams.Err, "error: datasource:", err)
			return 1
		}
		runtimeProject.Datasource = store
	}
	httpExecutor := httpstep.New(nil)
	eng := engine.Engine{HTTP: httpExecutor, Triggers: triggerexecutor.New(httpExecutor), TraceFactory: tracecontext.NewFactory()}
	runResult := eng.Run(ctx, runtimeProject)
	redactor := result.NewRedactor(sensitiveStrings(values)...)
	store, err := artifact.NewStore(*artifactsDir, redactor)
	if err != nil {
		fmt.Fprintln(streams.Err, "error: artifacts:", err)
		return 1
	}
	refs, err := store.Finalize(context.WithoutCancel(ctx), runResult, *keep)
	if err != nil {
		fmt.Fprintln(streams.Err, "error: artifacts:", err)
		return 1
	}
	runResult.Artifacts = refs
	format := def.Output
	if *output != "" {
		format = *output
	}
	renderer, err := render.New(render.Format(format), redactor)
	if err != nil {
		fmt.Fprintln(streams.Err, "error:", err)
		return 1
	}
	encoded, err := renderer.Render(runResult)
	if err != nil {
		fmt.Fprintln(streams.Err, "error: render:", err)
		return 1
	}
	_, _ = streams.Out.Write(encoded)
	for _, ref := range refs {
		fmt.Fprintln(streams.Err, "artifacts:", ref.Path)
	}
	return result.ExitCode(runResult)
}

func normalizeRunArgs(args []string) []string {
	flags := make([]string, 0, len(args))
	positionals := make([]string, 0, 1)
	valueFlags := map[string]bool{"--tag": true, "--output": true, "--artifacts-dir": true, "--var": true, "--config": true, "-c": true, "--working-dir": true, "-w": true}
	for index := 0; index < len(args); index++ {
		argument := args[index]
		if strings.HasPrefix(argument, "-") {
			flags = append(flags, argument)
			name := argument
			if before, _, found := strings.Cut(argument, "="); found {
				name = before
			}
			if valueFlags[name] && !strings.Contains(argument, "=") && index+1 < len(args) {
				index++
				flags = append(flags, args[index])
			}
			continue
		}
		positionals = append(positionals, argument)
	}
	return append(flags, positionals...)
}

func datasourceStore(def *model.DatasourceDefinition, values map[string]model.SensitiveValue) (model.DataStore, error) {
	endpoint, err := evalString(def.Endpoint, values)
	if err != nil {
		return nil, err
	}
	headers := map[string]string{}
	for name, e := range def.Headers {
		headers[name], err = evalString(e, values)
		if err != nil {
			return nil, fmt.Errorf("header %s: %w", name, err)
		}
	}
	token := ""
	if def.BearerToken != nil {
		token, err = evalString(def.BearerToken, values)
		if err != nil {
			return nil, err
		}
	}
	return datasource.New(datasource.Config{Kind: def.Kind, Endpoint: endpoint, Headers: headers, BearerToken: token, TLSSkipVerify: def.TLSSkipVerify})
}

func evalString(expression hcl.Expression, values map[string]model.SensitiveValue) (string, error) {
	v, diags := expr.Evaluate(expression, &hcl.EvalContext{Variables: map[string]cty.Value{"var": expr.Variables(values)}, Functions: expr.Functions()})
	if diags.HasErrors() || !v.IsKnown() || v.IsNull() || v.Type() != cty.String {
		return "", fmt.Errorf("must evaluate to string: %s", diags.Error())
	}
	return v.AsString(), nil
}

func collectExpressions(def *model.ProjectDefinition, tests []model.TestDefinition) []hcl.Expression {
	var out []hcl.Expression
	if def.HTTPProxy != nil {
		out = append(out, def.HTTPProxy)
	}
	if d := def.Datasource; d != nil {
		out = append(out, d.Endpoint, d.BearerToken)
		for _, e := range d.Headers {
			out = append(out, e)
		}
	}
	for _, t := range tests {
		for _, e := range t.Outputs {
			out = append(out, e)
		}
		for _, s := range t.Steps {
			out = append(out, s.HTTP.Method, s.HTTP.URL, s.HTTP.Body)
			for _, e := range s.HTTP.Headers {
				out = append(out, e)
			}
			for _, e := range s.Trigger.Attributes {
				out = append(out, e)
			}
			for _, e := range s.Outputs {
				out = append(out, e)
			}
			for _, c := range s.Checks {
				for _, e := range c.Response {
					out = append(out, e)
				}
				if c.Spans != nil {
					out = append(out, c.Spans.Matching)
					for _, e := range c.Spans.Assertions {
						out = append(out, e)
					}
				}
			}
		}
	}
	return out
}

func sensitiveStrings(values map[string]model.SensitiveValue) []string {
	var out []string
	for _, v := range values {
		if v.Sensitive && v.Value.IsKnown() && !v.Value.IsNull() && v.Value.Type() == cty.String {
			out = append(out, v.Value.AsString())
		}
	}
	return out
}
func testNeedsDatasource(t model.TestDefinition) bool {
	for _, s := range t.Steps {
		for _, c := range s.Checks {
			if c.Spans != nil {
				return true
			}
		}
	}
	return false
}
func printDiagnostics(w io.Writer, diags []model.Diagnostic) bool {
	has := false
	for _, d := range diags {
		fmt.Fprintf(w, "%s[%s]: %s", d.Severity, d.Code, d.Message)
		if d.File != "" {
			fmt.Fprintf(w, " (%s:%d:%d)", d.File, d.Range.StartLine, d.Range.StartColumn)
		}
		fmt.Fprintln(w)
		if d.Severity == "error" {
			has = true
		}
	}
	return has
}
func usage(w io.Writer) {
	fmt.Fprint(w, `Lamplight runs trace-based integration tests.

Usage:
  lamplight <command> [options]

Commands:
  init                 Create a new Lamplight project
  validate             Validate the project without running tests
  list tests           List discovered tests
  run [TEST_NAME]      Run all tests or one named test
  mcp                  Start the MCP server over stdio
  migrate tracetest    Convert Tracetest YAML tests to Lamplight
  help [COMMAND]       Show help for a command

Run "lamplight help <command>" for details about a command.
`)
}

func handleHelp(args []string, streams IO) (bool, int) {
	if len(args) == 0 {
		return false, 0
	}
	if args[0] == "help" {
		if len(args) == 1 {
			usage(streams.Out)
			return true, 0
		}
		if printCommandHelp(streams.Out, args[1:]) {
			return true, 0
		}
		fmt.Fprintf(streams.Err, "error: unknown help topic %q\n\n", strings.Join(args[1:], " "))
		usage(streams.Err)
		return true, 1
	}
	if args[0] == "--help" || args[0] == "-h" {
		usage(streams.Out)
		return true, 0
	}
	for _, arg := range args[1:] {
		if arg == "--help" || arg == "-h" {
			path := args[:1]
			if (args[0] == "list" || args[0] == "migrate") && len(args) > 1 {
				path = args[:2]
			}
			if printCommandHelp(streams.Out, path) {
				return true, 0
			}
			return false, 0
		}
	}
	return false, 0
}

func printCommandHelp(w io.Writer, path []string) bool {
	topic := strings.Join(path, " ")
	help := map[string]string{
		"init": `Create a new Lamplight project.

Usage:
  lamplight init [options]

Options:
  -w, --working-dir DIR  Directory to initialize (default: current directory)
  -h, --help             Show this help
`,
		"validate": `Validate the project without executing tests or contacting a datasource.

Usage:
  lamplight validate [options]

Options:
  -c, --config FILE      Project config path
  -w, --working-dir DIR  Working directory
  -h, --help             Show this help
`,
		"list": `List project resources.

Usage:
  lamplight list tests [options]

Commands:
  tests  List discovered tests
`,
		"list tests": `List discovered tests, their tags, source files, and datasource requirements.

Usage:
  lamplight list tests [options]

Options:
  -c, --config FILE      Project config path
  -w, --working-dir DIR  Working directory
  -h, --help             Show this help
`,
		"run": `Run all tests, one named test, or tests matching a tag.

Usage:
  lamplight run [options] [TEST_NAME]
  lamplight run [TEST_NAME] [options]

Options:
  -c, --config FILE       Project config path
  -w, --working-dir DIR   Working directory
      --tag TAG           Run tests containing TAG
      --var NAME=VALUE    Override a variable (repeatable)
      --output FORMAT     Output format: pretty, text, or json
      --keep-artifacts    Keep artifacts for successful runs
      --artifacts-dir DIR Artifact parent directory
  -h, --help              Show this help
`,
		"mcp": `Start the Lamplight MCP server over stdio.

Usage:
  lamplight mcp [options]

Options:
  -c, --config FILE      Project config path
  -w, --working-dir DIR  Working directory
  -h, --help             Show this help
`,
		"migrate": `Convert tests from another test format.

Usage:
  lamplight migrate tracetest [options] INPUT

Commands:
  tracetest  Convert a Tracetest YAML file or directory
`,
		"migrate tracetest": `Convert Tracetest YAML tests into a Lamplight project.

Usage:
  lamplight migrate tracetest [options] INPUT

Arguments:
  INPUT  Tracetest YAML file or directory containing .yaml/.yml files

Options:
      --output-dir DIR  Output project directory (default: current directory)
      --force           Overwrite generated .wick files
  -h, --help            Show this help
`,
	}
	text, ok := help[topic]
	if ok {
		fmt.Fprint(w, text)
	}
	return ok
}
