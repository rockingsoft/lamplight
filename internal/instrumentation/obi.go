package instrumentation

import (
	"context"
	"fmt"
	"net/url"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"lamplight/internal/model"
)

type OBI struct {
	Command func(context.Context, string, ...string) *exec.Cmd
}

func (o OBI) StartLocal(ctx context.Context, definition model.InstrumentationDefinition, endpoint string) (func() error, error) {
	if runtime.GOOS != "linux" {
		return nil, fmt.Errorf("local OBI instrumentation requires Linux; use a docker_compose or kubernetes target on %s", runtime.GOOS)
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Port() == "" {
		return nil, fmt.Errorf("embedded OTLP endpoint must include a port: %q", endpoint)
	}
	command := o.Command
	if command == nil {
		command = exec.CommandContext
	}
	name := fmt.Sprintf("lamplight-obi-%d", time.Now().UnixNano())
	args := []string{"run", "-d", "--name", name, "--label", "io.lamplight.managed=true", "--network=host", "--pid=host", "--privileged", "-v", "/sys/fs/cgroup:/sys/fs/cgroup:ro", "-v", "/sys/kernel/security:/sys/kernel/security:ro", "-e", "OTEL_EBPF_OPEN_PORT=" + joinPorts(definition.OpenPorts), "-e", "OTEL_EXPORTER_OTLP_ENDPOINT=" + endpoint, "-e", "OTEL_EBPF_BPF_CONTEXT_PROPAGATION=" + definition.ContextPropagation, "-e", "OTEL_EBPF_METRICS_FEATURES=application", "-e", "OTEL_EBPF_METRICS_INTERVAL=500ms", definition.Image}
	if out, err := command(ctx, "docker", args...).CombinedOutput(); err != nil {
		return nil, fmt.Errorf("start local OBI instrumentation: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return func() error { return command(context.WithoutCancel(ctx), "docker", "rm", "-f", name).Run() }, nil
}

func joinPorts(values []int) string {
	parts := make([]string, len(values))
	for i, value := range values {
		parts[i] = fmt.Sprint(value)
	}
	return strings.Join(parts, ",")
}
