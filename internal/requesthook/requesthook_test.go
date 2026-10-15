package requesthook

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunRelativeExecutableInProjectBaseDir(t *testing.T) {
	directory := t.TempDir()
	script := "#!/bin/sh\nread request\nprintf '%s' \"$request\"\n"
	if err := os.WriteFile(filepath.Join(directory, "hook.sh"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	output, err := Run(context.Background(), directory, []string{"./hook.sh"}, nil, []byte(`{"method":"GET"}`))
	if err != nil || string(output) != `{"method":"GET"}` {
		t.Fatalf("output=%q err=%v", output, err)
	}
}

func TestRunFailedCommandDoesNotReturnStdoutAsRequest(t *testing.T) {
	output, err := Run(context.Background(), t.TempDir(), []string{"sh", "-c", "printf invalid; echo failed >&2; exit 7"}, nil, []byte(`{}`))
	if err == nil || output != nil || !strings.Contains(err.Error(), "failed") {
		t.Fatalf("output=%q err=%v", output, err)
	}
}
