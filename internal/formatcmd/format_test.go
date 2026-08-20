package formatcmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSourceFormatsAndWrapsLongLogicalAndChains(t *testing.T) {
	input := []byte(`test "checkout" {
step "request" {
check "created" {
spans {
matching = span.name == "checkout.request.received" && span.attributes["http.request.method"] == "POST" && resource.attributes["service.name"] == "checkout-api"
at_least=1
}
}
}
}
`)
	want := `        matching = (
          span.name == "checkout.request.received" &&
          span.attributes["http.request.method"] == "POST" &&
          resource.attributes["service.name"] == "checkout-api"
        )`
	got, err := Source(input, "checkout.wick")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), want) {
		t.Fatalf("formatted source does not contain wrapped expression:\n%s", got)
	}
	again, err := Source(got, "checkout.wick")
	if err != nil {
		t.Fatal(err)
	}
	if string(again) != string(got) {
		t.Fatalf("formatter is not idempotent:\nfirst:\n%s\nsecond:\n%s", got, again)
	}
}

func TestSourceWrapsLongNestedFunctionCall(t *testing.T) {
	input := []byte(`test "checkout" {
  step "request" {
    check "message" {
      spans {
        matching = true
        span_assertions = {
          "assertion 1" = tostring(jsondecode(span.attributes["messaging.payload"]).id) == "143"
        }
      }
    }
  }
}
`)
	want := `          "assertion 1" = tostring(
            jsondecode(span.attributes["messaging.payload"]).id
          ) == "143"`
	got, err := Source(input, "checkout.wick")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), want) {
		t.Fatalf("formatted source does not contain wrapped call:\n%s", got)
	}
	again, err := Source(got, "checkout.wick")
	if err != nil {
		t.Fatal(err)
	}
	if string(again) != string(got) {
		t.Fatalf("formatter is not idempotent:\nfirst:\n%s\nsecond:\n%s", got, again)
	}
}

func TestRunDoesNotWriteAnyFileWhenOneHasInvalidSyntax(t *testing.T) {
	dir := t.TempDir()
	valid := filepath.Join(dir, "valid.wick")
	invalid := filepath.Join(dir, "invalid.wick")
	original := []byte("test \"valid\" {\nstep \"request\" {}\n}\n")
	if err := os.WriteFile(valid, original, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(invalid, []byte("test {"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(dir, nil); err == nil {
		t.Fatal("expected invalid syntax error")
	}
	got, err := os.ReadFile(valid)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(original) {
		t.Fatalf("valid file changed before validation completed: %q", got)
	}
}
