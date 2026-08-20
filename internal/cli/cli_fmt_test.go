package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFmtCommandFormatsWickFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "checkout.wick")
	source := `test "checkout" {
step "request" {
http_request {
method="GET"
url="http://example.test"
}
}
}
`
	if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := Main(context.Background(), []string{"fmt", "-w", dir}, IO{Out: &stdout, Err: &stderr})
	if code != 0 || stderr.Len() != 0 {
		t.Fatalf("fmt code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if stdout.String() != "Formatted 1 files\n" {
		t.Fatalf("unexpected output: %q", stdout.String())
	}
	formatted, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(formatted), "    http_request {\n      method = \"GET\"\n") {
		t.Fatalf("file was not formatted:\n%s", formatted)
	}
}
