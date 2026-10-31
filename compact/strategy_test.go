package compact

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/luckystrker/ago/config"
	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
)

// hungClient stands in for a provider that never answers: it reports that the
// call started, then waits for the call context to end — the observable
// behavior of a stuck provider whose transport still honors cancellation.
type hungClient struct {
	started chan struct{}
}

func newHungClient() *hungClient { return &hungClient{started: make(chan struct{})} }

func (h *hungClient) Call(ctx context.Context, _ []events.Event, _ *agocontext.Context, _ config.CallOptions) (*events.ModelResponse, error) {
	close(h.started)
	<-ctx.Done()
	return nil, ctx.Err()
}

// probeClient records how the call context was set up and answers immediately.
type probeClient struct {
	called   bool
	deadline time.Time
	bounded  bool
}

func (c *probeClient) Call(ctx context.Context, _ []events.Event, _ *agocontext.Context, _ config.CallOptions) (*events.ModelResponse, error) {
	c.called = true
	c.deadline, c.bounded = ctx.Deadline()
	return &events.ModelResponse{Message: &events.ModelMessage{Content: "s"}}, nil
}

// A hung summarizer must not hang Summarize — and through the post-turn hook,
// the whole turn — forever: the configured Timeout bounds the call and the
// deadline error is returned wrapped.
func TestLLMStrategyTimeoutBoundsHungClient(t *testing.T) {
	ctx, _ := newTestCtx(t)
	client := newHungClient()
	s := LLMStrategy{Client: client, Timeout: 50 * time.Millisecond}

	done := make(chan error, 1)
	go func() {
		_, err := s.Summarize(ctx, []events.Event{userReq("hello")})
		done <- err
	}()

	select {
	case <-client.started:
	case <-time.After(5 * time.Second):
		t.Fatal("summarizer call never started")
	}

	// The 5-second ceiling also catches the deadline silently falling back to
	// DefaultTimeout (120s) instead of the configured 50ms.
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("hung client must surface an error")
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v, want context.DeadlineExceeded", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Summarize hung: Timeout did not bound the call")
	}
}

// Zero Timeout must not mean "no deadline": the call runs under the package
// default.
func TestLLMStrategyCallGetsDefaultTimeout(t *testing.T) {
	ctx, _ := newTestCtx(t)
	c := &probeClient{}
	if _, err := (LLMStrategy{Client: c}).Summarize(ctx, []events.Event{userReq("hello")}); err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if !c.called {
		t.Fatal("client was not called")
	}
	if !c.bounded {
		t.Fatal("call ran without a deadline, want DefaultTimeout")
	}
	// The deadline sits DefaultTimeout away from the call, minus scheduling
	// delay.
	if left := time.Until(c.deadline); left <= 0 || left > DefaultTimeout {
		t.Fatalf("deadline %v away, want within (0, %v]", left, DefaultTimeout)
	}
}

// An explicitly configured Timeout reaches the call verbatim.
func TestLLMStrategyCustomTimeoutReachesCall(t *testing.T) {
	ctx, _ := newTestCtx(t)
	c := &probeClient{}
	want := 3 * time.Second
	if _, err := (LLMStrategy{Client: c, Timeout: want}).Summarize(ctx, []events.Event{userReq("hello")}); err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if left := time.Until(c.deadline); left <= 0 || left > want {
		t.Fatalf("deadline %v away, want within (0, %v]", left, want)
	}
}

func TestLLMStrategyNegativeTimeoutRejected(t *testing.T) {
	ctx, _ := newTestCtx(t)
	c := &probeClient{}
	if _, err := (LLMStrategy{Client: c, Timeout: -time.Second}).Summarize(ctx, []events.Event{userReq("hello")}); err == nil {
		t.Fatal("want error for negative Timeout")
	} else if c.called {
		t.Fatal("client must not be called on invalid Timeout")
	}
}
