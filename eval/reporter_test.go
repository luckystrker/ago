package eval

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestConsoleReporterRun(t *testing.T) {
	a := scriptedAgent(t, textResp("Paris"), textResp("Lyon"))
	suite := Suite{
		{Name: "right", Input: "capital?", Scorers: []Scorer{
			Correctness{Expected: "Paris", Mode: MatchContains}, TokenBudget{Max: 100},
		}},
		{Name: "wrong", Input: "capital?", Scorers: []Scorer{
			Correctness{Expected: "Paris", Mode: MatchContains}, TokenBudget{Max: 100},
		}},
	}
	var buf bytes.Buffer
	_, err := Run(context.Background(), a, suite,
		WithConcurrency(1), WithLabel("prompt-v2"), WithReporter(NewConsoleReporter(&buf)))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	want := "[prompt-v2] running 2 case(s)...\n" +
		"    right: 2/2 passed\n" +
		"    wrong: 1/2 passed\n" +
		"  ✓ done: 1/2 case(s) passed\n"
	if buf.String() != want {
		t.Fatalf("console output:\n%q\nwant:\n%q", buf.String(), want)
	}
}

func TestConsoleReporterNoLabelEmptySuite(t *testing.T) {
	var buf bytes.Buffer
	if _, err := Run(context.Background(), scriptedAgent(t), Suite{}, WithReporter(NewConsoleReporter(&buf))); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := buf.String(); !strings.HasPrefix(got, "running 0 case(s)...\n") || !strings.HasSuffix(got, "0/0 case(s) passed\n") {
		t.Fatalf("output = %q", got)
	}
}

// TestConsoleReporterConcurrentCases: cases overlap, every line still
// arrives whole (the reporter serializes writes).
func TestConsoleReporterConcurrentCases(t *testing.T) {
	const n = 6
	a := scriptedAgent(t, rendezvousResponses(n)...)
	var buf bytes.Buffer
	if _, err := Run(context.Background(), a, rendezvousSuite(n), WithConcurrency(n), WithReporter(NewConsoleReporter(&buf))); err != nil {
		t.Fatalf("run: %v", err)
	}
	lines := strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
	if len(lines) != n+2 {
		t.Fatalf("got %d lines, want %d:\n%s", len(lines), n+2, buf.String())
	}
	for _, l := range lines[1 : len(lines)-1] {
		if !strings.HasPrefix(l, "    ") || !strings.HasSuffix(l, " passed") {
			t.Errorf("garbled line %q", l)
		}
	}
}
