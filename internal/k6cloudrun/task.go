package k6cloudrun

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

func RunTask(ctx context.Context, client *Client) error {
	bucket, object, expectedHash := os.Getenv("LAMPLIGHT_CONFIG_BUCKET"), os.Getenv("LAMPLIGHT_CONFIG_OBJECT"), os.Getenv("LAMPLIGHT_CONFIG_SHA256")
	if bucket == "" || object == "" || expectedHash == "" {
		return errors.New("task config environment for Cloud Run is incomplete")
	}
	encodedConfig, err := client.download(ctx, bucket, object, 10<<20)
	if err != nil {
		return fmt.Errorf("download task config: %w", err)
	}
	if err := verifySHA256(encodedConfig, expectedHash); err != nil {
		return fmt.Errorf("verify task config: %w", err)
	}
	var config TaskConfig
	if err := json.Unmarshal(encodedConfig, &config); err != nil {
		return fmt.Errorf("decode task config: %w", err)
	}
	index, count, err := cloudRunTaskIdentity()
	if err != nil {
		return err
	}
	bundle, err := client.download(ctx, config.Bucket, config.BundleObject, 100<<20)
	if err != nil {
		return fmt.Errorf("download k6 bundle: %w", err)
	}
	if err := verifySHA256(bundle, config.BundleSHA256); err != nil {
		return fmt.Errorf("verify k6 bundle: %w", err)
	}
	directory, err := os.MkdirTemp("", "lamplight-k6-cloud-run-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(directory) }()
	if err := extractBundle(bundle, directory); err != nil {
		return fmt.Errorf("extract k6 bundle: %w", err)
	}
	if delay := time.Until(config.StartAt); delay > 0 {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
	}
	result := executeShard(ctx, directory, config, index, count)
	encodedResult, err := json.Marshal(result)
	if err != nil {
		return err
	}
	resultObject := strings.TrimSuffix(config.ResultPrefix, "/") + "/" + strconv.Itoa(index) + ".json"
	if err := client.upload(ctx, config.Bucket, resultObject, "application/json", encodedResult); err != nil {
		return fmt.Errorf("upload shard result: %w", err)
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("k6 exited with code %d", result.ExitCode)
	}
	return nil
}

func cloudRunTaskIdentity() (int, int, error) {
	index, err := strconv.Atoi(os.Getenv("CLOUD_RUN_TASK_INDEX"))
	if err != nil || index < 0 {
		return 0, 0, errors.New("CLOUD_RUN_TASK_INDEX must be a non-negative integer")
	}
	count, err := strconv.Atoi(os.Getenv("CLOUD_RUN_TASK_COUNT"))
	if err != nil || count < 1 || index >= count {
		return 0, 0, errors.New("CLOUD_RUN_TASK_COUNT must be greater than the task index")
	}
	return index, count, nil
}

func executeShard(ctx context.Context, directory string, config TaskConfig, index, count int) ShardResult {
	limit := config.OutputLimit
	if limit <= 0 {
		limit = 10 << 20
	}
	summaryPath := filepath.Join(directory, "summary.json")
	sequence := executionSegmentSequence(count)
	segment := executionSegment(index, count)
	args := []string{"run", "--summary-export", summaryPath, "--execution-segment", segment, "--execution-segment-sequence", sequence}
	args = append(args, config.Arguments...)
	args = append(args, filepath.FromSlash(config.Script))
	command := exec.CommandContext(ctx, "k6", args...)
	command.Dir = directory
	environment, envErr := taskEnvironment(&config)
	if envErr != nil {
		return ShardResult{Index: index, ExitCode: 1, Stderr: envErr.Error()}
	}
	command.Env = environment
	stdout, stderr := newTaskCappedBuffer(limit), newTaskCappedBuffer(limit)
	command.Stdout, command.Stderr = stdout, stderr
	runErr := command.Run()
	exitCode := 0
	if runErr != nil {
		var exit *exec.ExitError
		if errors.As(runErr, &exit) {
			exitCode = exit.ExitCode()
		} else {
			exitCode = 1
			_, _ = stderr.Write([]byte(runErr.Error()))
		}
	}
	result := ShardResult{Index: index, ExitCode: exitCode, Stdout: redactTaskOutput(stdout.String(), config.Environment), Stderr: redactTaskOutput(stderr.String(), config.Environment)}
	if encoded, err := os.ReadFile(summaryPath); err == nil && int64(len(encoded)) <= limit {
		_ = json.Unmarshal(encoded, &result.Summary)
	}
	return result
}

func executionSegment(index, count int) string {
	leftNumerator, leftDenominator := reduceFraction(index, count)
	rightNumerator, rightDenominator := reduceFraction(index+1, count)
	left, right := fmt.Sprintf("%d/%d", leftNumerator, leftDenominator), fmt.Sprintf("%d/%d", rightNumerator, rightDenominator)
	if index == 0 {
		left = "0"
	}
	if index+1 == count {
		right = "1"
	}
	return left + ":" + right
}

func executionSegmentSequence(count int) string {
	parts := make([]string, count+1)
	for index := 0; index <= count; index++ {
		if index == 0 {
			parts[index] = "0"
		} else if index == count {
			parts[index] = "1"
		} else {
			numerator, denominator := reduceFraction(index, count)
			parts[index] = fmt.Sprintf("%d/%d", numerator, denominator)
		}
	}
	return strings.Join(parts, ",")
}

func reduceFraction(numerator, denominator int) (int, int) {
	a, b := numerator, denominator
	for b != 0 {
		a, b = b, a%b
	}
	return numerator / a, denominator / a
}

func taskEnvironment(config *TaskConfig) ([]string, error) {
	values := map[string]string{}
	for _, entry := range os.Environ() {
		if name, value, ok := strings.Cut(entry, "="); ok {
			values[name] = value
		}
	}
	if config.Environment == nil {
		config.Environment = map[string]string{}
	}
	for name, value := range config.Environment {
		if name != "" && !strings.Contains(name, "=") {
			values[name] = value
		}
	}
	for _, name := range config.JobEnvironment {
		if _, bundled := config.Environment[name]; bundled {
			return nil, fmt.Errorf("job environment key %q is also present in bundled environment", name)
		}
		sourceName := "LAMPLIGHT_VAR_" + name
		value, exists := os.LookupEnv(sourceName)
		if !exists {
			return nil, fmt.Errorf("required Cloud Run Job environment variable %s is not configured", sourceName)
		}
		if direct, exists := os.LookupEnv(name); exists && direct != value {
			return nil, fmt.Errorf("preconfigured Job environment variables %s and %s are inconsistent", sourceName, name)
		}
		delete(values, sourceName)
		values[name] = value
		config.Environment[name] = value
	}
	if config.TraceParent != "" {
		values["LAMPLIGHT_TRACEPARENT"] = config.TraceParent
		values["LAMPLIGHT_TRACESTATE"] = config.TraceState
	}
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	result := make([]string, 0, len(names))
	for _, name := range names {
		result = append(result, name+"="+values[name])
	}
	return result, nil
}

func redactTaskOutput(value string, environment map[string]string) string {
	for _, secret := range environment {
		if secret != "" {
			value = strings.ReplaceAll(value, secret, "[REDACTED]")
		}
	}
	return value
}

func verifySHA256(data []byte, expected string) error {
	digest := sha256.Sum256(data)
	if !strings.EqualFold(hex.EncodeToString(digest[:]), expected) {
		return errors.New("SHA-256 mismatch")
	}
	return nil
}

func extractBundle(encoded []byte, directory string) error {
	gzipReader, err := gzip.NewReader(bytes.NewReader(encoded))
	if err != nil {
		return err
	}
	defer func() { _ = gzipReader.Close() }()
	tarReader := tar.NewReader(gzipReader)
	for {
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if header.Typeflag != tar.TypeReg {
			return fmt.Errorf("unsupported archive entry %q", header.Name)
		}
		target := filepath.Join(directory, filepath.FromSlash(header.Name))
		relative, err := filepath.Rel(directory, target)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return fmt.Errorf("archive entry %q escapes working directory", header.Name)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return err
		}
		file, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(file, io.LimitReader(tarReader, header.Size))
		closeErr := file.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
	}
}

type taskCappedBuffer struct {
	buffer bytes.Buffer
	limit  int64
}

func newTaskCappedBuffer(limit int64) *taskCappedBuffer { return &taskCappedBuffer{limit: limit} }

func (b *taskCappedBuffer) Write(data []byte) (int, error) {
	original := len(data)
	remaining := b.limit - int64(b.buffer.Len())
	if remaining > 0 {
		if int64(len(data)) > remaining {
			data = data[:remaining]
		}
		_, _ = b.buffer.Write(data)
	}
	return original, nil
}

func (b *taskCappedBuffer) String() string { return b.buffer.String() }
