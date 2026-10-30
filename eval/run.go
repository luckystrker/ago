package eval

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/luckystrker/ago/agent"
)

// Case is one unit of evaluation: a prompt for the agent, an optional Expected
// answer for reference-based scorers (the suite author typically builds
// Correctness{Expected: c.Expected} from it), and the scorers that grade the
// case's trace. Python parity: dataset/task.py Task, reduced to the fields the
// AGo contract carries; tags/metadata and JSONL loading are deferred (D-051).
type Case struct {
	Name     string   `json:"name"`
	Input    string   `json:"input"`
	Expected string   `json:"expected,omitempty"`
	Scorers  []Scorer `json:"-"`
}

// Suite is an ordered collection of cases (Python parity: dataset/suite.py).
type Suite []Case

// CaseResult is one case's outcome: the agent's final answer, its trace, the
// scorers' verdicts, and whether every scorer passed. A case with no scores is
// not counted as passed — nothing graded it. Err carries the ask failure, if
// any (Python parity: the exception on TaskResult/Trace; the AGo Trace has no
// exception slot, so it lives here — D-051).
type CaseResult struct {
	Name   string  `json:"name"`
	Output string  `json:"output"`
	Trace  *Trace  `json:"trace"`
	Scores []Score `json:"scores"`
	Passed bool    `json:"passed"`
	Err    string  `json:"err,omitempty"`
}

// RunResult is the outcome of one Run: per-case results and the fraction of
// passed cases (Python parity: results/result.py RunResult, reduced to the
// contract fields; per-key pass-rate/score-stats aggregates are deferred).
type RunResult struct {
	Cases    []CaseResult `json:"cases"`
	PassRate float64      `json:"pass_rate"`
}

// RunOption tunes a Run.
type RunOption func(*runOptions)

type runOptions struct {
	concurrency int
}

// WithConcurrency caps how many cases ask the agent at once (Python parity:
// run_agent's concurrency, default 4, clamped to >= 1). 1 runs the cases
// strictly in suite order on the calling goroutine — the deterministic mode a
// per-case scripted model double needs. Higher caps overlap cases; the agent
// is shared, every case runs on its own stream, and streams serialize their
// turns independently (agent.Agent.locks), so cases overlap without
// interleaving.
func WithConcurrency(n int) RunOption {
	return func(o *runOptions) { o.concurrency = n }
}

// DefaultConcurrency is how many cases Run asks at once (Python parity:
// runner.py concurrency=4).
const DefaultConcurrency = 4

// Run executes produce-then-grade: each case asks the agent, the reply history
// becomes a Trace (BuildTrace), the case's scorers grade it, and PassRate
// tallies the passed cases. Cases run in parallel up to DefaultConcurrency
// (WithConcurrency overrides; Python parity: runner.py's asyncio.Semaphore) —
// each goroutine writes its own CaseResult slot, so the result needs no locks,
// and Cases stays in suite order regardless of completion order. A failing ask
// does not abort the run — the case records Err, whatever history accumulated,
// and failing scores (Python parity: run_agent's error traces).
func Run(ctx context.Context, a *agent.Agent, s Suite, opts ...RunOption) (*RunResult, error) {
	if a == nil {
		return nil, errors.New("eval: Run needs an agent")
	}
	o := runOptions{concurrency: DefaultConcurrency}
	for _, opt := range opts {
		opt(&o)
	}
	limit := o.concurrency
	if limit < 1 {
		limit = 1
	}
	if limit > len(s) {
		limit = len(s)
	}
	res := &RunResult{Cases: make([]CaseResult, len(s))}
	if len(s) == 0 {
		return res, nil
	}
	if limit == 1 {
		// Deterministic mode: suite order, no goroutines (see WithConcurrency).
		for i := range s {
			res.Cases[i] = runCase(ctx, a, s[i])
		}
	} else {
		// The semaphore bounds the in-flight cases; each slot writes
		// res.Cases[i] and nothing else, so wg.Wait is the only
		// synchronization the result needs.
		sem := make(chan struct{}, limit)
		var wg sync.WaitGroup
		for i := range s {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				res.Cases[i] = runCase(ctx, a, s[i])
			}(i)
		}
		wg.Wait()
	}
	passed := 0
	for _, cr := range res.Cases {
		if cr.Passed {
			passed++
		}
	}
	if len(res.Cases) > 0 {
		res.PassRate = float64(passed) / float64(len(res.Cases))
	}
	return res, nil
}

func runCase(ctx context.Context, a *agent.Agent, c Case) CaseResult {
	cr := CaseResult{Name: c.Name, Trace: &Trace{AgentName: a.Name()}, Scores: []Score{}}
	start := time.Now()

	run, rerr := a.Run(ctx, c.Input)
	if rerr == nil {
		defer run.Close()
		if rerr = run.Start(ctx); rerr == nil {
			var reply *agent.AgentReply
			reply, rerr = run.Result(ctx)
			if reply != nil {
				cr.Output = reply.Body()
			}
		}
	}
	cr.Trace = traceFromRun(a.Name(), run, start)
	cr.Err = errText(rerr)
	cr.Trace.Err = cr.Err // the failure attribution scorer reads the ask error off the trace

	for _, s := range c.Scorers {
		cr.Scores = append(cr.Scores, ScoreWith(ctx, s, cr.Trace))
	}
	cr.Passed = len(cr.Scores) > 0
	for _, s := range cr.Scores {
		if !s.Passed {
			cr.Passed = false
		}
	}
	return cr
}

// traceFromRun rebuilds the trace from whatever the run's transcript holds —
// partial histories on a failed ask included (Python parity: the error trace).
func traceFromRun(name string, run *agent.AgentRun, start time.Time) *Trace {
	fallback := &Trace{AgentName: name, StartedAt: start, FinishedAt: time.Now(), LLMCalls: []LLMCall{}, ToolCalls: []ToolCall{}}
	if run == nil {
		return fallback
	}
	evs, err := run.Stream().History().GetEvents()
	if err != nil || len(evs) == 0 {
		return fallback
	}
	t := BuildTrace(evs)
	t.AgentName = name
	if t.StartedAt.IsZero() {
		t.StartedAt = start
	}
	if t.FinishedAt.IsZero() {
		t.FinishedAt = time.Now()
	}
	return t
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
