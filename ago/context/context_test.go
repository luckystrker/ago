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

// TestAskQuestionNotTranscribed pins the Python transcript parity: the question
// is delivered to the hook through the stream without being persisted (dispatch
// stops at the interrupt subscriber), only the answer reaches the transcript.
func TestAskQuestionNotTranscribed(t *testing.T) {
	s := stream.NewMemoryStream()
	c := New(s)
	c.SetHitlHook(func(ctx stdcontext.Context, req *events.HumanInputRequest) (*events.HumanMessage, error) {
		return &events.HumanMessage{Content: "so be it"}, nil
	})
	if _, err := c.Input(stdcontext.Background(), "shall we?", 0); err != nil {
		t.Fatalf("ask: %v", err)
	}
	h, _ := c.History()
	var answers, questions int
	for _, ev := range h {
		switch e := ev.(type) {
		case *events.HumanMessage:
			answers++
			if e.ParentID == "" {
				t.Fatal("answer must reference the request")
			}
		case *events.HumanInputRequest:
			questions++
		}
	}
	if answers != 1 || questions != 0 {
		t.Fatalf("transcript: %d answers, %d questions (question must not persist)", answers, questions)
	}
}

// TestAskExternalAnswerViaStream pins the ARCHITECTURE.md contract: Ask waits
// for the matching HumanMessage (parent_id == req.ID) via Stream.Get, so an
// external participant can answer instead of the hook.
func TestAskExternalAnswerViaStream(t *testing.T) {
	c := New(stream.NewMemoryStream())
	c.SetHitlHook(func(ctx stdcontext.Context, req *events.HumanInputRequest) (*events.HumanMessage, error) {
		<-ctx.Done() // never answers: the answer must come from the stream
		return nil, ctx.Err()
	})
	req := events.NewHumanInputRequest("route this to a human", 5)
	done := make(chan struct{})
	var answer string
	var err error
	go func() {
		defer close(done)
		answer, err = c.Ask(stdcontext.Background(), &req)
	}()
	time.Sleep(20 * time.Millisecond)
	if serr := c.Send(&events.HumanMessage{ParentID: req.ID, Content: "from transport"}); serr != nil {
		t.Fatalf("external send: %v", serr)
	}
	select {
	case <-done:
	case <-time.After(4 * time.Second):
		t.Fatal("Ask did not pick up the external answer")
	}
	if err != nil {
		t.Fatalf("ask: %v", err)
	}
	if answer != "from transport" {
		t.Fatalf("answer: %q", answer)
	}
}

// TestAskHungHookDeadlineEnforced pins the enforced timeout: a hook that ignores
// its context must not hold Ask past the request deadline (Python parity:
// asyncio.wait_for cancels the in-flight hook; AGo abandons the goroutine).
func TestAskHungHookDeadlineEnforced(t *testing.T) {
	c := New(stream.NewMemoryStream())
	c.SetHitlHook(func(ctx stdcontext.Context, req *events.HumanInputRequest) (*events.HumanMessage, error) {
		time.Sleep(10 * time.Second) // ignores ctx on purpose
		return &events.HumanMessage{Content: "too late"}, nil
	})
	start := time.Now()
	_, err := c.Input(stdcontext.Background(), "slow human?", 50*time.Millisecond)
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Ask waited %v for a hung hook", elapsed)
	}
	var te *hitl.TimeoutError
	if !errors.As(err, &te) {
		t.Fatalf("want TimeoutError, got %v", err)
	}
}

// TestAskHookFailureKeepsFailureAtDeadline pins the error classification: a
// hook failing on its own is a FailedError even when the request carries a
// timeout (Python: only the wait itself produces HumanInputTimeoutError).
func TestAskHookFailureKeepsFailureAtDeadline(t *testing.T) {
	c := New(stream.NewMemoryStream())
	c.SetHitlHook(func(ctx stdcontext.Context, req *events.HumanInputRequest) (*events.HumanMessage, error) {
		return nil, errors.New("channel down")
	})
	_, err := c.Input(stdcontext.Background(), "q", 5*time.Second)
	var fe *hitl.FailedError
	if !errors.As(err, &fe) {
		t.Fatalf("want FailedError, got %v", err)
	}
	var te *hitl.TimeoutError
	if errors.As(err, &te) {
		t.Fatal("hook failure must not classify as TimeoutError")
	}
}
