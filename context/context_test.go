package context

import (
	stdcontext "context"
	"errors"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/hitl"
	"github.com/luckystrker/ago/stream"
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

// TestEnqueueOriginForwarding pins the bridge contract: Enqueue stays service
// (tool/background updates must not open a user interval), EnqueueOrigin
// forwards the sender's origin to the stream inbox.
func TestEnqueueOriginForwarding(t *testing.T) {
	s := stream.NewMemoryStream()
	c := New(s)
	if err := c.EnqueueOrigin(events.OriginUser, &events.TextInput{Content: "human"}); err != nil {
		t.Fatalf("enqueue origin: %v", err)
	}
	if err := c.Enqueue(&events.TextInput{Content: "tool"}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	pending := s.Pending()
	if len(pending) != 2 {
		t.Fatalf("pending = %d requests, want 2", len(pending))
	}
	if pending[0].Origin != events.OriginUser || pending[1].Origin != events.OriginService {
		t.Fatalf("origins = %q, %q; want user, service", pending[0].Origin, pending[1].Origin)
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

// TestTurnContextCarried pins the SetTurnContext/TurnContext contract used to
// propagate turn cancellation into hook-bearing code that sees no stdlib
// context parameter (audit M-26).
func TestTurnContextCarried(t *testing.T) {
	c := New(stream.NewMemoryStream())
	if c.TurnContext() != stdcontext.Background() {
		t.Fatal("unset turn context must be Background")
	}
	ctx, cancel := stdcontext.WithCancel(stdcontext.Background())
	defer cancel()
	c.SetTurnContext(ctx)
	if c.TurnContext() != ctx {
		t.Fatal("TurnContext must return the installed context")
	}
	if c.Clone().TurnContext() != ctx {
		t.Fatal("a clone must carry the turn context")
	}
	c.SetTurnContext(nil)
	if c.TurnContext() != stdcontext.Background() {
		t.Fatal("a cleared turn context must fall back to Background")
	}
}

// TestAskSanitizesUntrustedTimeout pins audit L-19: NaN, infinite and negative
// timeouts coming from untrusted events must behave as "no deadline" instead of
// corrupting the deadline arithmetic (a +Inf conversion to Duration is
// undefined and effectively fires immediately). Zero counts too: 0 means "no
// deadline", and so does a positive value small enough to truncate to a zero
// Duration — a zero-Duration deadline would expire before the hook can answer.
func TestAskSanitizesUntrustedTimeout(t *testing.T) {
	for _, tc := range []struct {
		name    string
		timeout float64
	}{
		{"nan", math.NaN()},
		{"pos-inf", math.Inf(1)},
		{"neg-inf", math.Inf(-1)},
		{"negative", -7},
		{"huge-finite", 1e12}, // overflows the Duration conversion
		{"zero", 0},
		{"sub-nanosecond", 1e-10}, // Duration(1e-10 * time.Second) truncates to 0
	} {
		t.Run(tc.name, func(t *testing.T) {
			answer := make(chan struct{})
			c := New(stream.NewMemoryStream())
			c.SetHitlHook(func(hctx stdcontext.Context, req *events.HumanInputRequest) (*events.HumanMessage, error) {
				<-answer
				return &events.HumanMessage{Content: "ok"}, nil
			})
			type out struct {
				val string
				err error
			}
			got := make(chan out, 1)
			go func() {
				v, err := c.Ask(stdcontext.Background(), &events.HumanInputRequest{Content: "q", Timeout: tc.timeout})
				got <- out{v, err}
			}()
			select {
			case r := <-got:
				t.Fatalf("Ask returned (%q, %v) before the hook answered: bad timeout became a live deadline", r.val, r.err)
			case <-time.After(200 * time.Millisecond):
			}
			close(answer)
			select {
			case r := <-got:
				if r.err != nil || r.val != "ok" {
					t.Fatalf("Ask = (%q, %v), want (ok, nil)", r.val, r.err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("Ask did not return after the hook answered")
			}
		})
	}
}

// TestAskAppliesFiniteTimeout is the positive control for the sanitization
// above: a small finite timeout still fires as a hitl timeout.
func TestAskAppliesFiniteTimeout(t *testing.T) {
	c := New(stream.NewMemoryStream())
	c.SetHitlHook(func(hctx stdcontext.Context, req *events.HumanInputRequest) (*events.HumanMessage, error) {
		<-hctx.Done()
		return nil, hctx.Err()
	})
	_, err := c.Ask(stdcontext.Background(), &events.HumanInputRequest{Content: "q", Timeout: 0.05})
	var te *hitl.TimeoutError
	if !errors.As(err, &te) {
		t.Fatalf("want *hitl.TimeoutError, got %v", err)
	}
}

// TestZeroValueContextSetters pins the lazy map initialization: a zero-value
// Context (not built by New) must accept SetDependency/SetVariable and report
// the values back, instead of panicking on a nil map.
func TestZeroValueContextSetters(t *testing.T) {
	var c Context
	c.SetDependency("dep", "v")
	if v, ok := c.Dependency("dep"); !ok || v != "v" {
		t.Fatalf("Dependency after SetDependency = (%v, %v), want (v, true)", v, ok)
	}
	c.SetVariable("var", 42)
	if v, ok := c.Variable("var"); !ok || v != 42 {
		t.Fatalf("Variable after SetVariable = (%v, %v), want (42, true)", v, ok)
	}
	c.AddPrompt("line")
	if pl := c.PromptLines(); len(pl) != 1 || pl[0] != "line" {
		t.Fatalf("PromptLines = %v, want [line]", pl)
	}
}

// TestAskIgnoresForeignRequest pins the per-request routing: Ask's interrupt
// subscription only claims the request it asked — a HumanInputRequest from
// another participant must flow through untouched (not swallowed, not handed
// to this Ask's hook) while the wait keeps listening for its own answer
// (Python parity: the interrupter matches the request identity).
func TestAskIgnoresForeignRequest(t *testing.T) {
	s := stream.NewMemoryStream()
	c := New(s)
	calls := make(chan *events.HumanInputRequest, 4)
	release := make(chan struct{})
	c.SetHitlHook(func(_ stdcontext.Context, req *events.HumanInputRequest) (*events.HumanMessage, error) {
		calls <- req
		<-release
		return &events.HumanMessage{Content: "ok", ParentID: req.ID}, nil
	})
	type outcome struct {
		ans string
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		ans, err := c.Input(stdcontext.Background(), "mine", 0)
		done <- outcome{ans, err}
	}()
	// The hook being called proves Ask's subscription is live: it runs inside
	// the dispatch of Ask's own question.
	select {
	case req := <-calls:
		if req.Content != "mine" {
			t.Fatalf("hook called with %q before our own question", req.Content)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("hook never called for our request")
	}

	// A foreign request (different id) crosses the stream while we wait.
	foreign := events.NewHumanInputRequest("foreign", 0)
	if err := s.Send(&foreign); err != nil {
		t.Fatalf("send foreign: %v", err)
	}
	select {
	case req := <-calls:
		t.Fatalf("hook hijacked the foreign request %q", req.Content)
	case <-time.After(300 * time.Millisecond):
	}
	// Not swallowed either: a request that nobody intercepts persists like any
	// other event.
	h, err := c.History()
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	persisted := false
	for _, ev := range h {
		if hr, ok := ev.(*events.HumanInputRequest); ok && hr.Content == "foreign" {
			persisted = true
		}
	}
	if !persisted {
		t.Fatal("the foreign request was swallowed by our Ask's subscriber")
	}

	close(release)
	select {
	case o := <-done:
		if o.err != nil || o.ans != "ok" {
			t.Fatalf("Ask = (%q, %v), want (ok, nil)", o.ans, o.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Ask never returned after the hook answered")
	}
}

// TestAskHookDeadlineExceededWithoutDeadlineIsFailure pins the classification
// boundary: a hook failing with DeadlineExceeded of its OWN (no Ask-imposed
// deadline is active) is a channel failure, not a wait timeout (Python parity:
// only the wait produces HumanInputTimeoutError).
func TestAskHookDeadlineExceededWithoutDeadlineIsFailure(t *testing.T) {
	c := New(stream.NewMemoryStream())
	c.SetHitlHook(func(_ stdcontext.Context, _ *events.HumanInputRequest) (*events.HumanMessage, error) {
		// DeadlineExceeded of some inner operation, not of the Ask deadline —
		// the Ask itself carries no timeout here.
		return nil, fmt.Errorf("hook inner wait: %w", stdcontext.DeadlineExceeded)
	})
	_, err := c.Ask(stdcontext.Background(), &events.HumanInputRequest{Content: "q"})
	var fe *hitl.FailedError
	if !errors.As(err, &fe) {
		t.Fatalf("want *hitl.FailedError, got %T %v", err, err)
	}
	var te *hitl.TimeoutError
	if errors.As(err, &te) {
		t.Fatalf("a hook's own DeadlineExceeded must not classify as TimeoutError, got %v", err)
	}
}
