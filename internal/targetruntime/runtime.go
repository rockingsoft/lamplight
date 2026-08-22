package targetruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"lamplight/internal/buildinfo"
	"lamplight/internal/model"
)

type IO struct {
	In       io.Reader
	Out, Err io.Writer
}

type Launcher struct {
	Command   func(context.Context, string, ...string) *exec.Cmd
	OBISettle time.Duration
}

func (r Launcher) Run(ctx context.Context, target model.TargetDefinition, configDir string, otlpEndpoint string, instrumentation *model.InstrumentationDefinition, input io.Reader, streams IO) error {
	command := r.Command
	if command == nil {
		command = exec.CommandContext
	}
	switch target.Runtime {
	case "docker_compose":
		return r.dockerCompose(ctx, command, target, configDir, otlpEndpoint, instrumentation, input, streams)
	case "kubernetes":
		return r.kubernetes(ctx, command, target, otlpEndpoint, instrumentation, input, streams)
	default:
		return fmt.Errorf("unsupported remote runtime %q", target.Runtime)
	}
}

func (r Launcher) dockerCompose(ctx context.Context, command func(context.Context, string, ...string) *exec.Cmd, target model.TargetDefinition, configDir string, otlpEndpoint string, instrumentation *model.InstrumentationDefinition, input io.Reader, streams IO) error {
	composeArgs := []string{"compose"}
	if target.Compose.Project != "" {
		composeArgs = append(composeArgs, "--project-name", target.Compose.Project)
	}
	composeArgs = append(composeArgs, "ps", "-q")
	composeArgs = append(composeArgs, target.Compose.Services...)
	ps := command(ctx, "docker", composeArgs...)
	ps.Dir = configDir
	output, err := ps.Output()
	if err != nil {
		return commandError("discover Compose containers", err)
	}
	ids := strings.Fields(string(output))
	if len(ids) == 0 {
		return fmt.Errorf("no running Docker Compose containers found in %s", configDir)
	}
	inspectArgs := append([]string{"inspect", "--format", "{{range $name, $_ := .NetworkSettings.Networks}}{{println $name}}{{end}}"}, ids...)
	networkOutput, err := command(ctx, "docker", inspectArgs...).Output()
	if err != nil {
		return commandError("inspect Compose networks", err)
	}
	networkSet := map[string]struct{}{}
	for _, network := range strings.Fields(string(networkOutput)) {
		networkSet[network] = struct{}{}
	}
	if len(networkSet) == 0 {
		return fmt.Errorf("compose services have no Docker networks")
	}
	networks := make([]string, 0, len(networkSet))
	for network := range networkSet {
		networks = append(networks, network)
	}
	sort.Strings(networks)
	name := fmt.Sprintf("lamplight-run-%d", time.Now().UnixNano())
	createArgs := []string{"create", "-i", "--name", name, "--read-only", "--tmpfs", "/tmp", "--label", "io.lamplight.managed=true", "--network", networks[0], buildinfo.ExecutorImage(), "executor"}
	if out, err := command(ctx, "docker", createArgs...).CombinedOutput(); err != nil {
		return fmt.Errorf("create Lamplight executor: %w: %s", err, strings.TrimSpace(string(out)))
	}
	defer func() { _ = command(context.WithoutCancel(ctx), "docker", "rm", "-f", name).Run() }()
	for _, network := range networks[1:] {
		if out, err := command(ctx, "docker", "network", "connect", network, name).CombinedOutput(); err != nil {
			return fmt.Errorf("connect executor to network %s: %w: %s", network, err, strings.TrimSpace(string(out)))
		}
	}
	if instrumentation != nil {
		port, err := endpointPort(otlpEndpoint)
		if err != nil {
			return err
		}
		obiName := name + "-obi"
		defer func() { _ = command(context.WithoutCancel(ctx), "docker", "rm", "-f", obiName).Run() }()
		obiArgs := []string{"run", "-d", "--name", obiName, "--label", "io.lamplight.managed=true", "--pid=host", "--privileged", "--network", networks[0], "-v", "/sys/fs/cgroup:/sys/fs/cgroup:ro", "-v", "/sys/kernel/security:/sys/kernel/security:ro", "-e", "OTEL_EBPF_OPEN_PORT=" + ports(instrumentation.OpenPorts), "-e", "OTEL_EXPORTER_OTLP_ENDPOINT=http://" + name + ":" + port, "-e", "OTEL_EBPF_BPF_CONTEXT_PROPAGATION=" + instrumentation.ContextPropagation, instrumentation.Image}
		if out, err := command(ctx, "docker", obiArgs...).CombinedOutput(); err != nil {
			return fmt.Errorf("start OBI instrumentation: %w: %s", err, strings.TrimSpace(string(out)))
		}
		if err := waitDockerOBI(ctx, command, obiName, r.obiSettle()); err != nil {
			return err
		}
	}
	start := command(ctx, "docker", "start", "--attach", "--interactive", name)
	start.Stdin, start.Stdout, start.Stderr = input, streams.Out, streams.Err
	if err := start.Run(); err != nil {
		return commandError("run Lamplight container", err)
	}
	return nil
}

func (r Launcher) kubernetes(ctx context.Context, command func(context.Context, string, ...string) *exec.Cmd, target model.TargetDefinition, otlpEndpoint string, instrumentation *model.InstrumentationDefinition, input io.Reader, streams IO) error {
	name := fmt.Sprintf("lamplight-run-%d", time.Now().Unix())
	placement := []string{}
	if target.Kubernetes.Context != "" {
		placement = append(placement, "--context", target.Kubernetes.Context)
	}
	if target.Kubernetes.Namespace != "" {
		placement = append(placement, "--namespace", target.Kubernetes.Namespace)
	}
	deleteArgs := append(append([]string{}, placement...), "delete", "pod", name, "--ignore-not-found=true", "--wait=false")
	defer func() { _ = command(context.WithoutCancel(ctx), "kubectl", deleteArgs...).Run() }()
	if instrumentation != nil {
		port, err := endpointPort(otlpEndpoint)
		if err != nil {
			return err
		}
		resources := kubernetesOBIResources(name, target.Kubernetes.Namespace, port, instrumentation)
		applyArgs := append(append([]string{}, placement...), "apply", "-f", "-")
		apply := command(ctx, "kubectl", applyArgs...)
		apply.Stdin = strings.NewReader(resources)
		if out, err := apply.CombinedOutput(); err != nil {
			return fmt.Errorf("start OBI instrumentation: %w: %s", err, strings.TrimSpace(string(out)))
		}
		cleanupArgs := append(append([]string{}, placement...), "delete", "daemonset,service", name+"-obi", "--ignore-not-found=true", "--wait=false")
		defer func() { _ = command(context.WithoutCancel(ctx), "kubectl", cleanupArgs...).Run() }()
		rolloutArgs := append(append([]string{}, placement...), "rollout", "status", "daemonset/"+name+"-obi", "--timeout=60s")
		if out, err := command(ctx, "kubectl", rolloutArgs...).CombinedOutput(); err != nil {
			return fmt.Errorf("wait for OBI instrumentation: %w: %s", err, strings.TrimSpace(string(out)))
		}
		if err := waitContext(ctx, r.obiSettle()); err != nil {
			return err
		}
	}

	args := append([]string{}, placement...)
	args = append(args, "run", name, "--attach", "--stdin", "--rm", "--restart=Never", "--image", buildinfo.ExecutorImage())
	if target.Kubernetes.ServiceAccount != "" || instrumentation != nil {
		spec := map[string]any{}
		if target.Kubernetes.ServiceAccount != "" {
			spec["serviceAccountName"] = target.Kubernetes.ServiceAccount
		}
		override, _ := json.Marshal(map[string]any{"metadata": map[string]any{"labels": map[string]string{"io.lamplight.run": name}}, "spec": spec})
		args = append(args, "--overrides", string(override))
	}
	args = append(args, "--command", "--", "lamplight", "executor")
	cmd := command(ctx, "kubectl", args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = input, streams.Out, streams.Err
	if err := cmd.Run(); err != nil {
		return commandError("run Lamplight pod", err)
	}
	return nil
}

func endpointPort(endpoint string) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Port() == "" {
		return "", fmt.Errorf("embedded OTLP endpoint must include a port: %q", endpoint)
	}
	return u.Port(), nil
}

func ports(values []int) string {
	parts := make([]string, len(values))
	for i, value := range values {
		parts[i] = strconv.Itoa(value)
	}
	return strings.Join(parts, ",")
}

func (r Launcher) obiSettle() time.Duration {
	if r.OBISettle > 0 {
		return r.OBISettle
	}
	return 10 * time.Second
}

func waitDockerOBI(ctx context.Context, command func(context.Context, string, ...string) *exec.Cmd, name string, settle time.Duration) error {
	deadline := time.NewTimer(20 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		output, err := command(ctx, "docker", "logs", name).CombinedOutput()
		if err == nil && strings.Contains(string(output), "Launching p.Tracer") {
			return waitContext(ctx, settle)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("OBI instrumentation did not attach before the 20s startup deadline: %s", strings.TrimSpace(string(output)))
		case <-ticker.C:
		}
	}
}

func waitContext(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func kubernetesOBIResources(name, namespace, port string, instrumentation *model.InstrumentationDefinition) string {
	metadata := map[string]any{"name": name + "-obi"}
	if namespace != "" {
		metadata["namespace"] = namespace
	}
	service := map[string]any{"apiVersion": "v1", "kind": "Service", "metadata": metadata, "spec": map[string]any{"selector": map[string]string{"io.lamplight.run": name}, "ports": []map[string]any{{"port": mustAtoi(port), "targetPort": mustAtoi(port)}}}}
	daemon := map[string]any{"apiVersion": "apps/v1", "kind": "DaemonSet", "metadata": metadata, "spec": map[string]any{"selector": map[string]any{"matchLabels": map[string]string{"io.lamplight.obi": name}}, "template": map[string]any{"metadata": map[string]any{"labels": map[string]string{"io.lamplight.obi": name}}, "spec": map[string]any{"hostPID": true, "hostNetwork": true, "dnsPolicy": "ClusterFirstWithHostNet", "tolerations": []map[string]any{{"operator": "Exists"}}, "containers": []map[string]any{{"name": "obi", "image": instrumentation.Image, "securityContext": map[string]any{"privileged": true, "runAsUser": 0}, "env": []map[string]string{{"name": "OTEL_EBPF_OPEN_PORT", "value": ports(instrumentation.OpenPorts)}, {"name": "OTEL_EXPORTER_OTLP_ENDPOINT", "value": "http://" + name + "-obi:" + port}, {"name": "OTEL_EBPF_BPF_CONTEXT_PROPAGATION", "value": instrumentation.ContextPropagation}, {"name": "OTEL_EBPF_KUBE_METADATA_ENABLE", "value": "false"}}, "volumeMounts": []map[string]string{{"name": "cgroup", "mountPath": "/sys/fs/cgroup"}, {"name": "security", "mountPath": "/sys/kernel/security"}}}}, "volumes": []map[string]any{{"name": "cgroup", "hostPath": map[string]string{"path": "/sys/fs/cgroup"}}, {"name": "security", "hostPath": map[string]string{"path": "/sys/kernel/security"}}}}}}}
	a, _ := json.Marshal(service)
	b, _ := json.Marshal(daemon)
	return string(a) + "\n---\n" + string(b) + "\n"
}

func mustAtoi(value string) int { n, _ := strconv.Atoi(value); return n }

func commandError(action string, err error) error {
	var exit *exec.ExitError
	if !strings.Contains(err.Error(), "exit status") {
		return fmt.Errorf("%s: %w", action, err)
	}
	if errors.As(err, &exit) && len(exit.Stderr) > 0 {
		return fmt.Errorf("%s: %w: %s", action, err, strings.TrimSpace(string(exit.Stderr)))
	}
	return fmt.Errorf("%s: %w", action, err)
}

func ExitCode(err error) int {
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() > 0 {
		return exit.ExitCode()
	}
	return 1
}
