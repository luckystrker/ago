# Port status

Per-package porting status of the AG2 (Python) → AGo (Go) port, against the binding Go
contracts in `docs/ARCHITECTURE.md`. Deviations from Python parity are marked
`// AGO-DEVIATION` in code and recorded as D-xxx entries in `docs/DECISIONS.md`.
For a class/function-level Python→Go API map, see [docs/MIGRATION.md](MIGRATION.md).

Statuses:

- **ported** — the contracted scope is implemented and green.
- **partial** — implemented and green for the named scope; the listed pieces are deferred.
- **stub** — config/type surface only; the real implementation reports "not implemented".
- **deferred** — deliberately not ported (decision references given).

**Gates, verified 2026-10-30 on the whole repo** (fresh run, `-count=1`, from the repo
root):

- `go -C ago build ./...` — success.
- `go -C ago vet ./...` — clean; `gofmt -l ago` — empty.
- `go -C ago test -count=1 ./...` — 41 packages `ok`, 0 failures, 0 skipped tests
  (`ago/internal/uuid` has no test files of its own; the thirteen `ago/examples/*`
  programs live in the examples module).
- `go -C examples build ./...` and `go -C examples vet ./...` — clean; from inside
  `examples/`, `go run` of `./{ask,tools,hitl,subagent,group,knowledge_memory,mcp_http,
  mcp_stdio,multi_turn,observers,policies_compact,safety_halt,structured_output}` — all
  thirteen complete offline (scripted configs, no API keys; re-verified 2026-10-30).

## Packages

| Python (ag2/) | Go package | Status | Notes |
|---|---|---|---|
| `events/` (alert, base, conditions, input_events, lifecycle, sizing, task_events, tool_events, types) | `ago/events` | ported | wave 0. Full event vocabulary, `Usage` with `*int64` fields (D-006), Condition DSL as predicates (D-005), `@type` marshal registry — JSON round-trip losslessness is tested; the repair sentinel is Python's human-readable sentence (D-043). `events/voice.py` not ported (live/voice is deferred) |
| `stream.py`, `history.py` | `ago/stream` | ported | wave 0. `Stream`/`MemoryStream`/`SubStream`/`History`, `Storage` (memory backend), synchronous dispatch in the caller's goroutine (D-004); `SubStream.Send` forwards unconditionally — a view's condition filters reading, not writing (D-042) |
| `context.py` | `ago/context` | ported | wave 0. Stream handle, prompt fragments, dependencies (DI), variables, HITL `Ask`/`Input`, background spawn, `Clone` |
| `hitl.py` | `ago/hitl` | ported | wave 1. `HumanHook`, `DefaultHook`→`ErrNotProvided`, typed NotProvided/Failed/Timeout errors, `IsError` classifier shared with the tool layer; own test suite since 2026-10-30 |
| `exceptions.py` | per-package sentinel errors | ported | shape change: `errors.New` sentinels, `%w` wrapping, `errors.Is`/`As` on consumption; no exception hierarchy |
| `annotations.py` | `ago/response` + `ago/tools` struct tags | ported | folded into `agschema:"…"`, `aginject:"…"`, `agvariable:"…"`, `agdefault:"…"` tags (D-009, D-015, D-016) |
| `response/` | `ago/response` | ported | wave 1 (D-015). `Schema[T]` via reflection, `RawSchema`, `PromptedSchema`, decision/question helpers, strict stdlib-json validation with a required/enum pre-walk |
| `tools/` framework (tool.py, executor.py, precedence.py, schemas.py, toolkits, types.py) | `ago/tools` | ported | wave 1 (D-016). `Schema`/`ToolSchema`, `FunctionTool` (reflection binding, DI, timeout, middleware), `Toolkit`, `ClientTool`, parallel `Executor` (hitl abandon, `final=true`), `ResolveTools` precedence |
| `tools/subagents/` | `ago/agent` (`AsTool`, subtask tools) | ported | wave 2 + delegation-parity pass. `runTaskOn` reports `TaskStarted/Completed/Failed` on the caller's stream, rolls up one `UsageEvent(kind=subtask)` (single-pair attribution, nested rollups flattened), starts the child from a copy of the caller's dependencies/variables (approval bypass key dropped), bridges HITL to the parent's hook — the forwarded question is transcribed on the parent stream (D-041). `IncludeTools []string` (D-039); default `AsTool` name `task_<name>` (D-040) |
| `tools/builtin/` (web_search, web_fetch, code_execution, file_search, image_generation, shell, memory, mcp_server, tool_search) | `ago/tools` (`hosted.go`) | partial | Schema carriers ported (D-047): `Tool` impls whose `Schemas` return the provider-neutral `ToolSchema` (options = Python kwargs), `Register` no-op, `ToolSearchTool` defers member schemas with `defer_loading` and registers the members. Provider mappers map what the ported Python clients map: anthropic (web_search/code_execution/web_fetch/memory/mcp_server/tool_search + `mcp_servers` request param), gemini (googleSearch/urlContext/codeExecution/fileSearch); shell/file_search/image_generation fail fast. **Deferred:** the OpenAI Responses hosted mappings (file_search, image_generation, shell, MCP, tool_search), Gemini grounding/server-tool replay events, per-turn `Variable` resolution |
| `tools/{code,sandbox,shell,dynamic,final,search,skills}/` | — | deferred | Python-side tool content (code sandboxes, shell tool, dynamic agents, search/skill packs) out of the port scope |
| `config/{config.py,client.py,input_acceptance.py}` | `ago/config` + `ago/internal/httpx`, `ago/internal/sse` | ported | wave 1 (D-017). `Provider`/`ModelConfig`/`LLMClient`/`CallOptions`, sentinels, shared OpenAI-compatible engine (messages/tools/schema/usage, SSE streaming with tool-call delta merge), HTTP retries with Retry-After |
| `config/openai/` (openai_client.py, mappers.py, config.py) | `ago/config/openai` | ported | wave 1 (D-019). Chat Completions over the shared engine: SSE streaming, tools, strict structured output, env `OPENAI_API_KEY` |
| `config/openai/files.py` | `ago/config/openai` (`FilesClient` via `CreateFilesClient`) | ported | Files API over raw HTTP (D-046): multipart upload (`purpose`, default `assistants`), read = content bytes + metadata, list auto-pagination (`has_more`/`after`), delete; key/organization/project headers shared with Create |
| `config/openai/{openai_responses_client,openai_decisions_client}.py`, `containers.py` | — | deferred | D-008: Responses/Decisions APIs and Containers not ported; `FileIdInput` still fails fast in message mapping with `ErrUnsupportedInput` (D-017, D-020) |
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
| `agent.py` | `ago/agent` | ported | wave 2. `Agent`/`AgentRun`/`AgentReply`; turn loop with per-stream lock, inbox drain → tool rounds → leftover flush; middleware wiring; HITL with `CloseUnansweredToolCalls` history repair; `Resume`; `ContentAs[T]` validation retry; `PromptHook`; `Agent.Task`/`TaskConfig` (`IncludeTools` is `[]string` — D-039); `AsTool` (delegation machinery, `task_<name>` — D-040/D-041). Config resolution → `ErrConfigNotProvided` |
| `spec.py` | `ago/spec` | ported | wave 3 (D-023). `AgentSpec` snake_case wire JSON, `FromAgent` (response schema not publicly readable → nil), `ToAgent` over a tool pool |
| `network/` core (envelope, channel, identity, rule, transitions, handoff, hub core, adapters ×4, client subset, transport frames + local) | `ago/network` + `ago/network/transport` | partial | wave 3 (D-031, functional single-host subset of D-010). Envelope with the `ag2.*` wire vocabulary, channel state machine + handshake, passports, rules, TransitionGraph, Handoff/Finish recognition, Hub (send gates, TTL, WAL: memory/file/knowledge backends, Replay), Consulting/Conversation/Discussion/Workflow adapters, HubClient/AgentClient/HumanClient, Frame + LocalLink. **Deferred:** WebSocket link (`transport/ws.py`), federation (`remote.py`), view policies (`views/`), task mirror, hub expectation sweepers/arbiter, client LLM-tool set (peers/channels/tasks/context/delegate) |
| `mcp/` | `ago/mcp` | partial | wave 3 (D-029, minimal client). JSON-RPC 2.0 over streamable HTTP (session handling, 404 rebuild) or stdio subprocess; lazy `Initialize`, paginated `ListTools`, `CallTool` with content mapping; `mcp.Tools(cl)` adapts remote tools to `ago/tools` (`DeclaredInCode=false`); 30s default HTTP timeout (D-034). **Not ported:** `input_required` answering, discovery cache, protocol-era negotiation |
| `testing.py` | `ago/testing` (import as `agotest "ago/testing"`) | ported | wave 1 (D-018). `TestConfig` (scripted `ModelResponse`s, shared cursor, recorded call snapshots), `TrackingConfig` (recording wrapper, shared recorder across clones); Python Turn vocabulary collapsed to full-response scripts (documented deviation) |
| `mcp_ui/` (resources.py, actions.py) | `ago/mcpui` | ported | D-045 (supersedes the D-010 deferral for this slice, owner directive). Data constructors only: UI resources RawHTML/ExternalURL/RemoteDOM (text/blob encoding, `ui://` guard, prefixed+verbatim `_meta`), UI actions ToolCall/Prompt/Link/Intent/Notify, `PostMessage` byte-parity with Python json.dumps + html.escape, `WithUIResource` attach to `ToolResult.Parts`; exact wire bytes via `mcpui.Marshal`. **Not ported:** protocol-era SDK behavior beyond the constructors — none exists in the Python module to port |
| `live/`, `events/voice.py` | — | deferred | Live/voice stack (realtime providers, STT, sound devices) not ported |
| `a2a/` | `ago/a2a` | partial | D-049/D-050 (supersede the D-010 deferral for this slice, owner directive). Transport-level subset: protocol types (camelCase tags, `kind` part discriminator), agent-card discovery (`/.well-known/agent-card.json` + legacy `agent.json`) with the served card always carrying `protocolVersion` + `supportedInterfaces` (defaults: one JSONRPC interface at protocol version `1.0` — what 1.x clients require; config-declared interfaces verbatim), JSON-RPC 2.0 `message/send`/`message/stream` (two-event SSE)/`tasks/get`/`tasks/cancel`, `TaskStore` interface + memory impl, `Client` (AgentCard/SendMessage/TasksGet/TasksCancel). **Not ported:** the conversational `A2AClient` LLMClient machinery (history shipping, context-var sync, HITL over wire, `urn:ag2:client-tools:v1` client tools, reconnect/resubscribe), REST/gRPC transports, push notifications, card signing, cluster mode, tenants |
| `eval/` | `ago/eval` | partial | D-051 (supersedes the D-010 deferral for this slice). Produce-then-grade: `Run(ctx, agent, suite)` asks the agent per `Case` and grades the reply history. `BuildTrace` reconstructs the `Trace` **from the stream's event log — no OTEL** (the core deviation; Python rebuilds from spans): `ModelResponse` → `LLMCalls` paired with the preceding `model_call` `UsageEvent`, tool events → `ToolCalls` matched by call id, `Tokens()` sums `UsageEvent`s via `ago/usage`. Scorers: `Correctness` (exact/casefold/contains), `TokenBudget`, `ToolsCalled` (any/all), `Threshold`, `LLMJudge` (one headless client call, JSON `{score, reason}`, clamped), `Pairwise` (dual-order swap, flip → tie); scorer errors become failing scores, never aborts. `RunResult` `SaveJSON`/`LoadJSON` (events round-trip through `@type`, unlike Python's event-less load) + `Compare` win-rate tally (ties = 0.5). **Deferred:** OTEL/Tempo/OTLP sources, `evaluate_traces`, the `@scorer` DI decorator, attribution scorer, repeats/per-task configs/`BudgetThresholds`/lifecycle events, case concurrency (sequential in AGo), `RunDiff`, `Suite.from_jsonl`/tags/metadata, pairwise single-order mode/Wilson CI/Cohen's kappa |
| `acp/` (ag2/acp: agent.py, executor.py, bridge.py, client.py, permissions.py, elicitation.py) | `ago/acp` | partial | D-052 (supersedes the D-010 deferral for this slice, owner directive). Both roles over newline-delimited JSON-RPC 2.0 stdio (transport template `ago/mcp` stdio). Serve: `Serve`/`ServeAgent` — `initialize` (min-version, truthful text-only capabilities), `session/new`, `session/prompt` running `Handle(sessionID, userText, emit)` with per-emit `agent_message_chunk` updates and final-text-once dedup; `ServeAgent` gives per-session conversation continuity and live `ModelMessageChunk` streaming. Client: `NewBridge`/`SessionNew`/`Prompt`/`Close` — subprocess + handshake, chunk accumulation, inline answers to agent→client requests from the `Permission`/`Elicit` policies (allow picks the first allow_once/allow_always option; deny is never fabricated). Wire dialect = the Python SDK's: `content` blocks and nested `{"outcome": {"outcome", "optionId"}}` permission answers (`selected`/`cancelled`; deny selects a reject option the agent offered); the earlier `contentBlock`/flat spellings are still accepted on input (D-052), text-only elicitation. **Not ported:** `session/cancel` + `cancelled` stop reason, `session/load` replay, the multi-session store (LRU/TTL/queues — one request at a time), auth, thought/plan/tool-call/usage updates, mediated `fs/*` + `terminal/*` + `tool_gateway.py`, `guard.py`, model pickers/remote transports |
| `ag_ui/` (stream.py, asgi.py, run_input.py, capabilities.py, events.py) | `ago/agui` | partial | D-053 (supersedes the D-010 deferral for this slice, owner directive). AG-UI 1.0 serving: typed event structs (camelCase, omit-when-unset, `type` discriminator; REASONING_* — THINKING_* left the protocol in 1.0), `RunAgentInput` parsing (unknown-role strip, pre-run base64 refusal), the projection (chunks → TEXT_MESSAGE_*, reasoning spans, tool calls closed before execution, TOOL_CALL_RESULT string-or-parts, client hand-offs, task lifecycle → SUBAGENT_*, per-(provider, model) usage with the anthropic/bedrock cache correction, pendingToolCallIds), `Handler`/`ServeAgent` (GET capabilities, POST → SSE, 400 pre-stream, protocol-major refusal, cancelled outcome on server-side stop). **Not ported:** the held-turn interrupt machinery (`interrupts.py` — ServedTurns/retention/resume entries/proofs/tool-approval; a question fails the run as RUN_ERROR), REASONING_ENCRYPTED_VALUE, STATE_SNAPSHOT emission (agent variables/config/hook are not readable from outside `agent.Agent`; a `Run` callback can emit them), prompt-level inputs fold into the opening transcript request, camelCase-only input parsing |
| `a2ui/` | — | deferred | Protocol server out of scope (D-010) |
| `streams/redis/` | — | deferred | Redis-backed stream storage needs an external client (D-003); `ago/stream` ships the memory backend (D-004) |
| `files/` | `ago/files` (+ `ago/config/{openai,anthropic}` files clients) | ported | D-046. `FileProvider`/`UploadedFile`/`FileContent`, `Client` contract, `API` wrapper (`NewAPI`); Anthropic client on `/v1/files` with the `files-api-2025-04-14` beta flag, no purpose on upload, `next_page`/`page` pagination. Python's `created_at`/`media_type` survive via `Raw` |
| `plugin.py` (`Plugin`, `PluginTarget`) | `ago/plugin` | partial | D-044. `Plugin` interface + `NewBundle` builder carrying tools, middleware factories, static prompt fragments and dynamic prompt hooks, applied via `agent.WithPlugin` **at agent construction** (appends — plugin middleware lands innermost, hooks run like `WithPromptHook`). **Not carried:** plugin HITL hook, dependencies, variables, observers, assembly policies (no agent slot for them); Python's turn-scoped `additional_plugins` attach/detach is not ported |
| `extensions/`, `textual.py`, `mypy.py`, `_import_utils.py`, `_telemetry_consts.py`, `types.py`, `utils.py` | — | deferred | Python infrastructure without a Go counterpart in scope (typing helpers, textual UI, telemetry constants) |
| — | `ago/internal/uuid` | ported | RFC 4122 v4 from `crypto/rand` (D-003). No test files |
| — | `ago/examples/{ask,tools,hitl,subagent,group,knowledge_memory,mcp_http,mcp_stdio,multi_turn,observers,policies_compact,safety_halt,structured_output}` | ported | wave 3. Offline runnable programs (scripted configs, no API keys); all thirteen verified 2026-10-29 with `go run` (see Gates above) |

## Deferred subsystems (summary)

Everything below is a deliberate deferral with a decision record — none of it is silently
missing:

- **Redis streams** (`ag2/streams/redis`) and **SQLite/Redis knowledge backends** — need
  external deps; D-003/D-011.
- **WebSocket network link** (`ag2/network/transport/ws.py`) and **federation**
  (`remote.py`) — single-host subset only; D-010/D-031.
- **Live/voice** (`ag2/live/`, `events/voice.py`) — realtime audio stack; not ported.
- **Protocol/eval servers** — `acp/`, `a2ui/`; D-010. The A2A
  transport-level subset is ported (D-049/D-050); the conversational A2AClient
  machinery (history/context-sync/client-tools/HITL over the wire) is not. The
  eval harness is ported as `ago/eval` (D-051, partial — see its table row).
  ACP is ported as `ago/acp` (D-052, partial — both stdio roles, session
  store/guard/mediated fs deferred — see its table row). AG-UI is ported as
  `ago/agui` (D-053, partial — the served run is one synchronous exchange; the
  held-turn interrupt machinery is deferred — see its table row).
- **OpenAI Responses API** (and Decisions API, Containers) — chat completions is the
  supported OpenAI surface; D-008/D-017. The Files API client is ported (D-046), but
  `FileIdInput` message mapping still fails fast (D-017/D-020).
- **Hosted/builtin tools on OpenAI Responses** — the file_search, image_generation,
  shell, MCP and tool_search Responses mappings (`ag2/config/openai/mappers.py
  tool_to_responses_api`) have no client to attach to; chat completions rejects every
  hosted schema (D-047/D-008). Gemini grounding/server-side invocation replay has no
  event carrier (D-047). Tool content packages (sandbox/shell/search/skills/dynamic)
  stay deferred.
- **Ollama streaming** (D-022), **cron/cadence watches** (D-024), **middleware
  metrics/telemetry builtins** (D-013).
