package agotesting

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/luckystrker/ago/agent"
	"github.com/luckystrker/ago/config"
	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/stream"
	"github.com/luckystrker/ago/tools"
)

// turnClient builds a client over the config and returns it with a conversation
// context to publish on.
func turnClient(t *testing.T, cfg *TestConfig) (config.LLMClient, *agocontext.Context) {
	t.Helper()
	cl, err := cfg.Create()
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	s := stream.NewMemoryStream()
	conv := agocontext.New(s)
	return cl, conv
}

// TestTextTurn pins the plain-string turn: one response carrying the text.
func TestTextTurn(t *testing.T) {
	cl, conv := turnClient(t, NewTestScript(Text("hello")))
	resp, err := cl.Call(context.Background(), hist("hi"), conv, config.CallOptions{})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if resp == nil || resp.Message == nil || resp.Message.Content != "hello" {
		t.Fatalf("response = %+v, want the scripted text", resp)
	}
	if len(resp.ToolCalls.Calls) != 0 {
		t.Fatalf("a text turn must not request tools: %+v", resp.ToolCalls)
	}
}

// TestToolCallTurn pins the tool-call shortcut.
func TestToolCallTurn(t *testing.T) {
	calls := []events.ToolCallEvent{
		{ID: "c1", Name: "lookup", Arguments: `{"q":"x"}`},
		{ID: "c2", Name: "record", Arguments: `{"v":1}`},
	}
	cl, conv := turnClient(t, NewTestScript(ToolCall(calls...)))
	resp, err := cl.Call(context.Background(), hist("hi"), conv, config.CallOptions{})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !reflect.DeepEqual(resp.ToolCalls.Calls, calls) {
		t.Fatalf("calls = %+v, want %+v", resp.ToolCalls.Calls, calls)
	}
	// The script owns its copy: mutating the caller's slice must not change it.
	calls[0].Name = "mutated"
	if resp.ToolCalls.Calls[0].Name != "lookup" {
		t.Fatal("the script shares the caller's slice")
	}
}

// TestFailTurn pins the raised-exception turn: the call fails with that error.
func TestFailTurn(t *testing.T) {
	boom := errors.New("provider dropped")
	cl, conv := turnClient(t, NewTestScript(Fail(boom)))
	_, err := cl.Call(context.Background(), hist("hi"), conv, config.CallOptions{})
	if !errors.Is(err, boom) {
		t.Fatalf("Call error = %v, want the scripted failure", err)
	}
	// The failed turn still consumes one script slot, so the next call is a
	// different turn.
	if _, err := cl.Call(context.Background(), hist("hi"), conv, config.CallOptions{}); !errors.Is(err, ErrScriptedExhausted) {
		t.Fatalf("second Call error = %v, want ErrScriptedExhausted", err)
	}
}

// TestPublishTurnPublishesBeforeTheAnswer pins the mid-turn event vocabulary:
// a published chunk reaches the stream and does not consume the turn, so the
// script is read on for the text that answers.
func TestPublishTurnPublishesBeforeTheAnswer(t *testing.T) {
	chunk := &events.ModelMessageChunk{Content: "Tok"}
	cl, conv := turnClient(t, NewTestScript(Publish(chunk, &events.ModelReasoning{Content: "thinking"}), Text("done")))

	var seen []string
	conv.Stream.Subscribe(func(ev events.Event) error {
		seen = append(seen, ev.EventType())
		return nil
	}, stream.SubscribeOptions{})

	resp, err := cl.Call(context.Background(), hist("hi"), conv, config.CallOptions{})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if resp.Message == nil || resp.Message.Content != "done" {
		t.Fatalf("response = %+v, want the text after the published events", resp)
	}
	want := []string{"ModelMessageChunk", "ModelReasoning"}
	if !reflect.DeepEqual(seen, want) {
		t.Fatalf("stream saw %v, want %v", seen, want)
	}
}

// TestPublishThenFail pins the case the audit named: one turn streams a chunk
// and then fails.
func TestPublishThenFail(t *testing.T) {
	boom := errors.New("connection reset")
	cl, conv := turnClient(t, NewTestScript(Publish(&events.ModelMessageChunk{Content: "Tok"}), Fail(boom)))
	var chunkSeen bool
	conv.Stream.Subscribe(func(ev events.Event) error {
		if c, ok := events.AsValue(ev).(events.ModelMessageChunk); ok && c.Content == "Tok" {
			chunkSeen = true
		}
		return nil
	}, stream.SubscribeOptions{Condition: events.TypeOf[events.ModelMessageChunk]()})

	_, err := cl.Call(context.Background(), hist("hi"), conv, config.CallOptions{})
	if !errors.Is(err, boom) {
		t.Fatalf("Call error = %v, want the failure after the chunk", err)
	}
	if !chunkSeen {
		t.Fatal("the chunk was not published before the failure")
	}
}

// TestPublishNeedsConversation pins the error path for a publishing turn with no
// stream to publish on.
func TestPublishNeedsConversation(t *testing.T) {
	cl, _ := turnClient(t, NewTestScript(Publish(&events.ModelMessageChunk{Content: "x"}), Text("after")))
	_, err := cl.Call(context.Background(), hist("hi"), nil, config.CallOptions{})
	if !errors.Is(err, ErrNoConversation) {
		t.Fatalf("Call error = %v, want ErrNoConversation", err)
	}
}

// TestScriptedTurnIntegration drives the vocabulary through a real agent ask:
// a tool round scripted with ToolCall, then a text answer.
func TestScriptedTurnIntegration(t *testing.T) {
	echo, err := tools.NewFunctionTool("echo", "echo",
		func(args struct {
			X string `json:"x"`
		}) (string, error) {
			return args.X, nil
		})
	if err != nil {
		t.Fatalf("tool: %v", err)
	}
	cfg := NewTestScript(ToolCall(events.ToolCallEvent{ID: "c1", Name: "echo", Arguments: `{"x":"1"}`}), Text("finished"))
	a := agent.New("main", "", agent.WithConfig(cfg), agent.WithTools(echo))
	reply, err := a.Ask(context.Background(), "go")
	if err != nil {
		t.Fatalf("ask: %v", err)
	}
	if reply.Body() != "finished" {
		t.Fatalf("body = %q, want finished", reply.Body())
	}
}

// TestRaiseToolErrors pins both modes of the history inspection: on, a tool
// failure in the history fails the call; off (the default), the double models a
// real provider, which is handed the failure as an ordinary result and carries
// on.
func TestRaiseToolErrors(t *testing.T) {
	toolErr := events.ToolErrorEvent{
		ToolResultEvent: events.FromResultCall(events.ToolCallEvent{ID: "c1", Name: "shell"}, events.NewToolResult(false, "boom")),
		Error:           "exit status 1",
	}
	history := append(hist("go"), toolErr)

	t.Run("off by default", func(t *testing.T) {
		cl, conv := turnClient(t, NewTestScript(Text("recovered")))
		resp, err := cl.Call(context.Background(), history, conv, config.CallOptions{})
		if err != nil {
			t.Fatalf("Call: %v", err)
		}
		if resp.Message.Content != "recovered" {
			t.Fatalf("body = %q, want recovered", resp.Message.Content)
		}
	})

	t.Run("on", func(t *testing.T) {
		cfg := NewTestScript(Text("unreachable"))
		cfg.SetRaiseToolErrors(true)
		cl, conv := turnClient(t, cfg)
		_, err := cl.Call(context.Background(), history, conv, config.CallOptions{})
		if !errors.Is(err, ErrToolError) {
			t.Fatalf("Call error = %v, want ErrToolError", err)
		}
	})

	t.Run("a not-found event counts too", func(t *testing.T) {
		notFound := events.ToolNotFoundEvent{ToolErrorEvent: events.FromErrorCall(
			events.ToolCallEvent{ID: "c2", Name: "ghost"}, errors.New("no such tool"))}
		cfg := NewTestScript(Text("unreachable"))
		cfg.SetRaiseToolErrors(true)
		cl, conv := turnClient(t, cfg)
		_, err := cl.Call(context.Background(), append(hist("go"), notFound), conv, config.CallOptions{})
		if !errors.Is(err, ErrToolError) {
			t.Fatalf("Call error = %v, want ErrToolError", err)
		}
	})

	t.Run("a clone keeps the setting", func(t *testing.T) {
		cfg := NewTestScript(Text("unreachable"))
		cfg.SetRaiseToolErrors(true)
		clone, err := cfg.Clone(nil)
		if err != nil {
			t.Fatalf("Clone: %v", err)
		}
		cl, err := clone.Create()
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if _, err := cl.Call(context.Background(), history, agocontext.New(stream.NewMemoryStream()), config.CallOptions{}); !errors.Is(err, ErrToolError) {
			t.Fatalf("clone Call error = %v, want the setting to survive Clone", err)
		}
	})
}

// TestRaiseToolErrorsSeesRealToolFailure pins that the check is not limited to
// injected events: a tool that fails during a real agent turn sends a
// ToolErrorEvent, so the next model call fails with ErrToolError.
func TestRaiseToolErrorsSeesRealToolFailure(t *testing.T) {
	boom := errors.New("disk full")
	failing, err := tools.NewFunctionTool("store", "always fails",
		func(args struct {
			V string `json:"v"`
		}) (string, error) {
			return "", boom
		})
	if err != nil {
		t.Fatalf("tool: %v", err)
	}
	cfg := NewTestScript(
		ToolCall(events.ToolCallEvent{ID: "c1", Name: "store", Arguments: `{"v":"x"}`}),
		Text("unreachable"),
	)
	cfg.SetRaiseToolErrors(true)
	a := agent.New("main", "", agent.WithConfig(cfg), agent.WithTools(failing))
	_, err = a.Ask(context.Background(), "store it")
	if !errors.Is(err, ErrToolError) {
		t.Fatalf("ask error = %v, want ErrToolError from the failed tool call", err)
	}
	if !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("error = %v, want the tool's own message", err)
	}
}
