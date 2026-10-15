// Package requesthook runs user-owned request transformations over JSON stdio.
package requesthook

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const maxOutputBytes = 10 << 20

type cappedBuffer struct {
	bytes.Buffer
	overflow bool
	limit    int
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	remaining := b.limit - b.Len()
	if remaining < len(p) {
		b.overflow = true
		p = p[:max(0, remaining)]
	}
	_, _ = b.Buffer.Write(p)
	return n, nil
}

// Run sends one request JSON document to stdin and reads one replacement from stdout.
// The command executes in the project's base directory, with the caller's environment
// plus only the explicit overrides supplied by the hook definition.
func Run(ctx context.Context, baseDir string, command []string, env map[string]string, input []byte) ([]byte, error) {
	if len(command) == 0 || strings.TrimSpace(command[0]) == "" {
		return nil, errors.New("request_hook.command must be a non-empty list of strings")
	}
	for _, arg := range command {
		if strings.ContainsRune(arg, 0) {
			return nil, errors.New("request_hook.command contains a NUL byte")
		}
	}
	if len(input) > maxOutputBytes {
		return nil, fmt.Errorf("request_hook input exceeds %d bytes", maxOutputBytes)
	}
	executable := command[0]
	if !filepath.IsAbs(executable) && strings.ContainsRune(executable, filepath.Separator) {
		absoluteBase, err := filepath.Abs(baseDir)
		if err != nil {
			return nil, fmt.Errorf("resolve request_hook working directory: %w", err)
		}
		executable = filepath.Join(absoluteBase, executable)
	}
	cmd := exec.CommandContext(ctx, executable, command[1:]...)
	cmd.Dir = baseDir
	cmd.Stdin = bytes.NewReader(input)
	cmd.Env = os.Environ()
	for key, value := range env {
		if key == "" || strings.ContainsAny(key, "=\x00") || strings.ContainsRune(value, 0) {
			return nil, fmt.Errorf("request_hook.env contains an invalid key or value for %q", key)
		}
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	stdout := &cappedBuffer{limit: maxOutputBytes}
	stderr := &cappedBuffer{limit: 4096}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("request_hook interrupted: %w", ctx.Err())
		}
		return nil, fmt.Errorf("request_hook command failed: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	if stdout.overflow {
		return nil, fmt.Errorf("request_hook output exceeds %d bytes", maxOutputBytes)
	}
	return stdout.Bytes(), nil
}
