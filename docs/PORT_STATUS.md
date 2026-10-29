# Port status

Per-package porting status of the AG2 (Python) → AGo (Go) port, against the binding Go
contracts in `docs/ARCHITECTURE.md`. Deviations from Python parity are marked
`// AGO-DEVIATION` in code and recorded as D-xxx entries in `docs/DECISIONS.md`.

Statuses:

- **ported** — the contracted scope is implemented and green.
- **partial** — implemented and green for the named scope; the listed pieces are deferred.
- **stub** — config/type surface only; the real implementation reports "not implemented".
- **deferred** — deliberately not ported (decision references given).

**Gates, verified 2026-10-29 on the whole repo** (fresh run, `-count=1`):

- `go build ./...` — success.
- `go vet ./...` — clean.
- `go test -count=1 ./...` — 33 packages `ok`, 0 failures, 0 skipped tests
  (`ago/hitl`, `ago/internal/uuid` and the five `ago/examples/*` programs have no test
  files of their own; hitl behavior is exercised by the `context` and `tools` test suites).
- `go run ./examples/{ask,tools,hitl,subagent,group}` — all five complete offline
  (scripted configs, no API keys).

## Packages

| Python (ag2/) | Go package | Status | Notes |
|---|---|---|---|
| `events/` (alert, base, conditions, input_events, lifecycle, sizing, task_events, tool_events, types) | `ago/events` | ported | wave 0. Full event vocabulary, `Usage` with `*int64` fields (D-006), Condition DSL as predicates (D-005), `@type` marshal registry — JSON round-trip losslessness is tested. `events/voice.py` not ported (live/voice is deferred) |
| `stream.py`, `history.py` | `ago/stream` | ported | wave 0. `Stream`/`MemoryStream`/`SubStream`/`History`, `Storage` (memory backend), synchronous dispatch in the caller's goroutine (D-004) |
| `context.py` | `ago/context` | ported | wave 0. Stream handle, prompt fragments, dependencies (DI), variables, HITL `Ask`/`Input`, background spawn, `Clone` |
| `hitl.py` | `ago/hitl` | ported | wave 1. `HumanHook`, `DefaultHook`→`ErrNotProvided`, typed NotProvided/Failed/Timeout errors. No test files of its own — covered indirectly by `context` and `tools` executor tests |
| `exceptions.py` | per-package sentinel errors | ported | shape change: `errors.New` sentinels, `%w` wrapping, `errors.Is`/`As` on consumption; no exception hierarchy |
| `annotations.py` | `ago/response` + `ago/tools` struct tags | ported | folded into `agschema:"…"`, `aginject:"…"`, `agvariable:"…"`, `agdefault:"…"` tags (D-009, D-015, D-016) |
| `response/` | `ago/response` | ported | wave 1 (D-015). `Schema[T]` via reflection, `RawSchema`, `PromptedSchema`, decision/question helpers, strict stdlib-json validation with a required/enum pre-walk |
| `tools/` framework (tool.py, executor.py, precedence.py, schemas.py, toolkits, types.py) | `ago/tools` | ported | wave 1 (D-016). `Schema`/`ToolSchema`, `FunctionTool` (reflection binding, DI, timeout, middleware), `Toolkit`, `ClientTool`, parallel `Executor` (hitl abandon, `final=true`), `ResolveTools` precedence |
| `tools/subagents/` | `ago/agent` (`AsTool`, subtask tools) | ported | wave 2. Agent-as-tool plus `run_subtask`/`run_subtasks` with rolled-up `UsageEvent(kind=subtask)` |
| `tools/builtin/` (web_search, code_execution, shell, file_search, image_generation, …) | — | deferred | Hosted/builtin tool *implementations* not ported; the `ToolSchema` interface, `BuiltinToolCallEvent`/`BuiltinToolResultEvent` events and hosted-type precedence exist so they can be added |
| `tools/{code,sandbox,shell,dynamic,final,search,skills}/` | — | deferred | Python-side tool content (code sandboxes, shell tool, dynamic agents, search/skill packs) out of the port scope |
| `config/{config.py,client.py,input_acceptance.py}` | `ago/config` + `ago/internal/httpx`, `ago/internal/sse` | ported | wave 1 (D-017). `Provider`/`ModelConfig`/`LLMClient`/`CallOptions`, sentinels, shared OpenAI-compatible engine (messages/tools/schema/usage, SSE streaming with tool-call delta merge), HTTP retries with Retry-After |
| `config/openai/` (openai_client.py, mappers.py, config.py) | `ago/config/openai` | ported | wave 1 (D-019). Chat Completions over the shared engine: SSE streaming, tools, strict structured output, env `OPENAI_API_KEY` |
| `config/openai/{openai_responses_client,openai_decisions_client}.py`, `files.py`, `containers.py` | — | deferred | D-008: Responses/Decisions APIs not ported; no Files/Containers client — `FileIdInput` fails fast with `ErrUnsupportedInput` (D-017, D-020) |
| `config/anthropic/` | `ago/config/anthropic` | ported | wave 1 (D-020). Messages API over raw HTTP/SSE: tool_use/tool_result replay with orphan repair, prompt caching, thinking blocks, usage incl. cache tokens, env `ANTHROPIC_API_KEY` |
| `config/gemini/` | `ago/config/gemini` | ported | wave 1 (D-022). `generateContent` + `streamGenerateContent?alt=sse`, function declarations, usageMetadata normalization, env `GEMINI_API_KEY` |
| `config/ollama/` | `ago/config/ollama` | partial | wave 1 (D-022). `/api/chat` non-streaming with tools and usage; **streaming deferred** — `Streaming=true` makes `Create()` return `ErrUnsupportedInput` |
| `config/{zai,mistral,dashscope}/` | `ago/config/{zai,mistral,dashscope}` | ported | wave 1 (D-021). OpenAI-compatible REST on the shared engine; env keys `ZAI_API_KEY`/`MISTRAL_API_KEY`/`DASHSCOPE_API_KEY` |
| `config/{xai,bedrock,typesafe}/` | `ago/config/{xai,bedrock,typesafe}` | stub | wave 1 (D-008, D-021). Config structs validate the model, then `Create()` returns `config.ErrNotImplemented` naming the provider (xai: gRPC SDK; bedrock: SigV4; typesafe: niche SDK) |
| `middleware/` (base.py, builtin/) | `ago/middleware` | ported | wave 1 (D-013). Four-hook onion (`OnTurn`/`OnLLMCall`/`OnToolExecution`/`OnHumanInput`), `Base`, `Conditional`/`If`, additive `Chain`; builtins HistoryLimiter, TokenLimiter, Retry, Logging, Approval. Python `metrics`/`telemetry` builtins not ported |
| `usage.py` | `ago/usage` | ported | wave 1 (D-014). `FromEvents` aggregation → Report with totals and per model/provider/kind buckets |
| `task.py` | `ago/task` | ported | wave 1 (D-014). Lifecycle with explicit terminal transitions, `CheckpointStore`, `"ag:task"` dependency stamping |
| `knowledge/{base,memory,disk,locked,log}.py` | `ago/knowledge` | partial | wave 3 (D-027). `Store` protocol, MemoryStore, DiskStore (traversal guard, in-process OnChange), LockedStore, EventLogWriter JSONL. SQLite/Redis/bootstrap deferred (see below) |
| `knowledge/{sqlite,redis,bootstrap,polling,config}.py` | — | deferred | D-011: SQLite/Redis need external deps (forbidden by D-003); bootstrap/polling/config not in scope |
| `policies/`, `assembly.py`, `_replay.py` | `ago/policies` | ported | wave 3 (D-030). AssemblyPolicy chain: ConversationPolicy, SlidingWindowPolicy, TokenBudgetPolicy (orphan tool-result widening), AlertPolicy, WorkingMemoryPolicy, EpisodicMemoryPolicy |
| `compact.py` | `ago/compact` | ported | wave 3 (D-028, brief-shaped). EventCount/Token triggers, LLM strategy (UsageEvent kind=compaction), post-turn CompactionMiddleware with keepRecent=2 cut |
| `aggregate.py` | `ago/aggregate` | ported | wave 3 (D-026, brief redefines it as transcript merging). SummaryCount/Usage triggers, LLM strategy, AggregationMiddleware folding old transcript + summaries into one CompactionSummary |
| `observers/` | `ago/observers` | ported | wave 3 (D-025). Observer protocol on direct stream subscription, StreamObserver, CompositeObserver, LoopDetector, TokenMonitor |
| `watch.py` | `ago/watch` | partial | wave 3 (D-024). Pull-model `Wait`: EventWatch, IntervalWatch, DelayWatch, AllOf/AnyOf/Sequence composites. **CronWatch/CadenceWatch deferred** |
| `agent.py` | `ago/agent` | ported | wave 2. `Agent`/`AgentRun`/`AgentReply`; turn loop with per-stream lock, inbox drain → tool rounds → leftover flush; middleware wiring; HITL with `CloseUnansweredToolCalls` history repair; `Resume`; `ContentAs[T]` validation retry; `PromptHook`; `Agent.Task`/`TaskConfig`; `AsTool`. Config resolution → `ErrConfigNotProvided` |
| `spec.py` | `ago/spec` | ported | wave 3 (D-023). `AgentSpec` snake_case wire JSON, `FromAgent` (response schema not publicly readable → nil), `ToAgent` over a tool pool |
| `network/` core (envelope, channel, identity, rule, transitions, handoff, hub core, adapters ×4, client subset, transport frames + local) | `ago/network` + `ago/network/transport` | partial | wave 3 (D-031, functional single-host subset of D-010). Envelope with the `ag2.*` wire vocabulary, channel state machine + handshake, passports, rules, TransitionGraph, Handoff/Finish recognition, Hub (send gates, TTL, WAL: memory/file/knowledge backends, Replay), Consulting/Conversation/Discussion/Workflow adapters, HubClient/AgentClient/HumanClient, Frame + LocalLink. **Deferred:** WebSocket link (`transport/ws.py`), federation (`remote.py`), view policies (`views/`), task mirror, hub expectation sweepers/arbiter, client LLM-tool set (peers/channels/tasks/context/delegate) |
| `mcp/` | `ago/mcp` | partial | wave 3 (D-029, minimal client). JSON-RPC 2.0 over streamable HTTP (session handling, 404 rebuild) or stdio subprocess; lazy `Initialize`, paginated `ListTools`, `CallTool` with content mapping; `MCPToolkit` → `ago/tools`. **Not ported:** `input_required` answering, discovery cache, protocol-era negotiation |
| `testing.py` | `ago/testing` (import as `agotest "ago/testing"`) | ported | wave 1 (D-018). `TestConfig` (scripted `ModelResponse`s, shared cursor, recorded call snapshots), `TrackingConfig` (recording wrapper, shared recorder across clones); Python Turn vocabulary collapsed to full-response scripts (documented deviation) |
| `live/`, `events/voice.py` | — | deferred | Live/voice stack (realtime providers, STT, sound devices) not ported |
| `a2a/`, `acp/`, `ag_ui/`, `a2ui/`, `mcp_ui/`, `eval/` | — | deferred | Protocol servers and the eval harness are out of scope (D-010) |
| `streams/redis/` | — | deferred | Redis-backed stream storage needs an external client (D-003); `ago/stream` ships the memory backend (D-004) |
| `files/`, `extensions/`, `plugin.py`, `textual.py`, `mypy.py`, `_import_utils.py`, `_telemetry_consts.py`, `types.py`, `utils.py` | — | deferred | Python infrastructure without a Go counterpart in scope (plugin loading, typing helpers, textual UI, telemetry constants) |
| — | `ago/internal/uuid` | ported | RFC 4122 v4 from `crypto/rand` (D-003). No test files |
| — | `ago/examples/{ask,tools,hitl,subagent,group}` | ported | wave 3. Offline runnable programs; verified 2026-10-29 with `go run` (see Gates above) |

## Deferred subsystems (summary)

Everything below is a deliberate deferral with a decision record — none of it is silently
missing:

- **Redis streams** (`ag2/streams/redis`) and **SQLite/Redis knowledge backends** — need
  external deps; D-003/D-011.
- **WebSocket network link** (`ag2/network/transport/ws.py`) and **federation**
  (`remote.py`) — single-host subset only; D-010/D-031.
- **Live/voice** (`ag2/live/`, `events/voice.py`) — realtime audio stack; not ported.
- **Protocol/eval servers** — `a2a/`, `acp/`, `ag_ui/`, `a2ui/`, `mcp_ui/`, `eval/`; D-010.
- **OpenAI Responses API** (and Decisions API, Files/Containers) — chat completions is the
  supported OpenAI surface; D-008/D-017.
- **Hosted/builtin tools** (`ag2/tools/builtin`: web_search, code_execution, …) and tool
  content packages (sandbox/shell/search/skills/dynamic) — the framework seam
  (`ToolSchema`, builtin tool events) exists; D-008/ARCHITECTURE contract.
- **Ollama streaming** (D-022), **cron/cadence watches** (D-024), **middleware
  metrics/telemetry builtins** (D-013).
