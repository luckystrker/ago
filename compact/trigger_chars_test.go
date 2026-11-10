package compact

import (
	"strings"
	"testing"

	"github.com/luckystrker/ago/events"
)

// TestTokenTriggerCharsPerToken pins chars_per_token on the trigger: a history
// under the default estimate exceeds the same budget at one char per token.
func TestTokenTriggerCharsPerToken(t *testing.T) {
	evs := []events.Event{userReq(strings.Repeat("a", 20))} // 5 tokens default, 20 at cpt=1

	if TokenTrigger(10).ShouldTrigger(evs, events.Usage{}) {
		t.Fatal("default estimate (5 tokens) must stay under budget 10")
	}
	if !TokenTrigger(10, WithCharsPerToken(1)).ShouldTrigger(evs, events.Usage{}) {
		t.Fatal("one char per token (20 tokens) must exceed budget 10")
	}
	if TokenTrigger(10, WithCharsPerToken(0)).ShouldTrigger(evs, events.Usage{}) {
		t.Fatal("a non-positive estimate must keep the default")
	}
}
