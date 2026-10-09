package engine

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"lamplight/internal/model"
)

type barrierHTTP struct {
	started chan string
	release chan struct{}
	mu      sync.Mutex
	seen    map[string][]string
}

func (h *barrierHTTP) Execute(ctx context.Context, request model.HTTPRequest, _ model.HTTPClientConfig, _ *model.TestTraceContext) (model.Response, error) {
	parts := strings.Split(strings.TrimPrefix(request.URL, "http://example.test/"), "/")
	h.mu.Lock()
	h.seen[parts[0]] = append(h.seen[parts[0]], parts[1])
	h.mu.Unlock()
	if parts[1] == "first" {
		h.started <- parts[0]
		select {
		case <-ctx.Done():
			return model.Response{}, ctx.Err()
		case <-h.release:
		}
	}
	return model.Response{StatusCode: 200}, nil
}

func TestRunConcurrentTestsKeepStepsSequentialAndResultsOrdered(t *testing.T) {
	http := &barrierHTTP{started: make(chan string, 2), release: make(chan struct{}), seen: map[string][]string{}}
	project := &model.Project{Definition: &model.ProjectDefinition{}, Tests: []model.TestDefinition{}}
	for _, name := range []string{"alpha", "beta"} {
		steps := []model.StepDefinition{}
		for _, step := range []string{"first", "second"} {
			steps = append(steps, model.StepDefinition{Name: step, HTTP: model.HTTPRequestDefinition{Method: parseExpr(t, `"GET"`), URL: parseExpr(t, `"http://example.test/`+name+`/`+step+`"`)}})
		}
		project.Tests = append(project.Tests, model.TestDefinition{Name: name, Steps: steps})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan model.RunResult, 1)
	go func() { done <- (&Engine{HTTP: http, Workers: 2}).Run(ctx, project) }()
	started := func() string {
		select {
		case name := <-http.started:
			return name
		case <-ctx.Done():
			t.Fatal("tests did not start concurrently")
			return ""
		}
	}
	first := started()
	second := started()
	if first == second {
		t.Fatalf("only one test started: %q, %q", first, second)
	}
	close(http.release)
	run := <-done
	if run.Status != model.StatusPassed || run.Tests[0].Name != "alpha" || run.Tests[1].Name != "beta" {
		t.Fatalf("run=%#v", run)
	}
	for name, steps := range http.seen {
		if strings.Join(steps, ",") != "first,second" {
			t.Fatalf("%s steps=%v", name, steps)
		}
	}
}
