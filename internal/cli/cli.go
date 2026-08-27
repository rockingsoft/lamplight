// Package cli implements Lamplight's command-line interface.
package cli

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/hashicorp/hcl/v2"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/zclconf/go-cty/cty"
	"lamplight/internal/artifact"
	"lamplight/internal/buildinfo"
	"lamplight/internal/config"
	"lamplight/internal/datasource"
	"lamplight/internal/debuglog"
	"lamplight/internal/diagnostic"
	"lamplight/internal/engine"
	"lamplight/internal/executorproto"
	"lamplight/internal/expr"
	"lamplight/internal/formatcmd"
	"lamplight/internal/hclloader"
	"lamplight/internal/httpstep"
	"lamplight/internal/initcmd"
	"lamplight/internal/instrumentation"
	"lamplight/internal/mcpserver"
	"lamplight/internal/model"
	"lamplight/internal/render"
	"lamplight/internal/result"
	"lamplight/internal/runtimevars"
	"lamplight/internal/selection"
	"lamplight/internal/targetruntime"
	"lamplight/internal/tracecontext"
	"lamplight/internal/tracetestmigrate"
	triggerexecutor "lamplight/internal/trigger"
)

type IO struct {
	In       io.Reader
	Out, Err io.Writer
}

func selectTarget(def *model.ProjectDefinition, requested string) (model.TargetDefinition, bool) {
	name := requested
	if name == "" {
		name = def.DefaultTarget
	}
	if name == "" || name == "local" {
		return model.TargetDefinition{Name: "local", Runtime: "local", Variables: map[string]hcl.Expression{}}, true
	}
	target, ok := def.Targets[name]
	return target, ok
}

func evaluateTargetVariables(target model.TargetDefinition, definitions map[string]model.VariableDefinition) (map[string]cty.Value, []model.Diagnostic) {
	values := make(map[string]cty.Value, len(target.Variables))
	var diags []model.Diagnostic
	for name, expression := range target.Variables {
		if _, exists := definitions[name]; !exists {
			diags = append(diags, model.Diagnostic{Severity: diagnostic.SeverityError, Code: diagnostic.CodeVariable, Message: fmt.Sprintf("target %q sets undefined variable %q", target.Name, name)})
			continue
		}
		value, hclDiags := expr.Evaluate(expression, &hcl.EvalContext{Functions: expr.Functions()})
		if hclDiags.HasErrors() {
			diags = append(diags, diagnostic.FromHCL(hclDiags, diagnostic.CodeVariable)...)
			continue
		}
		values[name] = value
	}
	return values, diags
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return "local"
}

func Main(ctx context.Context, args []string, streams IO) int {
	if streams.Out == nil {
		streams.Out = os.Stdout
	}
	if streams.Err == nil {
		streams.Err = os.Stderr
	}
	var verbose bool
	args, verbose = extractVerbose(args)
	if verbose {
		ctx = debuglog.With(ctx, streams.Err)
	}
	command := ""
	if len(args) > 0 {
		command = args[0]
	}
	debuglog.Debug(ctx, "starting command", "command", command, "argument_count", max(0, len(args)-1))
	if handled, exitCode := handleHelp(args, streams); handled {
		return exitCode
	}
	if len(args) == 0 {
		usage(streams.Err)
		return 1
	}
	switch args[0] {
	case "version", "--version":
		if len(args) != 1 {
			writeLine(streams.Err, "error: version accepts no arguments")
			return 1
		}
		writeLine(streams.Out, "lamplight", buildinfo.Version)
		return 0
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
		debuglog.Debug(ctx, "initializing project", "working_dir", dir)
		if err := initcmd.Run(dir); err != nil {
			writeLine(streams.Err, "error:", err)
			return 1
		}
		return 0
	case "validate":
		return validate(ctx, args[1:], streams)
	case "fmt":
		return formatTests(args[1:], streams)
	case "list":
		if len(args) < 2 || args[1] != "tests" {
			usage(streams.Err)
			return 1
		}
		return listTests(ctx, args[2:], streams)
	case "run":
		return run(ctx, args[1:], streams)
	case "executor":
		if len(args) != 1 {
			writeLine(streams.Err, "error: executor accepts no arguments")
			return 1
		}
		if err := executorproto.Serve(ctx, streams.In, streams.Out); err != nil {
			writeLine(streams.Err, "error: executor:", err)
			return 1
		}
		return 0
	case "mcp":
		return runMCP(ctx, args[1:], streams)
	case "migrate":
		return migrate(ctx, args[1:], streams)
	default:
		usage(streams.Err)
		return 1
	}
}

func formatTests(args []string, streams IO) int {
	fs := flag.NewFlagSet("fmt", flag.ContinueOnError)
	fs.SetOutput(streams.Err)
	working := fs.String("working-dir", "", "working directory")
	fs.StringVar(working, "w", "", "working directory")
	if fs.Parse(args) != nil {
		return 1
	}
	changed, err := formatcmd.Run(*working, fs.Args())
	if err != nil {
		writeLine(streams.Err, "error:", err)
		return 1
	}
	writef(streams.Out, "Formatted %d files\n", changed)
	return 0
}

func extractVerbose(args []string) ([]string, bool) {
	filtered := make([]string, 0, len(args))
	verbose := false
	for _, arg := range args {
		switch arg {
		case "-v", "--verbose", "--verbose=true":
			verbose = true
		case "--verbose=false":
			verbose = false
		default:
			filtered = append(filtered, arg)
		}
	}
	return filtered, verbose
}

func migrate(ctx context.Context, args []string, streams IO) int {
	if len(args) == 0 || args[0] != "tracetest" {
		writeLine(streams.Err, "error: migrate requires the source format: tracetest")
		return 1
	}
	fs := flag.NewFlagSet("migrate tracetest", flag.ContinueOnError)
	fs.SetOutput(streams.Err)
	outputDir := fs.String("output-dir", ".", "Lamplight project output directory")
	force := fs.Bool("force", false, "overwrite generated .wick files")
	fs.BoolVar(force, "f", false, "overwrite generated .wick files")
	normalized := normalizeMigrateArgs(args[1:])
	debuglog.Debug(ctx, "parsing migration arguments", "arguments", strings.Join(normalized, " "))
	if fs.Parse(normalized) != nil {
		return 1
	}
	if len(fs.Args()) != 1 {
		debuglog.Debug(ctx, "invalid migration input count", "inputs", strings.Join(fs.Args(), ","), "count", len(fs.Args()))
		writeLine(streams.Err, "error: migrate tracetest requires one YAML file or directory")
		return 1
	}
	input := fs.Args()[0]
	debuglog.Debug(ctx, "migration configured", "input", input, "output_dir", *outputDir, "force", *force)
	results, err := tracetestmigrate.RunContext(ctx, input, *outputDir, *force)
	if err != nil {
		writeLine(streams.Err, "error:", err)
		return 1
	}
	printMigrationResults(streams, results)
	return 0
}

func printMigrationResults(streams IO, results []tracetestmigrate.FileResult) {
	formatter := render.NewAutoPrettyFormatter(streams.Out)
	errFormatter := render.NewAutoPrettyFormatter(streams.Err)
	importedTests, importedDatasources, ignored := 0, 0, 0
	for _, result := range results {
		if result.ImportedTests == 0 && result.ImportedDatasources == 0 {
			ignored++
			writef(streams.Out, "%s %s %s\n", formatter.Muted("·"), result.Source, formatter.Muted("ignored · no resources found"))
		} else {
			importedTests += result.ImportedTests
			importedDatasources += result.ImportedDatasources
			var resources []string
			if result.ImportedTests > 0 {
				resource := "tests"
				if result.ImportedTests == 1 {
					resource = "test"
				}
				resources = append(resources, fmt.Sprintf("%d %s", result.ImportedTests, resource))
			}
			if result.ImportedDatasources > 0 {
				resources = append(resources, fmt.Sprintf("%d datasource", result.ImportedDatasources))
			}
			writef(streams.Out, "%s %s %s %s\n", formatter.Success("✓"), result.Source, formatter.Success("processed"), formatter.Accent("· "+strings.Join(resources, ", ")))
		}
		for _, message := range result.Warnings {
			writef(streams.Err, "%s %s\n", errFormatter.Warning("! warning"), message)
		}
	}
	writeLine(streams.Out)
	testLabel, datasourceLabel := "tests", "datasources"
	if importedTests == 1 {
		testLabel = "test"
	}
	if importedDatasources == 1 {
		datasourceLabel = "datasource"
	}
	writef(streams.Out, "%s %s\n", formatter.Success(fmt.Sprintf("Imported %d %s · %d %s", importedTests, testLabel, importedDatasources, datasourceLabel)), formatter.Muted(fmt.Sprintf("· Ignored %d files", ignored)))
}

func normalizeMigrateArgs(args []string) []string {
	flags := make([]string, 0, len(args))
	positionals := make([]string, 0, 1)
	for index := 0; index < len(args); index++ {
		argument := args[index]
		if strings.HasPrefix(argument, "-") {
			flags = append(flags, argument)
			name := argument
			if before, _, found := strings.Cut(argument, "="); found {
				name = before
			}
			if name == "--output-dir" && !strings.Contains(argument, "=") && index+1 < len(args) {
				index++
				flags = append(flags, args[index])
			}
			continue
		}
		positionals = append(positionals, argument)
	}
	return append(flags, positionals...)
}

func runMCP(ctx context.Context, args []string, streams IO) int {
	fs, common := commonFlags("mcp", streams.Err)
	if fs.Parse(args) != nil {
		return 1
	}
	if len(fs.Args()) != 0 {
		writeLine(streams.Err, "error: mcp accepts no positional arguments")
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
		ObserveTrace: func(observeCtx context.Context, request mcpserver.ObserveTraceRequest) (mcpserver.TraceEvidence, error) {
			return observeTraceForMCP(observeCtx, config.Options{ConfigPath: common.config, WorkingDir: common.working}, request)
		},
	})
	if err := server.Run(ctx, &mcp.StdioTransport{}); err != nil {
		writeLine(streams.Err, "error: mcp:", err)
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

func loadContext(ctx context.Context, c *common, streams IO) (*model.ProjectDefinition, bool) {
	debuglog.Debug(ctx, "loading project", "config", c.config, "working_dir", c.working)
	def, diags := hclloader.Loader{}.LoadProject(config.Options{ConfigPath: c.config, WorkingDir: c.working})
	hasErrors := printDiagnostics(streams.Err, diags)
	if def != nil {
		debuglog.Debug(ctx, "project loaded", "config", def.ConfigPath, "tests", len(def.Tests), "variables", len(def.Variables), "diagnostics", len(diags))
	}
	return def, !hasErrors && def != nil
}

func validate(ctx context.Context, args []string, streams IO) int {
	fs, c := commonFlags("validate", streams.Err)
	if fs.Parse(args) != nil {
		return 1
	}
	def, ok := loadContext(ctx, c, streams)
	if !ok {
		return 1
	}
	formatter := render.NewAutoPrettyFormatter(streams.Out)
	writef(streams.Out, "%s %s %s\n", formatter.Success("✓"), formatter.Success("Valid:"), formatter.Accent(fmt.Sprintf("%d tests · %d variables", len(def.Tests), len(def.Variables))))
	return 0
}

func listTests(ctx context.Context, args []string, streams IO) int {
	fs, c := commonFlags("list tests", streams.Err)
	if fs.Parse(args) != nil {
		return 1
	}
	def, ok := loadContext(ctx, c, streams)
	if !ok {
		return 1
	}
	names := make([]string, 0, len(def.Tests))
	for n := range def.Tests {
		names = append(names, n)
	}
	sort.Strings(names)
	formatter := render.NewAutoPrettyFormatter(streams.Out)
	for _, name := range names {
		t := def.Tests[name]
		writef(streams.Out, "%s %s %s\n", formatter.Success("✓"), formatter.Accent(t.Name), formatter.Muted(fmt.Sprintf("· tags=%s · file=%s · requires_datasource=%t", strings.Join(t.Tags, ","), t.File, testNeedsDatasource(t))))
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

type stringsFlag []string

func (v *stringsFlag) String() string { return strings.Join(*v, ",") }
func (v *stringsFlag) Set(value string) error {
	if value == "" {
		return fmt.Errorf("selector value cannot be empty")
	}
	*v = append(*v, value)
	return nil
}

func run(ctx context.Context, args []string, streams IO) int {
	fs, c := commonFlags("run", streams.Err)
	vars := varsFlag{}
	var tags, files stringsFlag
	fs.Var(&tags, "tag", "select tag (repeatable)")
	fs.Var(&files, "file", "select definition file relative to project.base_dir (repeatable)")
	exclude := fs.Bool("exclude", false, "exclude tests matching the selector")
	output := fs.String("output", "", "output format")
	keep := fs.Bool("keep-artifacts", false, "keep successful artifacts")
	failFast := fs.Bool("fail-fast", false, "stop after the first failed or errored test")
	artifactsDir := fs.String("artifacts-dir", "", "artifact parent directory")
	targetName := fs.String("target", "", "execution target")
	fs.Var(&vars, "var", "NAME=VALUE")
	if fs.Parse(normalizeRunArgs(args)) != nil {
		return 1
	}
	remaining := fs.Args()
	if len(remaining) > 1 {
		writeLine(streams.Err, "error: run accepts at most one test name")
		return 1
	}
	name := ""
	if len(remaining) == 1 {
		name = remaining[0]
	}
	def, ok := loadContext(ctx, c, streams)
	if !ok {
		return 1
	}
	target, targetOK := selectTarget(def, *targetName)
	if !targetOK {
		writeLine(streams.Err, "error: target:", fmt.Sprintf("target %q is not declared", firstNonEmptyString(*targetName, def.DefaultTarget)))
		return 1
	}
	tests, err := selection.Select(def, selection.Selector{Name: name, Tags: tags, Files: files, Exclude: *exclude})
	if err != nil {
		writeLine(streams.Err, "error:", err)
		return 1
	}
	if target.Runtime != "local" && containsExecutableK6(tests) {
		writeLine(streams.Err, "error: k6 script triggers currently require the local target")
		return 1
	}
	debuglog.Debug(ctx, "selected tests", "count", len(tests), "name", name, "tags", strings.Join(tags, ","), "files", strings.Join(files, ","), "exclude", *exclude)
	expressions := collectExpressions(def, tests)
	targetValues, targetDiags := evaluateTargetVariables(target, def.Variables)
	if printDiagnostics(streams.Err, targetDiags) {
		return 1
	}
	values, diags := runtimevars.Resolve(def.Variables, runtimevars.Input{Vars: vars, Target: targetValues}, expressions...)
	if printDiagnostics(streams.Err, diags) {
		return 1
	}
	debuglog.Debug(ctx, "resolved runtime variables", "count", len(values), "overrides", len(vars))
	runtimeProject := &model.Project{Definition: def, Variables: values, Tests: tests, HTTPClient: def.HTTPClient}
	if def.HTTPProxy != nil {
		value, err := evalString(def.HTTPProxy, values)
		if err != nil {
			writeLine(streams.Err, "error: proxy:", err)
			return 1
		}
		runtimeProject.HTTPClient.Proxy = value
	}
	var datasourceConfig *datasource.Config
	if def.Datasource != nil {
		resolved, err := resolveDatasourceConfig(def.Datasource, values)
		if err != nil {
			writeLine(streams.Err, "error: datasource:", err)
			return 1
		}
		datasourceConfig = &resolved
	}
	redactor := result.NewRedactor(sensitiveStrings(values)...)
	format := render.Format(def.Output)
	if *output != "" {
		format = render.Format(*output)
	}
	var renderer model.Renderer
	if format == render.FormatPretty {
		if isCIEnvironment(os.Getenv) {
			renderer = render.NewPrettyRenderer(false, redactor)
		} else {
			renderer = render.NewAutoPrettyRenderer(streams.Out, redactor)
		}
	} else {
		renderer, err = render.New(format, redactor)
	}
	if err != nil {
		writeLine(streams.Err, "error:", err)
		return 1
	}
	var progressFunc engine.ProgressFunc
	if format == render.FormatPretty {
		if isCIEnvironment(os.Getenv) {
			progressFunc = newCIRunProgress(streams.Err, redactor).Report
		} else {
			progressFunc = newRunProgress(streams.Err, redactor).Report
		}
	}
	var httpExecutor model.HTTPExecutor = httpstep.New(nil)
	var triggers model.TriggerExecutor = triggerexecutor.New(httpExecutor)
	var closeRemote func() error
	var closeInstrumentation func() error
	if target.Runtime == "local" {
		if datasourceConfig != nil {
			store, err := datasource.New(*datasourceConfig)
			if err != nil {
				writeLine(streams.Err, "error: datasource:", err)
				return 1
			}
			runtimeProject.Datasource = store
			if closer, ok := store.(io.Closer); ok {
				defer func() { _ = closer.Close() }()
			}
		}
		if def.Instrumentation != nil {
			closeInstrumentation, err = (instrumentation.OBI{}).StartLocal(ctx, *def.Instrumentation, datasourceConfig.Endpoint)
			if err != nil {
				writeLine(streams.Err, "error: instrumentation:", err)
				return 1
			}
		}
	} else {
		client, closeExecutor, err := startRemoteExecutor(ctx, target, filepath.Dir(def.ConfigPath), datasourceConfig, def.Instrumentation, streams.Err)
		if err != nil {
			writeLine(streams.Err, "error:", err)
			return 1
		}
		closeRemote = closeExecutor
		httpExecutor = client
		triggers = executorproto.TriggerClient{Client: client}
		if datasourceConfig != nil {
			runtimeProject.Datasource = client
		}
	}
	eng := engine.Engine{HTTP: httpExecutor, Triggers: triggers, TraceFactory: tracecontext.NewFactory(), FailFast: *failFast, Progress: progressFunc}
	runResult := eng.Run(ctx, runtimeProject)
	if closeInstrumentation != nil {
		if err := closeInstrumentation(); err != nil {
			writeLine(streams.Err, "error: instrumentation:", err)
			return 1
		}
	}
	if closeRemote != nil {
		if err := closeRemote(); err != nil {
			if ctx.Err() != nil {
				return 130
			}
			writeLine(streams.Err, "error: executor:", err)
			return targetruntime.ExitCode(err)
		}
	}
	debuglog.Debug(ctx, "test run completed", "run_id", runResult.RunID, "status", runResult.Status, "tests", len(runResult.Tests))
	store, err := artifact.NewStore(*artifactsDir, redactor)
	if err != nil {
		writeLine(streams.Err, "error: artifacts:", err)
		return 1
	}
	refs, err := store.Finalize(context.WithoutCancel(ctx), runResult, *keep)
	if err != nil {
		writeLine(streams.Err, "error: artifacts:", err)
		return 1
	}
	runResult.Artifacts = refs
	encoded, err := renderer.Render(runResult)
	if err != nil {
		writeLine(streams.Err, "error: render:", err)
		return 1
	}
	_, _ = streams.Out.Write(encoded)
	return result.ExitCode(runResult)
}

func containsExecutableK6(tests []model.TestDefinition) bool {
	for _, test := range tests {
		for _, step := range test.Steps {
			if step.Trigger.Kind == model.TriggerK6 {
				if _, exists := step.Trigger.Attributes["script"]; exists {
					return true
				}
			}
		}
	}
	return false
}

func normalizeRunArgs(args []string) []string {
	flags := make([]string, 0, len(args))
	positionals := make([]string, 0, 1)
	valueFlags := map[string]bool{"--tag": true, "--file": true, "--output": true, "--artifacts-dir": true, "--var": true, "--target": true, "--config": true, "-c": true, "--working-dir": true, "-w": true}
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

func resolveDatasourceConfig(def *model.DatasourceDefinition, values map[string]model.SensitiveValue) (datasource.Config, error) {
	endpoint, err := evalString(def.Endpoint, values)
	if err != nil {
		return datasource.Config{}, err
	}
	headers := map[string]string{}
	for name, e := range def.Headers {
		headers[name], err = evalString(e, values)
		if err != nil {
			return datasource.Config{}, fmt.Errorf("header %s: %w", name, err)
		}
	}
	token := ""
	if def.BearerToken != nil {
		token, err = evalString(def.BearerToken, values)
		if err != nil {
			return datasource.Config{}, err
		}
	}
	return datasource.Config{Kind: def.Kind, Endpoint: endpoint, Headers: headers, BearerToken: token, TLSSkipVerify: def.TLSSkipVerify}, nil
}

func datasourceStore(def *model.DatasourceDefinition, values map[string]model.SensitiveValue) (model.DataStore, error) {
	config, err := resolveDatasourceConfig(def, values)
	if err != nil {
		return nil, err
	}
	return datasource.New(config)
}

func startRemoteExecutor(ctx context.Context, target model.TargetDefinition, configDir string, datasourceConfig *datasource.Config, instrumentationDefinition *model.InstrumentationDefinition, stderr io.Writer) (*executorproto.Client, func() error, error) {
	requestReader, requestWriter := io.Pipe()
	responseReader, responseWriter := io.Pipe()
	done := make(chan error, 1)
	otlpEndpoint := ""
	remoteDatasource := datasourceConfig
	if datasourceConfig != nil {
		otlpEndpoint = datasourceConfig.Endpoint
	}
	if instrumentationDefinition != nil && datasourceConfig != nil {
		copy := *datasourceConfig
		u, parseErr := url.Parse(copy.Endpoint)
		if parseErr != nil || u.Port() == "" {
			return nil, nil, fmt.Errorf("instrumentation OTLP endpoint must include a port: %q", copy.Endpoint)
		}
		u.Host = net.JoinHostPort("0.0.0.0", u.Port())
		copy.Endpoint = u.String()
		remoteDatasource = &copy
	}
	go func() {
		err := (targetruntime.Launcher{}).Run(ctx, target, configDir, otlpEndpoint, instrumentationDefinition, requestReader, targetruntime.IO{Out: responseWriter, Err: stderr})
		_ = responseWriter.CloseWithError(err)
		_ = requestReader.CloseWithError(err)
		done <- err
	}()
	client := executorproto.NewClient(requestWriter, responseReader, remoteDatasource)
	closeExecutor := func() error {
		return closeRemoteExecutor(ctx, requestWriter, responseReader, done)
	}
	return client, closeExecutor, nil
}

func closeRemoteExecutor(ctx context.Context, requestWriter *io.PipeWriter, responseReader *io.PipeReader, done <-chan error) error {
	_ = requestWriter.Close()
	// The runtime may emit trailing output while it tears down its container
	// or Pod. Drain it so os/exec can finish copying stdout before Wait
	// returns; waiting first deadlocks when no protocol reader remains.
	drained := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, responseReader)
		close(drained)
	}()

	select {
	case err := <-done:
		<-drained
		_ = responseReader.Close()
		return err
	case <-ctx.Done():
		// CommandContext kills the runtime process. Closing the response side
		// also releases any os/exec copy goroutine that outlives the child.
		_ = responseReader.CloseWithError(ctx.Err())
		select {
		case <-done:
		case <-time.After(time.Second):
		}
		return ctx.Err()
	}
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
		writef(w, "%s[%s]: %s", d.Severity, d.Code, d.Message)
		if d.File != "" {
			writef(w, " (%s:%d:%d)", d.File, d.Range.StartLine, d.Range.StartColumn)
		}
		writeLine(w)
		if d.Severity == "error" {
			has = true
		}
	}
	return has
}
func usage(w io.Writer) {
	writeString(w, `Lamplight runs trace-based integration tests.

Usage:
  lamplight <command> [options]

Commands:
  version              Print the Lamplight version
  init                 Create a new Lamplight project
  fmt                  Format Lamplight test files
  validate             Validate the project without running tests
  list tests           List discovered tests
  run [TEST_NAME]      Run all tests or one named test
  mcp                  Start the MCP server over stdio
  migrate tracetest    Convert Tracetest YAML tests to Lamplight
  help [COMMAND]       Show help for a command

Run "lamplight help <command>" for details about a command.

Global options:
  -v, --verbose        Write debug logs to stderr
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
		writef(streams.Err, "error: unknown help topic %q\n\n", strings.Join(args[1:], " "))
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
		"fmt": `Format Lamplight test files using the canonical, non-configurable style.

Usage:
  lamplight fmt [options] [FILE_OR_DIR ...]

Options:
  -w, --working-dir DIR  Working directory (default: current directory)
  -h, --help             Show this help
`,
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
		"run": `Run all tests, one named test, one or more files, or tests matching tags.

Usage:
  lamplight run [options] [TEST_NAME]
  lamplight run [TEST_NAME] [options]

Options:
  -c, --config FILE       Project config path
  -w, --working-dir DIR   Working directory
      --tag TAG           Run tests containing TAG (repeatable; matches any)
      --file FILE         Run tests from FILE relative to project.base_dir (repeatable)
      --exclude           Exclude tests matching the name, files, or tags
      --var NAME=VALUE    Override a variable (repeatable)
      --target NAME       Use a named execution target (default: project default or local)
      --output FORMAT     Output format: pretty, text, or json
      --fail-fast         Stop after the first failed or errored test
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
  -f, --force           Overwrite generated .wick files
  -h, --help            Show this help
`,
	}
	text, ok := help[topic]
	if ok {
		writeString(w, text)
	}
	return ok
}

// CLI output is best-effort: command results determine the exit code, while a
// closed output stream cannot be reported without risking another failed write.
func writeLine(w io.Writer, args ...any)             { _, _ = fmt.Fprintln(w, args...) }
func writef(w io.Writer, format string, args ...any) { _, _ = fmt.Fprintf(w, format, args...) }
func writeString(w io.Writer, value string)          { _, _ = io.WriteString(w, value) }
