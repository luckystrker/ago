package middleware

import (
	"strings"
	"testing"

	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
)

// TestTokenLimiterCountsStatusObservations pins the budget contract: a retained
// status observation is not conversational, but its projected view rides every
// model call, so TokenLimiter must spend the budget on its parts — the oldest
// request is dropped in favor of the status, and would be kept if the status
// weighed nothing.
func TestTokenLimiterCountsStatusObservations(t *testing.T) {
	reqA := &events.ModelRequest{Parts: []events.Input{&events.TextInput{Content: "aaaa"}}, Origin: events.OriginUser}
	status := &events.StatusObservation{Parts: []events.Input{&events.TextInput{Content: strings.Repeat("x", 400)}}} // 100 tokens
	reqB := &events.ModelRequest{Parts: []events.Input{&events.TextInput{Content: "bbbb"}}, Origin: events.OriginUser}
	evs := []events.Event{reqA, status, reqB} // tokens: 1 + 100 + 1

	m := TokenLimiter(101)()
	var saw []events.Event
	next := func(evs []events.Event, _ *agocontext.Context) (*events.ModelResponse, error) {
		saw = evs
		return &events.ModelResponse{}, nil
	}
	if _, err := m.OnLLMCall(next, evs, newTestCtx(t)); err != nil {
		t.Fatalf("OnLLMCall: %v", err)
	}

	if len(saw) != 2 {
		t.Fatalf("kept %d events, want 2 (the status pushed reqA out)", len(saw))
	}
	if _, ok := saw[0].(*events.StatusObservation); !ok {
		t.Fatalf("kept event 0 = %T, want the status observation", saw[0])
	}
	if req, ok := saw[1].(*events.ModelRequest); !ok || req.Parts[0].(*events.TextInput).Content != "bbbb" {
		t.Fatalf("kept event 1 = %v, want reqB", saw[1])
	}
}
