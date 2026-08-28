// Package prometheus queries Prometheus servers or continuously scrapes a
// Prometheus/OpenMetrics exposition endpoint into Lamplight's PromQL store.
package prometheus

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"lamplight/internal/model"
	"lamplight/internal/promqlstore"
)

const maxBodyBytes = 10 << 20

var metricNamePattern = regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*$`)

type Config struct {
	Endpoint       string
	Headers        map[string]string
	BearerToken    string
	TLSSkipVerify  bool
	Timeout        time.Duration
	QueryAPI       bool
	ScrapeInterval time.Duration
}

type Store struct {
	endpoint       string
	headers        map[string]string
	client         *http.Client
	queryAPI       bool
	scrapeInterval time.Duration
	local          *promqlstore.Store
	start          sync.Once
	firstScrape    chan struct{}
	stop           chan struct{}
	mu             sync.RWMutex
	lastScrapeErr  error
}

func New(config Config) (*Store, error) {
	endpoint, err := url.Parse(config.Endpoint)
	if err != nil || endpoint.Scheme != "http" && endpoint.Scheme != "https" || endpoint.Host == "" {
		return nil, fmt.Errorf("prometheus metrics endpoint must be an absolute HTTP URL: %q", config.Endpoint)
	}
	if endpoint.Fragment != "" {
		return nil, errors.New("prometheus metrics endpoint must not contain a fragment")
	}
	headers := map[string]string{}
	for name, value := range config.Headers {
		headers[name] = value
	}
	if config.BearerToken != "" {
		headers["Authorization"] = "Bearer " + config.BearerToken
	}
	timeout := config.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	transport := &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: config.TLSSkipVerify}} //nolint:gosec // Explicit local-development option.
	if config.QueryAPI && !strings.HasSuffix(endpoint.Path, "/api/v1/query") {
		endpoint.Path = strings.TrimSuffix(endpoint.Path, "/") + "/api/v1/query"
	}
	scrapeInterval := config.ScrapeInterval
	if scrapeInterval <= 0 {
		scrapeInterval = 500 * time.Millisecond
	}
	return &Store{endpoint: endpoint.String(), headers: headers, client: &http.Client{Timeout: timeout, Transport: transport}, queryAPI: config.QueryAPI, scrapeInterval: scrapeInterval, local: promqlstore.New(), firstScrape: make(chan struct{}), stop: make(chan struct{})}, nil
}

func (s *Store) Snapshot(ctx context.Context, query string) (model.MetricSnapshot, error) {
	if s.queryAPI {
		return s.query(ctx, query)
	}
	if strings.TrimSpace(query) == "" {
		return model.MetricSnapshot{}, errors.New("metric checks require a PromQL query")
	}
	s.start.Do(func() { go s.scrapeLoop() })
	select {
	case <-ctx.Done():
		return model.MetricSnapshot{}, ctx.Err()
	case <-s.firstScrape:
	}
	s.mu.RLock()
	lastErr := s.lastScrapeErr
	s.mu.RUnlock()
	if lastErr != nil {
		return model.MetricSnapshot{}, lastErr
	}
	return s.local.Snapshot(ctx, query)
}

func (s *Store) Close() error {
	select {
	case <-s.stop:
	default:
		close(s.stop)
	}
	return nil
}

func (s *Store) scrapeLoop() {
	first := true
	for {
		err := s.scrape(context.Background())
		s.mu.Lock()
		s.lastScrapeErr = err
		s.mu.Unlock()
		if first {
			close(s.firstScrape)
			first = false
		}
		timer := time.NewTimer(s.scrapeInterval)
		select {
		case <-s.stop:
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (s *Store) scrape(ctx context.Context) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, s.endpoint, nil)
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "application/openmetrics-text; version=1.0.0, text/plain; version=0.0.4")
	for name, value := range s.headers {
		request.Header.Set(name, value)
	}
	response, err := s.client.Do(request)
	if err != nil {
		return &model.ObservationError{Err: err, Retriable: true}
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		retriable := response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= 500
		return &model.ObservationError{Err: fmt.Errorf("prometheus metrics endpoint returned HTTP %d", response.StatusCode), Retriable: retriable}
	}
	limited := io.LimitReader(response.Body, maxBodyBytes+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return &model.ObservationError{Err: fmt.Errorf("read Prometheus metrics: %w", err), Retriable: true}
	}
	if len(body) > maxBodyBytes {
		return &model.ObservationError{Err: fmt.Errorf("prometheus metrics response exceeds %d bytes", maxBodyBytes), Retriable: false}
	}
	snapshot, err := Parse(body)
	if err != nil {
		return &model.ObservationError{Err: err, Retriable: false}
	}
	if err := s.local.Ingest(snapshot.Samples, time.Now()); err != nil {
		return &model.ObservationError{Err: err, Retriable: false}
	}
	return nil
}

func (s *Store) query(ctx context.Context, query string) (model.MetricSnapshot, error) {
	if strings.TrimSpace(query) == "" {
		return model.MetricSnapshot{}, errors.New("metrics \"prometheus\" checks require a PromQL query")
	}
	form := url.Values{"query": []string{query}}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, s.endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return model.MetricSnapshot{}, err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "application/json")
	for name, value := range s.headers {
		request.Header.Set(name, value)
	}
	response, err := s.client.Do(request)
	if err != nil {
		return model.MetricSnapshot{}, &model.ObservationError{Err: err, Retriable: true}
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		retriable := response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= 500
		return model.MetricSnapshot{}, &model.ObservationError{Err: fmt.Errorf("prometheus query API returned HTTP %d", response.StatusCode), Retriable: retriable}
	}
	var payload struct {
		Status    string `json:"status"`
		ErrorType string `json:"errorType"`
		Error     string `json:"error"`
		Data      struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Metric map[string]string `json:"metric"`
				Value  []json.RawMessage `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, maxBodyBytes)).Decode(&payload); err != nil {
		return model.MetricSnapshot{}, &model.ObservationError{Err: fmt.Errorf("decode Prometheus query response: %w", err), Retriable: false}
	}
	if payload.Status != "success" {
		return model.MetricSnapshot{}, &model.ObservationError{Err: fmt.Errorf("prometheus query failed (%s): %s", payload.ErrorType, payload.Error), Retriable: false}
	}
	if payload.Data.ResultType != "vector" {
		return model.MetricSnapshot{}, fmt.Errorf("prometheus instant query returned %q; expected vector", payload.Data.ResultType)
	}
	samples := make([]model.MetricSample, 0, len(payload.Data.Result))
	for _, result := range payload.Data.Result {
		if len(result.Value) != 2 {
			return model.MetricSnapshot{}, errors.New("prometheus query result has an invalid value tuple")
		}
		var encoded string
		if err := json.Unmarshal(result.Value[1], &encoded); err != nil {
			return model.MetricSnapshot{}, fmt.Errorf("decode Prometheus query value: %w", err)
		}
		value, err := strconv.ParseFloat(encoded, 64)
		if err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
			return model.MetricSnapshot{}, fmt.Errorf("prometheus query returned unsupported value %q", encoded)
		}
		labels := make(map[string]string, len(result.Metric))
		for name, labelValue := range result.Metric {
			labels[name] = labelValue
		}
		name := labels["__name__"]
		delete(labels, "__name__")
		samples = append(samples, model.MetricSample{Name: name, Value: value, Labels: labels})
	}
	sort.Slice(samples, func(i, j int) bool { return sampleKey(samples[i]) < sampleKey(samples[j]) })
	return model.MetricSnapshot{Samples: samples}, nil
}

func Parse(body []byte) (model.MetricSnapshot, error) {
	types := map[string]string{}
	samples := []model.MetricSample{}
	scanner := bufio.NewScanner(strings.NewReader(string(body)))
	scanner.Buffer(make([]byte, 64*1024), maxBodyBytes)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "# TYPE ") {
			fields := strings.Fields(line)
			if len(fields) == 4 && metricNamePattern.MatchString(fields[2]) {
				types[fields[2]] = fields[3]
			}
			continue
		}
		if strings.HasPrefix(line, "#") {
			continue
		}
		sample, err := parseSample(line)
		if err != nil {
			return model.MetricSnapshot{}, err
		}
		sample.Type = types[sample.Name]
		if sample.Type == "" {
			matchedFamily := ""
			for family, metricType := range types {
				if (metricType == "histogram" || metricType == "summary") && len(family) > len(matchedFamily) && strings.HasPrefix(sample.Name, family+"_") {
					sample.Type = metricType
					matchedFamily = family
				}
			}
		}
		samples = append(samples, sample)
	}
	if err := scanner.Err(); err != nil {
		return model.MetricSnapshot{}, fmt.Errorf("scan Prometheus metrics: %w", err)
	}
	sort.Slice(samples, func(i, j int) bool { return sampleKey(samples[i]) < sampleKey(samples[j]) })
	for index := 1; index < len(samples); index++ {
		if sampleKey(samples[index-1]) == sampleKey(samples[index]) {
			return model.MetricSnapshot{}, fmt.Errorf("prometheus exposition contains duplicate series %q", samples[index].Name)
		}
	}
	return model.MetricSnapshot{Samples: samples}, nil
}

func parseSample(line string) (model.MetricSample, error) {
	boundary := fieldBoundary(line)
	if boundary < 0 {
		return model.MetricSample{}, fmt.Errorf("invalid Prometheus sample %q", line)
	}
	identity, remainder := line[:boundary], strings.TrimSpace(line[boundary:])
	fields := strings.Fields(remainder)
	if len(fields) == 0 {
		return model.MetricSample{}, fmt.Errorf("prometheus sample has no value: %q", line)
	}
	value, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return model.MetricSample{}, fmt.Errorf("invalid Prometheus sample value %q: %w", fields[0], err)
	}
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return model.MetricSample{}, fmt.Errorf("prometheus sample %q has a non-finite value, which metric assertions do not support", identity)
	}
	name, labels := identity, map[string]string{}
	if opening := strings.IndexByte(identity, '{'); opening >= 0 {
		if !strings.HasSuffix(identity, "}") {
			return model.MetricSample{}, fmt.Errorf("invalid Prometheus labels in %q", line)
		}
		name = identity[:opening]
		labels, err = parseLabels(identity[opening+1 : len(identity)-1])
		if err != nil {
			return model.MetricSample{}, err
		}
	}
	if !metricNamePattern.MatchString(name) {
		return model.MetricSample{}, fmt.Errorf("invalid Prometheus metric name %q", name)
	}
	return model.MetricSample{Name: name, Value: value, Labels: labels}, nil
}

func fieldBoundary(line string) int {
	quoted, escaped, depth := false, false, 0
	for index, char := range line {
		if escaped {
			escaped = false
			continue
		}
		if quoted && char == '\\' {
			escaped = true
			continue
		}
		if char == '"' {
			quoted = !quoted
			continue
		}
		if !quoted {
			switch char {
			case '{':
				depth++
			case '}':
				depth--
			case ' ', '\t':
				if depth == 0 {
					return index
				}
			}
		}
	}
	return -1
}

func parseLabels(input string) (map[string]string, error) {
	labels := map[string]string{}
	for strings.TrimSpace(input) != "" {
		input = strings.TrimSpace(input)
		equals := strings.IndexByte(input, '=')
		if equals <= 0 {
			return nil, fmt.Errorf("invalid Prometheus label set %q", input)
		}
		name := strings.TrimSpace(input[:equals])
		input = strings.TrimSpace(input[equals+1:])
		if !metricNamePattern.MatchString(name) || !strings.HasPrefix(input, `"`) {
			return nil, fmt.Errorf("invalid Prometheus label %q", name)
		}
		end := quotedEnd(input)
		if end < 0 {
			return nil, fmt.Errorf("unterminated Prometheus label %q", name)
		}
		value, err := strconv.Unquote(input[:end+1])
		if err != nil {
			return nil, fmt.Errorf("decode Prometheus label %q: %w", name, err)
		}
		labels[name] = value
		input = strings.TrimSpace(input[end+1:])
		if input == "" {
			break
		}
		if input[0] != ',' {
			return nil, fmt.Errorf("invalid Prometheus label separator in %q", input)
		}
		input = input[1:]
	}
	return labels, nil
}

func quotedEnd(input string) int {
	escaped := false
	for index := 1; index < len(input); index++ {
		if escaped {
			escaped = false
			continue
		}
		if input[index] == '\\' {
			escaped = true
		} else if input[index] == '"' {
			return index
		}
	}
	return -1
}

func sampleKey(sample model.MetricSample) string {
	names := make([]string, 0, len(sample.Labels))
	for name := range sample.Labels {
		names = append(names, name)
	}
	sort.Strings(names)
	var value strings.Builder
	value.WriteString(sample.Name)
	for _, name := range names {
		value.WriteByte(0)
		value.WriteString(name)
		value.WriteByte('=')
		value.WriteString(sample.Labels[name])
	}
	return value.String()
}

var _ model.MetricStore = (*Store)(nil)
