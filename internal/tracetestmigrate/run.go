package tracetestmigrate

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
	"lamplight/internal/debuglog"
)

type FileResult struct {
	Source, Destination string
	Destinations        []string
	ImportedTests       int
	ImportedDatasources int
	Warnings            []string
}

type datasourceImport struct {
	Kind              string
	Endpoint          string
	ObservationWindow time.Duration
	Headers           map[string]string
	TLSSkipVerify     bool
}

// Run migrates a YAML file or every .yaml/.yml file below a directory into a
// Lamplight project rooted at outputDir.
func Run(input, outputDir string, force bool) ([]FileResult, error) {
	return RunContext(context.Background(), input, outputDir, force)
}

// RunContext is Run with caller-provided debug logging and cancellation context.
func RunContext(ctx context.Context, input, outputDir string, force bool) ([]FileResult, error) {
	debuglog.Debug(ctx, "discovering Tracetest input", "input", input)
	files, err := inputFiles(input)
	if err != nil {
		return nil, err
	}
	debuglog.Debug(ctx, "discovered Tracetest files", "count", len(files))
	type pending struct {
		source, destination string
		fileIndex           int
		result              Result
	}
	pendingFiles := make([]pending, 0, len(files))
	results := make([]FileResult, len(files))
	var importedDatasource *datasourceImport
	seen := map[string]string{}
	for fileIndex, source := range files {
		results[fileIndex].Source = source
		debuglog.Debug(ctx, "converting Tracetest file", "source", source)
		contents, err := os.ReadFile(source)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", source, err)
		}
		inspected, err := inspectResources(contents)
		if err != nil {
			return nil, fmt.Errorf("inspect %s: %w", source, err)
		}
		debuglog.Debug(ctx, "inspected Tracetest file", "source", source, "importable_tests", len(inspected.tests), "importable_datasources", len(inspected.datasources))
		for _, datasource := range inspected.datasources {
			if importedDatasource != nil {
				return nil, fmt.Errorf("multiple Tracetest DataStore resources are not supported")
			}
			copy := datasource
			importedDatasource = &copy
			results[fileIndex].ImportedDatasources++
		}
		for documentIndex, test := range inspected.tests {
			result, err := Convert(test)
			if err != nil {
				return nil, fmt.Errorf("migrate %s document %d: %w", source, documentIndex+1, err)
			}
			name := fileName(result.Name) + ".wick"
			destination := filepath.Join(outputDir, "lamplight", name)
			if prior, exists := seen[destination]; exists {
				return nil, fmt.Errorf("%s and %s both map to %s", prior, source, destination)
			}
			seen[destination] = source
			if !force {
				if _, err := os.Stat(destination); err == nil {
					return nil, fmt.Errorf("refusing to overwrite %s (use --force)", destination)
				} else if !os.IsNotExist(err) {
					return nil, err
				}
			}
			pendingFiles = append(pendingFiles, pending{source: source, destination: destination, fileIndex: fileIndex, result: result})
			debuglog.Debug(ctx, "prepared migrated test", "source", source, "destination", destination, "warnings", len(result.Warnings))
		}
	}
	if len(pendingFiles) == 0 && importedDatasource == nil {
		return results, nil
	}
	if err := os.MkdirAll(filepath.Join(outputDir, "lamplight"), 0o755); err != nil {
		return nil, fmt.Errorf("create output directory: %w", err)
	}
	if err := writeConfig(ctx, outputDir, importedDatasource); err != nil {
		return nil, err
	}
	for _, item := range pendingFiles {
		if err := os.WriteFile(item.destination, item.result.HCL, 0o644); err != nil {
			return results, fmt.Errorf("write %s: %w", item.destination, err)
		}
		fileResult := &results[item.fileIndex]
		if fileResult.Destination == "" {
			fileResult.Destination = item.destination
		}
		fileResult.Destinations = append(fileResult.Destinations, item.destination)
		fileResult.ImportedTests++
		fileResult.Warnings = append(fileResult.Warnings, item.result.Warnings...)
		debuglog.Debug(ctx, "wrote migrated file", "destination", item.destination)
	}
	return results, nil
}

type inspectedResources struct {
	tests       [][]byte
	datasources []datasourceImport
}

func inspectResources(contents []byte) (inspectedResources, error) {
	decoder := yaml.NewDecoder(strings.NewReader(string(contents)))
	var result inspectedResources
	var observationWindow time.Duration
	for {
		var document yaml.Node
		err := decoder.Decode(&document)
		if err != nil {
			if err == io.EOF {
				break
			}
			return result, fmt.Errorf("decode YAML: %w", err)
		}
		switch resourceType(document) {
		case "Test":
			encoded, err := yaml.Marshal(&document)
			if err != nil {
				return result, fmt.Errorf("encode Test resource: %w", err)
			}
			result.tests = append(result.tests, encoded)
		case "PollingProfile":
			var profile struct {
				Spec struct {
					Default  bool `yaml:"default"`
					Periodic struct {
						Timeout string `yaml:"timeout"`
					} `yaml:"periodic"`
				} `yaml:"spec"`
			}
			if err := document.Decode(&profile); err != nil {
				return result, fmt.Errorf("decode PollingProfile: %w", err)
			}
			if profile.Spec.Default && profile.Spec.Periodic.Timeout != "" {
				parsed, err := time.ParseDuration(profile.Spec.Periodic.Timeout)
				if err != nil || parsed <= 0 {
					return result, fmt.Errorf("PollingProfile timeout %q is invalid", profile.Spec.Periodic.Timeout)
				}
				observationWindow = parsed
			}
		case "DataStore":
			datasource, err := decodeDataStore(document)
			if err != nil {
				return result, err
			}
			result.datasources = append(result.datasources, datasource)
		}
	}
	for index := range result.datasources {
		result.datasources[index].ObservationWindow = observationWindow
	}
	return result, nil
}

func decodeDataStore(document yaml.Node) (datasourceImport, error) {
	type tlsConfig struct {
		Insecure           bool `yaml:"insecure"`
		InsecureSkipVerify bool `yaml:"insecureSkipVerify"`
	}
	type grpcConfig struct {
		Endpoint string            `yaml:"endpoint"`
		Headers  map[string]string `yaml:"headers"`
		TLS      tlsConfig         `yaml:"tls"`
	}
	type searchConfig struct {
		Addresses          []string `yaml:"addresses"`
		Username           string   `yaml:"username"`
		Password           string   `yaml:"password"`
		Index              string   `yaml:"index"`
		Certificate        string   `yaml:"certificate"`
		InsecureSkipVerify bool     `yaml:"insecureSkipVerify"`
	}
	var resource struct {
		Spec struct {
			Type   string     `yaml:"type"`
			Jaeger grpcConfig `yaml:"jaeger"`
			Tempo  struct {
				Type string     `yaml:"type"`
				GRPC grpcConfig `yaml:"grpc"`
				HTTP struct {
					URL     string            `yaml:"url"`
					Headers map[string]string `yaml:"headers"`
					TLS     tlsConfig         `yaml:"tls"`
				} `yaml:"http"`
			} `yaml:"tempo"`
			OpenSearch searchConfig `yaml:"opensearch"`
			ElasticAPM searchConfig `yaml:"elasticapm"`
			SignalFX   struct {
				Realm string `yaml:"realm"`
				Token string `yaml:"token"`
			} `yaml:"signalfx"`
		} `yaml:"spec"`
	}
	if err := document.Decode(&resource); err != nil {
		return datasourceImport{}, fmt.Errorf("decode DataStore: %w", err)
	}
	switch resource.Spec.Type {
	case "jaeger":
		endpoint, err := queryEndpoint(resource.Spec.Jaeger.Endpoint, resource.Spec.Jaeger.TLS.Insecure, "16685", "16686")
		if err != nil {
			return datasourceImport{}, fmt.Errorf("jaeger datastore: %w", err)
		}
		return datasourceImport{Kind: "jaeger", Endpoint: endpoint, Headers: resource.Spec.Jaeger.Headers, TLSSkipVerify: resource.Spec.Jaeger.TLS.InsecureSkipVerify}, nil
	case "tempo":
		return tempoDataStore(resource.Spec.Tempo.Type, resource.Spec.Tempo.GRPC.Endpoint, resource.Spec.Tempo.GRPC.Headers, resource.Spec.Tempo.GRPC.TLS, resource.Spec.Tempo.HTTP.URL, resource.Spec.Tempo.HTTP.Headers, resource.Spec.Tempo.HTTP.TLS)
	case "opensearch":
		return searchDataStore("opensearch", resource.Spec.OpenSearch)
	case "elasticapm":
		return searchDataStore("elasticapm", resource.Spec.ElasticAPM)
	case "signalfx":
		if resource.Spec.SignalFX.Realm == "" || resource.Spec.SignalFX.Token == "" {
			return datasourceImport{}, fmt.Errorf("signalfx datastore requires realm and token")
		}
		return datasourceImport{Kind: "signalfx", Endpoint: "https://api." + resource.Spec.SignalFX.Realm + ".signalfx.com", Headers: map[string]string{"X-SF-TOKEN": resource.Spec.SignalFX.Token}}, nil
	case "otlp", "newrelic", "lightstep", "datadog", "honeycomb", "signoz", "dynatrace", "instana", "dash0":
		return datasourceImport{Kind: resource.Spec.Type, Endpoint: "http://127.0.0.1:4318"}, nil
	case "awsxray", "azureappinsights", "sumologic":
		return datasourceImport{}, fmt.Errorf("datastore type %q uses a different integration mode in Lamplight and cannot be migrated without an OTLP listener choice", resource.Spec.Type)
	default:
		return datasourceImport{}, fmt.Errorf("datastore type %q is not supported by both Tracetest and Lamplight", resource.Spec.Type)
	}
}

func queryEndpoint(raw string, insecure bool, sourcePort, destinationPort string) (string, error) {
	scheme := "https"
	if insecure {
		scheme = "http"
	}
	if !strings.Contains(raw, "://") {
		raw = scheme + "://" + raw
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Hostname() == "" {
		return "", fmt.Errorf("endpoint %q is invalid", raw)
	}
	port := parsed.Port()
	if port == sourcePort {
		port = destinationPort
	}
	if port != "" {
		parsed.Host = net.JoinHostPort(parsed.Hostname(), port)
	}
	return parsed.String(), nil
}

func tempoDataStore(kind, grpcEndpoint string, grpcHeaders map[string]string, grpcTLS struct {
	Insecure           bool `yaml:"insecure"`
	InsecureSkipVerify bool `yaml:"insecureSkipVerify"`
}, httpURL string, httpHeaders map[string]string, httpTLS struct {
	Insecure           bool `yaml:"insecure"`
	InsecureSkipVerify bool `yaml:"insecureSkipVerify"`
}) (datasourceImport, error) {
	if kind == "http" || httpURL != "" {
		endpoint, err := queryEndpoint(httpURL, httpTLS.Insecure, "", "")
		if err != nil {
			return datasourceImport{}, fmt.Errorf("tempo HTTP datastore: %w", err)
		}
		return datasourceImport{Kind: "tempo", Endpoint: endpoint, Headers: httpHeaders, TLSSkipVerify: httpTLS.InsecureSkipVerify}, nil
	}
	endpoint, err := queryEndpoint(grpcEndpoint, grpcTLS.Insecure, "9095", "3200")
	if err != nil {
		return datasourceImport{}, fmt.Errorf("tempo gRPC datastore: %w", err)
	}
	return datasourceImport{Kind: "tempo", Endpoint: endpoint, Headers: grpcHeaders, TLSSkipVerify: grpcTLS.InsecureSkipVerify}, nil
}

func searchDataStore(kind string, config struct {
	Addresses          []string `yaml:"addresses"`
	Username           string   `yaml:"username"`
	Password           string   `yaml:"password"`
	Index              string   `yaml:"index"`
	Certificate        string   `yaml:"certificate"`
	InsecureSkipVerify bool     `yaml:"insecureSkipVerify"`
}) (datasourceImport, error) {
	if len(config.Addresses) != 1 || config.Index == "" {
		return datasourceImport{}, fmt.Errorf("%s datastore requires exactly one address and an index", kind)
	}
	if config.Certificate != "" {
		return datasourceImport{}, fmt.Errorf("%s datastore certificate files are not supported by Lamplight", kind)
	}
	parsed, err := url.Parse(config.Addresses[0])
	if err != nil || !parsed.IsAbs() || parsed.Hostname() == "" {
		return datasourceImport{}, fmt.Errorf("%s datastore address %q is invalid", kind, config.Addresses[0])
	}
	parsed.Path = path.Join(parsed.Path, config.Index)
	headers := map[string]string{}
	if config.Username != "" || config.Password != "" {
		headers["Authorization"] = "Basic " + base64.StdEncoding.EncodeToString([]byte(config.Username+":"+config.Password))
	}
	return datasourceImport{Kind: kind, Endpoint: parsed.String(), Headers: headers, TLSSkipVerify: config.InsecureSkipVerify}, nil
}

func writeConfig(ctx context.Context, outputDir string, datasource *datasourceImport) error {
	configPath := filepath.Join(outputDir, ".lamplight")
	contents := configContents(datasource)
	existing, err := os.ReadFile(configPath)
	if err == nil {
		baseline := configContents(nil)
		if string(existing) == contents || (datasource != nil && string(existing) == baseline) {
			if string(existing) == contents {
				return nil
			}
		} else {
			return fmt.Errorf("refusing to overwrite customized %s; merge the imported datasource manually", configPath)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("read %s: %w", configPath, err)
	}
	if err := os.WriteFile(configPath, []byte(contents), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", configPath, err)
	}
	debuglog.Debug(ctx, "wrote Lamplight config", "path", configPath, "datasource", datasource != nil)
	return nil
}

func configContents(datasource *datasourceImport) string {
	var out strings.Builder
	out.WriteString("project {\n  base_dir = \"./lamplight\"\n}\n")
	if datasource != nil {
		fmt.Fprintf(&out, "\ndatasource %q {\n  endpoint = %q\n", datasource.Kind, datasource.Endpoint)
		if datasource.ObservationWindow > 0 {
			fmt.Fprintf(&out, "  observation_window = duration(%q)\n", datasource.ObservationWindow.String())
		}
		if len(datasource.Headers) > 0 {
			out.WriteString("  headers = {\n")
			for _, name := range sortedKeys(datasource.Headers) {
				fmt.Fprintf(&out, "    %q = %q\n", name, datasource.Headers[name])
			}
			out.WriteString("  }\n")
		}
		if datasource.TLSSkipVerify {
			out.WriteString("  tls {\n    skip_verify = true\n  }\n")
		}
		out.WriteString("}\n")
	}
	return out.String()
}

func resourceType(document yaml.Node) string {
	if document.Kind != yaml.DocumentNode || len(document.Content) == 0 {
		return ""
	}
	root := document.Content[0]
	if root.Kind != yaml.MappingNode {
		return ""
	}
	for index := 0; index+1 < len(root.Content); index += 2 {
		if root.Content[index].Value == "type" {
			return root.Content[index+1].Value
		}
	}
	return ""
}

func inputFiles(input string) ([]string, error) {
	info, err := os.Stat(input)
	if err != nil {
		return nil, fmt.Errorf("stat input: %w", err)
	}
	if !info.IsDir() {
		if !isYAML(input) {
			return nil, fmt.Errorf("input file must end in .yaml or .yml")
		}
		return []string{input}, nil
	}
	var files []string
	err = filepath.WalkDir(input, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type().IsRegular() && isYAML(path) {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("scan input: %w", err)
	}
	sort.Strings(files)
	if len(files) == 0 {
		return nil, fmt.Errorf("input directory contains no .yaml or .yml files")
	}
	return files, nil
}

func isYAML(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	return ext == ".yaml" || ext == ".yml"
}

var unsafeFileCharacters = regexp.MustCompile(`[^a-z0-9_-]+`)

func fileName(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	name = unsafeFileCharacters.ReplaceAllString(name, "-")
	name = strings.Trim(name, "-")
	if name == "" {
		return "migrated-test"
	}
	return name
}
