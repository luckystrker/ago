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
