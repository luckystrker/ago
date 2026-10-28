package tools

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	agocontext "ago/context"
	"ago/events"
	"ago/middleware"
	"ago/stream"
)

// Executor drives tool calls on a conversation stream (Python parity:
// ag2/tools/executor.py). Per turn, Register subscribes the batch handler on
// ToolCallsEvent, registers every tool, and installs the not-found fallback. Each
// call is executed on its own goroutine; a hitl failure abandons the batch and
// propagates (it is never recorded as a tool result). An Executor is stateless and
// safe for concurrent use.
type Executor struct{}

// NewExecutor creates an executor.
func NewExecutor() *Executor { return &Executor{} }

// Register wires the executor for one turn: the batch handler for ToolCallsEvent,
// then each tool's own subscription (with the agent's middleware instances), then the
// fallback that answers unknown names with ToolNotFoundEvent.
func (e *Executor) Register(cx *agocontext.Context, ts []Tool, known map[string]bool, mws []middleware.Middleware) error {
	s := cx.Stream
	s.Subscribe(e.batchHandler(cx), stream.SubscribeOptions{Condition: events.TypeOf[events.ToolCallsEvent]()})
	for _, t := range ts {
		if err := t.Register(cx, mws); err != nil {
			return fmt.Errorf("tools: register tool %s: %w", t.Name(), err)
		}
	}
	s.Subscribe(notFoundHandler(cx, known), stream.SubscribeOptions{Condition: events.TypeOf[events.ToolCallEvent]()})
	return nil
}

func (e *Executor) batchHandler(cx *agocontext.Context) stream.Subscriber {
	return func(ev events.Event) error {
		tce, ok := events.AsValue(ev).(events.ToolCallsEvent)
		if !ok {
			return nil
		}
		return e.executeTools(cx, tce)
	}
}

// notFoundHandler answers calls whose name no registered tool owns (Python parity:
// _tool_not_found). Known names — registered function names and hosted tool types —
// are skipped: hosted calls are settled by the provider, not on this stream.
func notFoundHandler(cx *agocontext.Context, known map[string]bool) stream.Subscriber {
	return func(ev events.Event) error {
		call, ok := events.AsValue(ev).(events.ToolCallEvent)
		if !ok {
			return nil
		}
		if known[call.Name] {
			return nil
		}
		nf := events.ToolNotFoundEvent{ToolErrorEvent: events.FromErrorCall(call, notFoundError(call.Name))}
		return cx.Send(nf)
	}
}

// resultCondition matches the events that can settle one call: its result, its
// error, or the client hand-off.
func resultCondition(callID string) events.Condition {
	return func(ev events.Event) bool {
		switch e := events.AsValue(ev).(type) {
		case events.ToolResultEvent:
			return e.ParentID == callID
		case events.ToolErrorEvent:
			return e.ParentID == callID
		case events.ToolNotFoundEvent:
			return e.ParentID == callID
		case events.ClientToolCallEvent:
			return e.ID == callID
		}
		return false
	}
}

type callOutcome struct {
	idx int
	ev  events.Event
	err error
}

// errAbandoned marks an outcome discarded because the batch failed; it never leaves
// the executor.
var errAbandoned = errors.New("tools: call abandoned")

// executeTools runs the batch in parallel (Python parity: one task per call with
// asyncio.gather) and settles it: client hand-offs replay the approved calls as a
// forced ModelResponse, a final=true result ends the turn with a forced
// ModelResponse, everything else gathers into ToolResultsEvent.
func (e *Executor) executeTools(cx *agocontext.Context, tce events.ToolCallsEvent) error {
	abandoned := make(chan struct{})
	och := make(chan callOutcome, len(tce.Calls))
	var wg sync.WaitGroup
	for i, call := range tce.Calls {
		wg.Add(1)
		go func(i int, call events.ToolCallEvent) {
			defer wg.Done()
			ev, err := e.executeCall(cx, call, abandoned)
			och <- callOutcome{idx: i, ev: ev, err: err}
		}(i, call)
	}

	outcomes := make([]*callOutcome, len(tce.Calls))
	for range tce.Calls {
		o := <-och
		if o.err != nil {
			if isHitlError(o.err) {
				// Siblings stop being owed a result. Their goroutines cannot be
				// interrupted; the seam below lets them finish while everything
				// they produced is discarded (Python parity: task.cancel()).
				close(abandoned)
				wg.Wait()
				return o.err
			}
			return o.err
		}
		outcomes[o.idx] = &o
	}

	var (
		results     []events.ToolResultEvent
		clientCalls = map[string]events.ClientToolCallEvent{}
	)
	for _, o := range outcomes {
		switch res := events.AsValue(o.ev).(type) {
		case events.ClientToolCallEvent:
			clientCalls[res.ID] = res
		case events.ToolNotFoundEvent:
			// AGO-DEVIATION: the batch carries the embedded ToolResultEvent —
			// Go's []ToolResultEvent cannot hold the subtype's Error field; the
			// error text travels in the result parts (set by FromErrorCall).
			results = append(results, res.ToolResultEvent)
		case events.ToolErrorEvent:
			results = append(results, res.ToolResultEvent)
		case events.ToolResultEvent:
			if res.Result.Final {
				content, err := finalContent(res.Result)
				if err != nil {
					return err
				}
				return cx.Send(events.ModelResponse{
					Message:       &events.ModelMessage{Content: content, Metadata: res.Result.Metadata},
					ResponseForce: true,
				})
			}
			results = append(results, res)
		default:
			return fmt.Errorf("tools: unexpected outcome %T for call %s", o.ev, tce.Calls[o.idx].Name)
		}
	}

	if len(clientCalls) > 0 {
		// Preserve the provider fields of the model's calls while dispatching the
		// names and arguments the middleware approved (Python parity:
		// _client_call_for_response).
		calls := make([]events.ToolCallEvent, 0, len(clientCalls))
		for _, call := range tce.Calls {
			if out, ok := clientCalls[call.ID]; ok {
				preserved := call
				preserved.Name = out.Name
				preserved.Arguments = out.Arguments
				calls = append(calls, preserved)
			}
		}
		return cx.Send(events.ModelResponse{
			ToolCalls:     events.ToolCallsEvent{Calls: calls},
			ResponseForce: true,
		})
	}
	return cx.Send(events.ToolResultsEvent{Results: results})
}

// finalContent renders the single part of a final=true result as the forced answer
// text (Python parity: TextInput → content, DataInput → JSON).
func finalContent(r events.ToolResult) (string, error) {
	if len(r.Parts) != 1 {
		return "", errors.New("tools: ToolResult with final=true must have exactly one part")
	}
	switch part := events.AsValue(r.Parts[0]).(type) {
	case events.TextInput:
		return part.Content, nil
	case events.DataInput:
		b, err := json.Marshal(part.Data)
		if err != nil {
			return "", fmt.Errorf("tools: final DataInput part: %w", err)
		}
		return string(b), nil
	default:
		return "", fmt.Errorf("tools: unsupported final part type %T", r.Parts[0])
	}
}

// executeCall runs one call: it waits for the call's settlement while sending the
// call event, so the tool's subscriber can answer during the dispatch (Python
// parity: stream.get opened before the send).
func (e *Executor) executeCall(cx *agocontext.Context, call events.ToolCallEvent, abandoned <-chan struct{}) (events.Event, error) {
	s := cx.Stream
	ch := make(chan events.Event, 4)
	sub := s.Subscribe(func(ev events.Event) error {
		ch <- ev
		return nil
	}, stream.SubscribeOptions{Condition: resultCondition(call.ID)})
	defer s.Unsubscribe(sub)

	if err := s.Send(call); err != nil {
		// The error comes from the tool's own subscriber: a hitl failure ends the
		// turn, any other failure is recorded as this call's error result.
		if isHitlError(err) {
			return nil, err
		}
		return events.FromErrorCall(call, err), nil
	}
	select {
	case ev := <-ch:
		return ev, nil
	case <-abandoned:
		return nil, errAbandoned
	}
}
