package agent

import (
	stdcontext "context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/luckystrker/ago/config"
	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/stream"
)

// gateConfig blocks every model call until the test releases it, and reports
// when the first call started.
type gateConfig struct {
	started  chan struct{}
	release  chan struct{}
	startOne sync.Once
}

func (g *gateConfig) Provider() config.Provider                        { return "test" }
func (g *gateConfig) Model() string                                    { return "test" }
func (g *gateConfig) Clone(map[string]any) (config.ModelConfig, error) { return g, nil }
func (g *gateConfig) Create() (config.LLMClient, error)                { return &gateClient{g: g}, nil }

type gateClient struct{ g *gateConfig }

func (c *gateClient) Call(stdcontext.Context, []events.Event, *agocontext.Context, config.CallOptions) (*events.ModelResponse, error) {
	c.g.startOne.Do(func() { close(c.g.started) })
	<-c.g.release
	return &events.ModelResponse{Message: &events.ModelMessage{Content: "background answer"}}, nil
}

// TestBackgroundToolReturnsBeforeChildAndReportsOnCallerStream pins
// background_agent_tool: the tool call returns a task id at once, before the
// child finishes, and the child's completion reaches the caller's stream.
func TestBackgroundToolReturnsBeforeChildAndReportsOnCallerStream(t *testing.T) {
	gate := &gateConfig{started: make(chan struct{}), release: make(chan struct{})}
	child := New("worker", "", WithConfig(gate))
	tool := child.AsBackgroundTool(WithToolName("bg"), WithToolDescription("background"))

	caller := stream.NewMemoryStream()
	cx := agocontext.New(caller)
	if _, err := tool.Register(cx, nil); err != nil {
		t.Fatalf("register: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		done <- cx.Send(events.ToolCallEvent{ID: "call-bg", Name: "bg", Arguments: `{"objective":"do it"}`})
	}()

	select {
	case <-gate.started:
	case <-time.After(5 * time.Second):
		t.Fatal("background child never started")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("tool send: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the background tool call did not return before the child finished")
	}

	close(gate.release)
	caller.WaitBackground()

	// The child's terminal reached the caller's stream.
	te := collectTaskEvents(t, caller)
	if len(te.started) != 1 || len(te.completed) != 1 {
		t.Fatalf("caller stream lifecycle: started=%d completed=%d, want 1 and 1",
			len(te.started), len(te.completed))
	}
	if te.completed[0].AgentName != "worker" {
		t.Fatalf("completed agent = %q, want worker", te.completed[0].AgentName)
	}
	if te.started[0].TaskID == "" || te.started[0].TaskID != te.completed[0].TaskID {
		t.Fatalf("task ids: started=%q completed=%q", te.started[0].TaskID, te.completed[0].TaskID)
	}

	// The result is delivered as a follow-up turn on the caller's stream.
	pending := caller.Pending()
	if len(pending) != 1 {
		t.Fatalf("pending follow-up messages = %d, want 1", len(pending))
	}
	var text string
	for _, p := range pending[0].Parts {
		text += events.RenderForPrompt(p)
	}
	if !strings.Contains(text, "Background task") || !strings.Contains(text, "completed") ||
		!strings.Contains(text, "background answer") {
		t.Fatalf("follow-up message = %q, want the task id, completion and the child's answer", text)
	}
}
