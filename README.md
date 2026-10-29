# AGo — AG2, ported to Go

AGo is a Go port of [AG2](https://github.com/ag2ai/ag2) — the event-sourced Python agent
framework (port target: AG2 `main` at commit `fa4e0fb`, 2026-10-27). One `Agent` type, an
event stream per conversation, tools registered as stream subscribers, middleware wrapped
around every turn, LLM call, tool execution and human-input hop, and a hub that hosts
multi-agent channels. The port was made against a local clone of the Python sources; the
distilled behavior specs are in `docs/specs/` and the binding Go contracts in
[docs/ARCHITECTURE.md](docs/ARCHITECTURE.md). Licensed Apache-2.0 like AG2 — see
[NOTICE.md](NOTICE.md) for provenance.

- **Zero external dependencies.** Stdlib only (`net/http`, `encoding/json`, `os/exec`,
  `crypto/rand`). LLM providers talk raw HTTP/SSE — no vendor SDKs.
- **Go 1.24**, module name `ago`; packages live at the repo root (`ago/agent`, `ago/tools`, …).
- **Wire-compatible events.** JSON tags match the Python field names; every event
  round-trips through `events.MarshalEvent`/`UnmarshalEvent` with a `@type` discriminator.

**Status:** the core framework is ported — `go build ./...`, `go vet ./...` and
`go test ./...` pass for every package (verified 2026-10-29). What is ported, partial,
stubbed or deferred per package: [docs/PORT_STATUS.md](docs/PORT_STATUS.md). Every deviation
from Python parity carries a `// AGO-DEVIATION` comment and a decision record in
[docs/DECISIONS.md](docs/DECISIONS.md).

## 5-minute example

A complete agent with one tool. The LLM is a scripted test double, so it runs offline with
no API keys — swap `TestConfig` for a real provider config and nothing else changes.

```go
package main

import (
	"context"
	"fmt"

	"ago/agent"
	"ago/events"
	agotest "ago/testing"
	"ago/tools"
)

func main() {
	// A tool: the argument struct's fields become the JSON-schema parameters.
	weather, err := tools.NewFunctionTool("get_weather",
		"Current weather for a city.",
		func(args struct {
			City string `json:"city" agschema:"description=City name"`
		}) (string, error) {
			return "sunny, +24 C", nil
		})
	if err != nil {
		panic(err)
	}

	// Scripted "model" so the example runs offline without API keys.
	cfg := agotest.NewTestConfig(
		&events.ModelResponse{
			ToolCalls: events.ToolCallsEvent{Calls: []events.ToolCallEvent{
				{ID: "call-1", Name: "get_weather", Arguments: `{"city":"Lisbon"}`},
			}},
		},
		&events.ModelResponse{
			Message: &events.ModelMessage{Content: "It is sunny, +24 C in Lisbon."},
		},
	)

	a := agent.New("assistant", "Answer using your tools.",
		agent.WithConfig(cfg),
		agent.WithTools(weather),
	)

	reply, err := a.Ask(context.Background(), "What is the weather in Lisbon?")
	if err != nil {
		panic(err)
	}
	fmt.Println(reply.Body()) // It is sunny, +24 C in Lisbon.
}
```

The agent drives the whole loop itself: model call → tool call → tool result → second model
call → final answer. `reply.Body()` is the final text; `reply.History()` returns the durable
event transcript and `reply.Usage()` the token report aggregated from the stream's
`UsageEvent`s. `reply.Ask(...)` continues the same conversation.

To run against a real model, replace the test double with a provider config
(key is also read from the environment):

```go
cfg := config.NewOpenAICompletions("gpt-4o-mini") // OPENAI_API_KEY
// or: config.NewAnthropic("claude-sonnet-4-5"), config.NewGemini(key, "gemini-2.5-flash"),
//     config.NewOllama("llama3.2"), config.NewMistral / NewDashScope / NewZAI ...
```

## Package map

| Package | What it provides |
|---|---|
| `ago/events` | The event vocabulary (inputs, model messages/responses, tool calls/results, human-in-the-loop, task lifecycle, alerts), `Usage` token counters, the `Condition` predicate DSL, JSON marshaling with a `@type` discriminator |
| `ago/stream` | `Stream`/`MemoryStream`/`SubStream` pub-sub over an event history, `Storage` (memory backend), subscriber conditions |
| `ago/context` | Per-conversation `Context`: stream handle, prompt fragments, dependencies (DI), variables, HITL access |
| `ago/hitl` | `HumanHook` contract and typed errors for human-input requests (no hook / failed / timeout) |
| `ago/response` | Structured-response schemas: `Schema[T]` via reflection (`agschema` tags), raw schemas, validation errors, decision/question helpers |
| `ago/tools` | `Tool`, `FunctionTool` (reflection argument binding, DI injection, timeout, per-tool middleware), `Toolkit`, `ClientTool`, the parallel tool `Executor`, tool-resolution precedence |
| `ago/config` | `ModelConfig`/`LLMClient` contracts, per-provider configs and clients (see table below), shared OpenAI-compatible engine |
| `ago/middleware` | The four-hook middleware chain (`OnTurn`, `OnLLMCall`, `OnToolExecution`, `OnHumanInput`), conditional wrapper, builtins: history/token limiters, retry, approval, logging |
| `ago/usage` | Usage reports aggregated from `UsageEvent`s (totals, per model/provider/kind) |
| `ago/task` | Task lifecycle (`Start`/`Progress`/`Complete`/`Fail`/`Cancel`/`Expire`), checkpoints |
| `ago/knowledge` | `Store` protocol with memory and disk backends, locked wrapper, append-only JSONL event log |
| `ago/policies` | Context-assembly policies: conversation filter, sliding window, token budget, working/episodic memory, alert policy |
| `ago/compact` | Context compaction: triggers (event count / tokens), summarizing strategy, compaction middleware |
| `ago/aggregate` | Post-turn aggregation: folds old transcript + summaries into one summary via an LLM strategy |
| `ago/observers` | Stream observers: `LoopDetector` (repetition alerts), `TokenMonitor` (budget alerts) |
| `ago/watch` | Composable watches (`EventWatch`, `IntervalWatch`, `AllOf`/`AnyOf`/`Sequence`) with a pull-model `Wait` |
| `ago/agent` | The `Agent` turn loop: `Ask`/`Run`/`Resume`, `AgentReply`/`AgentRun`, HITL with history repair, structured-output retry, subtasks, `AsTool` |
| `ago/spec` | `AgentSpec` wire format: serialize an agent / rebuild one from a spec + tool pool |
| `ago/network` (+ `network/transport`) | Multi-agent hub: channels, envelopes (`ag2.*` wire vocabulary), passports, routing rules, transition graphs, handoff/finish, Consulting/Conversation/Discussion/Workflow adapters, clients, in-memory `LocalLink` |
| `ago/mcp` | Minimal MCP client: JSON-RPC 2.0 over streamable HTTP or stdio, tool listing with pagination, tool calls mapped to AGo tools |
| `agotest "ago/testing"` | Scripted model doubles: `TestConfig` (queued `ModelResponse`s + call recording), `TrackingConfig` (records calls through any real config) |
| `ago/internal/{httpx,sse,uuid}` | Shared plumbing: HTTP with retries/Retry-After, SSE line parser, RFC 4122 UUIDs (internal, not importable) |
| `ago/examples/…` | Runnable offline programs (see below) |

## Running the tests

```sh
go build ./...   # compiles everything
go vet ./...     # static checks
go test ./...    # full suite — stdlib testing only, no network, no external deps
```

A single package: `go test ./ago/agent/`. Provider tests use fake HTTP servers
(`httptest`) — no API keys are needed anywhere in the suite.

## Provider status

| Provider | Status | Notes |
|---|---|---|
| `config/openai` | full | Chat Completions over raw HTTP: SSE streaming, tool-call delta merge, strict structured output, usage normalization (D-019) |
| `config/anthropic` | full | Messages API over raw HTTP: native SSE streaming, tool use, prompt caching (`cache_control`), thinking replay (D-020) |
| `config/gemini` | full | `generateContent` + SSE streaming, function declarations, thinking-token usage (D-022) |
| `config/ollama` | full (non-streaming) | `/api/chat` with tools; a `Streaming=true` config refuses to create a client (D-022) |
| `config/zai`, `config/mistral`, `config/dashscope` | full | OpenAI-compatible REST on the shared engine (D-021) |
| `config/xai`, `config/bedrock`, `config/typesafe` | stub | Config structs exist; `Create()` reports "has no client yet" wrapping `config.ErrNotImplemented` (D-008) |
| OpenAI Responses API | deferred | Not ported; chat completions is the supported OpenAI surface (D-008) |

API keys resolve config field first, then the standard environment variable
(`OPENAI_API_KEY`, `ANTHROPIC_API_KEY`, `GEMINI_API_KEY`, `ZAI_API_KEY`, `MISTRAL_API_KEY`,
`DASHSCOPE_API_KEY`; Ollama needs none for a local server).

## Examples

All examples run offline on scripted configs — `go run ./examples/<name>`:

| Example | Demonstrates |
|---|---|
| `examples/ask` | Smallest agent: two turns on one stream, history transcript, usage report |
| `examples/tools` | Function tools with and without arguments; scripted tool-call rounds |
| `examples/hitl` | Human-in-the-loop tool approval through the `Approval` middleware |
| `examples/subagent` | Agent delegation: a specialist exposed to a lead via `AsTool` |
| `examples/group` | Network hub: two participants take turns in a workflow channel under a `TransitionGraph` |

## Documentation

- [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) — binding contracts: package layout, public APIs, Python→Go rules.
- [docs/PORT_STATUS.md](docs/PORT_STATUS.md) — per-package porting status and everything deferred.
- [docs/DECISIONS.md](docs/DECISIONS.md) — decision records: every deviation from Python parity, with reasons.
- [docs/specs/](docs/specs/) — behavior specs distilled from the Python reference (`core.md`, `config-llm.md`, `tools.md`, `events-network.md`).

Working on AGo itself? Read [AGENTS.md](AGENTS.md) first — conventions, hard rules (stdlib
only, no import cycles) and the definition of done (`go build ./... && go vet ./... && go test ./...`).
