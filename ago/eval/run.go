package eval

import (
	"context"
	"errors"
	"time"

	"ago/agent"
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

// Run executes produce-then-grade: each case asks the agent, the reply history
// becomes a Trace (BuildTrace), the case's scorers grade it, and PassRate
// tallies the passed cases. A failing ask does not abort the run — the case
// records Err, whatever history accumulated, and failing scores (Python parity:
// run_agent's error traces). AGO-DEVIATION: cases run sequentially; Python
// bounds them with a concurrency semaphore (default 4) — deferred, D-051.
func Run(ctx context.Context, a *agent.Agent, s Suite) (*RunResult, error) {
	if a == nil {
		return nil, errors.New("eval: Run needs an agent")
	}
	res := &RunResult{Cases: make([]CaseResult, 0, len(s))}
	for _, c := range s {
		res.Cases = append(res.Cases, runCase(ctx, a, c))
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
