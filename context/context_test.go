package context

import (
	stdcontext "context"
	"errors"
	"testing"
	"time"

	"ago/events"
	"ago/hitl"
	"ago/stream"
)

func TestEnqueuePendingDrain(t *testing.T) {
	c := New(stream.NewMemoryStream())
	if err := c.Enqueue(&events.TextInput{Content: "hi"}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if len(c.Pending()) != 1 {
		t.Fatalf("pending: %d", len(c.Pending()))
	}
	if len(c.Drain()) != 1 {
		t.Fatal("drain")
	}
}

func TestAskWithoutHook(t *testing.T) {
	c := New(stream.NewMemoryStream())
	_, err := c.Ask(stdcontext.Background(), &events.HumanInputRequest{Content: "yes?"})
	if !errors.Is(err, hitl.ErrNotProvided) {
		t.Fatalf("want ErrNotProvided, got %v", err)
	}
}

func TestAskWithHookAndTranscript(t *testing.T) {
	s := stream.NewMemoryStream()
	c := New(s)
	c.SetHitlHook(func(ctx stdcontext.Context, req *events.HumanInputRequest) (*events.HumanMessage, error) {
		return &events.HumanMessage{Content: "approved"}, nil
	})
	answer, err := c.Input(stdcontext.Background(), "approve?", 0)
	if err != nil {
		t.Fatalf("ask: %v", err)
	}
	if answer != "approved" {
		t.Fatalf("answer: %q", answer)
	}
	// The answer must be on the transcript with a matching parent id.
	h, _ := c.History()
	var found bool
	for _, ev := range h {
		if hm, ok := ev.(*events.HumanMessage); ok && hm.Content == "approved" && hm.ParentID != "" {
			found = true
		}
	}
	if !found {
		t.Fatal("HumanMessage with parent id not persisted")
	}
}

func TestAskTimeout(t *testing.T) {
	c := New(stream.NewMemoryStream())
	c.SetHitlHook(func(ctx stdcontext.Context, req *events.HumanInputRequest) (*events.HumanMessage, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	_, err := c.Input(stdcontext.Background(), "slow?", 30*time.Millisecond)
	var te *hitl.TimeoutError
	if !errors.As(err, &te) {
		t.Fatalf("want TimeoutError, got %v", err)
	}
}

func TestAskHookFailure(t *testing.T) {
	c := New(stream.NewMemoryStream())
	c.SetHitlHook(func(ctx stdcontext.Context, req *events.HumanInputRequest) (*events.HumanMessage, error) {
		return nil, errors.New("channel down")
	})
	_, err := c.Ask(stdcontext.Background(), &events.HumanInputRequest{Content: "x"})
	var fe *hitl.FailedError
	if !errors.As(err, &fe) {
		t.Fatalf("want FailedError, got %v", err)
	}
}

func TestDependenciesVariablesClone(t *testing.T) {
	s := stream.NewMemoryStream()
	c := NewWithDeps(s, map[string]any{"agent": "main"}, map[string]any{"k": "v"})
	c.AddPrompt("system line")
	c.SetHitlHook(func(ctx stdcontext.Context, req *events.HumanInputRequest) (*events.HumanMessage, error) {
		return &events.HumanMessage{Content: "hooked"}, nil
	})

	cc := c.WithToolCallID("call-1")
	if _, ok := cc.Dependency("agent"); !ok {
		t.Fatal("clone lost dependencies")
	}
	cc.SetVariable("k", "changed")
	if v, _ := c.Variable("k"); v != "v" {
		t.Fatal("clone must not mutate the original variables")
	}
	if len(cc.PromptLines()) != 1 {
		t.Fatal("clone lost prompt fragments")
	}
	if cc.ToolCallID != "call-1" || c.ToolCallID != "" {
		t.Fatal("tool call id stamping wrong")
	}
	// Hook is shared.
	a, err := cc.Input(stdcontext.Background(), "q", 0)
	if err != nil || a != "hooked" {
		t.Fatalf("shared hook: %q %v", a, err)
	}
}

func TestSendPublishes(t *testing.T) {
	c := New(stream.NewMemoryStream())
	seen := make(chan events.Event, 1)
	c.Stream.Subscribe(func(ev events.Event) error { seen <- ev; return nil },
		stream.SubscribeOptions{Condition: events.TypeOf[events.ModelMessageChunk]()})
	if err := c.Send(&events.ModelMessageChunk{Content: "zz"}); err != nil {
		t.Fatalf("send: %v", err)
	}
	select {
	case ev := <-seen:
		if ev.(*events.ModelMessageChunk).Content != "zz" {
			t.Fatal("wrong event")
		}
	case <-time.After(time.Second):
		t.Fatal("subscriber did not receive")
	}
}
