package agent

import (
	stdcontext "context"
	"sync"
	"testing"
	"time"

	"github.com/luckystrker/ago/config"
	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
)

// overlapProbe records how many child model calls run at the same time. A call
// waits briefly for a sibling to overlap, so a missing cap shows up reliably
// instead of depending on goroutine scheduling.
type overlapProbe struct {
	mu     sync.Mutex
	active int
	peak   int
	calls  int
	two    chan struct{}
	twoOne sync.Once
}

func newOverlapProbe() *overlapProbe { return &overlapProbe{two: make(chan struct{})} }

func (p *overlapProbe) Provider() config.Provider                        { return "test" }
func (p *overlapProbe) Model() string                                    { return "test" }
func (p *overlapProbe) Clone(map[string]any) (config.ModelConfig, error) { return p, nil }
func (p *overlapProbe) Create() (config.LLMClient, error)                { return &overlapClient{p: p}, nil }

func (p *overlapProbe) stats() (peak, calls int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.peak, p.calls
}

type overlapClient struct{ p *overlapProbe }

func (c *overlapClient) Call(stdcontext.Context, []events.Event, *agocontext.Context, config.CallOptions) (*events.ModelResponse, error) {
	p := c.p
	p.mu.Lock()
	p.active++
	p.calls++
	if p.active > p.peak {
		p.peak = p.active
	}
	if p.active >= 2 {
		p.twoOne.Do(func() { close(p.two) })
	}
	p.mu.Unlock()

	select {
	case <-p.two:
	case <-time.After(150 * time.Millisecond):
	}

	p.mu.Lock()
	p.active--
	p.mu.Unlock()
	return textResp("child done"), nil
}

// TestTaskConfigMaxConcurrencyIsOneSharedPerAgentPool pins that one agent's
// subtask cap holds across concurrent parent turns and covers run_subtask, not
// just run_subtasks (Python parity: Agent._task_slots_for — one semaphore per
// agent shared by _spawn_subtask and run_subtasks).
func TestTaskConfigMaxConcurrencyIsOneSharedPerAgentPool(t *testing.T) {
	probe := newOverlapProbe()
	// A fresh script per client: each concurrent parent turn replays its own.
	parentCfg := freshScriptConfig{script: []*events.ModelResponse{
		toolResp("run_subtask", `{"task":"x"}`),
		textResp("wrapped"),
	}}
	a := New("parent", "", WithConfig(parentCfg),
		WithTasks(TaskConfig{Config: probe, MaxConcurrency: 1}))

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := a.Ask(stdcontext.Background(), "go"); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("ask: %v", err)
	}

	peak, calls := probe.stats()
	if calls != 2 {
		t.Fatalf("child model calls = %d, want 2 (one per parent turn)", calls)
	}
	if peak != 1 {
		t.Fatalf("peak concurrent child calls = %d, want 1 (one shared pool per agent)", peak)
	}
}
