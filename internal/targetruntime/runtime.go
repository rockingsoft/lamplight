package targetruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sort"
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
	Command func(context.Context, string, ...string) *exec.Cmd
}

func (r Launcher) Run(ctx context.Context, target model.TargetDefinition, configDir string, input io.Reader, streams IO) error {
	command := r.Command
	if command == nil {
		command = exec.CommandContext
	}
	switch target.Runtime {
	case "docker_compose":
		return r.dockerCompose(ctx, command, target, configDir, input, streams)
	case "kubernetes":
		return r.kubernetes(ctx, command, target, input, streams)
	default:
		return fmt.Errorf("unsupported remote runtime %q", target.Runtime)
	}
}

func (r Launcher) dockerCompose(ctx context.Context, command func(context.Context, string, ...string) *exec.Cmd, target model.TargetDefinition, configDir string, input io.Reader, streams IO) error {
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
	start := command(ctx, "docker", "start", "--attach", "--interactive", name)
	start.Stdin, start.Stdout, start.Stderr = input, streams.Out, streams.Err
	if err := start.Run(); err != nil {
		return commandError("run Lamplight container", err)
	}
	return nil
}

func (r Launcher) kubernetes(ctx context.Context, command func(context.Context, string, ...string) *exec.Cmd, target model.TargetDefinition, input io.Reader, streams IO) error {
	name := fmt.Sprintf("lamplight-run-%d", time.Now().Unix())
	args := []string{}
	if target.Kubernetes.Context != "" {
		args = append(args, "--context", target.Kubernetes.Context)
	}
	if target.Kubernetes.Namespace != "" {
		args = append(args, "--namespace", target.Kubernetes.Namespace)
	}
	args = append(args, "run", name, "--attach", "--stdin", "--rm", "--restart=Never", "--image", buildinfo.ExecutorImage())
	if target.Kubernetes.ServiceAccount != "" {
		override, _ := json.Marshal(map[string]any{"spec": map[string]any{"serviceAccountName": target.Kubernetes.ServiceAccount}})
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
