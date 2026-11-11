package eval

import (
	"fmt"
	"io"
	"os"
	"sync"
)

// Reporter observes a Run's progress (WithReporter). CaseEvaluated is called
// once per case as soon as it has been graded — from the case's goroutine, so
// with concurrency above 1 calls overlap and an implementation must be safe
// for concurrent use; the CaseResult is read-only. RunCompleted runs once all
// cases are in, before the result is stored.
//
// AGO-DEVIATION: Python's console_reporter subscribes to eval lifecycle
// events on a stream (EvalStarted, TaskEvaluated, VariantStarted, ...). AGo
// has no such events yet (D-070) and no variants, so a Reporter is a Run
// callback set instead, covering the single-run lifecycle only; variant and
// pairwise lines have no counterpart.
type Reporter interface {
	RunStarted(total int, label string)
	CaseEvaluated(CaseResult)
	RunCompleted(*RunResult)
}

// ConsoleReporter prints a Run's progress as plain lines (Python parity:
// reporters.py console_reporter). Safe for concurrent use.
type ConsoleReporter struct {
	mu sync.Mutex
	w  io.Writer
}

// NewConsoleReporter returns a reporter writing to w; nil means os.Stdout.
func NewConsoleReporter(w io.Writer) *ConsoleReporter {
	if w == nil {
		w = os.Stdout
	}
	return &ConsoleReporter{w: w}
}

// RunStarted prints "[label] running N case(s)...".
func (c *ConsoleReporter) RunStarted(total int, label string) {
	tag := ""
	if label != "" {
		tag = "[" + label + "] "
	}
	c.printf("%srunning %d case(s)...\n", tag, total)
}

// CaseEvaluated prints "    name: P/N passed" — P of the case's N scorers.
func (c *ConsoleReporter) CaseEvaluated(cr CaseResult) {
	passed := 0
	for _, s := range cr.Scores {
		if s.Passed {
			passed++
		}
	}
	c.printf("    %s: %d/%d passed\n", cr.Name, passed, len(cr.Scores))
}

// RunCompleted prints "  ✓ done: P/N case(s) passed".
func (c *ConsoleReporter) RunCompleted(res *RunResult) {
	passed := 0
	for _, cr := range res.Cases {
		if cr.Passed {
			passed++
		}
	}
	c.printf("  ✓ done: %d/%d case(s) passed\n", passed, len(res.Cases))
}

func (c *ConsoleReporter) printf(format string, args ...any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	fmt.Fprintf(c.w, format, args...)
}
