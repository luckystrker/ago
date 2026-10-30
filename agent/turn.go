package agent

import (
	"errors"
	"fmt"

	"github.com/luckystrker/ago/config"
	"github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/hitl"
	"github.com/luckystrker/ago/stream"
	"github.com/luckystrker/ago/usage"
)

// executeTurn drives the turn on its goroutine: runs the OnTurn-wrapped loop,
// records the reply or the failure, and closes the done channel.
func (r *AgentRun) executeTurn() {
	defer close(r.done)
	defer func() {
		if rec := recover(); rec != nil {
			r.err = fmt.Errorf("agent: turn panicked: %v", rec)
		}
	}()

	resp, err := r.runTurn()
	if err != nil {
		r.err = err
		// A failed HITL prompt leaves model-requested tool calls unanswered;
		// repair the transcript so the next model call is valid (Python parity:
		// close_unanswered_tool_calls on HumanInputError).
		if isHitlFailure(err) {
			if _, rerr := r.stream.History().CloseUnanswered(); rerr != nil {
				r.err = errors.Join(err, fmt.Errorf("agent: repair history: %w", rerr))
			}
		}
		// The event log persists on failed turns too (Python parity: the
		// post-yield write in _KnowledgeContext.enter runs on the way out of the
		// scope, whatever the turn's outcome).
		r.persistEventLog()
		return
	}
	r.reply = &AgentReply{agent: r.agent, resp: resp, cx: r.cx, schema: r.schema}
	r.persistEventLog()
}

// isHitlFailure reports whether err is (or wraps) a hitl refusal, timeout or failure.
func isHitlFailure(err error) bool {
	var np *hitl.NotProvidedError
	var te *hitl.TimeoutError
	var fe *hitl.FailedError
	return errors.As(err, &np) || errors.As(err, &te) || errors.As(err, &fe)
}

// runTurn executes the OnTurn-wrapped turn body (Python parity: _execute_turn).
func (r *AgentRun) runTurn() (*events.ModelResponse, error) {
	// The turn body consumes the inbox: everything pending becomes the initial
	// ModelRequest (Python parity: the initial drain in _execute_turn).
	drained := r.stream.Drain()
	initial := mergeRequests(drained)

	inner := func(ev *events.ModelRequest, cx *context.Context) (*events.ModelResponse, error) {
		v, err := r.turnBody(ev, cx)
		if err != nil {
			return nil, err
		}
		return &v, nil
	}
	call := inner
	for i := len(r.instances) - 1; i >= 0; i-- {
		mw, next := r.instances[i], call
		call = func(ev *events.ModelRequest, cx *context.Context) (*events.ModelResponse, error) {
			return mw.OnTurn(next, ev, cx)
		}
	}
	return call(initial, r.cx)
}

// turnBody is the innermost turn: send the initial request (triggering the LLM),
// loop over tool-call rounds, then flush leftover enqueued input into one final
// model call (Python parity: _execute_turn).
func (r *AgentRun) turnBody(initial *events.ModelRequest, _ *context.Context) (events.ModelResponse, error) {
	seen := r.scanStart
	if !r.isReentry() {
		n, err := r.historyLen()
		if err != nil {
			return events.ModelResponse{}, err
		}
		seen = n
		if err := r.stream.Send(initial); err != nil {
			return events.ModelResponse{}, err
		}
	}
	resp, seen, err := r.waitResponse(seen)
	if err != nil {
		return events.ModelResponse{}, err
	}

	// Tool-call loop: ends when the model stops requesting tools or a final=true
	// tool forced the response (Python parity: while tool_calls and not response_force).
	for len(resp.ToolCalls.Calls) > 0 && !resp.ResponseForce {
		tce := resp.ToolCalls
		// The executor runs inline inside this Send: tool goroutines emit
		// ToolResultsEvent, whose subscriber is the LLM trigger; the next
		// ModelResponse is in the transcript by the time Send returns.
		if err := r.stream.Send(&tce); err != nil {
			return events.ModelResponse{}, err
		}
		resp, seen, err = r.waitResponse(seen)
		if err != nil {
			return events.ModelResponse{}, err
		}
	}

	// Leftover mid-turn input gets one extra model call (Python parity: the
	// final drain in _execute_turn).
	if leftover := r.stream.Drain(); len(leftover) > 0 {
		if err := r.stream.Send(mergeRequests(leftover)); err != nil {
			return events.ModelResponse{}, err
		}
		resp, _, err = r.waitResponse(seen)
		if err != nil {
			return events.ModelResponse{}, err
		}
	}
	return resp, nil
}

// isReentry reports a Resume whose trigger was already sent at scope open.
func (r *AgentRun) isReentry() bool { return r.reentry != nil }

// llmTrigger is the LLM subscriber: drains late inbox input, guards the halt, and
// performs one middleware-wrapped model call, emitting UsageEvent then the durable
// ModelResponse (Python parity: _call_client).
func (r *AgentRun) llmTrigger(ev events.Event) error {
	if err := r.turnCtx.Err(); err != nil {
		return err // cancellation parity: a cancelled ask stops before the model call
	}
	// Merge any input that arrived after the triggering event into the transcript
	// as a DrainedModelRequest (the trigger condition skips it — no recursion).
	if pending := r.stream.Drain(); len(pending) > 0 {
		dm := &events.DrainedModelRequest{ModelRequest: *mergeRequests(pending)}
		if err := r.stream.Send(dm); err != nil {
			return err
		}
	}

	r.haltMu.Lock()
	halted, reason := r.halted, r.haltReason
	r.haltMu.Unlock()
	if halted {
		// Python parity: _HaltCheckMiddleware's synthetic "HALTED: <reason>".
		return r.stream.Send(&events.ModelResponse{
			Message: &events.ModelMessage{Content: "HALTED: " + reason},
		})
	}

	evs, err := r.stream.History().GetEvents()
	if err != nil {
		return fmt.Errorf("agent: read history: %w", err)
	}

	call := func(evs []events.Event, cx *context.Context) (*events.ModelResponse, error) {
		opts := config.CallOptions{Tools: r.resolved.Schemas, ResponseSchema: r.schema}
		return r.llm.Call(r.turnCtx, evs, cx, opts)
	}
	for i := len(r.instances) - 1; i >= 0; i-- {
		mw, next := r.instances[i], call
		call = func(evs []events.Event, cx *context.Context) (*events.ModelResponse, error) {
			return mw.OnLLMCall(next, evs, cx)
		}
	}
	resp, err := call(evs, r.cx)
	if err != nil {
		return err
	}

	// Token accounting is event-sourced: one UsageEvent per model call, before
	// the durable response (Python parity: UsageEvent then ModelResponse).
	if usage.Reported(resp.Usage) {
		ue := &events.UsageEvent{
			Usage:        resp.Usage,
			Kind:         events.UsageKindModelCall,
			Model:        resp.Model,
			Provider:     resp.Provider,
			FinishReason: resp.FinishReason,
		}
		if err := r.cx.Send(ue); err != nil {
			return err
		}
	}
	return r.stream.Send(resp)
}

// waitResponse scans the transcript from index seen for the next ModelResponse,
// waiting on the stream when none has arrived yet. The subscription is taken
// before the first scan, so an event that lands between a scan and the wait is
// always seen either in the transcript or as a wake-up.
func (r *AgentRun) waitResponse(seen int) (events.ModelResponse, int, error) {
	sub := r.stream.Subscribe(func(events.Event) error {
		select {
		case r.wake <- struct{}{}:
		default:
		}
		return nil
	}, stream.SubscribeOptions{})
	defer r.stream.Unsubscribe(sub)

	for {
		evs, err := r.stream.History().GetEvents()
		if err != nil {
			return events.ModelResponse{}, seen, err
		}
		for i := seen; i < len(evs); i++ {
			if mr, ok := events.AsValue(evs[i]).(events.ModelResponse); ok {
				return mr, i + 1, nil
			}
		}
		seen = len(evs)
		select {
		case <-r.wake:
		case <-r.turnCtx.Done():
			return events.ModelResponse{}, seen, r.turnCtx.Err()
		}
	}
}

func (r *AgentRun) historyLen() (int, error) {
	evs, err := r.stream.History().GetEvents()
	if err != nil {
		return 0, err
	}
	return len(evs), nil
}

// mergeRequests folds pending inbox requests into one ModelRequest.
func mergeRequests(reqs []*events.ModelRequest) *events.ModelRequest {
	merged := &events.ModelRequest{}
	for _, req := range reqs {
		merged.Parts = append(merged.Parts, req.Parts...)
		for k, v := range req.Metadata {
			if merged.Metadata == nil {
				merged.Metadata = map[string]any{}
			}
			merged.Metadata[k] = v
		}
	}
	return merged
}
