package targetruntime

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	err := (Launcher{}).Run(context.Background(), model.TargetDefinition{Runtime: "docker_compose"}, dir, "", nil, strings.NewReader("requests"), IO{Out: &out, Err: &out})
	if err != nil {
		t.Fatal(err)
	}
	commands, _ := os.ReadFile(log)
	for _, want := range []string{"compose ps -q", "inspect --format", "create -i --name lamplight-run --read-only", "--network shop_default", "lamplight:test executor", "start --attach --interactive lamplight-run", "rm -f lamplight-run"} {
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
	if err := (Launcher{}).Run(context.Background(), target, dir, "", nil, strings.NewReader("requests"), IO{Out: &bytes.Buffer{}, Err: &bytes.Buffer{}}); err != nil {
		t.Fatal(err)
	}
	commands, _ := os.ReadFile(log)
	got := string(commands)
	for _, want := range []string{"--context prod", "--namespace shop", "get pod lamplight-run", "run lamplight-run --attach", "--image lamplight:test", "serviceAccountName", "lamplight executor", "delete pod lamplight-run", "--ignore-not-found=true", "--wait=false"} {
		if !strings.Contains(got, want) {
			t.Fatalf("command %q missing %q", got, want)
		}
	}
}

func TestDockerComposeStartsPinnedOBI(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "log")
	script := `#!/bin/sh
echo "$@" >> "$FAKE_LOG"
case "$1 $2" in
  "compose ps") echo app-id ;;
  "inspect --format") echo app_default ;;
  "start --attach") cat ;;
  "logs lamplight-run-obi") echo "Launching p.Tracer" ;;
esac
`
	writeExecutable(t, filepath.Join(dir, "docker"), script)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_LOG", log)
	t.Setenv("LAMPLIGHT_RUNNER_IMAGE", "lamplight:test")
	definition := &model.InstrumentationDefinition{Image: "otel/ebpf-instrument:v0.11.0", OpenPorts: []int{8080, 9090}, ContextPropagation: "all"}
	if err := (Launcher{OBISettle: time.Nanosecond}).Run(context.Background(), model.TargetDefinition{Runtime: "docker_compose"}, dir, "http://127.0.0.1:4318", definition, strings.NewReader(""), IO{Out: &bytes.Buffer{}, Err: &bytes.Buffer{}}); err != nil {
		t.Fatal(err)
	}
	commands, _ := os.ReadFile(log)
	got := string(commands)
	for _, want := range []string{"--pid=host --privileged", "OTEL_EBPF_OPEN_PORT=8080,9090", "OTEL_EBPF_METRICS_FEATURES=application", "OTEL_EBPF_METRICS_INTERVAL=500ms", "OTEL_EXPORTER_OTLP_ENDPOINT=http://lamplight-run:4318", "otel/ebpf-instrument:v0.11.0"} {
		if !strings.Contains(got, want) {
			t.Fatalf("commands %q missing %q", got, want)
		}
	}
}

func TestKubernetesCreatesOBIDaemonSetAndService(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "log")
	manifests := filepath.Join(dir, "manifests")
	script := `#!/bin/sh
echo "$@" >> "$FAKE_LOG"
case "$*" in *"create -f -"*) cat >> "$FAKE_MANIFESTS";; *) cat;; esac
`
	writeExecutable(t, filepath.Join(dir, "kubectl"), script)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_LOG", log)
	t.Setenv("FAKE_MANIFESTS", manifests)
	t.Setenv("LAMPLIGHT_RUNNER_IMAGE", "lamplight:test")
	definition := &model.InstrumentationDefinition{Image: "otel/ebpf-instrument:v0.11.0", OpenPorts: []int{8080}, ContextPropagation: "all"}
	target := model.TargetDefinition{Runtime: "kubernetes", Kubernetes: model.KubernetesTarget{Namespace: "shop"}}
	if err := (Launcher{OBISettle: time.Nanosecond}).Run(context.Background(), target, dir, "http://127.0.0.1:4318", definition, strings.NewReader(""), IO{Out: &bytes.Buffer{}, Err: &bytes.Buffer{}}); err != nil {
		t.Fatal(err)
	}
	body, _ := os.ReadFile(manifests)
	got := string(body)
	for _, want := range []string{`"kind":"Service"`, `"kind":"DaemonSet"`, `"hostPID":true`, `"hostNetwork":true`, `OTEL_EBPF_OPEN_PORT`, `OTEL_EBPF_METRICS_FEATURES`, `application`, `OTEL_EBPF_METRICS_INTERVAL`, `500ms`, `otel/ebpf-instrument:v0.11.0`} {
		if !strings.Contains(got, want) {
			t.Fatalf("manifest %q missing %q", got, want)
		}
	}
}

func TestDockerComposeOBICollisionDoesNotRemoveExistingContainer(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "log")
	script := `#!/bin/sh
echo "$@" >> "$FAKE_LOG"
case "$1 $2" in
  "compose ps") echo app-id ;;
  "inspect --format") echo app_default ;;
  "run -d") echo collision >&2; exit 1 ;;
esac
`
	writeExecutable(t, filepath.Join(dir, "docker"), script)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_LOG", log)
	definition := &model.InstrumentationDefinition{Image: "obi:test", OpenPorts: []int{8080}}
	err := (Launcher{}).Run(context.Background(), model.TargetDefinition{Runtime: "docker_compose"}, dir, "http://127.0.0.1:4318", definition, strings.NewReader(""), IO{Out: &bytes.Buffer{}, Err: &bytes.Buffer{}})
	if err == nil || !strings.Contains(err.Error(), "collision") {
		t.Fatalf("expected collision, got %v", err)
	}
	commands, _ := os.ReadFile(log)
	if strings.Contains(string(commands), "rm -f lamplight-run-obi") {
		t.Fatalf("removed existing OBI container: %s", commands)
	}
}

func TestKubernetesCollisionDoesNotDeleteExistingPod(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "log")
	script := `#!/bin/sh
echo "$@" >> "$FAKE_LOG"
case "$*" in
  *"get pod lamplight-run"*) echo pod/lamplight-run ;;
esac
`
	writeExecutable(t, filepath.Join(dir, "kubectl"), script)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_LOG", log)
	err := (Launcher{}).Run(context.Background(), model.TargetDefinition{Runtime: "kubernetes"}, dir, "", nil, strings.NewReader(""), IO{Out: &bytes.Buffer{}, Err: &bytes.Buffer{}})
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("expected collision, got %v", err)
	}
	commands, _ := os.ReadFile(log)
	if strings.Contains(string(commands), "delete pod") || strings.Contains(string(commands), "run lamplight-run") {
		t.Fatalf("changed existing pod: %s", commands)
	}
}

func writeExecutable(t *testing.T, path, source string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(source), 0o700); err != nil {
		t.Fatal(err)
	}
}
