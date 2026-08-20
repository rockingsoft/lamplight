package targetruntime

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"lamplight/internal/model"
)

func TestDockerComposeDiscoversNetworkAndStreamsInput(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "log")
	script := `#!/bin/sh
echo "$@" >> "$FAKE_LOG"
case "$1 $2" in
  "compose ps") echo container-id ;;
  "inspect --format") echo shop_default ;;
  "start --attach") cat; echo runner-output ;;
esac
`
	writeExecutable(t, filepath.Join(dir, "docker"), script)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_LOG", log)
	t.Setenv("LAMPLIGHT_RUNNER_IMAGE", "lamplight:test")
	var out bytes.Buffer
	err := (Launcher{}).Run(context.Background(), model.TargetDefinition{Runtime: "docker_compose"}, dir, strings.NewReader("requests"), IO{Out: &out, Err: &out})
	if err != nil {
		t.Fatal(err)
	}
	commands, _ := os.ReadFile(log)
	for _, want := range []string{"compose ps -q", "inspect --format", "create -i", "--network shop_default", "lamplight:test executor", "start --attach --interactive"} {
		if !strings.Contains(string(commands), want) {
			t.Fatalf("commands %q missing %q", commands, want)
		}
	}
	if !strings.Contains(out.String(), "requests") || !strings.Contains(out.String(), "runner-output") {
		t.Fatalf("output: %q", out.String())
	}
}

func TestKubernetesUsesConfiguredPlacement(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "log")
	script := "#!/bin/sh\necho \"$@\" >> \"$FAKE_LOG\"\ncat\n"
	writeExecutable(t, filepath.Join(dir, "kubectl"), script)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_LOG", log)
	t.Setenv("LAMPLIGHT_RUNNER_IMAGE", "lamplight:test")
	target := model.TargetDefinition{Runtime: "kubernetes", Kubernetes: model.KubernetesTarget{Context: "prod", Namespace: "shop", ServiceAccount: "runner"}}
	if err := (Launcher{}).Run(context.Background(), target, dir, strings.NewReader("requests"), IO{Out: &bytes.Buffer{}, Err: &bytes.Buffer{}}); err != nil {
		t.Fatal(err)
	}
	commands, _ := os.ReadFile(log)
	got := string(commands)
	for _, want := range []string{"--context prod", "--namespace shop", "run lamplight-run-", "--image lamplight:test", "serviceAccountName", "lamplight executor", "delete pod lamplight-run-", "--ignore-not-found=true", "--wait=false"} {
		if !strings.Contains(got, want) {
			t.Fatalf("command %q missing %q", got, want)
		}
	}
}

func writeExecutable(t *testing.T, path, source string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(source), 0o700); err != nil {
		t.Fatal(err)
	}
}
