package stream

import (
	"context"
	"errors"
	"testing"

	"github.com/luckystrker/ago/events"
)

func response(content string) *events.ModelResponse {
	return &events.ModelResponse{Message: &events.ModelMessage{Content: content}}
}

// subCount reports the stream's registered subscription count for tests that
// assert a Join detaches.
func subCount(s *MemoryStream) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.subs)
}

// TestJoinPullsLiveEvents pins the pull-iterator shape: events sent after the
// join are buffered and handed out in order, and Next blocks for the next one.
func TestJoinPullsLiveEvents(t *testing.T) {
	s := NewMemoryStream()
	j := s.Join(context.Background(), JoinOptions{})
	defer j.Close()

	if err := s.Send(response("one")); err != nil {
		t.Fatalf("send one: %v", err)
	}
	if err := s.Send(response("two")); err != nil {
		t.Fatalf("send two: %v", err)
	}
	for _, want := range []string{"one", "two"} {
		ev, err := j.Next()
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		resp, ok := ev.(*events.ModelResponse)
		if !ok || resp.Content() != want {
			t.Fatalf("Next = %T %q, want %q", ev, events.RenderForPrompt(ev), want)
		}
	}
}

// TestJoinMaxEventsEnds pins max_events: after the window is filled, Next
// reports ErrJoinDone instead of blocking for more.
func TestJoinMaxEventsEnds(t *testing.T) {
	s := NewMemoryStream()
	j := s.Join(context.Background(), JoinOptions{MaxEvents: 1})
	defer j.Close()

	if err := s.Send(response("one")); err != nil {
		t.Fatalf("send one: %v", err)
	}
	if err := s.Send(response("two")); err != nil {
		t.Fatalf("send two: %v", err)
	}
	ev, err := j.Next()
	if err != nil {
		t.Fatalf("first Next: %v", err)
	}
	if got := events.RenderForPrompt(ev); got != "Assistant: one" {
		t.Fatalf("first Next = %q, want Assistant: one", got)
	}
	if _, err := j.Next(); !errors.Is(err, ErrJoinDone) {
		t.Fatalf("second Next error = %v, want ErrJoinDone", err)
	}
}

// TestJoinConditionFilters pins the condition option.
func TestJoinConditionFilters(t *testing.T) {
	s := NewMemoryStream()
	j := s.Join(context.Background(), JoinOptions{Condition: events.TypeOf[*events.ModelRequest]()})
	defer j.Close()

	if err := s.Send(response("skip")); err != nil {
		t.Fatalf("send response: %v", err)
	}
	if err := s.Send(&events.ModelRequest{}); err != nil {
		t.Fatalf("send request: %v", err)
	}
	ev, err := j.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if _, ok := events.AsValue(ev).(events.ModelRequest); !ok {
		t.Fatalf("Next = %T, want ModelRequest", ev)
	}
}

// TestJoinHonorsContext pins cancellation: a blocked Next returns the context
// error instead of waiting forever.
func TestJoinHonorsContext(t *testing.T) {
	s := NewMemoryStream()
	ctx, cancel := context.WithCancel(context.Background())
	j := s.Join(ctx, JoinOptions{})
	defer j.Close()

	cancel()
	if _, err := j.Next(); !errors.Is(err, context.Canceled) {
		t.Fatalf("Next error = %v, want context.Canceled", err)
	}
}

// TestJoinCloseReleasesSubscription pins that Close unsubscribes: the stream no
// longer holds the Join subscription, and a later Send neither buffers for the
// closed iterator nor fails because of it.
func TestJoinCloseReleasesSubscription(t *testing.T) {
	s := NewMemoryStream()
	if got := subCount(s); got != 0 {
		t.Fatalf("fresh stream has %d subscribers, want 0", got)
	}
	j := s.Join(context.Background(), JoinOptions{})
	if got := subCount(s); got != 1 {
		t.Fatalf("after Join the stream has %d subscribers, want 1", got)
	}
	j.Close()
	if got := subCount(s); got != 0 {
		t.Fatalf("after Close the stream has %d subscribers, want 0", got)
	}

	if _, err := j.Next(); !errors.Is(err, ErrJoinClosed) {
		t.Fatalf("Next after Close error = %v, want ErrJoinClosed", err)
	}
	if err := s.Send(response("after close")); err != nil {
		t.Fatalf("send after close: %v", err)
	}
	j.Close() // idempotent
	if got := subCount(s); got != 0 {
		t.Fatalf("repeated Close left %d subscribers", got)
	}
}

// TestJoinMaxEventsDetachesSubscription pins that filling a bounded window
// releases the subscription too, so the stream stops buffering for an iterator
// nobody will read again.
func TestJoinMaxEventsDetachesSubscription(t *testing.T) {
	s := NewMemoryStream()
	j := s.Join(context.Background(), JoinOptions{MaxEvents: 1})
	if got := subCount(s); got != 1 {
		t.Fatalf("after Join the stream has %d subscribers, want 1", got)
	}
	if err := s.Send(response("one")); err != nil {
		t.Fatalf("send one: %v", err)
	}
	if _, err := j.Next(); err != nil {
		t.Fatalf("Next: %v", err)
	}
	if got := subCount(s); got != 0 {
		t.Fatalf("after the window filled the stream has %d subscribers, want 0", got)
	}
	if err := s.Send(response("two")); err != nil {
		t.Fatalf("send two: %v", err)
	}
	if _, err := j.Next(); !errors.Is(err, ErrJoinDone) {
		t.Fatalf("Next after the window error = %v, want ErrJoinDone", err)
	}
}

// TestJoinOnFilteredView pins that joining a Where view keeps the view's
// filter, like every other read path on SubStream.
func TestJoinOnFilteredView(t *testing.T) {
	s := NewMemoryStream()
	view := s.Where(events.TypeOf[*events.ModelRequest]()).(*SubStream)
	j := view.Join(context.Background(), JoinOptions{})
	defer j.Close()

	if err := s.Send(response("skip")); err != nil {
		t.Fatalf("send response: %v", err)
	}
	if err := s.Send(&events.ModelRequest{}); err != nil {
		t.Fatalf("send request: %v", err)
	}
	ev, err := j.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if _, ok := events.AsValue(ev).(events.ModelRequest); !ok {
		t.Fatalf("Next = %T, want ModelRequest", ev)
	}
}
