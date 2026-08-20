package datasource

import (
	"testing"

	"lamplight/internal/model"
)

func TestAllSupportedBackendsConstruct(t *testing.T) {
	for _, kind := range model.SupportedDatasourceKinds {
		t.Run(kind, func(t *testing.T) {
			store, err := New(Config{Kind: kind, Endpoint: "http://127.0.0.1:4318"})
			if err != nil || store == nil {
				t.Fatalf("store=%T err=%v", store, err)
			}
		})
	}
}

func TestUnknownBackend(t *testing.T) {
	if _, err := New(Config{Kind: "unknown", Endpoint: "http://127.0.0.1:4318"}); err == nil {
		t.Fatal("unknown backend accepted")
	}
}
