package agotesting

import (
	"errors"
	"fmt"

	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
)

// Turn is one scripted LLM turn: what the model double does on one call (Python
// parity: ag2.testing.Turn). The vocabulary comes from the constructors below:
//
//   - Text — the model replies with that text.
//   - Reply — the response, spelled out in full.
//   - ToolCall — the model asks for those tools; the agent runs them.
//   - Fail — the call fails with that error, the way a provider client would.
//   - Publish — the event reaches the stream *during* the turn (a streamed chunk,
//     reasoning, server-side activity) and the script is read on for whatever
//     ends the call.
//
// A script mixes them freely, so one turn can stream and then fail:
//
//	agotest.NewTestScript(agotest.Publish(&events.ModelMessageChunk{...}), agotest.Fail(err))
//
// The interface is closed: the unexported method keeps the vocabulary exactly
// those five shapes, so a test double cannot drift from the Python contract.
type Turn interface {
	// turn performs the scripted turn. done=false means it only published events
	// and the script must be read on for the turn that ends the call.
	turn(conv *agocontext.Context) (resp *events.ModelResponse, done bool, err error)
}

// Text replies with the given text (Python parity: a plain str Turn). The
// response carries the message; the agent publishes it, so a scripted text turn
// is one transcript entry, not two.
//
// AGO-DEVIATION: Python's TestClient also sends the ModelMessage to the stream
// before returning; AGo's agent publishes the returned ModelResponse itself, so
// sending the message here would duplicate it.
func Text(text string) Turn { return textTurn{text: text} }

type textTurn struct{ text string }

func (t textTurn) turn(*agocontext.Context) (*events.ModelResponse, bool, error) {
	return &events.ModelResponse{Message: &events.ModelMessage{Content: t.text}}, true, nil
}

// Reply scripts one complete response (Python parity: a ModelResponse Turn).
func Reply(resp *events.ModelResponse) Turn { return replyTurn{resp: resp} }

type replyTurn struct{ resp *events.ModelResponse }

func (t replyTurn) turn(*agocontext.Context) (*events.ModelResponse, bool, error) {
	return t.resp, true, nil
}

// ToolCall scripts the model asking for those tools (Python parity: a
// ToolCallEvent or an iterable of them). The calls are copied, so a caller's
// slice is not shared with the script.
func ToolCall(calls ...events.ToolCallEvent) Turn {
	return toolCallTurn{calls: append([]events.ToolCallEvent(nil), calls...)}
}

type toolCallTurn struct{ calls []events.ToolCallEvent }

func (t toolCallTurn) turn(*agocontext.Context) (*events.ModelResponse, bool, error) {
	calls := append([]events.ToolCallEvent(nil), t.calls...)
	return &events.ModelResponse{ToolCalls: events.ToolCallsEvent{Calls: calls}}, true, nil
}

// Fail scripts a failing model call (Python parity: a BaseException Turn). The
// agent surfaces the error to its caller as a provider failure would.
func Fail(err error) Turn {
	if err == nil {
		err = errors.New("agotesting: scripted failure")
	}
	return failTurn{err: err}
}

type failTurn struct{ err error }

func (t failTurn) turn(*agocontext.Context) (*events.ModelResponse, bool, error) {
	return nil, false, t.err
}

// Publish sends events to the conversation stream during the turn and reads the
// script on for whatever ends the call (Python parity: any other BaseEvent Turn
// — streamed chunks, reasoning, server-side tool activity). A script that
// publishes and then runs out of turns ends with ErrScriptedExhausted, exactly
// as a model that published and never answered would.
func Publish(evs ...events.Event) Turn {
	return publishTurn{evs: append([]events.Event(nil), evs...)}
}

type publishTurn struct{ evs []events.Event }

// ErrNoConversation is returned by Call when the script reaches a publishing
// turn but the call carried no conversation context to publish on.
var ErrNoConversation = errors.New("agotesting: a publishing turn needs a conversation context")

func (t publishTurn) turn(conv *agocontext.Context) (*events.ModelResponse, bool, error) {
	if conv == nil {
		return nil, false, ErrNoConversation
	}
	for _, ev := range t.evs {
		if ev == nil {
			continue
		}
		if err := conv.Send(ev); err != nil {
			return nil, false, fmt.Errorf("agotesting: publish %s: %w", ev.EventType(), err)
		}
	}
	return nil, false, nil
}

// ErrToolError is wrapped by the error Call returns when RaiseToolErrors is on
// and the history handed to the call carries a tool failure. Match it with
// errors.Is.
var ErrToolError = errors.New("agotesting: tool error in history")

// toolErrorInHistory re-raises the first tool failure in the history, the way
// Python's TestClient raises the exception a ToolErrorEvent carries.
//
// A real agent turn does put these events in the history: a call no tool owns is
// answered with a ToolNotFoundEvent, and a tool that fails (including a panic or
// a timeout) sends a ToolErrorEvent on the stream before the executor settles the
// batch. The check therefore fires for agent-run failures too, not only for
// events a test injected.
//
// AGO-DEVIATION: the Go ToolErrorEvent carries the message text, not an error
// value, so the original exception cannot be re-raised — the error wraps
// ErrToolError and names the tool instead.
func toolErrorInHistory(evs []events.Event) error {
	for _, ev := range evs {
		switch e := events.AsValue(ev).(type) {
		case events.ToolNotFoundEvent:
			return fmt.Errorf("%w: tool %q not found: %s", ErrToolError, e.Name, e.Error)
		case events.ToolErrorEvent:
			return fmt.Errorf("%w: tool %q: %s", ErrToolError, e.Name, e.Error)
		}
	}
	return nil
}
