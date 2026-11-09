package agent

import (
	stdcontext "context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/luckystrker/ago/config"
	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/stream"
	agotest "github.com/luckystrker/ago/testing"
)

// r4FailFirstConfig fails the first model call, succeeds afterwards.
type r4FailFirstConfig struct {
	mu   sync.Mutex
	call int
}

func (c *r4FailFirstConfig) Provider() config.Provider { return "test" }
func (c *r4FailFirstConfig) Model() string             { return "fail-first" }
func (c *r4FailFirstConfig) Clone(map[string]any) (config.ModelConfig, error) {
	return c, nil
}
func (c *r4FailFirstConfig) Create() (config.LLMClient, error) { return c, nil }
func (c *r4FailFirstConfig) Call(stdcontext.Context, []events.Event, *agocontext.Context, config.CallOptions) (*events.ModelResponse, error) {
	c.mu.Lock()
	c.call++
	n := c.call
	c.mu.Unlock()
	if n == 1 {
		return nil, errors.New("child model failed")
	}
	return textResp("child done"), nil
}

// TestPublicParallelFailureDoesNotDuplicateTerminal pins the round-4 critical:
// one root turn issues two AsTool calls sharing a child stream; the first fails
// and the second queues. The failed terminal must not be relayed twice or carry
// the sibling's provenance.
func TestPublicParallelFailureDoesNotDuplicateTerminal(t *testing.T) {
	childStream := stream.NewMemoryStream()
	child := New("child", "", WithConfig(&r4FailFirstConfig{}), WithStreamBase(childStream))
	base := stream.NewMemoryStream()
	root := New("root", "", WithConfig(agotest.NewTestConfig(
		&events.ModelResponse{ToolCalls: events.ToolCallsEvent{Calls: []events.ToolCallEvent{
			{ID: "a", Name: "task_child", Arguments: `{"objective":"one"}`},
			{ID: "b", Name: "task_child", Arguments: `{"objective":"two"}`},
		}}},
		textResp("root done"),
	)), WithStreamBase(base), WithTools(child.AsTool(WithToolName("task_child"))))
	if _, err := root.Ask(stdcontext.Background(), "go"); err != nil {
		t.Fatalf("ask: %v", err)
	}
	evs, _ := base.History().GetEvents()
	var started, completed, failed int
	for _, ev := range evs {
		switch e := events.AsValue(ev).(type) {
		case events.TaskStarted:
			started++
		case events.TaskCompleted:
			completed++
		case events.TaskFailed:
			failed++
			if len(e.Path) != 1 || e.Path[0] != e.TaskID {
				t.Fatalf("TaskFailed %s carries foreign path %v", e.TaskID, e.Path)
			}
		}
	}
	if started != 2 || failed != 1 || completed != 1 {
		t.Fatalf("lifecycle = started %d failed %d completed %d, want 2/1/1", started, failed, completed)
	}
}

// TestRejectedNestedDelegationPublishesLifecycle pins upstream reject on the
// same-stream guard: a rejected delegation is still observable as one
// TaskStarted plus one TaskFailed on the parent stream.
func TestRejectedNestedDelegationPublishesLifecycle(t *testing.T) {
	shared := stream.NewMemoryStream()
	child := New("child", "", WithConfig(agotest.NewTestConfig(textResp("never"))), WithStreamBase(shared))
	root := New("root", "", WithConfig(agotest.NewTestConfig(
		&events.ModelResponse{ToolCalls: events.ToolCallsEvent{Calls: []events.ToolCallEvent{{
			ID: "a", Name: "task_child", Arguments: `{"objective":"x"}`,
		}}}},
		textResp("root done"),
	)), WithStreamBase(shared), WithTools(child.AsTool(WithToolName("task_child"))))
	if _, err := root.Ask(stdcontext.Background(), "go"); err != nil {
		t.Fatalf("ask: %v", err)
	}
	evs, _ := shared.History().GetEvents()
	var started, failed int
	for _, ev := range evs {
		switch e := events.AsValue(ev).(type) {
		case events.TaskStarted:
			started++
			if e.AgentName != "child" {
				t.Fatalf("TaskStarted agent = %q", e.AgentName)
			}
		case events.TaskFailed:
			failed++
			if e.AgentName != "child" {
				t.Fatalf("TaskFailed agent = %q", e.AgentName)
			}
		}
	}
	if started != 1 || failed != 1 {
		t.Fatalf("rejected lifecycle = started %d failed %d, want 1/1", started, failed)
	}
}

// TestGrandchildOnRootStreamRejectedViaRealHops pins the held-stream chain
// through actual nested runs: the guard must reject a grandchild configured
// with the root stream instead of deadlocking, and the reject lifecycle must be
// observable on the root.
func TestGrandchildOnRootStreamRejectedViaRealHops(t *testing.T) {
	rootStream := stream.NewMemoryStream()
	leaf := New("leaf", "", WithConfig(agotest.NewTestConfig(textResp("leaf done"))), WithStreamBase(rootStream))
	middle := New("middle", "", WithConfig(agotest.NewTestConfig(
		&events.ModelResponse{ToolCalls: events.ToolCallsEvent{Calls: []events.ToolCallEvent{{
			ID: "g", Name: "task_leaf", Arguments: `{"objective":"deep"}`,
		}}}},
		textResp("middle done"),
	)), WithTools(leaf.AsTool(WithToolName("task_leaf"))))
	root := New("root", "", WithConfig(agotest.NewTestConfig(
		&events.ModelResponse{ToolCalls: events.ToolCallsEvent{Calls: []events.ToolCallEvent{{
			ID: "m", Name: "task_middle", Arguments: `{"objective":"delegate"}`,
		}}}},
		textResp("root done"),
	)), WithStreamBase(rootStream), WithTools(middle.AsTool(WithToolName("task_middle"))))

	done := make(chan struct{})
	var err error
	go func() {
		defer close(done)
		_, err = root.Ask(stdcontext.Background(), "go")
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("grandchild on the root stream hung")
	}
	if err != nil {
		t.Fatalf("ask: %v", err)
	}
	evs, _ := rootStream.History().GetEvents()
	var started, completed, failed int
	for _, ev := range evs {
		switch events.AsValue(ev).(type) {
		case events.TaskStarted:
			started++
		case events.TaskCompleted:
			completed++
		case events.TaskFailed:
			failed++
		}
	}
	if started != 2 || completed != 1 || failed != 1 {
		t.Fatalf("lifecycle = started %d completed %d failed %d, want 2/1/1", started, completed, failed)
	}
}

// TestCloseBeforeStartIsNoOp pins the nil guards on an unstarted run: Close and
// awaitTurn must return without touching a cancel func or goroutine.
func TestCloseBeforeStartIsNoOp(t *testing.T) {
	var r AgentRun
	r.awaitTurn()
	r.Close()
	var nilRun *AgentRun
	nilRun.awaitTurn()
}
