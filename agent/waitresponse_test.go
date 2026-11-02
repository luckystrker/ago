package agent

import (
	stdcontext "context"
	"errors"
	"testing"

	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/stream"
)

// TestWaitResponseCancellation pins the scan-and-wait contract of the transcript
// scan: when no new ModelResponse has arrived, the scan ends through turnCtx
// cancellation (the loop bound must stay exclusive — the transcript is read by
// index, and an inclusive bound reads one past its end).
func TestWaitResponseCancellation(t *testing.T) {
	ms := stream.NewMemoryStream()
	if err := ms.Send(&events.ModelRequest{}); err != nil {
		t.Fatalf("seed history: %v", err)
	}
	ctx, cancel := stdcontext.WithCancel(stdcontext.Background())
	cancel()
	r := &AgentRun{stream: ms, wake: make(chan struct{}, 1), turnCtx: ctx}
	_, _, err := r.waitResponse(0)
	if !errors.Is(err, stdcontext.Canceled) {
		t.Fatalf("waitResponse under a cancelled turn context: want context.Canceled, got %v", err)
	}
}
