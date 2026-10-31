package agent

import (
	stdcontext "context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/luckystrker/ago/config"
	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/stream"
	agotest "github.com/luckystrker/ago/testing"
	"github.com/luckystrker/ago/tools"
)

// promptSpy is a model double recording the system prompt each call was handed
// (the wire surface: conv.PromptLines becomes the system message).
type promptSpy struct {
	mu      sync.Mutex
	prompts [][]string
}

func (p *promptSpy) Provider() config.Provider                        { return "test" }
func (p *promptSpy) Model() string                                    { return "test" }
func (p *promptSpy) Create() (config.LLMClient, error)                { return p, nil }
func (p *promptSpy) Clone(map[string]any) (config.ModelConfig, error) { return p, nil }
func (p *promptSpy) Call(_ stdcontext.Context, _ []events.Event, conv *agocontext.Context, _ config.CallOptions) (*events.ModelResponse, error) {
	p.mu.Lock()
	p.prompts = append(p.prompts, conv.PromptLines())
	p.mu.Unlock()
	return textResp("ok"), nil
}

func (p *promptSpy) recorded() [][]string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([][]string, len(p.prompts))
	copy(out, p.prompts)
	return out
}

// TestContinuationPromptNotDuplicated pins Python parity: the system prompt is
// seeded once per conversation — a continuation clones a context that already
// carries the prompt and must not re-add fragments nor re-run prompt hooks
// (_prepare_turn's `if not context.prompt`).
func TestContinuationPromptNotDuplicated(t *testing.T) {
	var hooks int
	spy := &promptSpy{}
	a := New("bot", "FRAG-A", WithConfig(spy),
		WithPromptFragments("FRAG-B"),
		WithPromptHook(func(*agocontext.Context) ([]string, error) {
			hooks++
			return []string{"HOOK-1"}, nil
		}))
	r1, err := a.Ask(stdcontext.Background(), "1")
	if err != nil {
		t.Fatalf("first ask: %v", err)
	}
	if _, err := r1.Ask(stdcontext.Background(), "2"); err != nil {
		t.Fatalf("continuation: %v", err)
	}
	got := spy.recorded()
	if len(got) != 2 {
		t.Fatalf("client saw %d calls, want 2", len(got))
	}
	want := "FRAG-A,FRAG-B,HOOK-1"
	for i, p := range got {
		if have := strings.Join(p, ","); have != want {
			t.Fatalf("call %d prompt = [%s], want [%s]", i+1, have, want)
		}
	}
	if hooks != 1 {
		t.Fatalf("prompt hook ran %d times, want 1", hooks)
	}
}

// blockingClient parks every Call until release is closed or the call context
// is cancelled, signalling entered once a call has started.
type blockingClient struct {
	release chan struct{}
	entered chan struct{}
}

func (b *blockingClient) Provider() config.Provider                        { return "test" }
func (b *blockingClient) Model() string                                    { return "test" }
func (b *blockingClient) Create() (config.LLMClient, error)                { return b, nil }
func (b *blockingClient) Clone(map[string]any) (config.ModelConfig, error) { return b, nil }
func (b *blockingClient) Call(ctx stdcontext.Context, _ []events.Event, _ *agocontext.Context, _ config.CallOptions) (*events.ModelResponse, error) {
	select {
	case b.entered <- struct{}{}:
	default:
	}
	select {
	case <-b.release:
		return textResp("late"), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// TestCloseCancelsRunningTurn pins Python parity (AgentRun.__aexit__ cancels an
// unfinished task): Close during a live turn unblocks Result with
// context.Canceled and frees the per-stream lock for the next ask.
func TestCloseCancelsRunningTurn(t *testing.T) {
	b := &blockingClient{release: make(chan struct{}), entered: make(chan struct{}, 1)}
	base := stream.NewMemoryStream()
	a := New("a", "", WithConfig(b), WithStreamBase(base))

	run, err := a.Run(stdcontext.Background(), "go")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if err := run.Start(stdcontext.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	select {
	case <-b.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("turn never reached the model call")
	}
	run.Close() // the turn is parked inside the model call

	errCh := make(chan error, 1)
	go func() {
		_, err := run.Result(stdcontext.Background())
		errCh <- err
	}()
	select {
	case err := <-errCh:
		if !errors.Is(err, stdcontext.Canceled) {
			t.Fatalf("Result after Close = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Result still blocked after Close: turn goroutine leaked")
	}
	// Unpark the client for the follow-up ask (a leaked pre-fix turn, if any,
	// would also finish here — the assertion above has already failed).
	close(b.release)

	// The per-stream lock must be free: a follow-up ask on the same stream proceeds.
	done := make(chan string, 1)
	errCh2 := make(chan error, 1)
	go func() {
		reply, err := a.Ask(stdcontext.Background(), "again")
		if err != nil {
			errCh2 <- err
			return
		}
		done <- reply.Body()
	}()
	select {
	case body := <-done:
		if body != "late" {
			t.Fatalf("follow-up ask body = %q, want %q", body, "late")
		}
	case err := <-errCh2:
		t.Fatalf("follow-up ask: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("follow-up ask blocked: per-stream lock not released by Close")
	}
}

// flakyCreateConfig fails Create until armed, then serves a scripted client.
type flakyCreateConfig struct {
	mu   sync.Mutex
	on   bool
	resp *agotest.TestConfig
}

func (f *flakyCreateConfig) arm() {
	f.mu.Lock()
	f.on = true
	f.mu.Unlock()
}

func (f *flakyCreateConfig) Provider() config.Provider { return "test" }
func (f *flakyCreateConfig) Model() string             { return "test" }
func (f *flakyCreateConfig) Create() (config.LLMClient, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.on {
		return nil, errors.New("create disabled")
	}
	if f.resp == nil {
		f.resp = agotest.NewTestConfig(textResp("ok"))
	}
	return f.resp.Create()
}
func (f *flakyCreateConfig) Clone(map[string]any) (config.ModelConfig, error) { return f, nil }

// TestFailedStartReleasesStreamLock pins Python parity (_turn_scope releases the
// lock when the body fails): a Start that fails inside open() must not strand
// the stream, even when the caller forgets Close.
func TestFailedStartReleasesStreamLock(t *testing.T) {
	fc := &flakyCreateConfig{}
	base := stream.NewMemoryStream()
	a := New("a", "p", WithConfig(fc), WithStreamBase(base))

	run, err := a.Run(stdcontext.Background(), "hi")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	err = run.Start(stdcontext.Background())
	if err == nil || !strings.Contains(err.Error(), "create model client") {
		t.Fatalf("want create failure, got %v", err)
	}
	// No Close: the buggy behavior kept the lock and blocked every later ask.

	fc.arm()
	done := make(chan error, 1)
	go func() {
		_, err := a.Ask(stdcontext.Background(), "again")
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("follow-up ask: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("follow-up ask blocked: per-stream lock leaked by failed open")
	}
}

// TestFailedPromptHookReleasesStreamLock covers a scope failure after the lock
// and config resolution: the prompt-hook error path must release the lock too.
func TestFailedPromptHookReleasesStreamLock(t *testing.T) {
	var armed atomic.Bool
	base := stream.NewMemoryStream()
	a := New("a", "p",
		WithConfig(agotest.NewTestConfig(textResp("ok"))),
		WithStreamBase(base),
		WithPromptHook(func(*agocontext.Context) ([]string, error) {
			if !armed.Load() {
				return nil, errors.New("hook not armed")
			}
			return []string{"dyn"}, nil
		}))

	run, err := a.Run(stdcontext.Background(), "hi")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	err = run.Start(stdcontext.Background())
	if err == nil || !strings.Contains(err.Error(), "prompt hook") {
		t.Fatalf("want prompt-hook failure, got %v", err)
	}
	// No Close.

	armed.Store(true)
	done := make(chan error, 1)
	go func() {
		_, err := a.Ask(stdcontext.Background(), "again")
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("follow-up ask: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("follow-up ask blocked: per-stream lock leaked by failed open")
	}
}

// gatedConfig parks every model call until release is closed and records the
// history each call saw. Unlike blockingClient it ignores the call context: the
// tests below need a turn that cannot be interrupted through cancellation.
type gatedConfig struct {
	release chan struct{}
	entered chan struct{}
	mu      sync.Mutex
	seen    [][]events.Event
}

func (g *gatedConfig) Provider() config.Provider                        { return "test" }
func (g *gatedConfig) Model() string                                    { return "test" }
func (g *gatedConfig) Create() (config.LLMClient, error)                { return g, nil }
func (g *gatedConfig) Clone(map[string]any) (config.ModelConfig, error) { return g, nil }
func (g *gatedConfig) Call(_ stdcontext.Context, evs []events.Event, _ *agocontext.Context, _ config.CallOptions) (*events.ModelResponse, error) {
	cp := make([]events.Event, len(evs))
	copy(cp, evs)
	g.mu.Lock()
	g.seen = append(g.seen, cp)
	g.mu.Unlock()
	select {
	case g.entered <- struct{}{}:
	default:
	}
	<-g.release
	return textResp("gated"), nil
}

func (g *gatedConfig) histories() [][]events.Event {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([][]events.Event, len(g.seen))
	copy(out, g.seen)
	return out
}

// transcriptRequests renders the request texts one model call was handed.
func transcriptRequests(evs []events.Event) string {
	var b strings.Builder
	for _, ev := range evs {
		if mr, ok := events.AsValue(ev).(events.ModelRequest); ok {
			for _, p := range mr.Parts {
				if ti, ok := p.(*events.TextInput); ok {
					b.WriteString(ti.Content)
					b.WriteByte('\n')
				}
			}
		}
	}
	return b.String()
}

// TestResultHonorsCtxCancellation pins audit M-26: Result must give up when its
// own context is cancelled, not only when the turn finishes — a caller must not
// park forever on a turn stuck in a model call that ignores cancellation.
func TestResultHonorsCtxCancellation(t *testing.T) {
	g := &gatedConfig{release: make(chan struct{}), entered: make(chan struct{}, 1)}
	a := New("a", "", WithConfig(g))

	run, err := a.Run(stdcontext.Background(), "go")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if err := run.Start(stdcontext.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	select {
	case <-g.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("turn never reached the model call")
	}

	ctx, cancel := stdcontext.WithCancel(stdcontext.Background())
	errCh := make(chan error, 1)
	go func() {
		_, err := run.Result(ctx)
		errCh <- err
	}()
	time.Sleep(100 * time.Millisecond) // let Result park in the wait
	cancel()
	select {
	case err := <-errCh:
		if !errors.Is(err, stdcontext.Canceled) {
			t.Fatalf("Result after cancel = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Result ignored its context's cancellation")
	}
	close(g.release) // let the abandoned turn goroutine finish
	run.Close()
}

// TestRunInputWaitsForStreamLock pins audit M-25: a run's input enters the
// inbox only after its turn holds the per-stream lock, so a sibling delegation
// blocked on a shared stream base cannot have its objective drained into the
// running turn's prompt.
func TestRunInputWaitsForStreamLock(t *testing.T) {
	g := &gatedConfig{release: make(chan struct{}), entered: make(chan struct{}, 1)}
	base := stream.NewMemoryStream()
	a := New("a", "", WithConfig(g), WithStreamBase(base))

	first, err := a.Run(stdcontext.Background(), "objective-A")
	if err != nil {
		t.Fatalf("run A: %v", err)
	}
	if err := first.Start(stdcontext.Background()); err != nil {
		t.Fatalf("start A: %v", err)
	}
	select {
	case <-g.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("turn A never reached the model call")
	}

	second, err := a.Run(stdcontext.Background(), "objective-B")
	if err != nil {
		t.Fatalf("run B: %v", err)
	}
	if pending := base.Pending(); len(pending) != 0 {
		t.Fatalf("input enqueued before its turn owns the stream lock: %d pending", len(pending))
	}

	close(g.release) // turn A finishes on its parked call
	if _, err := first.Result(stdcontext.Background()); err != nil {
		t.Fatalf("turn A: %v", err)
	}
	first.Close() // hands the per-stream lock over to the waiting sibling
	if err := second.Start(stdcontext.Background()); err != nil {
		t.Fatalf("start B: %v", err)
	}
	if _, err := second.Result(stdcontext.Background()); err != nil {
		t.Fatalf("turn B: %v", err)
	}

	hist := g.histories()
	if len(hist) != 2 {
		t.Fatalf("model calls = %d, want 2", len(hist))
	}
	if got := transcriptRequests(hist[0]); !strings.Contains(got, "objective-A") || strings.Contains(got, "objective-B") {
		t.Fatalf("turn A drained the sibling's input; requests seen:\n%s", got)
	}
	if got := transcriptRequests(hist[1]); !strings.Contains(got, "objective-B") {
		t.Fatalf("turn B never saw its own objective; requests seen:\n%s", got)
	}
}

// TestTurnLockMapShrinksAfterRuns pins audit L-19: the per-stream lock map must
// not keep one mutex per stream after the last run on that stream finished —
// agents asked over many fresh streams must not leak lock entries.
func TestTurnLockMapShrinksAfterRuns(t *testing.T) {
	a := New("a", "", WithConfig(agotest.NewTestConfig(textResp("ok"), textResp("ok"), textResp("ok"))))
	for i := 0; i < 3; i++ {
		if _, err := a.Ask(stdcontext.Background(), "hi"); err != nil {
			t.Fatalf("ask %d: %v", i, err)
		}
	}
	a.turnsMu.Lock()
	n := len(a.turns)
	a.turnsMu.Unlock()
	if n != 0 {
		t.Fatalf("per-stream lock map kept %d entries after all runs finished", n)
	}
}

// loopToolConfig answers every call with one tool request: the turn never
// converges without a round cap.
type loopToolConfig struct {
	mu    sync.Mutex
	calls int
}

func (l *loopToolConfig) Provider() config.Provider                        { return "test" }
func (l *loopToolConfig) Model() string                                    { return "test" }
func (l *loopToolConfig) Create() (config.LLMClient, error)                { return l, nil }
func (l *loopToolConfig) Clone(map[string]any) (config.ModelConfig, error) { return l, nil }
func (l *loopToolConfig) Call(_ stdcontext.Context, _ []events.Event, _ *agocontext.Context, _ config.CallOptions) (*events.ModelResponse, error) {
	l.mu.Lock()
	l.calls++
	l.mu.Unlock()
	return toolResp("tick", "{}"), nil
}

func (l *loopToolConfig) callCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.calls
}

// TestMaxToolRoundsStopsLoop pins audit H-11: WithMaxToolRounds stops a turn
// whose model keeps requesting tools, with ErrMaxToolRounds and a repaired
// transcript (no unanswered tool calls left behind).
func TestMaxToolRoundsStopsLoop(t *testing.T) {
	tick, err := tools.NewFunctionTool("tick", "Ticks", func(args struct{}) (string, error) {
		return "tock", nil
	})
	if err != nil {
		t.Fatalf("tool: %v", err)
	}
	cfg := &loopToolConfig{}
	a := New("a", "", WithConfig(cfg), WithTools(tick), WithMaxToolRounds(2))

	run, err := a.Run(stdcontext.Background(), "go")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	defer run.Close()
	_, err = run.Result(stdcontext.Background())
	if !errors.Is(err, ErrMaxToolRounds) {
		t.Fatalf("want ErrMaxToolRounds, got %v", err)
	}
	// Cap 2 = the initial call plus two executed rounds; the fourth model call
	// never happens.
	if got := cfg.callCount(); got != 3 {
		t.Fatalf("model calls = %d, want 3", got)
	}
	evs, herr := run.Stream().History().GetEvents()
	if herr != nil {
		t.Fatalf("history: %v", herr)
	}
	if fixes := events.CloseUnansweredToolCalls(evs); len(fixes) != 0 {
		t.Fatalf("transcript kept %d unanswered tool calls after the round cap", len(fixes))
	}
}
