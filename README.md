<a name="readme-top"></a>

<p align="center">
  <img src="docs/assets/banner.svg" alt="AGo — AG2, ported to Go" width="720">
</p>

<p align="center">
  <a href="https://github.com/luckystrker/ago/actions/workflows/ci.yml">
    <img src="https://github.com/luckystrker/ago/actions/workflows/ci.yml/badge.svg" alt="CI">
  </a>
  <a href="https://github.com/luckystrker/ago/releases">
    <img src="https://img.shields.io/github/v/release/luckystrker/ago?color=green&label=release" alt="Release">
  </a>
  <a href="https://github.com/luckystrker/ago/blob/main/ago/go.mod">
    <img src="https://img.shields.io/badge/go-1.24%2B-00ADD8?logo=go&logoColor=white" alt="Go 1.24+">
  </a>
  <a href="LICENSE">
    <img src="https://img.shields.io/badge/license-Apache--2.0-blue.svg" alt="License">
  </a>
  <img src="https://img.shields.io/badge/external%20deps-0-7ee787" alt="Zero dependencies">
</p>

<p align="center">
  <a href="docs/GUIDE.md">📚 Guide</a> |
  <a href="docs/MIGRATION.md">🔁 Migration from Python</a> |
  <a href="#-examples">💡 Examples</a> |
  <a href="docs/PORT_STATUS.md">🧭 Port status</a> |
  <a href="docs/DECISIONS.md">🧾 Decision records</a> |
  <a href="https://github.com/ag2ai/ag2">🐍 Original AG2</a>
</p>

> [!IMPORTANT]
> **Looking for `ConversableAgent`, `GroupChat`, or `initiate_chat`?** Those are the classic
> AutoGen APIs. AGo ports the **2026 event-sourced AG2** — one `Agent` type with
> `ask`/`run`, events on a stream, middleware, HITL hooks, and a multi-agent hub. The
> class-by-class mapping to Python lives in [docs/MIGRATION.md](docs/MIGRATION.md).

# AGo: the AG2 agent framework, compiled

AGo is an independent Go rewrite of [AG2](https://github.com/ag2ai/ag2) (port target:
AG2 `main` at commit `fa4e0fb`, 2026-10-27). One `Agent` type, an event stream per
conversation, tools registered as stream subscribers, middleware wrapped around every
turn, LLM call, tool execution and human-input hop — and a hub that hosts multi-agent
channels. **Zero external dependencies**: the framework is Go standard library only
(`net/http`, `encoding/json`, `os/exec`, `crypto/rand`); LLM providers talk raw
HTTP/SSE, no vendor SDKs.

Every package is gated by `go build` + `go vet` + `go test`, the event vocabulary is
wire-compatible with Python (JSON tags match field names, every event round-trips
through a `@type`-discriminated codec), and all 13 examples run offline on scripted
models — no API keys needed.

## ✨ What's in the box

| | |
|---|---|
| 🧠 **One `Agent` type** | blocking `Ask`, observable `Run`, continuations on the same stream, `Resume` from mid-trajectory |
| 📡 **Event-sourced core** | every observation is an event: pub/sub stream, durable transcript, replays and usage accounting are event logs |
| 🔧 **Tools as subscribers** | function tools with schema generation and dependency injection, toolkits, parallel execution, `final=true` tools, name precedence |
| 🌐 **7 LLM providers** | OpenAI, Anthropic, Gemini, Ollama, Z.AI, Mistral, DashScope — full clients; xAI/Bedrock/TypeSafe as config stubs |
| 🧅 **Middleware everywhere** | four hooks (turn / LLM call / tool execution / human input), conditional composition, limiters, retries, approvals |
| 🙋 **Human-in-the-loop** | pluggable human hooks, tool approvals, timed-out asks, transcript repair for abandoned tool calls |
| 📦 **Structured output** | JSON-schema from Go types, validated answers with automatic re-asking |
| 🗂️ **Tasks & subtasks** | task lifecycle with checkpoints, `run_subtask`/`run_subtasks` fan-out with usage roll-up, `AsTool` delegation |
| 🧠 **Knowledge & policies** | virtual filesystem store, context-assembly policies (sliding window, token budget, alerts) |
| 🗜️ **Compaction & aggregation** | LLM-driven history summarization behind triggers, keeping the transcript valid for replay |
| 👀 **Observers & watches** | loop detection, token monitors, alert→halt flow, composable triggers |
| 🔌 **MCP client** | JSON-RPC 2.0 over stdio and streamable HTTP, tool discovery with pagination, content mapping |
| 🕸️ **Multi-agent network** | hub, channels, envelopes (`ag2.*` wire vocabulary), four channel adapters, workflow transitions, WAL replay |

## 🚀 Quick start

```bash
git clone https://github.com/luckystrker/ago
cd ago
```

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
	fmt.Println(reply.Body())
}
```

> The framework module lives in [`ago/`](ago/) (import paths `ago/agent`, `ago/tools`, …);
> examples are a separate module in [`examples/`](examples/) tied by `go.work`.
> Verify everything: `cd ago && go test ./...`.

## 📚 Documentation

| Document | Contents |
|---|---|
| [docs/GUIDE.md](docs/GUIDE.md) | the full user guide: concepts, every subsystem, working snippets |
| [docs/MIGRATION.md](docs/MIGRATION.md) | class-by-class map: Python AG2 → AGo |
| [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) | the binding package contracts |
| [docs/PORT_STATUS.md](docs/PORT_STATUS.md) | what is ported / partial / stubbed / deferred |
| [docs/DECISIONS.md](docs/DECISIONS.md) | 35+ decision records behind every deviation from Python |

## 💡 Examples

All examples run offline on scripted models — clone and `go run ./examples/<name>`:

| Example | Demonstrates |
|---|---|
| [`ask`](examples/ask) | basic ask, transcript, usage report |
| [`tools`](examples/tools) | function tools, tool-call round trip |
| [`hitl`](examples/hitl) | tool approval through a human hook |
| [`subagent`](examples/subagent) | agent delegation via `AsTool` |
| [`group`](examples/group) | multi-agent orchestration over the network hub |
| [`structured_output`](examples/structured_output) | schema-validated answers with re-asking |
| [`multi_turn`](examples/multi_turn) | multi-turn conversations on one stream |
| [`observers`](examples/observers) | loop detection, token monitoring, alerts |
| [`knowledge_memory`](examples/knowledge_memory) | persistent memory via the knowledge store |
| [`policies_compact`](examples/policies_compact) | context-assembly policies and compaction |
| [`safety_halt`](examples/safety_halt) | alert→halt safety flow |
| [`mcp_stdio`](examples/mcp_stdio) | MCP over a stdio subprocess |
| [`mcp_http`](examples/mcp_http) | MCP over streamable HTTP |

## 🧭 Status

- **Ported and gated:** the core framework, all 7 full providers, tools, middleware,
  HITL, tasks, knowledge, policies, compaction, observers, watches, spec, MCP client,
  multi-agent network — `go build`/`vet`/`test` green on every package.
- **Stubbed:** xAI, Bedrock, TypeSafe configs (client construction returns an explicit
  "not implemented").
- **Deferred:** protocol servers (a2a/acp/ag_ui/a2ui), live voice, Redis/SQLite stores,
  WebSocket link — see [docs/PORT_STATUS.md](docs/PORT_STATUS.md).
- **Reviewed:** an independent port-parity review against the Python sources found and
  fixed 20 confirmed defects; the review trail is in the git history and decision records.

## License

Apache-2.0 — the same license as AG2. See [LICENSE](LICENSE) and [NOTICE.md](NOTICE.md)
for provenance. AGo is an independent rewrite and is not affiliated with or endorsed by
the AG2ai maintainers.
