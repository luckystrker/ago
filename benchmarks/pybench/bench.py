"""AG2 (Python) framework-overhead benchmark with a scripted LLM client.

Measures the framework only — event dispatch, stream, tool executor, history —
against a fully scripted (fake) LLM client, no network. The scenario constants
below (COMMON) must stay byte-identical to the Go-side benchmark
(benchmarks/agobench); message texts, chunk counts, tool rounds and conversation
sizes are shared contract.

Conversation shape: one iteration = ONE conversation; turns 2+ continue it via
``AgentReply.ask`` / ``AgentReply.run`` (ag2/ag2/agent.py:288-301), which reuse
the reply's context/stream. A bare ``agent.ask``/``agent.run`` would open a
fresh stream per call (``_open_run``'s ``stream or MemoryStream()``), which is
NOT the shape under measurement. The benchmark asserts the continuation shape
once at startup (sanity_check).

Run:  .venv-bench/Scripts/python bench.py [--scenario=...] [--warmup=3]
          [--runs=15] [--out=results_py.json]

Methodology (COMMON): warmup runs, then N measured runs; each measured run is
one iteration timed with time.perf_counter, reported in milliseconds (for
concurrent — the wall time of all conversations together). Stats: median,
p95 (nearest-rank), min, max. tracemalloc peak is measured in a separate,
untimed run per scenario (tracemalloc itself distorts timing).
"""

from __future__ import annotations

import argparse
import asyncio
import json
import logging
import math
import platform
import statistics
import sys
import time
import tracemalloc
from dataclasses import dataclass

logging.disable(logging.CRITICAL)  # no logging in the hot path

import ag2  # noqa: E402  (import after logging is silenced)
from ag2.events import ModelMessageChunk, ToolCallEvent  # noqa: E402
from ag2.testing import TestClient, TestConfig  # noqa: E402
from ag2.tools import tool  # noqa: E402

# --- COMMON: byte-identical scenario contract with the Go benchmark ---------
# (benchmarks/agobench/main.go constants block — every string here is repeated
# verbatim there: user texts, assistant replies, tool arguments, chunk texts,
# system prompt.)

BENCH_PROMPT = "You are a benchmark agent."
USER_MSGS_3 = ("Hello, agent.", "Continue the conversation.", "And one more question.")
USER_MSGS_2 = USER_MSGS_3[:2]
REPLIES_3 = ("Assistant reply 1.", "Assistant reply 2.", "Assistant reply 3.")
REPLIES_2 = REPLIES_3[:2]
TOOL_ROUNDS = 5  # 5 tool-call rounds, 6th round is the final answer
TOOL_NAME = "add"
TOOL_USER_MSG = "What is 2 + 3?"
TOOL_CALL_ARGS = '{"a":2,"b":3}'  # identical arguments every round; byte-identical to Go
TOOL_FINAL_REPLY = "The sum is 5."
STREAM_USER_MSGS = ("Stream a story, please.", "Keep going, please.")
STREAM_REPLIES = ("Streamed reply one.", "Streamed reply two.")
STREAM_CHUNKS = 20  # chunks per streamed turn
STREAM_TURNS = 2
CHUNK_FMT = "chunk-{i:02d}"
CONCURRENT_CONVERSATIONS = 50


@tool
def add(a: int, b: int) -> int:
    """The scripted tool under test: add(a, b) -> a + b."""
    return a + b


def tool_calls() -> tuple[ToolCallEvent, ...]:
    """TOOL_ROUNDS identical add() calls: every round adds (2, 3) -> 5."""
    return tuple(
        ToolCallEvent(id=f"call-{k}", name=TOOL_NAME, arguments=TOOL_CALL_ARGS)
        for k in range(1, TOOL_ROUNDS + 1)
    )


# ---------------------------------------------------------------------------


class ConversationConfig(TestConfig):
    """TestConfig whose scripted client is created once per conversation.

    Plain ``TestConfig.create()`` builds a fresh client per turn, so a multi-turn
    conversation would replay the script from the start on every ``ask``. One
    shared client makes the script advance turn over turn — the semantics the
    benchmark's "one scripted LLM" per conversation requires.
    """

    def __init__(self, *events: object, **kwargs: object) -> None:
        super().__init__(*events, **kwargs)  # type: ignore[arg-type]
        self._shared: TestClient | None = None

    def create(self) -> TestClient:
        if self._shared is None:
            self._shared = TestClient(*self.events, raise_tool_errors=self._raise_tool_errors)
        return self._shared


@dataclass
class Evidence:
    """What one iteration observed, verified after the clock is stopped."""

    bodies: tuple[object, ...]
    model_responses: int
    chunks: int


# --- Scenario iterations (everything inside the timed region) ---------------


async def iter_plain_chat() -> Evidence:
    """One fresh 3-turn conversation: turns 2-3 continue it via reply.ask."""
    agent = ag2.Agent("bench-plain", BENCH_PROMPT, config=ConversationConfig(*REPLIES_3))
    reply = await agent.ask(USER_MSGS_3[0])
    bodies = [reply.body]
    for msg in USER_MSGS_3[1:]:
        reply = await reply.ask(msg)
        bodies.append(reply.body)
    return Evidence(tuple(bodies), 0, 0)


async def iter_tool_roundtrip() -> Evidence:
    """One conversation: 5 identical add() rounds, 6th round is the final answer."""
    agent = ag2.Agent(
        "bench-tool",
        BENCH_PROMPT,
        config=ConversationConfig(*tool_calls(), TOOL_FINAL_REPLY),
        tools=[add],
    )
    reply = await agent.ask(TOOL_USER_MSG)
    responses = 0
    for event in await reply.history.get_events():
        if type(event).__name__ == "ModelResponse":
            responses += 1
    return Evidence((reply.body,), responses, 0)


async def _stream_turn(runnable, msg: str) -> tuple[object, str | None, int]:
    """One turn on `runnable` (the agent for turn 1, the reply for continuations
    — AgentReply.run reuses the same context/stream): drive the run while
    draining STREAM_CHUNKS chunk events."""
    async with runnable.run(msg) as run:
        with run.stream.where(ModelMessageChunk).join(max_events=STREAM_CHUNKS) as events:
            run.start()
            received = 0
            async for _event in events:
                received += 1
            reply = await run.result()
    return reply, reply.body, received


async def iter_streaming() -> Evidence:
    """One conversation, 2 turns, 20 streamed chunks per turn, all consumed."""
    chunks = tuple(ModelMessageChunk(content=CHUNK_FMT.format(i=i)) for i in range(STREAM_CHUNKS))
    agent = ag2.Agent(
        "bench-stream",
        BENCH_PROMPT,
        config=ConversationConfig(*chunks, STREAM_REPLIES[0], *chunks, STREAM_REPLIES[1]),
    )
    reply, body1, received1 = await _stream_turn(agent, STREAM_USER_MSGS[0])
    reply2, body2, received2 = await _stream_turn(reply, STREAM_USER_MSGS[1])
    return Evidence((body1, body2), 0, received1 + received2)


async def _concurrent_one(index: int) -> tuple[object, object]:
    agent = ag2.Agent(f"bench-conc-{index}", BENCH_PROMPT, config=ConversationConfig(*REPLIES_2))
    first = await agent.ask(USER_MSGS_2[0])
    second = await first.ask(USER_MSGS_2[1])
    return first.body, second.body


async def iter_concurrent() -> Evidence:
    """50 fresh 2-turn conversations, all driven together by asyncio.gather."""
    replies = await asyncio.gather(*(_concurrent_one(i) for i in range(CONCURRENT_CONVERSATIONS)))
    return Evidence(tuple(replies), 0, 0)


# --- Verification (outside the timed region) --------------------------------


def verify(scenario: str, ev: Evidence) -> None:
    if scenario == "plain_chat":
        expected = REPLIES_3
        if ev.bodies != expected:
            raise AssertionError(f"plain_chat bodies {ev.bodies!r} != {expected!r}")
    elif scenario == "tool_roundtrip":
        if ev.bodies != (TOOL_FINAL_REPLY,):
            raise AssertionError(f"tool_roundtrip body {ev.bodies!r} != {(TOOL_FINAL_REPLY,)!r}")
        # 5 tool rounds + 1 final answer = 6 model calls in one ask()
        if ev.model_responses != TOOL_ROUNDS + 1:
            raise AssertionError(f"tool_roundtrip model calls {ev.model_responses} != {TOOL_ROUNDS + 1}")
    elif scenario == "streaming":
        if ev.bodies != STREAM_REPLIES:
            raise AssertionError(f"streaming bodies {ev.bodies!r} != {STREAM_REPLIES!r}")
        if ev.chunks != STREAM_TURNS * STREAM_CHUNKS:
            raise AssertionError(f"streaming chunks {ev.chunks} != {STREAM_TURNS * STREAM_CHUNKS}")
    elif scenario == "concurrent":
        if len(ev.bodies) != CONCURRENT_CONVERSATIONS:
            raise AssertionError(f"concurrent conversations {len(ev.bodies)} != {CONCURRENT_CONVERSATIONS}")
        if any(pair != REPLIES_2 for pair in ev.bodies):
            raise AssertionError(f"concurrent replies wrong: {ev.bodies[:3]!r}...")


SCENARIOS = {
    "plain_chat": iter_plain_chat,
    "tool_roundtrip": iter_tool_roundtrip,
    "streaming": iter_streaming,
    "concurrent": iter_concurrent,
}


async def sanity_check() -> None:
    """One untimed conversation guarding the benchmark's shape: turns 2+ must
    continue the SAME conversation. A continuation reply must share the first
    reply's history (a bare agent.ask would open a fresh stream per call), and
    the transcript must hold both turns' request/response pairs."""
    agent = ag2.Agent("bench-sanity", BENCH_PROMPT, config=ConversationConfig(*REPLIES_2))
    first = await agent.ask(USER_MSGS_2[0])
    second = await first.ask(USER_MSGS_2[1])
    if first.history is not second.history:
        raise AssertionError(
            "sanity: continuation opened a fresh stream (history objects differ) — "
            "turns must run on one conversation"
        )
    names = [type(e).__name__ for e in await second.history.get_events()]
    if names != ["ModelRequest", "ModelResponse", "ModelRequest", "ModelResponse"]:
        raise AssertionError(f"sanity: continued history is {names!r}, expected one 2-turn conversation")


# --- Harness -----------------------------------------------------------------


async def measure(scenario: str, warmup: int, runs: int) -> tuple[list[float], str]:
    """Warm up, then time `runs` iterations; verify every iteration's evidence."""
    iterate = SCENARIOS[scenario]

    for _ in range(warmup):
        verify(scenario, await iterate())

    times_ms: list[float] = []
    for _ in range(runs):
        start = time.perf_counter()
        evidence = await iterate()
        elapsed = (time.perf_counter() - start) * 1000.0
        verify(scenario, evidence)
        times_ms.append(elapsed)
    return times_ms, ""


async def measure_memory(scenario: str) -> int:
    """Peak tracemalloc bytes for a single iteration (untimed: tracemalloc skews timing)."""
    iterate = SCENARIOS[scenario]
    tracemalloc.start()
    try:
        tracemalloc.reset_peak()
        evidence = await iterate()
        _peak, peak = tracemalloc.get_traced_memory()
    finally:
        tracemalloc.stop()
    verify(scenario, evidence)
    return peak


def p95_nearest_rank(sorted_times: list[float]) -> float:
    """Nearest-rank percentile: value at ceil(p * n), 1-based."""
    rank = math.ceil(0.95 * len(sorted_times))
    return sorted_times[rank - 1]


def describe(scenario: str, times_ms: list[float], mem_peak: int, warmup: int, runs: int) -> dict:
    ordered = sorted(times_ms)
    return {
        "params": {
            "turns": {"plain_chat": 3, "tool_roundtrip": 1, "streaming": STREAM_TURNS, "concurrent": 2}[scenario],
            "tool_rounds": TOOL_ROUNDS if scenario == "tool_roundtrip" else 0,
            "chunks_per_turn": STREAM_CHUNKS if scenario == "streaming" else 0,
            "conversations": CONCURRENT_CONVERSATIONS if scenario == "concurrent" else 1,
        },
        "warmup": warmup,
        "runs": runs,
        "unit": "ms",
        "times_ms": [round(t, 6) for t in times_ms],
        "median_ms": round(statistics.median(ordered), 6),
        "p95_ms": round(p95_nearest_rank(ordered), 6),
        "min_ms": round(ordered[0], 6),
        "max_ms": round(ordered[-1], 6),
        "memNote": f"tracemalloc peak {mem_peak} bytes (single iteration, untimed run)",
    }


def main() -> int:
    parser = argparse.ArgumentParser(description="AG2 Python framework-overhead benchmark")
    parser.add_argument("--scenario", default="all", choices=["plain_chat", "tool_roundtrip", "streaming", "concurrent", "all"])
    parser.add_argument("--warmup", type=int, default=3)
    parser.add_argument("--runs", type=int, default=15)
    parser.add_argument("--out", default="results_py.json")
    args = parser.parse_args()

    names = list(SCENARIOS) if args.scenario == "all" else [args.scenario]
    asyncio.run(sanity_check())
    report: dict = {
        "framework": "ag2",
        "framework_version": getattr(ag2, "__version__", "unknown"),
        "python": platform.python_version(),
        "os": platform.platform(),
        "p95_method": "nearest-rank: sorted[ceil(0.95*n)-1]",
        "timed_region": "fresh agent + scripted config + all turns of the conversation; verification runs after the clock stops",
        "scenarios": {},
    }

    for name in names:
        times_ms, _ = asyncio.run(measure(name, args.warmup, args.runs))
        mem_peak = asyncio.run(measure_memory(name))
        report["scenarios"][name] = describe(name, times_ms, mem_peak, args.warmup, args.runs)

    with open(args.out, "w", encoding="utf-8") as fh:
        json.dump(report, fh, indent=2, ensure_ascii=False)
        fh.write("\n")

    print(json.dumps(report, indent=2, ensure_ascii=False))
    return 0


if __name__ == "__main__":
    sys.exit(main())
