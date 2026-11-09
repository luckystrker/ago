package agent

import (
	stdcontext "context"
	"errors"
	"testing"

	"github.com/luckystrker/ago/config"
	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/knowledge"
	"github.com/luckystrker/ago/middleware"
	"github.com/luckystrker/ago/stream"
	agotest "github.com/luckystrker/ago/testing"
	"github.com/luckystrker/ago/tools"
)

type r5CreateFailsConfig struct{}

func (r5CreateFailsConfig) Provider() config.Provider { return "test" }
func (r5CreateFailsConfig) Model() string             { return "create-fails" }
func (r5CreateFailsConfig) Clone(map[string]any) (config.ModelConfig, error) {
	return r5CreateFailsConfig{}, nil
}
func (r5CreateFailsConfig) Create() (config.LLMClient, error) {
	return nil, errors.New("client creation failed")
}

type r5BadSchemaTool struct{}

func (r5BadSchemaTool) Name() string         { return "bad_schema" }
func (r5BadSchemaTool) DeclaredInCode() bool { return true }
func (r5BadSchemaTool) Schemas(*agocontext.Context) ([]any, error) {
	return nil, errors.New("schema resolution failed")
}
func (r5BadSchemaTool) Register(*agocontext.Context, []middleware.Middleware) ([]stream.SubID, error) {
	return nil, nil
}

type r5BadRegisterTool struct{}

func (r5BadRegisterTool) Name() string         { return "bad_register" }
func (r5BadRegisterTool) DeclaredInCode() bool { return true }
func (r5BadRegisterTool) Schemas(*agocontext.Context) ([]any, error) {
	return []any{tools.Schema{Name: "bad_register", Description: "x", Parameters: map[string]any{"type": "object"}}}, nil
}
func (r5BadRegisterTool) Register(*agocontext.Context, []middleware.Middleware) ([]stream.SubID, error) {
	return nil, errors.New("tool registration failed")
}

type r5FailingStore struct{ *knowledge.MemoryStore }

func (s r5FailingStore) Write(string, []byte) error { return errors.New("knowledge write failed") }

// TestOpenFailureAfterStartedKeepsChildTerminal pins the upstream
// Forwarder.execution finally on every preparation failure inside AgentRun.open
// after TaskStarted: the child stream must end with one started and one
// terminal, and the parent sees the same counts through the relay.
func TestOpenFailureAfterStartedKeepsChildTerminal(t *testing.T) {
	tests := []struct {
		name string
		opts []Option
	}{
		{name: "config create", opts: []Option{WithConfig(r5CreateFailsConfig{})}},
		{name: "prompt hook", opts: []Option{
			WithConfig(agotest.NewTestConfig(textResp("never"))),
			WithPromptHook(func(*agocontext.Context) ([]string, error) {
				return nil, errors.New("prompt hook failed")
			}),
		}},
		{name: "resolve tools", opts: []Option{
			WithConfig(agotest.NewTestConfig(textResp("never"))),
			WithTools(r5BadSchemaTool{}),
		}},
		{name: "register tools", opts: []Option{
			WithConfig(agotest.NewTestConfig(textResp("never"))),
			WithTools(r5BadRegisterTool{}),
		}},
		{name: "knowledge bootstrap", opts: []Option{
			WithConfig(agotest.NewTestConfig(textResp("never"))),
			WithKnowledgeConfig(knowledge.Config{Store: r5FailingStore{MemoryStore: knowledge.NewMemoryStore()}}),
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parent := agocontext.New(stream.NewMemoryStream())
			childStream := stream.NewMemoryStream()
			opts := append([]Option{}, tt.opts...)
			opts = append(opts, WithStreamBase(childStream))
			child := New("child", "", opts...)
			res, err := runTaskOn(stdcontext.Background(), parent, child, "work", "")
			if err != nil {
				t.Fatalf("runTaskOn returned a turn-ending error: %v", err)
			}
			if res.completed || res.err == nil {
				t.Fatalf("result = %+v, want a failed delegation", res)
			}
			assertLifecycle := func(label string, s stream.Stream) {
				t.Helper()
				evs, herr := s.History().GetEvents()
				if herr != nil {
					t.Fatalf("%s history: %v", label, herr)
				}
				var started, failed, completed int
				for _, ev := range evs {
					switch events.AsValue(ev).(type) {
					case events.TaskStarted:
						started++
					case events.TaskFailed:
						failed++
					case events.TaskCompleted:
						completed++
					}
				}
				if started != 1 || failed != 1 || completed != 0 {
					t.Fatalf("%s lifecycle = started %d failed %d completed %d, want 1/1/0", label, started, failed, completed)
				}
			}
			assertLifecycle("parent", parent.Stream)
			assertLifecycle("child", childStream)
		})
	}
}
