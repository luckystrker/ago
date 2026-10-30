package agent

import (
	stdcontext "context"
	"errors"
	"strings"
	"testing"

	"github.com/luckystrker/ago/aggregate"
	"github.com/luckystrker/ago/compact"
	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/knowledge"
	"github.com/luckystrker/ago/stream"
	agotest "github.com/luckystrker/ago/testing"
)

// TestKnowledgeToolAnswersRead drives a full turn where the scripted model
// calls the auto-injected knowledge tool with action=read; the tool result in
// the transcript must carry the store content.
func TestKnowledgeToolAnswersRead(t *testing.T) {
	store := knowledge.NewMemoryStore()
	if err := store.Write("/memory/working.md", []byte("espresso machine decision is pending")); err != nil {
		t.Fatalf("seed store: %v", err)
	}
	cfg := agotest.NewTestConfig(
		toolResp("knowledge", `{"action":"read","path":"/memory/working.md"}`),
		textResp("done"),
	)
	a := New("kp", "", WithConfig(cfg), WithKnowledgeConfig(knowledge.NewConfig(store)))
	reply, err := a.Ask(stdcontext.Background(), "what do you remember?")
	if err != nil {
		t.Fatalf("ask: %v", err)
	}
	if reply.Body() != "done" {
		t.Fatalf("body: %q", reply.Body())
	}

	evs, err := reply.History().GetEvents()
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	var sawRead bool
	for _, ev := range evs {
		if tre, ok := events.AsValue(ev).(events.ToolResultsEvent); ok {
			for _, r := range tre.Results {
				if r.Name != "knowledge" {
					continue
				}
				sawRead = true
				if txt := events.RenderForPrompt(r.Result.Parts[0]); !strings.Contains(txt, "espresso machine decision") {
					t.Fatalf("knowledge read result = %q, want store content", txt)
				}
			}
		}
	}
	if !sawRead {
		t.Fatal("transcript has no knowledge tool result")
	}
}

// TestKnowledgeConfigWiring checks the construction-time wiring of
// WithKnowledgeConfig end to end: the store lands in the dependencies under
// knowledge.DepKey, the bootstrap lays out the store behind the sentinel, and
// the event log holds the full turn.
func TestKnowledgeConfigWiring(t *testing.T) {
	store := knowledge.NewMemoryStore()
	var dep any
	cfg := agotest.NewTestConfig(textResp("done"))
	a := New("kp", "", WithConfig(cfg),
		WithKnowledgeConfig(knowledge.NewConfig(store)),
		WithPromptHook(func(cx *agocontext.Context) ([]string, error) {
			dep, _ = cx.Dependency(knowledge.DepKey)
			return nil, nil
		}),
	)
	if _, err := a.Ask(stdcontext.Background(), "hi"); err != nil {
		t.Fatalf("ask: %v", err)
	}

	// (а) dependency registration.
	if s, ok := dep.(knowledge.Store); !ok || s == nil {
		t.Fatalf("dependency %q is %T, want knowledge.Store", knowledge.DepKey, dep)
	}

	// (г) bootstrap: sentinel with the actor name + standard layout.
	if !store.Exists(knowledge.SentinelPath) {
		t.Fatal("bootstrap sentinel missing after ask")
	}
	if sent, err := store.Read(knowledge.SentinelPath); err != nil || string(sent) != "kp" {
		t.Fatalf("sentinel = %q (%v), want actor name %q", sent, err, "kp")
	}
	if !store.Exists("/SKILL.md") {
		t.Fatal("root SKILL.md missing after ask")
	}

	// (в) event log: one stream log holding the whole turn.
	w := knowledge.NewEventLogWriter(store)
	streams, err := w.Streams()
	if err != nil {
		t.Fatalf("streams: %v", err)
	}
	if len(streams) != 1 {
		t.Fatalf("event log streams = %v, want exactly one", streams)
	}
	logged, err := w.Replay(streams[0])
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if len(logged) < 2 {
		t.Fatalf("event log holds %d events, want at least the request and the response", len(logged))
	}
	var finalBody string
	for _, ev := range logged {
		if mr, ok := events.AsValue(ev).(events.ModelResponse); ok {
			finalBody = mr.Content()
		}
	}
	if finalBody != "done" {
		t.Fatalf("logged final response = %q, want %q", finalBody, "done")
	}

	// (б) the tool is exposed by default.
	var hasKnowledgeTool bool
	for _, tool := range a.Tools() {
		if tool.Name() == "knowledge" {
			hasKnowledgeTool = true
		}
	}
	if !hasKnowledgeTool {
		t.Fatal("knowledge tool not exposed with ExposeTool=true")
	}
}

// TestKnowledgeBootstrapSkippedForFreshAgent checks the cross-agent sentinel:
// a second agent over the same store must not re-run the layout.
func TestKnowledgeBootstrapSkippedForFreshAgent(t *testing.T) {
	store := knowledge.NewDiskStore(t.TempDir())
	// One shared script cursor: response 1 for agent 1, response 2 for agent 2.
	cfg := agotest.NewTestConfig(textResp("one"), textResp("two"))

	noTool := knowledge.Config{Store: store, WriteEventLog: true}
	a1 := New("kp", "", WithConfig(cfg), WithKnowledgeConfig(noTool))
	if _, err := a1.Ask(stdcontext.Background(), "hi"); err != nil {
		t.Fatalf("agent 1 ask: %v", err)
	}
	if err := store.Write("/memory/SKILL.md", []byte("TAMPERED")); err != nil {
		t.Fatalf("tamper: %v", err)
	}

	a2 := New("kp", "", WithConfig(cfg), WithKnowledgeConfig(noTool))
	if _, err := a2.Ask(stdcontext.Background(), "hi again"); err != nil {
		t.Fatalf("agent 2 ask: %v", err)
	}
	got, err := store.Read("/memory/SKILL.md")
	if err != nil {
		t.Fatalf("read tampered file: %v", err)
	}
	if string(got) != "TAMPERED" {
		t.Fatalf("second agent re-bootstrapped the store: %q", got)
	}
	sent, err := store.Read(knowledge.SentinelPath)
	if err != nil {
		t.Fatalf("read sentinel: %v", err)
	}
	if string(sent) != "kp" {
		t.Fatalf("sentinel = %q, want the first agent's name", sent)
	}
}

// TestKnowledgeNoToolWhenNotExposed checks ExposeTool=false: no knowledge tool,
// and the root SKILL.md uses the no-tool intro.
func TestKnowledgeNoToolWhenNotExposed(t *testing.T) {
	store := knowledge.NewMemoryStore()
	cfg := agotest.NewTestConfig(textResp("done"))
	c := knowledge.NewConfig(store)
	c.ExposeTool = false
	a := New("kp", "", WithConfig(cfg), WithKnowledgeConfig(c))
	for _, tool := range a.Tools() {
		if tool.Name() == "knowledge" {
			t.Fatal("knowledge tool exposed despite ExposeTool=false")
		}
	}
	if _, err := a.Ask(stdcontext.Background(), "hi"); err != nil {
		t.Fatalf("ask: %v", err)
	}
	root, err := store.Read("/SKILL.md")
	if err != nil {
		t.Fatalf("read root SKILL.md: %v", err)
	}
	if strings.Contains(string(root), "`knowledge` tool") {
		t.Errorf("no-tool store advertises the knowledge tool:\n%s", root)
	}
}

// TestKnowledgeEventLogDisabled checks WriteEventLog=false: no log file lands
// in the store.
func TestKnowledgeEventLogDisabled(t *testing.T) {
	store := knowledge.NewMemoryStore()
	cfg := agotest.NewTestConfig(textResp("done"))
	c := knowledge.NewConfig(store)
	c.WriteEventLog = false
	a := New("kp", "", WithConfig(cfg), WithKnowledgeConfig(c))
	if _, err := a.Ask(stdcontext.Background(), "hi"); err != nil {
		t.Fatalf("ask: %v", err)
	}
	streams, err := knowledge.NewEventLogWriter(store).Streams()
	if err != nil {
		t.Fatalf("streams: %v", err)
	}
	if len(streams) != 0 {
		t.Fatalf("event log written despite WriteEventLog=false: %v", streams)
	}
}

// failingLogStore fails writes to stream log files, to exercise the
// EventLogFailed path without breaking the bootstrap (whose SKILL.md writes
// share the log directory).
type failingLogStore struct {
	knowledge.Store
}

func (f failingLogStore) Write(path string, data []byte) error {
	if strings.HasSuffix(path, ".jsonl") {
		return errors.New("disk full")
	}
	return f.Store.Write(path, data)
}

// TestKnowledgeEventLogFailureEmitsEvent checks that a failing log write sends
// EventLogFailed on the stream and the reply is still delivered (EventLogFailed
// contract: the failure never interrupts the turn). EventLogFailed is
// transient — it reaches subscribers, not the transcript — so the test watches
// the stream the way an observer would.
func TestKnowledgeEventLogFailureEmitsEvent(t *testing.T) {
	store := failingLogStore{Store: knowledge.NewMemoryStore()}
	base := stream.NewMemoryStream()
	var failed *events.EventLogFailed
	base.Subscribe(func(ev events.Event) error {
		if f, ok := ev.(*events.EventLogFailed); ok {
			failed = f
		}
		return nil
	}, stream.SubscribeOptions{})
	cfg := agotest.NewTestConfig(textResp("delivered"))
	a := New("kp", "", WithConfig(cfg), WithStreamBase(base), WithKnowledgeConfig(knowledge.NewConfig(store)))
	reply, err := a.Ask(stdcontext.Background(), "hi")
	if err != nil {
		t.Fatalf("ask: %v", err)
	}
	if reply.Body() != "delivered" {
		t.Fatalf("body: %q", reply.Body())
	}
	if failed == nil {
		t.Fatal("no EventLogFailed on the stream after a failed log write")
	}
	if failed.Agent != "kp" {
		t.Errorf("EventLogFailed.Agent = %q, want %q", failed.Agent, "kp")
	}
	if failed.Error == "" {
		t.Error("EventLogFailed.Error is empty")
	}
}

// TestKnowledgeNilStoreOptionIsNoop checks that a Config without a store
// contributes nothing (the Ask then fails the usual way, with no config).
func TestKnowledgeNilStoreOptionIsNoop(t *testing.T) {
	a := New("kp", "p", WithKnowledgeConfig(knowledge.Config{}))
	if _, err := a.Ask(stdcontext.Background(), "hi"); !errors.Is(err, ErrConfigNotProvided) {
		t.Fatalf("want ErrConfigNotProvided, got %v", err)
	}
	if tools := a.Tools(); len(tools) != 0 {
		t.Fatalf("nil-store config added tools: %v", tools)
	}
}

// compactStub is a compact.Strategy double: it records the span it is handed
// and answers a fixed text.
type compactStub struct {
	last []events.Event
}

func (s *compactStub) Summarize(_ *agocontext.Context, evs []events.Event) (string, error) {
	s.last = evs
	return "COMPACT-STUB-SUMMARY", nil
}

// aggregateStub is an aggregate.Strategy double: it records its inputs and
// answers a fixed merged text.
type aggregateStub struct {
	summaries []string
	evs       []events.Event
}

func (s *aggregateStub) Aggregate(_ *agocontext.Context, summaries []string, evs []events.Event) (string, error) {
	s.summaries = summaries
	s.evs = evs
	return "AGG-STUB-SUMMARY", nil
}

// alwaysTrigger fires on every turn; one type serves both compact.Trigger and
// aggregate.Trigger (identical method shapes).
type alwaysTrigger struct{}

func (alwaysTrigger) ShouldTrigger([]events.Event, events.Usage) bool { return true }

var (
	_ compact.Trigger    = alwaysTrigger{}
	_ aggregate.Trigger  = alwaysTrigger{}
	_ compact.Strategy   = (*compactStub)(nil)
	_ aggregate.Strategy = (*aggregateStub)(nil)
)

// twoTurns runs a scripted ask plus a continuation over one stream: after the
// second model call the pre-processing history holds six events — per turn a
// ModelRequest, the call's UsageEvent and the durable ModelResponse (the fake
// config reports 1/2/3 tokens) — four of them conversational. The returned
// snapshot is read after the turn's post-processing, so a wired compaction or
// aggregation pass has already rewritten it; the pre-rewrite counts live on the
// lifecycle events.
func twoTurns(t *testing.T, a *Agent) []events.Event {
	t.Helper()
	reply, err := a.Ask(stdcontext.Background(), "first")
	if err != nil {
		t.Fatalf("ask 1: %v", err)
	}
	reply, err = reply.Ask(stdcontext.Background(), "second")
	if err != nil {
		t.Fatalf("ask 2: %v", err)
	}
	evs, err := reply.History().GetEvents()
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	return evs
}

// TestKnowledgeMiddlewareOptionsWiring checks the construction-time shape of
// applyKnowledgeMiddleware: the compaction factory joins the agent's middleware
// list when a strategy is set (even without a trigger), the aggregation factory
// only when both halves are set, and nothing joins without strategies.
func TestKnowledgeMiddlewareOptionsWiring(t *testing.T) {
	stb := &compactStub{}
	agg := &aggregateStub{}
	trg := alwaysTrigger{}

	t.Run("compact strategy only", func(t *testing.T) {
		a := New("kp", "", WithKnowledgeConfig(knowledge.Config{
			Store:   knowledge.NewMemoryStore(),
			Compact: stb,
		}))
		if len(a.opts.middleware) != 1 {
			t.Fatalf("middleware factories = %d, want 1 (compaction wires without a trigger)", len(a.opts.middleware))
		}
	})

	t.Run("aggregate strategy without trigger", func(t *testing.T) {
		a := New("kp", "", WithKnowledgeConfig(knowledge.Config{
			Store:     knowledge.NewMemoryStore(),
			Aggregate: agg,
		}))
		if len(a.opts.middleware) != 0 {
			t.Fatalf("middleware factories = %d, want 0 (aggregation needs a trigger)", len(a.opts.middleware))
		}
	})

	t.Run("aggregate strategy with trigger", func(t *testing.T) {
		a := New("kp", "", WithKnowledgeConfig(knowledge.Config{
			Store:            knowledge.NewMemoryStore(),
			Aggregate:        agg,
			AggregateTrigger: trg,
		}))
		if len(a.opts.middleware) != 1 {
			t.Fatalf("middleware factories = %d, want 1", len(a.opts.middleware))
		}
	})

	t.Run("both", func(t *testing.T) {
		a := New("kp", "", WithKnowledgeConfig(knowledge.Config{
			Store:            knowledge.NewMemoryStore(),
			Compact:          stb,
			CompactTrigger:   trg,
			Aggregate:        agg,
			AggregateTrigger: trg,
		}))
		if len(a.opts.middleware) != 2 {
			t.Fatalf("middleware factories = %d, want 2 (compaction, then aggregation)", len(a.opts.middleware))
		}
	})

	t.Run("strategies without a store", func(t *testing.T) {
		a := New("kp", "", WithKnowledgeConfig(knowledge.Config{Compact: stb, Aggregate: agg, AggregateTrigger: trg}))
		if len(a.opts.middleware) != 0 {
			t.Fatalf("nil store wired middlewares: %d", len(a.opts.middleware))
		}
	})

	t.Run("store only", func(t *testing.T) {
		a := New("kp", "", WithKnowledgeConfig(knowledge.NewConfig(knowledge.NewMemoryStore())))
		if len(a.opts.middleware) != 0 {
			t.Fatalf("store-only config wired middlewares: %d", len(a.opts.middleware))
		}
	})
}

// TestKnowledgeCompactWiring drives two scripted turns through an agent whose
// knowledge Config carries a compaction strategy with EventCountTrigger(2):
// after the second turn (4 conversational events > 2) the old span collapses
// into one CompactionSummary and the lifecycle events carry the agent name —
// the Python constructor wiring (agent.py:846) end to end.
func TestKnowledgeCompactWiring(t *testing.T) {
	store := knowledge.NewMemoryStore()
	base := stream.NewMemoryStream()
	var started, completed []events.Event
	base.Subscribe(func(ev events.Event) error {
		switch e := ev.(type) {
		case *events.CompactionStarted:
			started = append(started, e)
		case *events.CompactionCompleted:
			completed = append(completed, e)
		}
		return nil
	}, stream.SubscribeOptions{})

	stb := &compactStub{}
	cfg := agotest.NewTestConfig(textResp("one"), textResp("two"))
	a := New("kp", "", WithConfig(cfg), WithStreamBase(base),
		WithKnowledgeConfig(knowledge.Config{
			Store:          store,
			ExposeTool:     false,
			WriteEventLog:  true,
			Compact:        stb,
			CompactTrigger: compact.EventCountTrigger(2),
		}))

	evs := twoTurns(t, a)

	if len(started) != 1 {
		t.Fatalf("CompactionStarted count = %d, want 1", len(started))
	}
	s := started[0].(*events.CompactionStarted)
	if s.Agent != "kp" {
		t.Errorf("CompactionStarted.Agent = %q, want %q", s.Agent, "kp")
	}
	// The label identifies the strategy; a pointer-carried double reports the
	// full reflect string (compact.typeLabel: unnamed types fall back to it).
	if !strings.HasSuffix(s.Strategy, "compactStub") {
		t.Errorf("CompactionStarted.Strategy = %q, want the double's type name", s.Strategy)
	}
	if s.EventCount != 6 {
		t.Errorf("CompactionStarted.EventCount = %d, want 6", s.EventCount)
	}
	if len(completed) != 1 {
		t.Fatalf("CompactionCompleted count = %d, want 1", len(completed))
	}
	c := completed[0].(*events.CompactionCompleted)
	if c.Agent != "kp" {
		t.Errorf("CompactionCompleted.Agent = %q, want %q", c.Agent, "kp")
	}
	if c.EventsBefore != 6 || c.EventsAfter != 4 {
		t.Errorf("CompactionCompleted before/after = %d/%d, want 6/4 (summary + kept tail + telemetry)",
			c.EventsBefore, c.EventsAfter)
	}
	if c.LLMCalls != 0 {
		t.Errorf("CompactionCompleted.LLMCalls = %d, want 0 (the stub sends no usage)", c.LLMCalls)
	}

	// The strategy saw the old conversational span, not the whole history.
	if len(stb.last) != 2 {
		t.Fatalf("strategy received %d events, want the 2 conversational events before the kept tail", len(stb.last))
	}

	// The history now opens with the stub's summary; the second turn's events
	// ride behind it verbatim.
	var summaries []events.CompactionSummary
	for _, ev := range evs {
		if cs, ok := events.AsValue(ev).(events.CompactionSummary); ok {
			summaries = append(summaries, cs)
		}
	}
	if len(summaries) != 1 {
		t.Fatalf("history holds %d CompactionSummary events, want 1", len(summaries))
	}
	if summaries[0].Content != "COMPACT-STUB-SUMMARY" {
		t.Errorf("summary content = %q, want the stub's answer", summaries[0].Content)
	}
	if n := len(evs); n != 4 {
		t.Errorf("final history holds %d events, want 4 (summary + the second turn's three)", n)
	}
}

// TestKnowledgeAggregateWiring drives two scripted turns through an agent whose
// knowledge Config carries an aggregation strategy with an always-firing
// trigger: after the turn the superseded span folds into one CompactionSummary,
// bracketed by AggregationStarted/Completed — the Python constructor wiring
// (agent.py:851) end to end.
func TestKnowledgeAggregateWiring(t *testing.T) {
	store := knowledge.NewMemoryStore()
	base := stream.NewMemoryStream()
	var started, completed, failed []events.Event
	base.Subscribe(func(ev events.Event) error {
		switch e := ev.(type) {
		case *events.AggregationStarted:
			started = append(started, e)
		case *events.AggregationCompleted:
			completed = append(completed, e)
		case *events.AggregationFailed:
			failed = append(failed, e)
		}
		return nil
	}, stream.SubscribeOptions{})

	agg := &aggregateStub{}
	cfg := agotest.NewTestConfig(textResp("one"), textResp("two"))
	a := New("kp", "", WithConfig(cfg), WithStreamBase(base),
		WithKnowledgeConfig(knowledge.Config{
			Store:            store,
			ExposeTool:       false,
			Aggregate:        agg,
			AggregateTrigger: alwaysTrigger{},
		}))

	evs := twoTurns(t, a)

	if len(failed) != 0 {
		t.Fatalf("AggregationFailed sent: %v", failed)
	}
	// The always-firing trigger fires once per turn: the first turn's history
	// (cut = 1 conversational event before the kept tail) already folds, and so
	// does the second's.
	if len(started) != 2 || len(completed) != 2 {
		t.Fatalf("lifecycle counts started/completed = %d/%d, want 2/2", len(started), len(completed))
	}
	c := completed[1].(*events.AggregationCompleted)
	if c.EventsBefore != 6 || c.EventsAfter != 4 {
		t.Errorf("final AggregationCompleted before/after = %d/%d, want 6/4",
			c.EventsBefore, c.EventsAfter)
	}

	// The second fold merges the first fold's summary plus the conversational
	// events below the raw-index tail (the first turn's response and the second
	// request); the kept tail stays verbatim.
	if len(agg.summaries) != 1 || agg.summaries[0] != "AGG-STUB-SUMMARY" {
		t.Errorf("strategy saw summaries %v, want the first fold's answer", agg.summaries)
	}
	if len(agg.evs) != 2 {
		t.Errorf("strategy saw %d superseded events on the second fold, want 2", len(agg.evs))
	}
	if n := len(evs); n != 4 {
		t.Errorf("final history holds %d events, want 4 (summary + telemetry + kept tail)", n)
	}

	var summaries int
	var content string
	for _, ev := range evs {
		if cs, ok := events.AsValue(ev).(events.CompactionSummary); ok {
			summaries++
			content = cs.Content
		}
	}
	if summaries != 1 {
		t.Fatalf("history holds %d CompactionSummary events, want 1", summaries)
	}
	if content != "AGG-STUB-SUMMARY" {
		t.Errorf("merged summary = %q, want the stub's answer", content)
	}
}

// TestKnowledgeMiddlewareOff checks the silent shapes: with the strategy fields
// unset (or a trigger-less compaction strategy, which wires as a pass-through)
// two scripted turns leave the history untouched and send no lifecycle events.
func TestKnowledgeMiddlewareOff(t *testing.T) {
	cases := []struct {
		name string
		cfg  func(store knowledge.Store) knowledge.Config
	}{
		{"store only", func(store knowledge.Store) knowledge.Config {
			return knowledge.Config{Store: store, ExposeTool: false}
		}},
		{"compact strategy, nil trigger", func(store knowledge.Store) knowledge.Config {
			return knowledge.Config{Store: store, ExposeTool: false, Compact: &compactStub{}}
		}},
		{"aggregate strategy, nil trigger", func(store knowledge.Store) knowledge.Config {
			return knowledge.Config{Store: store, ExposeTool: false, Aggregate: &aggregateStub{}}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base := stream.NewMemoryStream()
			var lifecycle []events.Event
			base.Subscribe(func(ev events.Event) error {
				switch ev.(type) {
				case *events.CompactionStarted, *events.AggregationStarted:
					lifecycle = append(lifecycle, ev)
				}
				return nil
			}, stream.SubscribeOptions{})
			cfg := agotest.NewTestConfig(textResp("one"), textResp("two"))
			a := New("kp", "", WithConfig(cfg), WithStreamBase(base),
				WithKnowledgeConfig(tc.cfg(knowledge.NewMemoryStore())))
			evs := twoTurns(t, a)
			if len(lifecycle) != 0 {
				t.Fatalf("lifecycle events sent without wiring: %v", lifecycle)
			}
			if n := len(evs); n != 6 {
				t.Errorf("history holds %d events, want the untouched 6", n)
			}
			for _, ev := range evs {
				if _, ok := events.AsValue(ev).(events.CompactionSummary); ok {
					t.Fatal("history was rewritten without wiring")
				}
			}
		})
	}
}
