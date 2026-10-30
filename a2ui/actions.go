package a2ui

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
)

// ActionFunc is a server-side A2UI action handler (actions.py): the function a
// button click runs on the server. It is NOT an agent tool — the agent never
// sees or calls it; params are the click's event.context (or a
// callAgentFunction's args).
type ActionFunc func(ctx context.Context, params map[string]any) (any, error)

// Action is a clickable A2UI button bound to a server-side handler: the
// declaration (described to the LLM so it can render the button) plus the
// handler (run on click without invoking the agent).
type Action struct {
	Name string
	// Description is injected into the system prompt so the LLM knows the
	// action exists.
	Description string
	// ExampleContext is the example event.context shown to the LLM.
	ExampleContext map[string]any
	// Fn runs on click. AGO-DEVIATION: Python derives a placeholder example
	// context from the handler's parameter schema (fast_depends); a Go
	// func signature has no such schema, so ExampleContext must be given
	// explicitly when it should appear in the prompt.
	Fn ActionFunc
}

// ActionRegistry maps action names to the handlers that run on click.
// Safe for concurrent use.
type ActionRegistry struct {
	mu     sync.RWMutex
	byName map[string]Action
}

// NewActionRegistry builds an empty registry.
func NewActionRegistry() *ActionRegistry {
	return &ActionRegistry{byName: map[string]Action{}}
}

// Register adds (or replaces) an action. A click on a name with no registered
// action is rewritten into a generic prompt instead (see ActionToPrompt).
func (r *ActionRegistry) Register(a Action) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.byName[a.Name] = a
}

// Get looks up a registered action by name. A nil registry registers nothing.
func (r *ActionRegistry) Get(name string) (Action, bool) {
	if r == nil {
		return Action{}, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	a, ok := r.byName[name]
	return a, ok
}

// Names lists the registered action names, sorted.
func (r *ActionRegistry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.byName))
	for name := range r.byName {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Actions lists the registered actions sorted by name.
func (r *ActionRegistry) Actions() []Action {
	names := r.Names()
	out := make([]Action, 0, len(names))
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, name := range names {
		out = append(out, r.byName[name])
	}
	return out
}

// server-action error codes for agentFunctionResponse (server_action.py).
const (
	CodeUnknownFunction = "UNKNOWN_FUNCTION" // nothing registered under the name
	CodeInvalidCall     = "INVALID_FUNCTION_CALL"
	CodeFunctionFailed  = "FUNCTION_FAILED"
)

// FunctionResponseMessage builds the agentFunctionResponse answering a
// callAgentFunction: FunctionCallID echoed verbatim, with exactly one of value
// or error (server_action.py _function_response).
func FunctionResponseMessage(functionCallID, version string, value any, err *FunctionError) Message {
	return Message{
		Version: version,
		AgentFunctionResponse: &FunctionResponse{
			FunctionCallID: functionCallID,
			Value:          value,
			Error:          err,
		},
	}
}

// IsMessageEnvelope reports whether value looks like an A2UI server→client
// message envelope (server_action.py _is_message).
func IsMessageEnvelope(value any) bool {
	m, ok := value.(map[string]any)
	if !ok {
		return false
	}
	return len(presentPayloadKeys(m)) > 0
}

// MessagesOf maps a handler return onto the A2UI messages to emit verbatim
// (server_action.py _messages_of): one message object → [it, version-stamped];
// a non-empty list whose every item is a message → each stamped; anything
// else → nil (a click is fire-and-forget, a plain value has no channel).
// Both typed Message values and decoded JSON maps are accepted.
func MessagesOf(result any, version string) []Message {
	switch t := result.(type) {
	case Message:
		return []Message{stamp(t, version)}
	case []Message:
		if len(t) == 0 {
			return nil
		}
		out := make([]Message, len(t))
		for i, m := range t {
			out[i] = stamp(m, version)
		}
		return out
	case map[string]any:
		m, err := messageFromMap(t)
		if err != nil {
			return nil
		}
		return []Message{stamp(m, version)}
	case []any:
		if len(t) == 0 {
			return nil
		}
		out := make([]Message, 0, len(t))
		for _, item := range t {
			m, ok := item.(map[string]any)
			if !ok {
				return nil
			}
			msg, err := messageFromMap(m)
			if err != nil {
				return nil
			}
			out = append(out, msg)
		}
		return out
	default:
		return nil
	}
}

func messageFromMap(m map[string]any) (Message, error) {
	raw, err := json.Marshal(m)
	if err != nil {
		return Message{}, err
	}
	var msg Message
	if err := json.Unmarshal(raw, &msg); err != nil {
		return Message{}, err
	}
	return msg, nil
}

func stamp(m Message, version string) Message {
	if m.Version == "" {
		m.Version = version
	}
	return m
}

// InvalidArgumentsResponse builds the INVALID_FUNCTION_CALL answer for a
// callAgentFunction whose arguments do not fit (server_action.py
// _invalid_function_arguments).
func InvalidArgumentsResponse(call *IncomingFunctionCall, version string) Message {
	return FunctionResponseMessage(call.FunctionCallID, version, nil,
		&FunctionError{Code: CodeInvalidCall,
			Message: fmt.Sprintf("Invalid arguments for function '%s'.", call.Call)})
}
