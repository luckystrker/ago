# Benchmarks: framework overhead — AG2 (Python) vs AGo (Go)

Honest measurement of framework overhead only: event dispatch, the conversation
stream, middleware wiring, the tool executor, history. The LLM is a fake — a
scripted client replays deterministic responses. No network, no real model
calls, no logging in the hot path. Real LLM latency (hundreds of milliseconds
per call) dominates production and is deliberately out of scope; these numbers
bound what the framework itself adds around it.

Harness sources: `benchmarks/agobench/main.go` (Go), `benchmarks/pybench/bench.py`
(Python). Raw results compared here: `benchmarks/results_go.json`,
`benchmarks/results_py.json`.

## Methodology

- **Scenarios** — identical structure on both sides: same conversation shapes,
  same workload parameters (3 turns / 5 tool rounds / 20 chunks per turn /
  50 conversations), and byte-identical payloads (below).
  - `plain_chat` — one fresh conversation, 3 turns on the same conversation
    (user message → assistant reply; turns 2–3 continue it). Go:
    `agent.Ask` then two `AgentReply.Ask` continuations on one stream;
    Python: `agent.ask` then two `AgentReply.ask` continuations
    (ag2/ag2/agent.py:288-301 — a bare `agent.ask` would open a fresh stream
    per call, which is not the shape under measurement; the Python harness
    asserts the continuation shape once at startup by checking both replies
    share one history object).
  - `tool_roundtrip` — one conversation; the fake LLM requests the tool
    `add(a, b)` in 5 consecutive rounds (identical arguments every round),
    round 6 is the final text answer. One iteration = 6 model calls + 5 tool
    executions through the tool executor.
  - `streaming` — one conversation, 2 turns (turn 2 a continuation); the fake
    client emits 20 chunk deltas per turn before the complete response. Both
    sides attach a real chunk consumer before the turns run — Python joins a
    `where(ModelMessageChunk)` subscription per turn and drains 20 events; Go
    subscribes `Stream().Subscribe(..., Condition: events.TypeOf[
    events.ModelMessageChunk]())` with a counting closure over the whole
    conversation — and both verify full receipt (40 chunks = 2 turns × 20) in
    addition to the final bodies.
  - `concurrent` — 50 fresh 2-turn conversations launched simultaneously
    (`asyncio.gather` vs goroutines); one iteration = all 50 together. The
    scripted responses are shared objects, pre-stamped (`CreatedAt`) once at
    startup on the Go side so no goroutine ever writes to a shared response.
- **Identical payloads** — user texts `Hello, agent.` /
  `Continue the conversation.` / `And one more question.`; assistant replies
  `Assistant reply 1.` / `Assistant reply 2.` / `Assistant reply 3.`; system
  prompt `You are a benchmark agent.`; tool user text `What is 2 + 3?` with
  arguments `{"a":2,"b":3}` (byte-identical JSON, ids `call-1`..`call-5`) and
  final answer `The sum is 5.`; streaming user texts `Stream a story, please.` /
  `Keep going, please.`, replies `Streamed reply one.` / `Streamed reply two.`,
  chunks `chunk-00`..`chunk-19`. Declared once, repeated verbatim in both
  harnesses.
- **Fake client symmetry** — both sides use a non-recording scripted client:
  Python's `TestClient` pops its script without recording
  (ag2/ag2/testing.py); the Go bench uses a local `scriptFakeConfig` that
  replays a queue of `*events.ModelResponse` the same way (implementing
  `config.ModelConfig` + `config.LLMClient`). No history recording or copying
  lands inside the timed window on either side.
- **Timed region** — fresh agent + fake config construction + all turns of the
  conversation. Correctness (final bodies, model-call and chunk counts) is
  verified on every iteration on both sides.
- **Warmup and runs** — warmup 5 iterations / 30 measured runs on Go, warmup 3 /
  15 runs on Python (per the wave methodology). Each recorded sample is the
  wall time of one iteration in milliseconds (for `concurrent` — the wall time
  of all 50 conversations together).
- **Statistics** — median, p95 (nearest-rank: `sorted[ceil(0.95*n)-1]` —
  identical formula on both sides), min, max.
- **One machine, one session** — Windows 11 (build 26200), go1.24.1
  windows/amd64, Python 3.12.10, AG2 1.2.2. GC left at defaults on both sides.
- **Go-only sampling detail** — the Windows clock ticks at ~0.5 ms, so a single
  sub-millisecond iteration times as exactly 0. Each Go sample is therefore the
  mean over K consecutive iterations, K auto-calibrated so one batch spans
  ≥10 ms — the same batch timing `go test -bench` uses. K is disclosed per
  scenario in `results_go.json` notes (stored run: plain_chat K=512,
  tool_roundtrip K=256, streaming K=512, concurrent K=64; re-calibrated each
  run). Python's `perf_counter` resolves single iterations and does not need
  batching.

## Results

Milliseconds, lower is better. "Go faster" = Python median ÷ Go median,
computed from the stored samples.

| Scenario | Python median | Go median | Go faster | Python p95 | Go p95 |
|---|---:|---:|---:|---:|---:|
| plain_chat (3 turns) | 9.2363 | 0.0237 | 390.3× | 23.7286 | 0.0292 |
| tool_roundtrip (5 tool rounds) | 16.1068 | 0.0684 | 235.5× | 17.4631 | 0.0809 |
| streaming (2 turns × 20 chunks) | 13.2613 | 0.0232 | 571.3× | 14.0929 | 0.0254 |
| concurrent (50 × 2-turn conversations) | 270.9264 | 0.2792 | 970.5× | 312.6362 | 0.3033 |

Full sample arrays, per-scenario notes and batch K: `benchmarks/results_go.json`,
`benchmarks/results_py.json`.

## Conclusions (dry facts from the numbers)

- **plain_chat.** A full 3-turn conversation: 0.024 ms (Go) vs 9.24 ms (Python),
  390.3×. Per turn (derived): ≈0.008 ms vs ≈3.08 ms. Python's p95 is 2.6× its
  median (one 23.7 ms sample among 15); Go's p95 is 1.23× its median.
- **tool_roundtrip.** The 6-model-call loop with 5 tool executions:
  0.068 ms vs 16.11 ms, 235.5× — the smallest gap of the four scenarios. Per
  model call (derived): ≈0.011 ms vs ≈2.68 ms.
- **streaming.** 40 chunk dispatches, all received through a real subscription
  consumer on both sides, + 2 durable responses: 0.023 ms vs 13.26 ms, 571.3×.
  On the Go side streaming costs the same as plain_chat (0.023 vs 0.024 ms —
  both scenarios run the identical non-recording fake, and the
  chunk consumer's subscription check adds no measurable overhead); on the
  Python side streaming still costs more than plain chat (13.26 vs 9.24 ms
  median) despite fewer turns (2 vs 3) — the per-turn join subscription is a
  visible share of that.
- **concurrent.** 50 simultaneous 2-turn conversations: 0.279 ms total (Go) vs
  270.93 ms (Python), 970.5×. Per conversation (derived): ≈0.0056 ms vs
  ≈5.42 ms. Goroutines run the 50 conversations across cores — per conversation
  the wall cost lands below the sequential per-2-turn estimate from plain_chat
  (≈0.016 ms derived); Python's event loop interleaves them on one thread, so
  its per-conversation cost stays close to the sequential per-2-turn estimate
  (≈6.16 ms derived).

## Limitations

1. **Framework, not the model.** All calls are scripted and network-free.
   Results say nothing about answer quality or real model latency; they bound
   only the overhead added around real calls.
2. **Memory figures are not comparable across sides.** Go reports a whole-run
   `HeapAlloc` delta (`runtime.ReadMemStats` around all measurements, default
   GOGC, no forced GC; +1.56 MB over the entire session, informational).
   Python reports tracemalloc peak per single untimed iteration (e.g. 200 KB
   plain_chat, 7.94 MB concurrent). Different allocators, different accounting —
   do not read these as "one side uses less memory".
3. **Sampling granularity differs.** Go samples are batch means over K
   iterations, which smooths single-iteration tails; Python samples are
   single-iteration timings. Run counts differ (30 vs 15) and warmup differs
   (5 vs 3) by wave methodology. The p95 formula is identical, but Go's p95 is
   a mean over K iterations, not a true single-iteration tail.
4. **Stream-subscription styles are equivalent, not identical.** AG2 opens a
   filtered join per turn (`where(ModelMessageChunk).join(max_events=20)`,
   drained with `async for`); AGo holds one condition-filtered subscription
   over the whole conversation (a synchronous closure counter). Both receive
   every chunk and verify the 40-chunk total; the Python async iterator carries
   more per-event machinery than the Go closure — that difference is part of
   the measured numbers, not controlled away.
5. **One machine, Windows.** Absolute numbers move with hardware, OS and
   background load; the relative ordering per scenario is the durable signal.
   Re-run both harnesses on the same machine when comparing across sessions.

## Reproducing

From the repository root.

Go stand:

```sh
go run ./benchmarks/agobench -scenario=all -warmup=5 -runs=30 -out=benchmarks/results_go.json
```

Python stand (AG2 1.2.2 in `.venv-bench`):

```sh
.venv-bench/Scripts/python benchmarks/pybench/bench.py --warmup=3 --runs=15 --out=benchmarks/results_py.json
```

Both accept `-scenario`/`--scenario` = `plain_chat|tool_roundtrip|streaming|concurrent|all`
and write the JSON reports compared in this document. The Python bench
additionally runs an untimed tracemalloc pass per scenario and a one-shot
continuation sanity check before measuring; the Go bench auto-calibrates its
batch size K per scenario and records it in the notes.
