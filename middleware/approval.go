package middleware

import (
	stdcontext "context"
	"errors"
	"fmt"
	"strings"
	"sync"

	agocontext "ago/context"
	"ago/events"
)

// approvalPrompt is the question asked through HITL (Python parity:
// middleware/builtin/tools/approval.py).
const approvalPrompt = "Agent wants to call the tool:\n`%s`, %s\nPlease approve or deny this request.\nY/N/Always?\n"

// errDeniedByHuman becomes the ToolErrorEvent returned when the human declines.
var errDeniedByHuman = errors.New("denied by human")

// Approval gates every tool execution behind a human confirmation asked through
// ctx.Ask as an events.ToolApprovalRequest with ToolCallID = the call id.
//
// Answers "y", "yes", "approve" approve the current call. "always" approves it and
// grants all further calls with the same grant key on this middleware instance;
// the key is the call's Source, falling back to its Name when the source is empty
// (Python parity: the grant covers the tool implementation, so a same-named tool
// with a different source is still asked). Any other answer denies the call with a
// ToolErrorEvent "denied by human" and lets the model see the refusal. An error
// from Ask (no HITL hook, timeout, refusal to answer) propagates unchanged.
//
// Grants live on the middleware instance, i.e. for one turn: a Factory builds a
// fresh instance per turn. Safe for concurrent use (the tool executor may run
// calls in parallel).
//
// AGO-DEVIATION: Python stores "always" grants in context.variables
// (conversation-scoped, BYPASS_KEY); the AGo contract scopes them to the
// middleware instance (per turn). AGO-DEVIATION: middleware hooks carry no stdlib
// context, so Ask runs on context.Background() — turn cancellation is not
// propagated into the question. AGO-DEVIATION: Go embedding forces the request
// through ctx.Ask as its embedded *events.HumanInputRequest; the concrete
// events.ToolApprovalRequest value (with ToolCallID) is what the middleware
// constructs and the answer is matched back to the call by ID here.
func Approval() Factory {
	return func() Middleware {
		return &approval{granted: map[string]bool{}}
	}
}

type approval struct {
	Base
	mu      sync.Mutex
	granted map[string]bool
}

func (a *approval) OnToolExecution(next ToolExecFunc, ev *events.ToolCallEvent, ctx *agocontext.Context) (events.Event, error) {
	key := ev.Source
	if key == "" {
		key = ev.Name
	}
	if a.isGranted(key) {
		return next(ev, ctx)
	}

	req := events.ToolApprovalRequest{
		HumanInputRequest: events.NewHumanInputRequest(
			fmt.Sprintf(approvalPrompt, ev.Name, ev.Arguments), 0),
		ToolCallID: ev.ID,
	}
	answer, err := ctx.Ask(stdcontext.Background(), &req.HumanInputRequest)
	if err != nil {
		return nil, err
	}

	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "always":
		a.grant(key)
		return next(ev, ctx)
	case "y", "yes", "approve":
		return next(ev, ctx)
	}
	return events.FromErrorCall(*ev, errDeniedByHuman), nil
}

func (a *approval) isGranted(key string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.granted[key]
}

func (a *approval) grant(key string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.granted[key] = true
}

var _ Middleware = (*approval)(nil)
