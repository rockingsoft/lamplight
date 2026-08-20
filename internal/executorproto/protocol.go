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
	"lamplight/internal/httpstep"
	"lamplight/internal/model"
	triggerexecutor "lamplight/internal/trigger"
)

const protocolVersion = 1

const (
	methodHTTPExecute              = "http.execute"
	methodTriggerExecute           = "trigger.execute"
	methodDatasourceTestConnection = "datasource.test_connection"
	methodDatasourceObserve        = "datasource.observe"
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
}

type Response struct {
	Version     int                     `json:"version"`
	ID          uint64                  `json:"id"`
	Result      *model.Response         `json:"result,omitempty"`
	Observation *model.TraceObservation `json:"observation,omitempty"`
	Error       *RemoteError            `json:"error,omitempty"`
}

type RemoteError struct {
	Message    string `json:"message"`
	Retriable  bool   `json:"retriable,omitempty"`
	RetryAfter int64  `json:"retry_after_ns,omitempty"`
}

type Client struct {
	mu         sync.Mutex
	encoder    *json.Encoder
	decoder    *json.Decoder
	nextID     uint64
	datasource *datasource.Config
}

func NewClient(input io.Writer, output io.Reader, datasourceConfig *datasource.Config) *Client {
	return &Client{encoder: json.NewEncoder(input), decoder: json.NewDecoder(output), datasource: datasourceConfig}
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

func (c *Client) call(ctx context.Context, request Request) (Response, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return Response{}, err
	}
	c.nextID++
	request.Version, request.ID = protocolVersion, c.nextID
	if err := c.encoder.Encode(request); err != nil {
		return Response{}, fmt.Errorf("send executor request: %w", err)
	}
	var response Response
	if err := c.decoder.Decode(&response); err != nil {
		return Response{}, fmt.Errorf("receive executor response: %w", err)
	}
	if response.Version != protocolVersion || response.ID != request.ID {
		return Response{}, errors.New("executor protocol response mismatch")
	}
	if response.Error != nil {
		base := errors.New(response.Error.Message)
		if response.Error.Retriable {
			return Response{}, &model.ObservationError{Err: base, Retriable: true, RetryAfter: timeDuration(response.Error.RetryAfter)}
		}
		return Response{}, base
	}
	return response, nil
}

// TriggerClient gives the engine's trigger interface a distinct method while
// sharing one ordered protocol connection with HTTP and datasource calls.
type TriggerClient struct{ Client *Client }

func (c TriggerClient) Execute(ctx context.Context, request model.TriggerRequest, config model.HTTPClientConfig, trace *model.TestTraceContext) (model.Response, error) {
	return c.Client.ExecuteTrigger(ctx, request, config, trace)
}

func Serve(ctx context.Context, input io.Reader, output io.Writer) error {
	decoder, encoder := json.NewDecoder(input), json.NewEncoder(output)
	httpExecutor := httpstep.New(nil)
	triggerExecutor := triggerexecutor.New(httpExecutor)
	stores := map[string]model.DataStore{}
	defer func() {
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
		response := Response{Version: protocolVersion, ID: request.ID}
		if request.Version != protocolVersion {
			response.Error = &RemoteError{Message: fmt.Sprintf("unsupported executor protocol version %d", request.Version)}
		} else {
			handle(ctx, request, &response, httpExecutor, triggerExecutor, stores)
		}
		if err := encoder.Encode(response); err != nil {
			return fmt.Errorf("encode executor response: %w", err)
		}
	}
}

func handle(ctx context.Context, request Request, response *Response, httpExecutor model.HTTPExecutor, triggerExecutor model.TriggerExecutor, stores map[string]model.DataStore) {
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
	case methodDatasourceTestConnection, methodDatasourceObserve:
		if request.Datasource == nil {
			err = errors.New("datasource configuration is required")
			break
		}
		var store model.DataStore
		store, err = datasourceFor(*request.Datasource, stores)
		if err != nil {
			break
		}
		if request.Method == methodDatasourceTestConnection {
			err = store.TestConnection(ctx)
			break
		}
		var observation model.TraceObservation
		observation, err = store.Observe(ctx, request.TraceID)
		response.Observation = &observation
	default:
		err = fmt.Errorf("unsupported executor method %q", request.Method)
	}
	if err != nil {
		response.Result, response.Observation = nil, nil
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
