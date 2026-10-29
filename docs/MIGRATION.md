# Migration map: Python AG2 → AGo

This is the class/function-level map for moving code from AG2 (Python, commit `fa4e0fb`,
2026-10-27) to AGo (Go, module `ago`). The behavioral source of truth is the Python reference
in `ag2/` and its distilled specs in `docs/specs/`; the Go contracts are in
`docs/ARCHITECTURE.md`. Per-package completeness lives in [docs/PORT_STATUS.md](PORT_STATUS.md);
every deliberate deviation carries a decision record in [docs/DECISIONS.md](DECISIONS.md)
(referenced below as `D-xxx`).

Reading conventions:

- **Python column** names the 2026 AG2 API (classic `ConversableAgent`/`GroupChat` API is gone
  in AG2 itself; see §5).
- **Go column** gives the exact exported symbol, verified against the packages in this repo.
- `*agocontext.Context` is `ago/context` — the alias exists because it collides with stdlib
  `context`. Cancellable operations take a `context.Context` as first parameter where Python
  was `async`.

## 1. Core API

### Agent and turn lifecycle (`ag2/agent.py` → `ago/agent`)

| Python | Go | Differences |
|---|---|---|
| `Agent(name, prompt=..., config=..., hitl_hook=..., tools=..., middleware=..., dependencies=..., variables=..., response_schema=..., tasks=..., stream=...)` | `agent.New(name, prompt string, opts ...agent.Option)` | Dataclass kwargs → option funcs: `WithConfig`, `WithHitlHook`, `WithTools`, `WithMiddleware`, `WithResponseSchema`, `WithDependencies`, `WithVariables`, `WithStreamBase`, `WithTasks`, `WithPromptFragments`, `WithPromptHook`. Static prompt is one string; extra fragments via `WithPromptFragments`. No `plugins`/`observers`/`knowledge`/`assembly` kwargs (see §5) |
| `await agent.ask(*msg, ...)` | `a.Ask(ctx, msgs ...agent.Sendable) (*AgentReply, error)` | Blocking convenience wrapper over `Run`. `Sendable = any` (string, `events.Input`, JSON value) replaces `*msg` variadic inputs; no per-call dependencies/variables/prompt overrides (D-037) — set `WithDependencies`/`WithVariables`/`WithPromptFragments` at construction |
| `async with agent.run(...) as run: reply = await run.result()` | `run, _ := a.Run(ctx, msgs...); run.Start(ctx); defer run.Close(); reply, err := run.Result(ctx)` | Python async context manager → explicit `Start`/`Result`/`Close`; `Close` ends the turn scope (unsubscribe, release per-stream lock) |
| `run.enqueue(*msg)`, `run.stream` | `run.Enqueue(parts ...events.Input)`, `run.Stream() stream.Stream` | Mid-turn inbox parity |
| `AgentReply.body` | `reply.Body() string` | `""` when the model returned no text (Python `None`) |
| `await reply.content(retries=N)` (schema-validated) | `agent.ContentAs[T](ctx, reply, retries) (T, error)` | Package-level generic instead of a method (Go generics cannot be methods); re-asks on validation failure with the error appended, same semantics |
| `reply.files`, `reply.history`, `await reply.usage()` | `reply.Files()`, `reply.History() *stream.History`, `reply.Usage() *usage.Report` | `Usage()` is a synchronous aggregation of the stream's `UsageEvent`s — no await |
| `reply.ask(...)` / `reply.run(...)` | `reply.Ask(ctx, ...)` / `reply.Run(ctx, ...)` | Continuation on the same stream/context |
| `resume(*events, ...)` | `a.Resume(ctx, evs ...events.Event) (*AgentReply, error)` | All but last event seed history; last is the re-entry trigger |
| `as_tool(description=..., name=...)` | `a.AsTool(opts ...agent.AsToolOption) tools.Tool` | Options `WithToolName` (default `task_<name>`, Python `subagent_tool.py` parity — D-040) /`WithToolDescription`; the tool takes `objective` + optional `context` and each call runs the delegation machinery (`runTaskOn`): `TaskStarted`/`TaskCompleted`/`TaskFailed` plus one rolled-up subtask `UsageEvent` on the caller's stream, child state = copy of the caller's dependencies/variables (approval bypass key dropped), HITL bridge to the parent's hook (the forwarded question is transcribed on the parent stream — D-041). A failed sub-task is the text `Sub-task '<name>' failed: …`; a HITL failure ends the calling turn (subtask tools `run_subtask`/`run_subtasks` live in `agent` too) |
| `agent.task(title, ...)` | `a.Task(ctx, title string, opts ...task.Option) (*task.Task, error)` | Checkpoint store / TTL / resume via `task.WithCheckpointStore`, `task.WithTTL`, `task.WithResumeFrom`; `agent.TaskWithCheckpointStore`, `agent.TaskWithTTL` forwards; the task always opens its own private stream (D-030) |
| `PromptHook` (async prompt hook), `update_system_message` | `agent.WithPromptHook(fn agent.PromptHook)`, `agent.WithPromptFragments(...)` | `PromptHook func(*agocontext.Context) ([]string, error)` |
| `AgentReply.response` | `reply.Response() *events.ModelResponse` | Accessor instead of attribute |
| `ConfigNotProvidedError` on missing config | `agent.ErrConfigNotProvided` | Sentinel; `errors.Is` |

### Context, stream, HITL (`ag2/context.py`, `ag2/stream.py`, `ag2/history.py`, `ag2/hitl.py`)

| Python | Go | Differences |
|---|---|---|
| `ConversationContext` (alias `Context`) | `agocontext.Context` (package `ago/context`) | Struct with exported fields `Stream`, `Dependencies`, `Variables`, `Prompt`, `ToolCallID`; mutation via methods (`SetDependency`, `SetVariable`, `AddPrompt`, `PromptLines`). Safe for concurrent use; `Clone()` shares the stream, copies maps |
| `context.enqueue/input/ask/send/spawn_background` | `Enqueue/Pending/Drain`, `Input(ctx, msg, timeout)`, `Ask(ctx, req)`, `Send`, `SpawnBackground` | `Drain()` returns and clears pending input (Python drains implicitly in the turn loop) |
| `dependency_provider` / DI annotations `Inject`, `Variable`, `Context` | `Dependency(id)`, `MustDependency(id)`, `SetDependency`; tool-side: `tools.NewInject`, `tools.NewVariable`, a `*agocontext.Context` parameter (§2) | DI values keyed by string id (Python keys are arbitrary objects) |
| `hitl_hook: HumanHook`, `default_hitl_hook` | `hitl.HumanHook`, `hitl.DefaultHook` (`→ hitl.ErrNotProvided`), `ctx.SetHitlHook(h)` | `HumanHook func(ctx context.Context, req *events.HumanInputRequest) (*events.HumanMessage, error)`; typed errors `NotProvidedError`/`FailedError`/`TimeoutError` match the Python `HumanInputError` family |
| `Stream` Protocol, `MemoryStream`, `SubStream` | `stream.Stream`, `stream.MemoryStream`, `stream.SubStream` | Interfaces instead of protocols; `NewMemoryStream(opts ...MemoryOption)` with `WithStorage`/`WithStreamID`/`WithPersistAll`/`WithLogger`; `stream.LoadHistory(id, storage, evs)` to replay a saved transcript |
| `stream.subscribe(..., interrupt=, condition=, timeout=)` | `Subscribe(fn stream.Subscriber, opts stream.SubscribeOptions) stream.SubID` | `Subscriber func(events.Event) error` (no context arg); `SubscribeOptions{Interrupt, Condition, Timeout}` |
| `stream.where(cond)`, `stream.get(cond)`, `stream.join()` | `Where(c events.Condition) Stream`, `Get(c events.Condition) (events.Event, error)` | `Get` scans history + incoming; `join` has no direct equivalent — use `watch.EventWatch` (§5) |
| `Storage` Protocol (`save_event/get_history/...`) | `stream.Storage` interface | Only `MemoryStorage` ships; Redis backend deferred (D-003/D-004). Durable logs go through `knowledge.EventLogWriter` |
| `History.get_events()/replace()`, `unanswered_tool_calls`, `close_unanswered_tool_calls` | `stream.History.GetEvents()/Replace()`, `events.UnansweredToolCalls(evs)`, `events.CloseUnansweredToolCalls(evs)` | Repair helpers live in `events`, human-readable repair sentence preserved verbatim (Python HUMAN_INPUT_ABANDONED_TOOL_RESULT) |
| `EstimatedTokens(event, chars_per_token=4)` | `events.EstimatedTokens(ev, charsPerToken)` | Same flat weights (image/audio 1000, video/document 2000) |
| async dispatch, `sync_to_thread` | Synchronous dispatch in the caller's goroutine | D-004: no thread offloading; background work via `SpawnBackground` (goroutine + panic recovery) |

### Response schemas (`ag2/response/` → `ago/response`)

| Python | Go | Differences |
|---|---|---|
| `ResponseSchema[T]` (pydantic `TypeAdapter`) | `response.NewSchema[T](name, description) *response.Schema[T]` | JSON schema generated by reflection over the Go type (`response.SchemaOf`); field docs via `agschema:"description=...,enum=...,required,min=1"` tags; top-level scalars wrap into `{"data": ...}` (parity) |
| `RawSchema` | `response.RawSchema(schema map[string]any, opts ...RawOption) *RawSchemaImpl` | Options `WithRawName`/`WithRawDescription` |
| `PromptedSchema` | `response.NewPromptedSchema(inner response.Proto, opts ...PromptedOption)` | Wraps any `Proto`, adds prompt text, validation = parse only |
| `response_schema` decorator / `ResponseProto` protocol | `response.Proto` interface | `Name/Description/JSONSchema/SystemPrompt/Validate(response, ctx)`; the Python `provider` parameter of `validate` is dropped (D-007) |
| `DecisionSpec` / `Question` / `DecisionOption` | `response.DecisionSpec` / `response.Question` / `response.DecisionOption` | Same enum-schema shape |
| validation exception | `*response.ValidationError{Err, Response}`, `response.ErrInvalidResponse` | The agent's `ContentAs[T]` retry loop keys off `*ValidationError` |

### Usage, task, spec, testing

| Python | Go | Differences |
|---|---|---|
| `UsageReport` / `UsageRecord` / `Usage.from_events` (`ag2/usage.py`) | `usage.Report` / `usage.Record` / `usage.FromEvents(evs)` | Totals + `ByModel`/`ByProvider`/`ByKind` buckets; only `UsageEvent`s with reported tokens count (`usage.Reported`) |
| `TaskState`, `TaskSpec`, `TaskMetadata`, `CheckpointStore`, `Task` async CM (`ag2/task.py`) | `task.State`, `task.Spec`, `task.Metadata`, `task.CheckpointStore`, `task.MemoryCheckpointStore`, `task.Task` | Python's async context manager (auto-complete/auto-fail) → explicit terminal calls `Complete/Fail/Cancel/Expire` (each returns `error`); `task.Start(ctx, owner, spec, opts...)` opens; `task.DepKey = "ag:task"` dependency key preserved |
| `TaskConfig` (`ag2/agent.py`) | `agent.TaskConfig` + `agent.WithTasks(cfg)` | Fields `Config`, `Prompt`, `IncludeTools`, `ExcludeTools`, `ExtraTools`, `MaxConcurrency`; `IncludeTools []string` — nil = all parent tools (Python `include_tools=None` default), empty slice = none, allowlist of names otherwise (`ExcludeTools` applies after it). Breaking vs earlier AGo builds where `IncludeTools bool` defaulted to "no parent tools" |
| `AgentSpec.from_agent() / to_agent()` (`ag2/spec.py`) | `spec.FromAgent(a)`, `(*spec.AgentSpec).ToAgent(available map[string]tools.Tool, opts ...agent.Option)` | Wire keys stay snake_case (`tool_names`, `response_schema`); missing tool → `tools.ErrResolution` |
| `TestConfig` / `TrackingConfig` (`ag2/testing.py`) | `agotest.NewTestConfig(responses ...*events.ModelResponse)`, `agotest.NewTrackingConfig(inner config.ModelConfig)` | Import alias `agotest "ago/testing"`; scripted full responses with a shared cursor and recorded call snapshots (Python's partial-turn vocabulary collapsed — D-018) |
| `exceptions.py` hierarchy (`AG2Error`, `ConfigNotProvidedError`, `HumanInputError`, `ToolConflictError`, …) | Per-package sentinels: `agent.ErrConfigNotProvided`, `hitl.ErrNotProvided` (+ typed errors), `config.ErrNotProvided/ErrUnsupportedInput/ErrUnsupportedTool`, `tools.ErrConflict/ErrNotFound/ErrResolution`, `stream.ErrHandled`, `response.ErrInvalidResponse` | Exceptions → `errors.New` sentinels + `%w` wrapping + `errors.Is/As` (no exception classes) |
| `SendableMessage` (`ag2/types.py`) | `agent.Sendable = any` | JSON-decodable value, `events.Input`, or string |
| `MediaInput` factories (`ImageInput/DocumentInput/...`) | `events.ImageInput/DocumentInput/AudioInput/VideoInput(url, path, data, mediaType)` | One function per modality covering url/file/data sources; `events.MediaTypeForPath` for extension inference |

## 2. Tools and middleware

### Tools (`ag2/tools/` → `ago/tools`)

| Python | Go | Differences |
|---|---|---|
| `Tool` ABC (`schemas()`, `register(stack, context)`) | `tools.Tool` interface: `Name()`, `DeclaredInCode()`, `Schemas(*agocontext.Context) ([]any, error)`, `Register(*agocontext.Context, []middleware.Middleware) ([]stream.SubID, error)` | No `ExitStack`: `Register` returns the subscription ids it created; the turn scope unsubscribes them on `Close` (D-032) |
| `@tool` decorator / `FunctionTool` | `tools.NewFunctionTool(name, description string, fn any, opts ...FunctionToolOption)` | `fn` is a plain func; parameters bound by reflection from the model's JSON arguments (argument fields = one struct). Options: `WithTimeout`, `WithMiddleware`, `WithSource`, `WithToolSchema` (explicit parameters schema) |
| `fast_depends` + pydantic schema generation | `response.SchemaOf(reflect.Type)` | stdlib reflection + `json`/`agschema` tags; no-arg → `{"type":"object","properties":{}}` (parity). `fast_depends.Depends` factory-valued parameters are not ported (D-036) |
| `Inject(real_name, default=...)`, `Variable(name, default=...)` (`annotations.py`) | `tools.NewInject(id, def ...any)`, `tools.NewVariable(name, def ...any)`; struct tags `aginject:"id"`, `agvariable:"name"`, `agdefault:"..."` | Marker params resolved from `ctx.Dependencies` / `ctx.Variables` at call time (D-009/D-016) |
| `Context` annotation (inject conversation context) | Parameter of type `*agocontext.Context` (any param name) | `ToolCallID` stamped during execution |
| `Toolkit(*tools, name=, middleware=)` | `tools.NewToolkit(name string, mws []middleware.Middleware, ts ...Tool)` | Duplicate name → `tools.ErrConflict` |
| `ClientTool` (`declared_in_code=False`) | `tools.NewClientTool(schema map[string]any)` | Call → `events.ClientToolCallEvent` for the embedding application |
| `ToolSchema` / `FunctionToolSchema` dataclasses | `tools.Schema` struct (function schema) + `tools.ToolSchema` interface (`SchemaType()`) | Typed schemas for builtin/hosted tools; `DeferLoading` field preserved |
| `ToolResult(*inputs, final=, metadata=)` | `events.ToolResult` + `events.NewToolResult(final bool, values ...any)` | Lives in `events` (it is a wire value); values wrapped via `events.EnsureInput` |
| `ToolExecutor(serializer)` | `tools.NewExecutor()`, `(*Executor).Register(ctx, ts, known, mws)` | Parallel goroutine per call; `HumanInputError` cancels siblings; `final=true` forces a `ModelResponse{ResponseForce: true}`; timeout → `ToolErrorEvent` (same message text) |
| `resolve_tools(all_tools, context)` | `tools.ResolveTools(all []Tool, cx *agocontext.Context) (*tools.ResolvedTools, error)` | Same precedence: declared-in-code wins in declaration order; runtime tools never override; remote-vs-remote first wins; losing toolkit members pruned for the turn |
| `SubagentTool` (`tools/subagents/`) | `agent.AsTool` + `agent` subtask tools | Lives in `agent` to avoid an import cycle (`tools` cannot import `agent`) |
| `MCPToolkit` (`tools/toolkits/mcp_server/`) | `mcp.New(config) (*mcp.Client, ...)` + `mcp.Tools(cl) ([]tools.Tool, error)` | No `Toolkit` subtype — a helper converts remote tools to `tools.Tool` with `DeclaredInCode=false`; stdio + streamable HTTP, paginated `ListTools`, content mapping (D-029, D-034). Python's `mcp_tool` `Variable` indirection (and `Depends`/`Inject` parameters) is not ported — remote tools map their declared schemas directly (D-029, D-036) |
| `ToolSearchTool`, builtin/hosted tools (`tools/builtin/`, `search/`, `sandbox/`, `shell/`, `skills/`, `dynamic/`) | — | Deferred; the seams exist (`tools.ToolSchema`, `events.BuiltinToolCallEvent/BuiltinToolResultEvent`, hosted-type precedence) — see §5 |

### Middleware (`ag2/middleware/` → `ago/middleware`)

| Python | Go | Differences |
|---|---|---|
| `BaseMiddleware` (4 hooks, each with `call_next`) | `middleware.Middleware` interface: `OnTurn` / `OnLLMCall` / `OnToolExecution` / `OnHumanInput` | Hook funcs are named types (`TurnFunc`, `LLMCallFunc`, `ToolExecFunc`, `HumanInputFunc`); embed `middleware.Base` for pass-through defaults |
| `Middleware` factory wrapper (`(cls, **options)`) | `middleware.Factory func() Middleware` | One instance per turn, same lifecycle |
| `ConditionalMiddleware` | `middleware.If(cond events.Condition, f Factory) Factory` | |
| middleware composition (`fold` in Python wiring) | `middleware.Chain(fs ...Factory) Factory` | Additive composition |
| `HistoryLimiter(n)` | `middleware.HistoryLimiter(n) Factory` | |
| `TokenLimiter(budget)` | `middleware.TokenLimiter(budget) Factory` | Char-estimated budget |
| `RetryMiddleware` | `middleware.Retry(attempts int) Factory` | |
| `LoggingMiddleware` | `middleware.Logging() Factory` | stdlib `slog` |
| `ApprovalRequired` / `approval_required` | `middleware.Approval() Factory` | Raises `ToolApprovalRequest` through HITL; "always" grants keyed on call source |
| `MetricsMiddleware`, `TelemetryMiddleware` (OTEL) | — | Deferred (D-013) |

## 3. Provider configs

All Go configs live in `ago/config/<provider>` and implement `config.ModelConfig`
(`Provider()`, `Model()`, `Create() (LLMClient, error)`, `Clone(overrides)`). Clients implement
`config.LLMClient.Call(ctx, messages, conv, opts)`. Missing model/key →
`config.ErrNotProvided` (parity with `ConfigNotProvidedError`). No SDKs: every provider is raw
HTTP/SSE over `ago/internal/httpx` + `ago/internal/sse` (D-003, D-017).

| Python config | Go config (constructor) | Status | Notes / what is narrower |
|---|---|---|---|
| `ModelProvider` enum (11 values) | `config.Provider` (same 11 values, incl. `vertexai`) | full | Vocabulary preserved |
| `OpenAICompletionsConfig` (and deprecated alias `OpenAIConfig`) | `config/openai.OpenAICompletionsConfig` — `NewOpenAICompletions(model)` | **full** | Chat Completions: SSE streaming with tool-call delta merge, tools, strict structured output (`openai.StrictifySchema`, `openai.ResponseFormat`), usage incl. cached/reasoning tokens; env `OPENAI_API_KEY`. Sampling/verbosity fields subset |
| `OpenAIResponsesConfig` | — | deferred (D-008) | Responses API not ported; `defer_loading` tool schemas fail fast |
| `OpenAIDecisionsConfig` | — | deferred (D-008) | |
| `config/openai/files.py`, `containers.py`, `FilesAPI` | — | deferred (D-017) | `FileIdInput` fails fast with `config.ErrUnsupportedInput` |
| `AnthropicConfig` | `config/anthropic.AnthropicConfig` — `NewAnthropic(model)` | **full** | Messages API over raw HTTP/SSE: `tool_use`/`tool_result` replay with orphan repair, prompt caching (`cache_control` injection), thinking blocks + redacted-thinking handling, usage incl. cache tokens; env `ANTHROPIC_API_KEY` |
| `GeminiConfig` | `config/gemini.GeminiConfig` — `NewGemini(apiKey, model, opts...)` | **full** | `generateContent` + SSE streaming, function declarations, `usageMetadata` normalization (tool-use prompt tokens, thinking tokens); env `GEMINI_API_KEY` |
| `VertexAIConfig` | — (only the `ProviderVertexAI` enum value) | deferred (D-022) | Google auth chain not ported |
| `BedrockConfig` (Converse) | `config/bedrock.BedrockConfig` — `NewBedrock(model)` | **stub** (D-008) | Config validates the model; `Create()` → `config.ErrNotImplemented` (needs SigV4 + botocore credential chain) |
| `MistralConfig` | `config/mistral.MistralConfig` — `NewMistral(model)` | **full** (chat-compat subset) | OpenAI-compatible engine; guardrails, prediction, `prompt_mode`, `safe_prompt`, logprobs, Files not ported (D-008/D-021); env `MISTRAL_API_KEY` |
| `OllamaConfig` | `config/ollama.OllamaConfig` — `NewOllama(model, opts...)` | **full, non-streaming** (D-022) | `/api/chat` with tools + usage; `Streaming: true` → `ErrUnsupportedInput`. Default host `http://127.0.0.1:11434`; env `OLLAMA_API_KEY` for Ollama Cloud |
| `DashScopeConfig` | `config/dashscope.DashScopeConfig` — `NewDashScope(model)` | **full** (compatible-mode) | OpenAI-compatible dialect; default base URL is the `compatible-mode` root (AGO-DEVIATION, D-021); env `DASHSCOPE_API_KEY` |
| `ZAIConfig` | `config/zai.ZAIConfig` — `NewZAI(model)` | **full** (chat-compat subset) | Thinking flags, token-cache opt-out, sensitive-word check, Files API not ported (D-021); env `ZAI_API_KEY` |
| `XAIConfig` (gRPC SDK) | `config/xai.XAIConfig` — `NewXAI(model)` | **stub** (D-008) | `Create()` → `config.ErrNotImplemented` (gRPC transport out of scope) |
| `TypeSafeConfig` | `config/typesafe.TypeSafeConfig` — `NewTypeSafe(model)` | **stub** (D-008) | Same stub pattern (niche SDK out of scope) |
| `input_acceptance.py` (`accepts_input`, `input_modalities`, …) | — | not ported | No capability table; unsupported inputs fail fast per provider instead of being pre-checked |
| `ag2/mcp/sampling.py` pseudo-client (implements `ModelConfig` + `LLMClient`) | — | not ported | MCP server product is deferred (§5) |

Shared engine: `config.RunChatCompat` (the OpenAI Chat-Completions wire protocol used by
openai/zai/mistral/dashscope), `config.BuildMessages`, `config.ToolToAPI`,
`config.EnsureObjectSchema`, `config.NormalizeUsage`. Streaming is per-config `Streaming bool`
(default off), matching Python.

## 4. Events

`ag2/events/*` → `ago/events` (one Go file per Python module). Base contract: embed
`events.Base` + implement `Event` (`EventType()`, `CreatedAt()`, `Transient()`,
`Conversational()`). Python's `Field()` descriptors and condition DSL become plain structs plus
predicate combinators (D-005): `events.Condition`, `AllOf/AnyOf/Not/TypeOf/Equal/TypeOfAny`.
`to_dict()/from_dict()` → `events.MarshalEvent/UnmarshalEvent` with the `"@type"` discriminator
(JSON round-trip is tested lossless). JSON field names match Python (snake_case).

| Python event | Go struct | Notes |
|---|---|---|
| `BaseEvent` | `events.Base` (embedded) + `events.Event` interface | |
| `Input`, `ensure_input` / `ensure_request` | `events.Input` interface, `events.EnsureInput`, `events.EnsureRequest(msgs ...any)` | |
| `ModelRequest` / `DrainedModelRequest` / `MessageEnqueued` | `events.ModelRequest` / `events.DrainedModelRequest` / `events.MessageEnqueued` | Same drain + LLM-trigger semantics |
| `TextInput` / `DataInput` / `BinaryInput` / `FileIdInput` / `UrlInput` | `events.TextInput` / `events.DataInput` / `events.BinaryInput` / `events.FileIdInput` / `events.UrlInput` | Same fields; `BinaryType` enum values `binary/image/audio/document/video` |
| `ModelMessage` / `ModelMessageChunk` / `ModelReasoning` | `events.ModelMessage` / `events.ModelMessageChunk` / `events.ModelReasoning` | Chunks/reasoning stay transient |
| `ModelResponse` | `events.ModelResponse` | `Message *ModelMessage`, `ToolCalls`, `Usage`, `ResponseForce`, `Files`, tracing fields `Model/Provider/FinishReason/ResponseID` |
| `Usage` (frozen dataclass, `float \| None` fields) | `events.Usage` (`*int64` fields) + `Usage.Add` | D-006: integer counters, `nil` = not reported |
| `BinaryResult` | `events.BinaryResult` | |
| `UsageEvent` (non-conversational) | `events.UsageEvent` | `Kind`: `model_call` / `subtask` / `compaction` / `aggregation` (constants exported); recursive `Parts` |
| `HumanInputRequest` / `ToolApprovalRequest` / `HumanMessage` | `events.HumanInputRequest` (+ `NewHumanInputRequest`) / `events.ToolApprovalRequest` / `events.HumanMessage` | |
| `ToolResult` | `events.ToolResult` (+ `NewToolResult`) | `final` flag and metadata preserved |
| `ToolCallsEvent` / `ToolResultsEvent` | `events.ToolCallsEvent` / `events.ToolResultsEvent` | |
| `ToolCallEvent` / `ClientToolCallEvent` | `events.ToolCallEvent` (+ `NewToolCallEvent`) / `events.ClientToolCallEvent` | `id/name/arguments/vendor_metadata/source` preserved |
| `ToolResultEvent` / `ToolErrorEvent` / `ToolNotFoundEvent` | `events.ToolResultEvent` (+ `FromResultCall`) / `events.ToolErrorEvent` (`Error string` instead of `Exception`) / `events.ToolNotFoundEvent` | Errors are serialized to strings on the wire |
| `BuiltinToolCallEvent` / `BuiltinToolResultEvent` | `events.BuiltinToolCallEvent` / `events.BuiltinToolResultEvent` | Vocabulary present for future hosted tools |
| `CompactionSummary` | `events.CompactionSummary` | `[Summary of earlier conversation]` replay text preserved |
| `TaskStarted/Progress/Completed/Failed/Expired/Cancelled`, `TaskSpec` | `events.TaskStarted` … `events.TaskCancelled`, `events.TaskSpec` | `TaskProgress` transient |
| `Severity`, `ObserverAlert`, `HaltEvent` | `events.Severity` (`info…fatal`) / `events.ObserverAlert` / `events.HaltEvent` | Halt → synthetic `"HALTED: <reason>"` response in the agent |
| Lifecycle: `ObserverStarted/Completed`, `CompactionStarted/Completed/Failed`, `AggregationStarted/Completed/Failed`, `EventLogFailed`, `UnknownEvent` | same names in `ago/events` | All transient |
| `ProviderReplay` mixin + provider events (`OpenAIReasoningEvent`, `AnthropicRedactedThinkingEvent`) | — | Not separate events: provider-native replay state is handled inside each provider client's mapping |
| Condition DSL (`Event.field == x`, `A \| B`, `~c`) | `events.Condition` + `AllOf/AnyOf/Not/TypeOf[T]/Equal/TypeOfAny` | Predicate functions instead of operator overloading (D-005) |
| `events/voice.py` | — | Deferred (live/voice stack not ported) |

## 5. Not ported — and what replaces it

Everything below is a deliberate decision (recorded in `docs/DECISIONS.md`), not an oversight.

| Python AG2 | AGo answer |
|---|---|
| Classic API: `ConversableAgent`, `initiate_chat`, `generate_reply` | Already removed in the 2026 AG2 rewrite itself; AGo ports the replacement: `agent.Agent`, `Ask`/`Run` (D-002) |
| `GroupChat` / `GroupChatManager` | `ago/network`: `Hub` + channel adapters — `WorkflowAdapter` (declarative `TransitionGraph`), `DiscussionAdapter` (round-robin), `ConversationAdapter`, `ConsultingAdapter`; `Agent.AsTool` for delegation |
| `register_reply` (reply-function chain) | `middleware.Factory` list on the agent — the four-hook onion (`OnTurn`/`OnLLMCall`/`OnToolExecution`/`OnHumanInput`) |
| `human_input_mode` | `agent.WithHitlHook(h hitl.HumanHook)` |
| `update_system_message`, `register_hook` | `WithPromptHook`/`WithPromptFragments`; cross-cutting hooks → middleware or `observers.Observer` |
| Response cache (classic AG2 `cache_seed`) | Intentionally absent — the 2026 AG2 has no framework-level response cache either (spec `config-llm.md` §4); only provider-side prompt caching knobs (Anthropic `PromptCaching`, Gemini `cached_content`) and cache-token accounting |
| `config_list` + `register_model_client` | Gone in 2026 AG2; AGo has one typed `config.ModelConfig` per provider |
| `ExitStack`-based registration lifetime | Turn-scope ownership: tool `Register` returns subscription ids, agent unsubscribes on `Close` (D-032) |
| `Plugin` protocol, `plugins=[...]` | Not ported — compose via `WithMiddleware` + `WithTools` |
| `Agent(..., observers=[...])` wiring | No agent option: observers subscribe directly on the conversation stream — `observers.Observer.Register(ctx)` (D-025) |
| `Agent(..., assembly=[...])` (`AssemblerMiddleware`) | No built-in assembler: wrap `policies.AssemblyPolicy` transforms in a `middleware.Factory` yourself (pattern in `examples/policies_compact`) |
| `Agent(..., knowledge=KnowledgeConfig)` | No agent option: pass `knowledge.Store` to `policies.NewWorkingMemoryPolicy`/`NewEpisodicMemoryPolicy` and wire via middleware (pattern in `examples/knowledge_memory`) |
| `FilesAPI` / `FilesClient` (`ag2/files/`) | Deferred (D-017) — file-id inputs fail fast |
| OpenAI Responses API, Decisions API | Deferred (D-008) — Chat Completions is the supported OpenAI surface |
| Hosted/builtin tools: `web_search`, `code_execution`, `shell`, `file_search`, `image_generation`, `skills`, `tool_search`, … and the `search/sandbox/shell/code/skills/dynamic` tool packages | Deferred; the seams exist (`tools.ToolSchema`, `events.BuiltinToolCallEvent/ResultEvent`, hosted-type precedence in `tools.ResolveTools`) |
| `ag2/mcp` server product (`MCPServer`, `mcp_tool`, sampling, elicitation) | Deferred — only the minimal MCP **client** is ported (`ago/mcp`, D-029); `input_required` answering, discovery cache, protocol-era negotiation not ported (D-034 covers timeouts) |
| Network: WebSocket link (`transport/ws.py`), federation (`remote.py`), view policies (`views/`: FullTranscript, WindowedSummary, NamedTranscript, NamedWindowedSummary), task mirror, hub expectation sweepers/arbiter, client LLM tools (`peers`/`channels`/`tasks`/`context`/`delegate`) | Deferred (D-010/D-031) — the ported subset is single-host: envelope vocabulary (`ag2.*`), channel state machine, passports, rules, `TransitionGraph`, handoff/finish, `Hub`, four adapters, `HubClient`/`AgentClient`/`HumanClient`, WAL (memory/file/knowledge) with `Replay`, `transport.LocalLink` |
| Knowledge: `SqliteKnowledgeStore`, `RedisKnowledgeStore`, `bootstrap.py`, `polling.py` | Deferred (D-003/D-011 — external deps are forbidden); `MemoryStore`, `DiskStore`, `LockedStore`, `EventLogWriter` (JSONL) ship |
| `streams/redis/` | Deferred (D-003/D-004); `stream.MemoryStorage` ships |
| Live/voice (`ag2/live/`, `events/voice.py`) | Not ported — realtime audio stack out of scope |
| Protocol servers/clients: `a2a/`, `acp/`, `ag_ui/`, `a2ui/`, `mcp_ui/`, `eval/` | Deferred (D-010) |
| `CronWatch` / `CadenceWatch` (`ag2/watch.py`) | Deferred (D-024); `EventWatch`/`IntervalWatch`/`DelayWatch`/`AllOf`/`AnyOf`/`Sequence` are ported |
| `MetricsMiddleware`, `TelemetryMiddleware` (OTEL) | Deferred (D-013) |
| `fast_depends.Depends` (factory-valued tool parameters; re-exported by `ag2`) | Not ported (D-036) — compute the factory value in Go before the call or wrap it as a separate tool; the DI dictionary (`Inject`/`Variable`/context parameter, D-016) covers the resolved-value cases |
| `textual.py`, `plugin.py`, `mypy.py`, `_import_utils.py`, `_telemetry_consts.py`, `types.py`, `utils.py` | Python infrastructure without a Go counterpart (textual UI, plugin loading, typing helpers, telemetry constants); `utils.build_model` → `response.SchemaOf` |

## 6. Semantics cheat-sheet (things that look different but are parity)

- **Async → goroutines + `context.Context`:** every cancellable op takes `ctx` first; stream
  pub/sub dispatches synchronously in the caller's goroutine (D-004).
- **Errors:** exception classes → package sentinels; check with `errors.Is`/`errors.As`.
- **Optional fields:** `X | None` → `*X` (notably `events.Usage` is `*int64`, D-006).
- **Turn termination:** no max-turn counter in the core loop — parity with Python; the loop
  ends when the model stops requesting tools, a `final=true` tool forces a response, or a
  fatal alert produces `"HALTED: <reason>"`. `max_turns` exists only in network transitions.
- **HITL failure path:** a `HumanInputError` escaping the turn triggers
  `events.CloseUnansweredToolCalls` history repair before propagating — same as Python.
- **Usage roll-up:** child-agent calls stay on the child stream; only the rolled-up
  `UsageEvent(kind="subtask")` reaches the parent — no double counting.

## 7. Verification status

Gates re-run for this document on 2026-10-30 (whole repo, fresh `-count=1`, from the
repo root — the root itself is not a module):

- `go -C ago build ./...` — success; `go -C ago vet ./...` — clean.
- `go -C ago test -count=1 ./...` — 34 packages `ok`, 0 failures, 0 skipped; no test files in
  `ago/internal/uuid` and the 13 example programs (`ago/hitl` now ships its own suite).
- `go -C examples build ./...` and `go -C examples vet ./...` — clean;
  `go run` of the thirteen examples (`ask`, `tools`, `hitl`, `subagent`, `group`,
  `knowledge_memory`, `mcp_http`, `mcp_stdio`, `multi_turn`, `observers`,
  `policies_compact`, `safety_halt`, `structured_output`) — every one exits 0 offline.

Symbol columns above were checked against `go doc` output of the actual packages, not from
memory of the specs.
