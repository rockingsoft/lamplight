package k6cloudrun

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

const (
	cloudPlatformScope = "https://www.googleapis.com/auth/cloud-platform"
	defaultRunBase     = "https://run.googleapis.com"
	defaultStorageBase = "https://storage.googleapis.com"
)

type Request struct {
	Project, Region, Job, Bucket string
	Tasks                        int
	Timeout, StartDelay          time.Duration
	Script, BundleRoot           string
	Files                        []string
	Environment                  map[string]string
	JobEnvironment               []string
	Arguments                    []string
	TraceParent, TraceState      string
	OutputLimit                  int64
	Progress                     func(Progress)
}

type Progress struct {
	Phase           string
	Execution       string
	LogURI          string
	CompletedShards int
	TotalShards     int
	Elapsed         time.Duration
}

type Result struct {
	Execution string        `json:"execution"`
	LogURI    string        `json:"log_uri,omitempty"`
	Shards    []ShardResult `json:"shards"`
}

type ShardResult struct {
	Index    int    `json:"index"`
	ExitCode int    `json:"exit_code"`
	Stdout   string `json:"stdout,omitempty"`
	Stderr   string `json:"stderr,omitempty"`
	Summary  any    `json:"summary,omitempty"`
}

type TaskConfig struct {
	Bucket         string            `json:"bucket"`
	BundleObject   string            `json:"bundle_object"`
	BundleSHA256   string            `json:"bundle_sha256"`
	ResultPrefix   string            `json:"result_prefix"`
	Script         string            `json:"script"`
	Environment    map[string]string `json:"environment,omitempty"`
	JobEnvironment []string          `json:"job_environment,omitempty"`
	Arguments      []string          `json:"arguments,omitempty"`
	TraceParent    string            `json:"traceparent,omitempty"`
	TraceState     string            `json:"tracestate,omitempty"`
	StartAt        time.Time         `json:"start_at"`
	OutputLimit    int64             `json:"output_limit"`
}

type operation struct {
	Name     string          `json:"name"`
	Done     bool            `json:"done"`
	Error    *apiError       `json:"error,omitempty"`
	Metadata json.RawMessage `json:"metadata,omitempty"`
	Response json.RawMessage `json:"response,omitempty"`
}

type apiError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type execution struct {
	Name           string `json:"name"`
	LogURI         string `json:"logUri"`
	CompletionTime string `json:"completionTime"`
	FailedCount    int    `json:"failedCount"`
	CancelledCount int    `json:"cancelledCount"`
}

type job struct {
	Template struct {
		Parallelism int `json:"parallelism"`
		Template    struct {
			MaxRetries int `json:"maxRetries"`
		} `json:"template"`
	} `json:"template"`
}

type Client struct {
	http        *http.Client
	runBase     string
	storageBase string
	poll        time.Duration
	now         func() time.Time
}

func New(ctx context.Context) (*Client, error) {
	var httpClient *http.Client
	var err error
	if accessToken := strings.TrimSpace(os.Getenv("GOOGLE_OAUTH_ACCESS_TOKEN")); accessToken != "" {
		httpClient = oauth2.NewClient(ctx, oauth2.StaticTokenSource(&oauth2.Token{AccessToken: accessToken}))
	} else {
		httpClient, err = google.DefaultClient(ctx, cloudPlatformScope)
	}
	if err != nil {
		return nil, fmt.Errorf("google application default credentials: %w", err)
	}
	return &Client{http: httpClient, runBase: defaultRunBase, storageBase: defaultStorageBase, poll: time.Second, now: time.Now}, nil
}

func newWithHTTPClient(httpClient *http.Client) *Client {
	return &Client{http: httpClient, runBase: defaultRunBase, storageBase: defaultStorageBase, poll: time.Second, now: time.Now}
}

func (c *Client) Run(ctx context.Context, request Request) (result Result, runErr error) {
	startedAt := c.now()
	if err := validateRequest(request); err != nil {
		return result, err
	}
	runID, err := randomID()
	if err != nil {
		return result, fmt.Errorf("create run id: %w", err)
	}
	prefix := "runs/" + runID
	bundleObject, configObject := prefix+"/bundle.tar.gz", prefix+"/config.json"
	resultObjects := make([]string, request.Tasks)
	for index := range request.Tasks {
		resultObjects[index] = prefix + "/results/" + strconv.Itoa(index) + ".json"
	}
	cleanupObjects := append([]string{bundleObject, configObject}, resultObjects...)
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if cleanupErr := c.deleteObjects(cleanupCtx, request.Bucket, cleanupObjects); cleanupErr != nil && runErr == nil {
			runErr = fmt.Errorf("clean up Cloud Run k6 objects: %w", cleanupErr)
		}
	}()

	bundle, scriptName, err := buildBundle(request.BundleRoot, request.Script, request.Files)
	if err != nil {
		return result, err
	}
	bundleHash := sha256.Sum256(bundle)
	if err := c.upload(ctx, request.Bucket, bundleObject, "application/gzip", bundle); err != nil {
		return result, fmt.Errorf("upload k6 bundle: %w", err)
	}
	config := TaskConfig{Bucket: request.Bucket, BundleObject: bundleObject, BundleSHA256: hex.EncodeToString(bundleHash[:]), ResultPrefix: prefix + "/results", Script: scriptName, Environment: request.Environment, JobEnvironment: request.JobEnvironment, Arguments: request.Arguments, TraceParent: request.TraceParent, TraceState: request.TraceState, StartAt: c.now().Add(request.StartDelay).UTC(), OutputLimit: request.OutputLimit}
	encodedConfig, err := json.Marshal(config)
	if err != nil {
		return result, fmt.Errorf("encode Cloud Run task config: %w", err)
	}
	configHash := sha256.Sum256(encodedConfig)
	if err := c.upload(ctx, request.Bucket, configObject, "application/json", encodedConfig); err != nil {
		return result, fmt.Errorf("upload Cloud Run task config: %w", err)
	}

	jobName := fmt.Sprintf("projects/%s/locations/%s/jobs/%s", request.Project, request.Region, request.Job)
	var configuredJob job
	if err := c.jsonRequest(ctx, http.MethodGet, c.runBase+"/v2/"+jobName, nil, &configuredJob); err != nil {
		return result, fmt.Errorf("inspect Cloud Run Job: %w", err)
	}
	if configuredJob.Template.Template.MaxRetries != 0 {
		return result, fmt.Errorf("pre-provisioned Cloud Run Job must set maxRetries to 0, got %d", configuredJob.Template.Template.MaxRetries)
	}
	if configuredJob.Template.Parallelism > 0 && request.Tasks > configuredJob.Template.Parallelism {
		return result, fmt.Errorf("cloud_run.tasks %d exceeds configured Job parallelism %d", request.Tasks, configuredJob.Template.Parallelism)
	}
	body := map[string]any{"overrides": map[string]any{
		"taskCount": request.Tasks,
		"timeout":   formatDuration(request.Timeout),
		"containerOverrides": []any{map[string]any{"env": []any{
			map[string]any{"name": "LAMPLIGHT_CONFIG_BUCKET", "value": request.Bucket},
			map[string]any{"name": "LAMPLIGHT_CONFIG_OBJECT", "value": configObject},
			map[string]any{"name": "LAMPLIGHT_CONFIG_SHA256", "value": hex.EncodeToString(configHash[:])},
		}}},
	}}
	var started operation
	if err := c.jsonRequest(ctx, http.MethodPost, c.runBase+"/v2/"+jobName+":run", body, &started); err != nil {
		return result, fmt.Errorf("start Cloud Run Job: %w", err)
	}
	reportProgress(request.Progress, Progress{Phase: "running", Execution: started.Name, TotalShards: request.Tasks})
	finished, operationErr := c.waitOperation(ctx, started, request.Bucket, resultObjects, startedAt, request.Progress)
	executionPayload := finished.Response
	if len(executionPayload) == 0 {
		executionPayload = finished.Metadata
	}
	if len(executionPayload) == 0 {
		if operationErr != nil {
			return result, fmt.Errorf("wait for Cloud Run Job: %w", operationErr)
		}
		return result, errors.New("cloud Run operation completed without execution metadata")
	}
	var completed execution
	if err := json.Unmarshal(executionPayload, &completed); err != nil {
		return result, fmt.Errorf("decode Cloud Run execution: %w", err)
	}
	if completed.Name == "" {
		return result, errors.New("operation completed without a Cloud Run execution name")
	}
	completed, err = c.waitExecution(ctx, completed, request.Bucket, resultObjects, startedAt, request.Progress)
	if err != nil {
		return result, fmt.Errorf("wait for Cloud Run execution: %w", err)
	}
	result.Execution, result.LogURI = completed.Name, completed.LogURI
	totalResultBytes := int64(0)
	for index, object := range resultObjects {
		encoded, downloadErr := c.download(ctx, request.Bucket, object, request.OutputLimit)
		if downloadErr != nil {
			return result, fmt.Errorf("read Cloud Run shard %d result: %w", index, downloadErr)
		}
		totalResultBytes += int64(len(encoded))
		if totalResultBytes > request.OutputLimit {
			return result, fmt.Errorf("combined Cloud Run shard results exceed %d bytes", request.OutputLimit)
		}
		var shard ShardResult
		if err := json.Unmarshal(encoded, &shard); err != nil {
			return result, fmt.Errorf("decode Cloud Run shard %d result: %w", index, err)
		}
		result.Shards = append(result.Shards, shard)
		reportProgress(request.Progress, Progress{Phase: "collecting", Execution: result.Execution, LogURI: result.LogURI, CompletedShards: index + 1, TotalShards: request.Tasks, Elapsed: c.now().Sub(startedAt)})
	}
	sort.Slice(result.Shards, func(i, j int) bool { return result.Shards[i].Index < result.Shards[j].Index })
	for _, shard := range result.Shards {
		if shard.ExitCode != 0 {
			detail := strings.TrimSpace(shard.Stderr)
			if detail == "" {
				detail = strings.TrimSpace(shard.Stdout)
			}
			if detail != "" {
				return result, fmt.Errorf("k6 shard %d on Cloud Run exited with code %d: %s", shard.Index, shard.ExitCode, detail)
			}
			return result, fmt.Errorf("k6 shard %d on Cloud Run exited with code %d", shard.Index, shard.ExitCode)
		}
	}
	if operationErr != nil {
		return result, fmt.Errorf("wait for Cloud Run Job: %w", operationErr)
	}
	if completed.FailedCount > 0 || completed.CancelledCount > 0 {
		return result, fmt.Errorf("execution on Cloud Run completed with %d failed and %d cancelled tasks", completed.FailedCount, completed.CancelledCount)
	}
	reportProgress(request.Progress, Progress{Phase: "completed", Execution: result.Execution, LogURI: result.LogURI, CompletedShards: request.Tasks, TotalShards: request.Tasks, Elapsed: c.now().Sub(startedAt)})
	return result, nil
}

func validateRequest(request Request) error {
	for name, value := range map[string]string{"project": request.Project, "region": request.Region, "job": request.Job, "bucket": request.Bucket, "script": request.Script, "bundle root": request.BundleRoot} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("cloud_run.%s is required", name)
		}
	}
	if request.Tasks < 1 || request.Tasks > 10_000 {
		return errors.New("cloud_run.tasks must be between 1 and 10000")
	}
	if request.Timeout <= 0 {
		return errors.New("cloud_run.timeout must be positive")
	}
	if request.StartDelay < 0 {
		return errors.New("cloud_run.start_delay cannot be negative")
	}
	if request.OutputLimit <= 0 {
		return errors.New("output limit for Cloud Run must be positive")
	}
	seenJobEnv := map[string]bool{}
	for _, name := range request.JobEnvironment {
		if !validEnvironmentName(name) {
			return fmt.Errorf("cloud_run.job_env contains invalid environment key %q", name)
		}
		if seenJobEnv[name] {
			return fmt.Errorf("cloud_run.job_env contains duplicate key %q", name)
		}
		seenJobEnv[name] = true
		if _, bundled := request.Environment[name]; bundled {
			return fmt.Errorf("cloud_run.job_env key %q must not be included in the run bundle", name)
		}
	}
	return nil
}

func validEnvironmentName(name string) bool {
	if name == "" || !((name[0] >= 'A' && name[0] <= 'Z') || (name[0] >= 'a' && name[0] <= 'z') || name[0] == '_') {
		return false
	}
	for _, character := range name[1:] {
		if !((character >= 'A' && character <= 'Z') || (character >= 'a' && character <= 'z') || (character >= '0' && character <= '9') || character == '_') {
			return false
		}
	}
	return true
}

func randomID() (string, error) {
	value := make([]byte, 12)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}

func buildBundle(root, script string, additional []string) ([]byte, string, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, "", fmt.Errorf("resolve bundle root: %w", err)
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return nil, "", fmt.Errorf("resolve bundle root links: %w", err)
	}
	paths := append([]string{script}, additional...)
	entries := map[string]string{}
	for _, path := range paths {
		info, statErr := os.Stat(path)
		if statErr != nil {
			return nil, "", fmt.Errorf("inspect bundle path %q: %w", path, statErr)
		}
		if info.IsDir() {
			walkErr := filepath.WalkDir(path, func(name string, entry os.DirEntry, walkErr error) error {
				if walkErr != nil {
					return walkErr
				}
				if entry.Type()&os.ModeSymlink != 0 {
					return fmt.Errorf("bundle path %q contains a symbolic link", name)
				}
				if entry.IsDir() {
					return nil
				}
				return addBundleEntry(root, name, entries)
			})
			if walkErr != nil {
				return nil, "", walkErr
			}
			continue
		}
		if err := addBundleEntry(root, path, entries); err != nil {
			return nil, "", err
		}
	}
	var buffer bytes.Buffer
	gzipWriter := gzip.NewWriter(&buffer)
	tarWriter := tar.NewWriter(gzipWriter)
	names := make([]string, 0, len(entries))
	for name := range entries {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		data, readErr := os.ReadFile(entries[name])
		if readErr != nil {
			return nil, "", fmt.Errorf("read bundle file %q: %w", name, readErr)
		}
		header := &tar.Header{Name: filepath.ToSlash(name), Mode: 0o600, Size: int64(len(data)), ModTime: time.Unix(0, 0)}
		if err := tarWriter.WriteHeader(header); err != nil {
			return nil, "", err
		}
		if _, err := tarWriter.Write(data); err != nil {
			return nil, "", err
		}
	}
	if err := tarWriter.Close(); err != nil {
		return nil, "", err
	}
	if err := gzipWriter.Close(); err != nil {
		return nil, "", err
	}
	resolvedScript, err := filepath.EvalSymlinks(script)
	if err != nil {
		return nil, "", err
	}
	scriptName, err := filepath.Rel(root, resolvedScript)
	if err != nil {
		return nil, "", err
	}
	return buffer.Bytes(), filepath.ToSlash(scriptName), nil
}

func addBundleEntry(root, path string, entries map[string]string) error {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return err
	}
	relative, err := filepath.Rel(root, resolved)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return fmt.Errorf("bundle path %q escapes project.base_dir", path)
	}
	entries[relative] = resolved
	return nil
}

func formatDuration(value time.Duration) string {
	return strconv.FormatFloat(value.Seconds(), 'f', -1, 64) + "s"
}

func (c *Client) waitOperation(ctx context.Context, current operation, bucket string, resultObjects []string, startedAt time.Time, progress func(Progress)) (operation, error) {
	lastReportedAt := startedAt
	lastCheckedAt := time.Time{}
	lastCompleted := -1
	for !current.Done {
		select {
		case <-ctx.Done():
			return current, ctx.Err()
		case <-time.After(c.poll):
		}
		if err := c.jsonRequest(ctx, http.MethodGet, c.runBase+"/v2/"+current.Name, nil, &current); err != nil {
			return current, err
		}
		now := c.now()
		if current.Done || lastCheckedAt.IsZero() || now.Sub(lastCheckedAt) >= 5*time.Second {
			completed := c.completedResults(ctx, bucket, resultObjects)
			lastCheckedAt = now
			if completed != lastCompleted || now.Sub(lastReportedAt) >= 15*time.Second {
				reportProgress(progress, Progress{Phase: "running", Execution: current.Name, CompletedShards: completed, TotalShards: len(resultObjects), Elapsed: now.Sub(startedAt)})
				lastCompleted, lastReportedAt = completed, now
			}
		}
	}
	if current.Error != nil {
		return current, fmt.Errorf("google API error %d: %s", current.Error.Code, current.Error.Message)
	}
	return current, nil
}

func (c *Client) waitExecution(ctx context.Context, current execution, bucket string, resultObjects []string, startedAt time.Time, progress func(Progress)) (execution, error) {
	lastReportedAt := startedAt
	lastCheckedAt := time.Time{}
	lastCompleted := -1
	for current.CompletionTime == "" {
		select {
		case <-ctx.Done():
			return current, ctx.Err()
		case <-time.After(c.poll):
		}
		if err := c.jsonRequest(ctx, http.MethodGet, c.runBase+"/v2/"+current.Name, nil, &current); err != nil {
			return current, err
		}
		now := c.now()
		if current.CompletionTime != "" || lastCheckedAt.IsZero() || now.Sub(lastCheckedAt) >= 5*time.Second {
			completed := c.completedResults(ctx, bucket, resultObjects)
			lastCheckedAt = now
			if completed != lastCompleted || now.Sub(lastReportedAt) >= 15*time.Second {
				reportProgress(progress, Progress{Phase: "running", Execution: current.Name, LogURI: current.LogURI, CompletedShards: completed, TotalShards: len(resultObjects), Elapsed: now.Sub(startedAt)})
				lastCompleted, lastReportedAt = completed, now
			}
		}
	}
	return current, nil
}

func (c *Client) completedResults(ctx context.Context, bucket string, objects []string) int {
	completed := 0
	for _, object := range objects {
		endpoint := fmt.Sprintf("%s/storage/v1/b/%s/o/%s?alt=media", c.storageBase, url.PathEscape(bucket), url.PathEscape(object))
		req, err := http.NewRequestWithContext(ctx, http.MethodHead, endpoint, nil)
		if err != nil {
			continue
		}
		response, err := c.http.Do(req)
		if err != nil {
			continue
		}
		_ = response.Body.Close()
		if response.StatusCode >= 200 && response.StatusCode < 300 {
			completed++
		}
	}
	return completed
}

func reportProgress(callback func(Progress), progress Progress) {
	if callback != nil {
		callback(progress)
	}
}

func (c *Client) upload(ctx context.Context, bucket, object, contentType string, data []byte) error {
	endpoint := fmt.Sprintf("%s/upload/storage/v1/b/%s/o?uploadType=media&ifGenerationMatch=0&name=%s", c.storageBase, url.PathEscape(bucket), url.QueryEscape(object))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", contentType)
	return c.do(req, nil)
}

func (c *Client) download(ctx context.Context, bucket, object string, limit int64) ([]byte, error) {
	endpoint := fmt.Sprintf("%s/storage/v1/b/%s/o/%s?alt=media", c.storageBase, url.PathEscape(bucket), url.PathEscape(object))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	var data []byte
	if err := c.doBytes(req, &data, limit); err != nil {
		return nil, err
	}
	return data, nil
}

func (c *Client) deleteObjects(ctx context.Context, bucket string, objects []string) error {
	var errs []error
	for _, object := range objects {
		endpoint := fmt.Sprintf("%s/storage/v1/b/%s/o/%s", c.storageBase, url.PathEscape(bucket), url.PathEscape(object))
		req, err := http.NewRequestWithContext(ctx, http.MethodDelete, endpoint, nil)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if err := c.doAllowNotFound(req); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (c *Client) jsonRequest(ctx context.Context, method, endpoint string, input, output any) error {
	var body io.Reader
	if input != nil {
		encoded, err := json.Marshal(input)
		if err != nil {
			return err
		}
		body = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return err
	}
	if input != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return c.do(req, output)
}

func (c *Client) do(req *http.Request, output any) error {
	response, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		message, _ := io.ReadAll(io.LimitReader(response.Body, 64<<10))
		return fmt.Errorf("%s %s returned %s: %s", req.Method, req.URL.Redacted(), response.Status, strings.TrimSpace(string(message)))
	}
	if output != nil {
		if err := json.NewDecoder(io.LimitReader(response.Body, 10<<20)).Decode(output); err != nil {
			return err
		}
	}
	return nil
}

func (c *Client) doBytes(req *http.Request, output *[]byte, limit int64) error {
	response, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("%s %s returned %s", req.Method, req.URL.Redacted(), response.Status)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return err
	}
	if int64(len(data)) > limit {
		return fmt.Errorf("response exceeds %d bytes", limit)
	}
	*output = data
	return nil
}

func (c *Client) doAllowNotFound(req *http.Request) error {
	response, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode == http.StatusNotFound || (response.StatusCode >= 200 && response.StatusCode < 300) {
		return nil
	}
	return fmt.Errorf("%s %s returned %s", req.Method, req.URL.Redacted(), response.Status)
}
