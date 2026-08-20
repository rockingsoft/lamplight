// Package datasource constructs tracing backend adapters without leaking
// concrete implementations into the CLI.
package datasource

import (
	"fmt"

	"lamplight/internal/datasource/jaeger"
	"lamplight/internal/datasource/otlp"
	"lamplight/internal/datasource/search"
	"lamplight/internal/datasource/signalfx"
	"lamplight/internal/datasource/tempo"
	"lamplight/internal/model"
)

type Config struct {
	Kind          string
	Endpoint      string
	Headers       map[string]string
	BearerToken   string
	TLSSkipVerify bool
}

func New(config Config) (model.DataStore, error) {
	switch config.Kind {
	case "jaeger":
		return jaeger.New(jaeger.Config{Endpoint: config.Endpoint, Headers: config.Headers, BearerToken: config.BearerToken, TLSSkipVerify: config.TLSSkipVerify})
	case "tempo":
		return tempo.New(tempo.Config{
			Endpoint: config.Endpoint, Headers: config.Headers,
			BearerToken: config.BearerToken, TLSSkipVerify: config.TLSSkipVerify,
		})
	case "otlp", "newrelic", "lightstep", "datadog", "honeycomb", "signoz", "dynatrace", "instana", "dash0":
		return otlp.New(otlp.Config{Endpoint: config.Endpoint})
	case "elasticapm", "opensearch":
		return search.New(search.Config{Kind: config.Kind, Endpoint: config.Endpoint, Headers: config.Headers, BearerToken: config.BearerToken, TLSSkipVerify: config.TLSSkipVerify})
	case "signalfx":
		return signalfx.New(signalfx.Config{Endpoint: config.Endpoint, Headers: config.Headers, BearerToken: config.BearerToken, TLSSkipVerify: config.TLSSkipVerify})
	case "awsxray", "azureappinsights", "sumologic":
		// In Lamplight's local-first architecture these vendors use the same
		// embedded OTLP ingestion path as the other collector-backed services.
		return otlp.New(otlp.Config{Endpoint: config.Endpoint})
	default:
		if model.IsSupportedDatasourceKind(config.Kind) {
			return nil, fmt.Errorf("datasource %q adapter is not available", config.Kind)
		}
		return nil, fmt.Errorf("unsupported datasource %q", config.Kind)
	}
}
