// Command agobench measures the AGo framework overhead — event dispatch, the
// conversation stream, middleware wiring, the tool executor, history — on
// scripted, network-free scenarios shared with the Python AG2 side of the
// benchmark wave. The model is a local non-recording fake (scriptFakeConfig):
// it replays a queue of ModelResponses and, for streaming, publishes
// ModelMessageChunk deltas the way real streaming clients do (the LLMClient
// contract: stream progress goes to conv.Send before the response returns). It
// records nothing — Python's TestClient (ag2/ag2/testing.py) pops its script
// without recording either, so no recording work lands in the timed window on
// either side.
//
// Every iteration constructs a fresh agent and a fresh fake config, so a
// conversation never shares mutable state with the previous one. No logging
// happens in the hot path; data is deterministic; GC stays at its defaults.
//
// Output is a JSON array of ScenarioResult written to -out.
//
// Run: go run ./benchmarks/agobench -scenario=all -warmup=5 -runs=30 -out=results_go.json
package main

import (
	stdctx "context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/luckystrker/ago/agent"
	"github.com/luckystrker/ago/config"
	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/stream"
	agotest "github.com/luckystrker/ago/testing"
	"github.com/luckystrker/ago/tools"
)

// Scenario constants are the exact texts both benchmark sides must use; they
// are repeated verbatim in benchmarks/pybench/bench.py (the COMMON block), so
// the runs stay comparable byte-for-byte: same user texts, same assistant
// replies, same tool arguments on every round, same chunk texts.
const (
	turnUser1 = "Hello, agent."
	turnUser2 = "Continue the conversation."
	turnUser3 = "And one more question."
	reply1    = "Assistant reply 1."
	reply2    = "Assistant reply 2."
	reply3    = "Assistant reply 3."

	toolUserMsg  = "What is 2 + 3?"
	toolCallArgs = `{"a":2,"b":3}`
	toolFinal    = "The sum is 5."
	toolRounds   = 5

	streamUser1  = "Stream a story, please."
	streamUser2  = "Keep going, please."
	streamReply1 = "Streamed reply one."
	streamReply2 = "Streamed reply two."
	streamTurns  = 2

	chunksPerTurn           = 20
	concurrentConversations = 50

	benchPrompt = "You are a benchmark agent."
)

// ScenarioResult is one row of the output JSON. All durations are milliseconds.
type ScenarioResult struct {
	Side      string    `json:"side"`
	Scenario  string    `json:"scenario"`
	Warmup    int       `json:"warmup"`
	Runs      int       `json:"runs"`
	SamplesMs []float64 `json:"samplesMs"`
	MedianMs  float64   `json:"medianMs"`
	P95Ms     float64   `json:"p95Ms"`
	MinMs     float64   `json:"minMs"`
	MaxMs     float64   `json:"maxMs"`
	Notes     string    `json:"notes"`
	MemNote   string    `json:"memNote"`
}

// Scripted responses are shared by pointer across fresh conversations. They
// are stamped once at startup (init): stream.Send stamps a zero CreatedAt in
// place, which on shared pointers would be a write raced by the concurrent
// scenario's goroutines; a pre-stamped response makes every later Send a
// read-only touch, so sharing keeps setup out of the timed path without any
// mutation risk. Zero usage on every response: no UsageEvent rides the hot
// path on either side of the benchmark.
var (
	plainScript = stampScripts([]*events.ModelResponse{
		{Message: &events.ModelMessage{Content: reply1}},
		{Message: &events.ModelMessage{Content: reply2}},
		{Message: &events.ModelMessage{Content: reply3}},
	})
	twoTurnScript = plainScript[:2]
	toolScript    = stampScripts(buildToolScript())
	streamScript  = stampScripts([]*events.ModelResponse{
		{Message: &events.ModelMessage{Content: streamReply1}},
		{Message: &events.ModelMessage{Content: streamReply2}},
	})
	streamChunks = buildStreamChunks()
	addTool      = mustAddTool()
)

// stampScripts pre-stamps every scripted response so no goroutine ever writes
// to a shared one (see the comment on plainScript).
func stampScripts(rs []*events.ModelResponse) []*events.ModelResponse {
	now := time.Now()
	for _, r := range rs {
		r.StampNow(now)
	}
	return rs
}

// buildToolScript is tool_roundtrip: toolRounds responses each requesting
// add({"a":2,"b":3}), then one final text response.
func buildToolScript() []*events.ModelResponse {
	script := make([]*events.ModelResponse, 0, toolRounds+1)
	for i := 1; i <= toolRounds; i++ {
		script = append(script, &events.ModelResponse{
			ToolCalls: events.ToolCallsEvent{Calls: []events.ToolCallEvent{{
				ID:        fmt.Sprintf("call-%d", i),
				Name:      "add",
				Arguments: toolCallArgs,
			}}},
		})
	}
	script = append(script, &events.ModelResponse{
		Message: &events.ModelMessage{Content: toolFinal},
	})
	return script
}

// buildStreamChunks precomputes the deterministic chunk texts once.
func buildStreamChunks() []string {
	chunks := make([]string, chunksPerTurn)
	for i := range chunks {
		chunks[i] = fmt.Sprintf("chunk-%02d", i)
	}
	return chunks
}

// mustAddTool builds the add tool once; schema generation and the bind plan
// are startup setup, not part of a conversation.
func mustAddTool() tools.Tool {
	add, err := tools.NewFunctionTool("add",
		"Add two integers.",
		func(args struct {
			A int `json:"a" agschema:"description=First addend"`
			B int `json:"b" agschema:"description=Second addend"`
		}) (string, error) {
			return strconv.Itoa(args.A + args.B), nil
		})
	if err != nil {
		fmt.Fprintln(os.Stderr, "agobench: build add tool:", err)
		os.Exit(1)
	}
	return add
}

// scriptFakeConfig is the benchmark's scripted model fake for every scenario.
// It implements config.ModelConfig + config.LLMClient, replays one scripted
// ModelResponse per call and, when chunksPerCall > 0, first publishes that many
// transient events.ModelMessageChunk through conv.Send — the real streaming
// contract (config.LLMClient: stream progress goes to conv.Send before the
// response returns, as in config/openaicompat.go sendEvent). Unlike
// agotest.TestConfig it records nothing: Python's TestClient (the mirrored
// fake, ag2/ag2/testing.py) pops its script without recording either, so the
// timed window carries no recording work on either side. One instance serves
// one conversation; the turn cursor is per-conversation state, and a
// conversation never spans goroutines.
type scriptFakeConfig struct {
	script        []*events.ModelResponse
	chunksPerCall int
	turn          int
}

var (
	_ config.ModelConfig = (*scriptFakeConfig)(nil)
	_ config.LLMClient   = (*scriptFakeConfig)(nil)
)

func (c *scriptFakeConfig) Provider() config.Provider { return agotest.ProviderTest }
func (c *scriptFakeConfig) Model() string             { return "test" }

func (c *scriptFakeConfig) Create() (config.LLMClient, error) { return c, nil }

func (c *scriptFakeConfig) Clone(overrides map[string]any) (config.ModelConfig, error) {
	for k, v := range overrides {
		if k != "model" {
			return nil, fmt.Errorf("agobench: unknown scriptFakeConfig override key %q", k)
		}
		if _, ok := v.(string); !ok {
			return nil, fmt.Errorf("agobench: override %q must be a string, got %T", k, v)
		}
	}
	return &scriptFakeConfig{script: c.script, chunksPerCall: c.chunksPerCall}, nil
}

func (c *scriptFakeConfig) Call(_ stdctx.Context, _ []events.Event, conv *agocontext.Context, _ config.CallOptions) (*events.ModelResponse, error) {
	for i := 0; i < c.chunksPerCall; i++ {
		if err := conv.Send(&events.ModelMessageChunk{Content: streamChunks[i]}); err != nil {
			return nil, err
		}
	}
	if c.turn >= len(c.script) {
		return nil, errors.New("agobench: scripted responses exhausted")
	}
	resp := c.script[c.turn]
	c.turn++
	return resp, nil
}

// --- scenario iterations ------------------------------------------------------
//
// One iteration = one full fresh conversation (all conversations for the
// concurrent scenario). Construction of the agent and the fake config sits
// inside the timed window; the scripts themselves are prebuilt above.

func runPlainChat(ctx stdctx.Context) error {
	cfg := &scriptFakeConfig{script: plainScript}
	a := agent.New("bench-plain", benchPrompt, agent.WithConfig(cfg))
	r1, err := a.Ask(ctx, turnUser1)
	if err != nil {
		return err
	}
	r2, err := r1.Ask(ctx, turnUser2)
	if err != nil {
		return err
	}
	r3, err := r2.Ask(ctx, turnUser3)
	if err != nil {
		return err
	}
	if r3.Body() != reply3 {
		return fmt.Errorf("plain_chat: unexpected final body %q", r3.Body())
	}
	return nil
}

func runToolRoundtrip(ctx stdctx.Context) error {
	cfg := &scriptFakeConfig{script: toolScript}
	a := agent.New("bench-tool", benchPrompt, agent.WithConfig(cfg), agent.WithTools(addTool))
	reply, err := a.Ask(ctx, toolUserMsg)
	if err != nil {
		return err
	}
	if reply.Body() != toolFinal {
		return fmt.Errorf("tool_roundtrip: unexpected final body %q", reply.Body())
	}
	return nil
}

func runStreaming(ctx stdctx.Context) error {
	cfg := &scriptFakeConfig{script: streamScript, chunksPerCall: chunksPerTurn}
	a := agent.New("bench-stream", benchPrompt, agent.WithConfig(cfg))
	run, err := a.Run(ctx, streamUser1)
	if err != nil {
		return err
	}
	// Chunk consumer over the public API (Python parity: the per-turn join
	// subscription draining 20 chunks per turn): every ModelMessageChunk the
	// stream dispatches for this conversation is received and counted while the
	// turns run. A turn here completes only after its chunks were dispatched
	// through this subscription plus the durable ModelResponse.
	chunksSeen := 0
	sub := run.Stream().Subscribe(func(ev events.Event) error {
		chunksSeen++
		return nil
	}, stream.SubscribeOptions{Condition: events.TypeOf[events.ModelMessageChunk]()})
	defer run.Stream().Unsubscribe(sub)

	if err := run.Start(ctx); err != nil {
		return err
	}
	r1, err := run.Result(ctx)
	run.Close()
	if err != nil {
		return err
	}
	// Turn 2 continues the same conversation (same stream, so the subscription
	// above keeps receiving); chunksSeen ends up at both turns' total.
	r2, err := r1.Ask(ctx, streamUser2)
	if err != nil {
		return err
	}
	if r2.Body() != streamReply2 {
		return fmt.Errorf("streaming: unexpected final body %q", r2.Body())
	}
	if chunksSeen != streamTurns*chunksPerTurn {
		return fmt.Errorf("streaming: consumed %d chunks, want %d", chunksSeen, streamTurns*chunksPerTurn)
	}
	return nil
}

// runTwoTurnChat is one fresh two-turn conversation; runConcurrent launches
// concurrentConversations of these at once and waits for all of them.
func runTwoTurnChat(ctx stdctx.Context) error {
	cfg := &scriptFakeConfig{script: twoTurnScript}
	a := agent.New("bench-concurrent", benchPrompt, agent.WithConfig(cfg))
	r1, err := a.Ask(ctx, turnUser1)
	if err != nil {
		return err
	}
	r2, err := r1.Ask(ctx, turnUser2)
	if err != nil {
		return err
	}
	if r2.Body() != reply2 {
		return fmt.Errorf("concurrent: unexpected final body %q", r2.Body())
	}
	return nil
}

func runConcurrent(ctx stdctx.Context) error {
	errs := make([]error, concurrentConversations)
	var wg sync.WaitGroup
	for i := 0; i < concurrentConversations; i++ {
		wg.Add(1)
		go func(slot int) {
			defer wg.Done()
			errs[slot] = runTwoTurnChat(ctx)
		}(i)
	}
	wg.Wait()
	return errors.Join(errs...)
}

// --- measurement --------------------------------------------------------------

// Windows clock reality, measured on this machine (go1.24.1 windows/amd64):
// time.Now's monotonic and wall readings both tick at ~0.5ms, so a single
// sub-millisecond iteration times as exactly 0 (probe: 1999/2000 short busy
// loops timed as exactly 0, max tick gap 515.8µs). The standard fix — the same
// one go test -bench uses — is batch timing: one recorded sample is the wall
// time of K consecutive iterations divided by K, so every sample spans many
// clock ticks. Every sample is therefore the per-iteration time in ms,
// estimated over K iterations; K is calibrated per scenario and disclosed in
// the JSON notes.
const (
	batchTargetMs = 10.0
	batchMaxK     = 100_000
)

// calibrateK finds the smallest K such that a steady-state batch of K
// iterations spans at least batchTargetMs wall milliseconds. At each K the
// first batch is discarded — right after warmup the first batch is inflated by
// cold allocator and scheduler state, which would undershoot K. Calibration
// batches are untimed warmup work.
func calibrateK(ctx stdctx.Context, iter func(stdctx.Context) error) (int, error) {
	for k := 1; ; k *= 2 {
		if err := runBatch(ctx, iter, k); err != nil {
			return 0, err
		}
		ms, err := timeBatch(ctx, iter, k)
		if err != nil {
			return 0, err
		}
		if ms >= batchTargetMs || k >= batchMaxK {
			return k, nil
		}
	}
}

func runBatch(ctx stdctx.Context, iter func(stdctx.Context) error, k int) error {
	for i := 0; i < k; i++ {
		if err := iter(ctx); err != nil {
			return err
		}
	}
	return nil
}

func timeBatch(ctx stdctx.Context, iter func(stdctx.Context) error, k int) (float64, error) {
	start := time.Now()
	if err := runBatch(ctx, iter, k); err != nil {
		return 0, err
	}
	return float64(time.Since(start)) / float64(time.Millisecond), nil
}

// measure runs the warmup unrecorded, calibrates the batch size K, then
// records runs samples; sample i = wall time of one K-iteration batch / K
// (per-iteration milliseconds).
func measure(warmup, runs int, iter func(stdctx.Context) error) (samples []float64, k int, err error) {
	ctx := stdctx.Background()
	for i := 0; i < warmup; i++ {
		if err := iter(ctx); err != nil {
			return nil, 0, fmt.Errorf("warmup %d: %w", i+1, err)
		}
	}
	k, err = calibrateK(ctx, iter)
	if err != nil {
		return nil, 0, fmt.Errorf("calibration: %w", err)
	}
	samples = make([]float64, runs)
	for i := 0; i < runs; i++ {
		ms, berr := timeBatch(ctx, iter, k)
		if berr != nil {
			return nil, k, fmt.Errorf("run %d: %w", i+1, berr)
		}
		samples[i] = ms / float64(k)
	}
	return samples, k, nil
}

// summarize sorts a copy and reports median (mean of the two middle values on
// even n), p95 by the nearest-rank method, min and max.
func summarize(samples []float64) (median, p95, min, max float64) {
	sorted := append([]float64(nil), samples...)
	sort.Float64s(sorted)
	n := len(sorted)
	min, max = sorted[0], sorted[n-1]
	if n%2 == 1 {
		median = sorted[n/2]
	} else {
		median = (sorted[n/2-1] + sorted[n/2]) / 2
	}
	idx := int(math.Ceil(0.95*float64(n))) - 1
	if idx > n-1 {
		idx = n - 1
	}
	return median, sorted[idx], min, max
}

const (
	clockNotes = "sampling: Go on Windows has ~0.5ms clock ticks (measured: 1999/2000 short busy loops timed " +
		"as exactly 0, max tick gap 515.8µs), so a single sub-millisecond iteration cannot be resolved; " +
		"each recorded sample is the wall time of K consecutive iterations divided by K (per-iteration ms), " +
		"K auto-calibrated so one steady-state batch spans >=10ms — the same batch-timing approach go test " +
		"-bench uses; Python's perf_counter does not need this."

	commonNotes = "no network; no logging in the hot path; deterministic scripted data; GC left at defaults " +
		"(SetGCPercent untouched, no forced GC); one iteration includes fresh agent + fake config construction; " +
		"scripted responses carry zero usage, so no UsageEvent is dispatched; the fake (scriptFakeConfig) is a " +
		"non-recording scripted client mirroring Python's TestClient — neither side records the history inside " +
		"the timed window; scripted response objects are shared pointers, pre-stamped (CreatedAt) at startup so " +
		"no goroutine ever writes to one."

	plainNotes = "3 turns on one fresh conversation (agent.Ask then two AgentReply.Ask continuations on the " +
		"same stream — turns 2-3 see the accumulated history); fake = scriptFakeConfig with 3 text responses. " +
		"Texts, user: 'Hello, agent.' / 'Continue the conversation.' / 'And one more question.'; assistant: " +
		"'Assistant reply 1.' / 'Assistant reply 2.' / 'Assistant reply 3.'; system prompt 'You are a benchmark " +
		"agent.'; final body is validated. One iteration = the whole conversation."

	toolNotes = "one fresh conversation, one agent.Ask; script = 5 responses each requesting one tool call " +
		"add with arguments {\"a\":2,\"b\":3} (ids call-1..call-5), then a final text response 'The sum is 5.'; " +
		"tool = tools.NewFunctionTool('add') returning strconv.Itoa(a+b) = '5'. Measured = 6 model calls + " +
		"5 tool executions through the tool executor and stream dispatch. User text: 'What is 2 + 3?'. " +
		"One iteration = the whole conversation."

	streamNotes = "fake = scriptFakeConfig with chunksPerCall=20 following the real streaming contract " +
		"(config.LLMClient: stream progress goes to conv.Send before the response returns, as in " +
		"config/openaicompat.go sendEvent): every call publishes 20 transient events.ModelMessageChunk " +
		"('chunk-00'..'chunk-19') via conv.Send, then returns the complete ModelResponse. The conversation " +
		"subscribes a real chunk consumer through the public API (Stream().Subscribe with " +
		"events.TypeOf[events.ModelMessageChunk](), counter in the closure) before the turns run, so all chunk " +
		"dispatches are received and counted; the count is verified (40 = 2 turns x 20) after the final body. " +
		"Texts, user: 'Stream a story, please.' / 'Keep going, please.'; assistant: 'Streamed reply one.' / " +
		"'Streamed reply two.'. One iteration = the whole conversation."

	concurrentNotes = "50 fresh two-turn conversations launched simultaneously via sync.WaitGroup + 50 " +
		"goroutines; one iteration = all 50 together. Texts identical to plain_chat turns 1-2; each goroutine " +
		"builds its own fake config and agent, sharing only the pre-stamped scripted response pointers (stamped " +
		"once at startup, so concurrent Sends only read them); GOMAXPROCS left at default."
)

func main() {
	side := flag.String("side", "go", "side marker recorded in the results")
	scenario := flag.String("scenario", "all", "plain_chat|tool_roundtrip|streaming|concurrent|all")
	warmup := flag.Int("warmup", 5, "warmup iterations per scenario (not recorded)")
	runs := flag.Int("runs", 30, "recorded measurements per scenario")
	out := flag.String("out", "results_go.json", "output JSON path (array of ScenarioResult)")
	flag.Parse()

	if *runs < 1 {
		fmt.Fprintln(os.Stderr, "agobench: -runs must be at least 1")
		os.Exit(1)
	}
	selected := map[string]bool{}
	if *scenario == "all" {
		selected["plain_chat"], selected["tool_roundtrip"], selected["streaming"], selected["concurrent"] = true, true, true, true
	} else {
		switch *scenario {
		case "plain_chat", "tool_roundtrip", "streaming", "concurrent":
			selected[*scenario] = true
		default:
			fmt.Fprintf(os.Stderr, "agobench: unknown -scenario %q\n", *scenario)
			os.Exit(1)
		}
	}

	type entry struct {
		name  string
		notes string
		iter  func(stdctx.Context) error
	}
	entries := []entry{
		{"plain_chat", clockNotes + " " + commonNotes + " " + plainNotes, runPlainChat},
		{"tool_roundtrip", clockNotes + " " + commonNotes + " " + toolNotes, runToolRoundtrip},
		{"streaming", clockNotes + " " + commonNotes + " " + streamNotes, runStreaming},
		{"concurrent", clockNotes + " " + commonNotes + " " + concurrentNotes, runConcurrent},
	}

	var memBefore, memAfter runtime.MemStats
	runtime.ReadMemStats(&memBefore)

	var results []ScenarioResult
	for _, e := range entries {
		if !selected[e.name] {
			continue
		}
		samples, batchK, err := measure(*warmup, *runs, e.iter)
		if err != nil {
			fmt.Fprintf(os.Stderr, "agobench: %s: %v\n", e.name, err)
			os.Exit(1)
		}
		median, p95, min, max := summarize(samples)
		results = append(results, ScenarioResult{
			Side:      *side,
			Scenario:  e.name,
			Warmup:    *warmup,
			Runs:      *runs,
			SamplesMs: samples,
			MedianMs:  median,
			P95Ms:     p95,
			MinMs:     min,
			MaxMs:     max,
			Notes:     fmt.Sprintf("%s This run: batch K=%d.", e.notes, batchK),
		})
		fmt.Printf("side=%s scenario=%-14s runs=%-3d batchK=%-5d median=%.4fms p95=%.4fms min=%.4fms max=%.4fms\n",
			*side, e.name, *runs, batchK, median, p95, min, max)
	}

	runtime.ReadMemStats(&memAfter)
	memNote := fmt.Sprintf(
		"HeapAlloc before=%d after=%d delta=%+d bytes; runtime.ReadMemStats taken around all measurements; "+
			"no forced GC, default GOGC (informational)",
		memBefore.HeapAlloc, memAfter.HeapAlloc, int64(memAfter.HeapAlloc)-int64(memBefore.HeapAlloc))
	for i := range results {
		results[i].MemNote = memNote
	}

	data, err := json.MarshalIndent(results, "", "  ")
	if err != nil {
		fmt.Fprintln(os.Stderr, "agobench: marshal results:", err)
		os.Exit(1)
	}
	if dir := filepath.Dir(*out); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			fmt.Fprintln(os.Stderr, "agobench: create output dir:", err)
			os.Exit(1)
		}
	}
	if err := os.WriteFile(*out, append(data, '\n'), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "agobench: write results:", err)
		os.Exit(1)
	}
	fmt.Println("wrote", *out)
}
