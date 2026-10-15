// Package executorproto implements the private, versioned stdio protocol used
// between the local engine and an ephemeral executor in the target network.
package executorproto

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"lamplight/internal/datasource"
	"lamplight/internal/debuglog"
	"lamplight/internal/httpstep"
	"lamplight/internal/model"
	triggerexecutor "lamplight/internal/trigger"
)

const protocolVersion = 2

const (
	methodHTTPExecute              = "http.execute"
	methodTriggerExecute           = "trigger.execute"
	methodDatasourceTestConnection = "datasource.test_connection"
	methodDatasourceObserve        = "datasource.observe"
	methodMetricsSnapshot          = "metrics.snapshot"
	methodCancel                   = "cancel"
)

type Request struct {
	Version    int                     `json:"version"`
	ID         uint64                  `json:"id"`
	Method     string                  `json:"method"`
	HTTP       *model.HTTPRequest      `json:"http,omitempty"`
	Trigger    *model.TriggerRequest   `json:"trigger,omitempty"`
	HTTPClient *model.HTTPClientConfig `json:"http_client,omitempty"`
	Trace      *model.TestTraceContext `json:"trace,omitempty"`
	Datasource *datasource.Config      `json:"datasource,omitempty"`
	TraceID    model.TraceID           `json:"trace_id,omitempty"`
	Query      string                  `json:"query,omitempty"`
	CancelID   uint64                  `json:"cancel_id,omitempty"`
}

type Response struct {
	Version     int                     `json:"version"`
	ID          uint64                  `json:"id"`
	Result      *model.Response         `json:"result,omitempty"`
	Observation *model.TraceObservation `json:"observation,omitempty"`
	Metrics     *model.MetricSnapshot   `json:"metrics,omitempty"`
	Error       *RemoteError            `json:"error,omitempty"`
}

type RemoteError struct {
	Message    string `json:"message"`
	Retriable  bool   `json:"retriable,omitempty"`
	RetryAfter int64  `json:"retry_after_ns,omitempty"`
}

type Client struct {
	mu         sync.Mutex
	writeMu    sync.Mutex
	encoder    *json.Encoder
	decoder    *json.Decoder
	nextID     uint64
	pending    map[uint64]chan callResult
	readErr    error
	datasource *datasource.Config
}

type callResult struct {
	response Response
	err      error
}

func NewClient(input io.Writer, output io.Reader, datasourceConfig *datasource.Config) *Client {
	c := &Client{encoder: json.NewEncoder(input), decoder: json.NewDecoder(output), pending: make(map[uint64]chan callResult), datasource: datasourceConfig}
	go c.readResponses()
	return c
}

func (c *Client) Execute(ctx context.Context, request model.HTTPRequest, config model.HTTPClientConfig, trace *model.TestTraceContext) (model.Response, error) {
	response, err := c.call(ctx, Request{Method: methodHTTPExecute, HTTP: &request, HTTPClient: &config, Trace: trace})
	if err != nil {
		return model.Response{}, err
	}
	if response.Result == nil {
		return model.Response{}, errors.New("executor returned no HTTP result")
	}
	return *response.Result, nil
}

func (c *Client) ExecuteTrigger(ctx context.Context, request model.TriggerRequest, config model.HTTPClientConfig, trace *model.TestTraceContext) (model.Response, error) {
	response, err := c.call(ctx, Request{Method: methodTriggerExecute, Trigger: &request, HTTPClient: &config, Trace: trace})
	if err != nil {
		return model.Response{}, err
	}
	if response.Result == nil {
		return model.Response{}, errors.New("executor returned no trigger result")
	}
	return *response.Result, nil
}

func (c *Client) TestConnection(ctx context.Context) error {
	if c.datasource == nil {
		return errors.New("remote datasource is not configured")
	}
	_, err := c.call(ctx, Request{Method: methodDatasourceTestConnection, Datasource: c.datasource})
	return err
}

func (c *Client) Observe(ctx context.Context, traceID model.TraceID) (model.TraceObservation, error) {
	if c.datasource == nil {
		return model.TraceObservation{}, errors.New("remote datasource is not configured")
	}
	response, err := c.call(ctx, Request{Method: methodDatasourceObserve, Datasource: c.datasource, TraceID: traceID})
	if err != nil {
		return model.TraceObservation{}, err
	}
	if response.Observation == nil {
		return model.TraceObservation{}, errors.New("executor returned no trace observation")
	}
	return *response.Observation, nil
}

func (c *Client) Snapshot(ctx context.Context, query string) (model.MetricSnapshot, error) {
	if c.datasource == nil {
		return model.MetricSnapshot{}, errors.New("remote OTLP metrics receiver is not configured")
	}
	response, err := c.call(ctx, Request{Method: methodMetricsSnapshot, Datasource: c.datasource, Query: query})
	if err != nil {
		return model.MetricSnapshot{}, err
	}
	if response.Metrics == nil {
		return model.MetricSnapshot{}, errors.New("executor returned no metric snapshot")
	}
	return *response.Metrics, nil
}

func (c *Client) call(ctx context.Context, request Request) (Response, error) {
	c.mu.Lock()
	if err := ctx.Err(); err != nil {
		c.mu.Unlock()
		return Response{}, err
	}
	if c.readErr != nil {
		err := c.readErr
		c.mu.Unlock()
		return Response{}, err
	}
	c.nextID++
	request.Version, request.ID = protocolVersion, c.nextID
	answer := make(chan callResult, 1)
	c.pending[request.ID] = answer
	c.mu.Unlock()
	c.writeMu.Lock()
	err := ctx.Err()
	if err == nil {
		err = c.encoder.Encode(request)
		if err == nil && request.HTTP != nil {
			debuglog.Debug(ctx, "executor request sent", "request_id", request.ID, "method", request.Method, "url", request.HTTP.URL)
		}
	}
	c.writeMu.Unlock()
	if err != nil {
		c.removePending(request.ID)
		return Response{}, fmt.Errorf("send executor request: %w", err)
	}
	var received callResult
	select {
	case received = <-answer:
	case <-ctx.Done():
		c.removePending(request.ID)
		c.writeMu.Lock()
		_ = c.encoder.Encode(Request{Version: protocolVersion, Method: methodCancel, CancelID: request.ID})
		c.writeMu.Unlock()
		return Response{}, ctx.Err()
	}
	if received.err != nil {
		return Response{}, received.err
	}
	if request.HTTP != nil {
		debuglog.Debug(ctx, "executor response received", "request_id", request.ID, "method", request.Method, "url", request.HTTP.URL)
	}
	response := received.response
	if response.Error != nil {
		base := errors.New(response.Error.Message)
		if response.Error.Retriable {
			return Response{}, &model.ObservationError{Err: base, Retriable: true, RetryAfter: timeDuration(response.Error.RetryAfter)}
		}
		return Response{}, base
	}
	return response, nil
}

func (c *Client) removePending(id uint64) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}

func (c *Client) readResponses() {
	for {
		var response Response
		if err := c.decoder.Decode(&response); err != nil {
			c.failPending(fmt.Errorf("receive executor response: %w", err))
			return
		}
		if response.Version != protocolVersion || response.ID == 0 {
			c.failPending(errors.New("executor protocol response mismatch"))
			return
		}
		c.mu.Lock()
		answer := c.pending[response.ID]
		delete(c.pending, response.ID)
		c.mu.Unlock()
		if answer != nil {
			answer <- callResult{response: response}
		}
	}
}

func (c *Client) failPending(err error) {
	c.mu.Lock()
	c.readErr = err
	for id, answer := range c.pending {
		answer <- callResult{err: err}
		delete(c.pending, id)
	}
	c.mu.Unlock()
}

// TriggerClient gives the engine's trigger interface a distinct method while
// sharing one multiplexed protocol connection with HTTP and datasource calls.
type TriggerClient struct{ Client *Client }

func (c TriggerClient) Execute(ctx context.Context, request model.TriggerRequest, config model.HTTPClientConfig, trace *model.TestTraceContext) (model.Response, error) {
	return c.Client.ExecuteTrigger(ctx, request, config, trace)
}

func Serve(ctx context.Context, input io.Reader, output io.Writer) error {
	decoder, encoder := json.NewDecoder(input), json.NewEncoder(output)
	httpExecutor := httpstep.New(nil)
	triggerExecutor := triggerexecutor.New(httpExecutor)
	stores := map[string]model.DataStore{}
	var storesMu, writeMu, activeMu sync.Mutex
	active := map[uint64]context.CancelFunc{}
	var workers sync.WaitGroup
	limit := make(chan struct{}, 32)
	defer func() {
		activeMu.Lock()
		for _, cancel := range active {
			cancel()
		}
		activeMu.Unlock()
		workers.Wait()
		for _, store := range stores {
			if closer, ok := store.(io.Closer); ok {
				_ = closer.Close()
			}
		}
	}()
	for {
		var request Request
		if err := decoder.Decode(&request); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("decode executor request: %w", err)
		}
		if request.Method == methodCancel {
			activeMu.Lock()
			if cancel := active[request.CancelID]; cancel != nil {
				cancel()
			}
			activeMu.Unlock()
			continue
		}
		requestCtx, cancel := context.WithCancel(ctx)
		activeMu.Lock()
		active[request.ID] = cancel
		activeMu.Unlock()
		workers.Add(1)
		go func(request Request) {
			defer workers.Done()
			defer cancel()
			defer func() { activeMu.Lock(); delete(active, request.ID); activeMu.Unlock() }()
			select {
			case limit <- struct{}{}:
				defer func() { <-limit }()
			case <-requestCtx.Done():
				return
			}
			response := Response{Version: protocolVersion, ID: request.ID}
			if request.Version != protocolVersion {
				response.Error = &RemoteError{Message: fmt.Sprintf("unsupported executor protocol version %d", request.Version)}
			} else {
				handle(requestCtx, request, &response, httpExecutor, triggerExecutor, stores, &storesMu)
			}
			writeMu.Lock()
			_ = encoder.Encode(response)
			writeMu.Unlock()
		}(request)
	}
}

func handle(ctx context.Context, request Request, response *Response, httpExecutor model.HTTPExecutor, triggerExecutor model.TriggerExecutor, stores map[string]model.DataStore, storesMu *sync.Mutex) {
	var err error
	switch request.Method {
	case methodHTTPExecute:
		if request.HTTP == nil || request.HTTPClient == nil {
			err = errors.New("HTTP request and client configuration are required")
			break
		}
		var result model.Response
		result, err = httpExecutor.Execute(ctx, *request.HTTP, *request.HTTPClient, request.Trace)
		response.Result = &result
	case methodTriggerExecute:
		if request.Trigger == nil || request.HTTPClient == nil {
			err = errors.New("trigger request and HTTP client configuration are required")
			break
		}
		var result model.Response
		result, err = triggerExecutor.Execute(ctx, *request.Trigger, *request.HTTPClient, request.Trace)
		response.Result = &result
	case methodDatasourceTestConnection, methodDatasourceObserve, methodMetricsSnapshot:
		if request.Datasource == nil {
			err = errors.New("datasource configuration is required")
			break
		}
		var store model.DataStore
		storesMu.Lock()
		store, err = datasourceFor(*request.Datasource, stores)
		storesMu.Unlock()
		if err != nil {
			break
		}
		if request.Method == methodDatasourceTestConnection {
			err = store.TestConnection(ctx)
			break
		}
		if request.Method == methodMetricsSnapshot {
			metricStore, ok := store.(model.MetricStore)
			if !ok {
				err = errors.New("configured datasource does not accept OTLP metrics")
				break
			}
			var snapshot model.MetricSnapshot
			snapshot, err = metricStore.Snapshot(ctx, request.Query)
			response.Metrics = &snapshot
			break
		}
		var observation model.TraceObservation
		observation, err = store.Observe(ctx, request.TraceID)
		response.Observation = &observation
	default:
		err = fmt.Errorf("unsupported executor method %q", request.Method)
	}
	if err != nil {
		response.Result, response.Observation, response.Metrics = nil, nil, nil
		remote := &RemoteError{Message: err.Error()}
		var observationError *model.ObservationError
		if errors.As(err, &observationError) {
			remote.Retriable, remote.RetryAfter = observationError.Retriable, int64(observationError.RetryAfter)
		}
		response.Error = remote
	}
}

func datasourceFor(config datasource.Config, stores map[string]model.DataStore) (model.DataStore, error) {
	encoded, err := json.Marshal(config)
	if err != nil {
		return nil, err
	}
	key := string(encoded)
	if store, exists := stores[key]; exists {
		return store, nil
	}
	store, err := datasource.New(config)
	if err != nil {
		return nil, err
	}
	stores[key] = store
	return store, nil
}

func timeDuration(value int64) time.Duration { return time.Duration(value) }

var _ model.MetricStore = (*Client)(nil)
