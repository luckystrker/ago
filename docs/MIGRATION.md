# Migration map: Python AG2 → AGo

This is the class/function-level map for moving code from AG2 (Python, commit `fa4e0fb`,
2026-10-27) to AGo (Go, module `github.com/luckystrker/ago`). The behavioral source of truth is the Python reference
in `ag2/` and its distilled specs in `docs/specs/`; the Go contracts are in
`docs/ARCHITECTURE.md`. Per-package completeness lives in [docs/PORT_STATUS.md](PORT_STATUS.md);
every deliberate deviation carries a decision record in [docs/DECISIONS.md](DECISIONS.md)
(referenced below as `D-xxx`).

Reading conventions:

- **Python column** names the 2026 AG2 API (classic `ConversableAgent`/`GroupChat` API is gone
  in AG2 itself; see §5).
- **Go column** gives the exact exported symbol, verified against the packages in this repo.
- `*agocontext.Context` is `github.com/luckystrker/ago/context` — the alias exists because it collides with stdlib
  `context`. Cancellable operations take a `context.Context` as first parameter where Python
  was `async`.

## 1. Core API

### Agent and turn lifecycle (`ag2/agent.py` → `github.com/luckystrker/ago/agent`)

| Python | Go | Differences |
|---|---|---|
| `Agent(name, prompt=..., config=..., hitl_hook=..., tools=..., middleware=..., dependencies=..., variables=..., response_schema=..., tasks=..., stream=...)` | `agent.New(name, prompt string, opts ...agent.Option)` | Dataclass kwargs → option funcs: `WithConfig`, `WithHitlHook`, `WithTools`, `WithMiddleware`, `WithResponseSchema`, `WithDependencies`, `WithVariables`, `WithStreamBase`, `WithTasks`, `WithPromptFragments`, `WithPromptHook`, `WithPlugin` (D-044). Static prompt is one string; extra fragments via `WithPromptFragments`. No `observers`/`knowledge`/`assembly` kwargs (see §5) |
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
| `Plugin` / `PluginTarget._apply_plugin` (`ag2/plugin.py`), `Agent(plugins=[...])` | `plugin.NewBundle(name)` builder (`.WithTool`, `.WithMiddleware`, `.WithPrompt`, `.WithPromptHook`) handed to `agent.WithPlugin(p plugin.Plugin)` | Applied once at agent construction — Python's turn-scoped `additional_plugins` attach/detach is not ported (D-044). Carried legs: tools append after the agent's own, middleware appends after the agent's own (innermost), fragments extend the collectors, hooks run like `WithPromptHook`. Not carried (no agent slot): plugin HITL hook, dependencies, variables, observers, assembly policies. A nil plugin contributes nothing |
| `AgentReply.response` | `reply.Response() *events.ModelResponse` | Accessor instead of attribute |
| `ConfigNotProvidedError` on missing config | `agent.ErrConfigNotProvided` | Sentinel; `errors.Is` |

### Context, stream, HITL (`ag2/context.py`, `ag2/stream.py`, `ag2/history.py`, `ag2/hitl.py`)

| Python | Go | Differences |
|---|---|---|
| `ConversationContext` (alias `Context`) | `agocontext.Context` (package `github.com/luckystrker/ago/context`) | Struct with exported fields `Stream`, `Dependencies`, `Variables`, `Prompt`, `ToolCallID`; mutation via methods (`SetDependency`, `SetVariable`, `AddPrompt`, `PromptLines`). Safe for concurrent use; `Clone()` shares the stream, copies maps |
| `context.enqueue/input/ask/send/spawn_background` | `Enqueue/Pending/Drain`, `Input(ctx, msg, timeout)`, `Ask(ctx, req)`, `Send`, `SpawnBackground` | `Drain()` returns and clears pending input (Python drains implicitly in the turn loop) |
| `dependency_provider` / DI annotations `Inject`, `Variable`, `Context` | `Dependency(id)`, `MustDependency(id)`, `SetDependency`; tool-side: `tools.NewInject`, `tools.NewVariable`, a `*agocontext.Context` parameter (§2) | DI values keyed by string id (Python keys are arbitrary objects) |
| `hitl_hook: HumanHook`, `default_hitl_hook` | `hitl.HumanHook`, `hitl.DefaultHook` (`→ hitl.ErrNotProvided`), `ctx.SetHitlHook(h)` | `HumanHook func(ctx context.Context, req *events.HumanInputRequest) (*events.HumanMessage, error)`; typed errors `NotProvidedError`/`FailedError`/`TimeoutError` match the Python `HumanInputError` family |
| `Stream` Protocol, `MemoryStream`, `SubStream` | `stream.Stream`, `stream.MemoryStream`, `stream.SubStream` | Interfaces instead of protocols; `NewMemoryStream(opts ...MemoryOption)` with `WithStorage`/`WithStreamID`/`WithPersistAll`/`WithLogger`; `stream.LoadHistory(id, storage, evs)` to replay a saved transcript |
| `stream.subscribe(..., interrupt=, condition=, timeout=)` | `Subscribe(fn stream.Subscriber, opts stream.SubscribeOptions) stream.SubID` | `Subscriber func(events.Event) error` (no context arg); `SubscribeOptions{Interrupt, Condition, Timeout}` |
| `stream.where(cond)`, `stream.get(cond)`, `stream.join()` | `Where(c events.Condition) Stream`, `Get(c events.Condition) (events.Event, error)` | `Get` scans history + incoming; `join` has no direct equivalent — use `watch.EventWatch` (§5) |
| `Storage` Protocol (`save_event/get_history/...`) | `stream.Storage` interface | `MemoryStorage` ships; the Redis backend is the optional `contrib/streamredis.NewStorage(rdb, opts...)` module (D-059). Durable logs go through `knowledge.EventLogWriter` |
| `History.get_events()/replace()`, `unanswered_tool_calls`, `close_unanswered_tool_calls` | `stream.History.GetEvents()/Replace()`, `events.UnansweredToolCalls(evs)`, `events.CloseUnansweredToolCalls(evs)` | Repair helpers live in `events`, human-readable repair sentence preserved verbatim (Python HUMAN_INPUT_ABANDONED_TOOL_RESULT) |
| `EstimatedTokens(event, chars_per_token=4)` | `events.EstimatedTokens(ev, charsPerToken)` | Same flat weights (image/audio 1000, video/document 2000) |
| async dispatch, `sync_to_thread` | Synchronous dispatch in the caller's goroutine | D-004: no thread offloading; background work via `SpawnBackground` (goroutine + panic recovery) |

### Response schemas (`ag2/response/` → `github.com/luckystrker/ago/response`)

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
| `TestConfig` / `TrackingConfig` (`ag2/testing.py`) | `agotest.NewTestConfig(responses ...*events.ModelResponse)`, `agotest.NewTrackingConfig(inner config.ModelConfig)` | Import alias `agotest "github.com/luckystrker/ago/testing"`; scripted full responses with a shared cursor and recorded call snapshots (Python's partial-turn vocabulary collapsed — D-018) |
| `exceptions.py` hierarchy (`AG2Error`, `ConfigNotProvidedError`, `HumanInputError`, `ToolConflictError`, …) | Per-package sentinels: `agent.ErrConfigNotProvided`, `hitl.ErrNotProvided` (+ typed errors), `config.ErrNotProvided/ErrUnsupportedInput/ErrUnsupportedTool`, `tools.ErrConflict/ErrNotFound/ErrResolution`, `stream.ErrHandled`, `response.ErrInvalidResponse` | Exceptions → `errors.New` sentinels + `%w` wrapping + `errors.Is/As` (no exception classes) |
| `SendableMessage` (`ag2/types.py`) | `agent.Sendable = any` | JSON-decodable value, `events.Input`, or string |
| `MediaInput` factories (`ImageInput/DocumentInput/...`) | `events.ImageInput/DocumentInput/AudioInput/VideoInput(url, path, data, mediaType)` | One function per modality covering url/file/data sources; `events.MediaTypeForPath` for extension inference |

## 2. Tools and middleware

### Tools (`ag2/tools/` → `github.com/luckystrker/ago/tools`)

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
| Builtin/hosted tools (`tools/builtin/`: `web_search`, `web_fetch`, `code_execution`, `file_search`, `image_generation`, `shell`, `memory`, `mcp_server`, `tool_search`) | `tools.NewWebSearch` / `NewWebFetch` / `NewCodeExecution` / `NewFileSearch` / `NewImageGeneration` / `NewShell` / `NewMemory` / `NewMCPServer(url, label, …)` / `NewToolSearch(mode, tools…)` (`github.com/luckystrker/ago/tools`, `hosted.go`) | Schema carriers (D-047): constructor options = Python kwargs, `Schemas` return the provider-neutral `ToolSchema` (Python wire field names), `Register` subscribes nothing (provider executes). `NewToolSearch` defers member schemas with `defer_loading` and registers the members (their calls are answered client-side). Provider mappers map what the ported Python clients map — anthropic: web_search/web_fetch/code_execution/memory/mcp_server/tool_search; gemini: googleSearch/urlContext/codeExecution/fileSearch; **openai: via the Responses client (D-060)** — web_search/file_search/code_interpreter/image_generation/shell/mcp/tool_search; chat-completions + ollama reject every hosted schema (`config.ErrUnsupportedTool`). The tool **content** packages are `github.com/luckystrker/ago/toolpacks` (row below) |

### Middleware (`ag2/middleware/` → `github.com/luckystrker/ago/middleware`)

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
| `MetricsMiddleware` | — | Deferred (D-013) — no consumer; compose your own from the four hooks |
| `TelemetryMiddleware` (OTEL, `middleware/builtin/telemetry.py`) | `contrib/otelx` (own module): `otelx.Middleware(tp trace.TracerProvider) middleware.Factory`, `otelx.InstallDefault(ctx, opts...) (middleware.Factory, shutdown, error)` with `WithStdout`/`WithStdoutWriter`/`WithCollector` | **full** (D-064/D-069) | Spans `ago.turn`/`ago.llm_call`/`ago.tool_execution` with model/provider/usage/finish-reason and tool name/call-id attributes, error status on pipeline errors and `ToolErrorEvent` answers. Deviations: span names `ago.*` (not `invoke_agent`/`chat`/`execute_tool`), OTLP/HTTP only in `InstallDefault`. Deferred: content capture, constructor knobs, human-input span, record_usage spans, traceparent relaying |

## 3. Provider configs

All Go configs live in `github.com/luckystrker/ago/config/<provider>` and implement `config.ModelConfig`
(`Provider()`, `Model()`, `Create() (LLMClient, error)`, `Clone(overrides)`). Clients implement
`config.LLMClient.Call(ctx, messages, conv, opts)`. Missing model/key →
`config.ErrNotProvided` (parity with `ConfigNotProvidedError`). No SDKs: every provider is raw
HTTP/SSE over `github.com/luckystrker/ago/internal/httpx` + `github.com/luckystrker/ago/internal/sse` (D-003, D-017).

| Python config | Go config (constructor) | Status | Notes / what is narrower |
|---|---|---|---|
| `ModelProvider` enum (11 values) | `config.Provider` (same 11 values, incl. `vertexai`) | full | Vocabulary preserved |
| `OpenAICompletionsConfig` (and deprecated alias `OpenAIConfig`) | `config/openai.OpenAICompletionsConfig` — `NewOpenAICompletions(model)` | **full** | Chat Completions: SSE streaming with tool-call delta merge, tools, strict structured output (`openai.StrictifySchema`, `openai.ResponseFormat`), usage incl. cached/reasoning tokens; env `OPENAI_API_KEY`. Sampling/verbosity fields subset |
| `OpenAIResponsesConfig` (+ `containers.py`) | `config/openai.OpenAIResponsesConfig` — `NewOpenAIResponses(model)`; client `ResponsesClient` | **full** (D-060; closes the D-008 deferral — D-068) | Responses API over raw HTTP/SSE: input mapping incl. `FileIdInput` → `input_file.file_id`, hosted tools on the wire (web_search/file_search/code_interpreter/image_generation/shell container auto|reference/mcp/tool_search with `defer_loading`), hosted output items → builtin call/result events with second-call item replay, strict `text.format`, usage incl. cached/reasoning tokens. Fields: APIKey/BaseURL/Temperature/TopP/MaxOutputTokens/MaxToolCalls/Store(nil=default true)/ParallelToolCalls(nil=default true)/TopLogprobs/Metadata/PromptCacheKey/PromptCacheOptions/Reasoning{Effort,Summary}/ServiceTier/User/Truncation/Timeout/MaxRetries/Organization/Project/DefaultHeaders/ExtraBody; `Clone` takes Go names + snake_case aliases, nil clears. Deferred: containers, prompt-cache diagnostics events, reasoning-item replay, shell delta chunks, `websocket_base_url` |
| `OpenAIDecisionsConfig` | `config/openai.OpenAIDecisionsConfig` — `NewOpenAIDecisions()` (model `DefaultDecisionsModel` "gpt-6-luna", `BooleanThreshold` 0.5); client `DecisionsClient` | **full** (D-060/D-068) | Decision-only schemas (bool, string enum, int enum from 0, 0..1 number — otherwise the schema-guidance error), one question per request, thresholded answers rendered through the schema, refusal as `*DecisionRefusedError{Name}`, usage normalization; `Descriptions`/`QuestionInstructions`/`SafetyIdentifier`/`ExtraBody` carried. Deferred: question-instruction parts as Input sequences |
| `ag2/files/` (`FilesAPI`, `FilesClient` protocol), `config/openai/files.py`, `config/anthropic/files.py` | `github.com/luckystrker/ago/files`: `files.NewAPI(client files.Client) *files.API`, types `UploadedFile`/`FileContent`/`FileProvider`; provider clients via `(*openai.OpenAICompletionsConfig).CreateFilesClient()` and `(*anthropic.AnthropicConfig).CreateFilesClient()` | **full** (D-046) | `API` delegates `Upload/Read/List/Delete` and keeps Python's two upload validations (`files.ErrNoData`, `files.ErrFilenameRequired`); OpenAI: multipart upload (empty purpose → `assistants`), `has_more`/`after` pagination; Anthropic: `/v1/files` under `files-api-2025-04-14`, no purpose on upload, `next_page`/`page` pagination, 600s default timeout; non-2xx → per-provider `ErrFilesAPI`. Python's `created_at`/`media_type` survive in `Raw`. `containers.py` deferred; a file reference as a *message input* still fails fast (`events.FileIdInput` → `config.ErrUnsupportedInput`, D-017/D-020) |
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
| `ag2/mcp/sampling.py` pseudo-client (implements `ModelConfig` + `LLMClient`) | — | not ported | The MCP server product itself is ported (`mcpserver`, §5); the sampling pseudo-client — letting a *remote MCP client* borrow the local model — is not |

Shared engine: `config.RunChatCompat` (the OpenAI Chat-Completions wire protocol used by
openai/zai/mistral/dashscope), `config.BuildMessages`, `config.ToolToAPI`,
`config.EnsureObjectSchema`, `config.NormalizeUsage`. Streaming is per-config `Streaming bool`
(default off), matching Python.

## 4. Events

`ag2/events/*` → `github.com/luckystrker/ago/events` (one Go file per Python module). Base contract: embed
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
| `BuiltinToolCallEvent` / `BuiltinToolResultEvent` | `events.BuiltinToolCallEvent` / `events.BuiltinToolResultEvent` | Wire vocabulary for provider-executed hosted tools (D-047 carriers subscribe nothing — the provider settles those calls) |
| `CompactionSummary` | `events.CompactionSummary` | `[Summary of earlier conversation]` replay text preserved |
| `TaskStarted/Progress/Completed/Failed/Expired/Cancelled`, `TaskSpec` | `events.TaskStarted` … `events.TaskCancelled`, `events.TaskSpec` | `TaskProgress` transient |
| `Severity`, `ObserverAlert`, `HaltEvent` | `events.Severity` (`info…fatal`) / `events.ObserverAlert` / `events.HaltEvent` | Halt → synthetic `"HALTED: <reason>"` response in the agent |
| Lifecycle: `ObserverStarted/Completed`, `CompactionStarted/Completed/Failed`, `AggregationStarted/Completed/Failed`, `EventLogFailed`, `UnknownEvent` | same names in `github.com/luckystrker/ago/events` | All transient |
| `ProviderReplay` mixin + provider events (`OpenAIReasoningEvent`, `AnthropicRedactedThinkingEvent`) | — | Not separate events: provider-native replay state is handled inside each provider client's mapping |
| Condition DSL (`Event.field == x`, `A \| B`, `~c`) | `events.Condition` + `AllOf/AnyOf/Not/TypeOf[T]/Equal/TypeOfAny` | Predicate functions instead of operator overloading (D-005) |
| `events/voice.py` | — | Deferred (live/voice stack not ported) |

## 5. Not ported — and what replaces it

Everything below is a deliberate decision (recorded in `docs/DECISIONS.md`), not an oversight.

| Python AG2 | AGo answer |
|---|---|
| Classic API: `ConversableAgent`, `initiate_chat`, `generate_reply` | Already removed in the 2026 AG2 rewrite itself; AGo ports the replacement: `agent.Agent`, `Ask`/`Run` (D-002) |
| `GroupChat` / `GroupChatManager` | `github.com/luckystrker/ago/network`: `Hub` + channel adapters — `WorkflowAdapter` (declarative `TransitionGraph`), `DiscussionAdapter` (round-robin), `ConversationAdapter`, `ConsultingAdapter`; `Agent.AsTool` for delegation |
| `register_reply` (reply-function chain) | `middleware.Factory` list on the agent — the four-hook onion (`OnTurn`/`OnLLMCall`/`OnToolExecution`/`OnHumanInput`) |
| `human_input_mode` | `agent.WithHitlHook(h hitl.HumanHook)` |
| `update_system_message`, `register_hook` | `WithPromptHook`/`WithPromptFragments`; cross-cutting hooks → middleware or `observers.Observer` |
| Response cache (classic AG2 `cache_seed`) | Intentionally absent — the 2026 AG2 has no framework-level response cache either (spec `config-llm.md` §4); only provider-side prompt caching knobs (Anthropic `PromptCaching`, Gemini `cached_content`) and cache-token accounting |
| `config_list` + `register_model_client` | Gone in 2026 AG2; AGo has one typed `config.ModelConfig` per provider |
| `ExitStack`-based registration lifetime | Turn-scope ownership: tool `Register` returns subscription ids, agent unsubscribes on `Close` (D-032) |
| `Plugin` protocol, `plugins=[...]` | Ported at construction (D-044): `plugin.Bundle` → `plugin.Plugin` via `agent.WithPlugin` (map in §1). Not ported: turn-scoped `additional_plugins` attach/detach and the plugin legs without an agent slot (HITL hook, dependencies, variables, observers, assembly policies) |
| `Agent(..., observers=[...])` wiring | No agent option: observers subscribe directly on the conversation stream — `observers.Observer.Register(ctx)` (D-025) |
| `Agent(..., assembly=[...])` (`AssemblerMiddleware`) | No built-in assembler: wrap `policies.AssemblyPolicy` transforms in a `middleware.Factory` yourself (pattern in `examples/policies_compact`) |
| `Agent(..., knowledge=KnowledgeConfig)` | No agent option: pass `knowledge.Store` to `policies.NewWorkingMemoryPolicy`/`NewEpisodicMemoryPolicy` and wire via middleware (pattern in `examples/knowledge_memory`) |
| `FilesAPI` / `FilesClient` (`ag2/files/`) | Ported (D-046): `github.com/luckystrker/ago/files` (`files.NewAPI` over any `files.Client`), provider clients via `CreateFilesClient()` on `config/openai` / `config/anthropic` (map in §3). File references as message inputs still fail fast with `config.ErrUnsupportedInput` (D-017/D-020) |
| OpenAI Responses API, Decisions API | Ported (D-060/D-068, closing the D-008 deferral): `config/openai` — `NewOpenAIResponses(model)`/`ResponsesClient` (map in §3) and `NewOpenAIDecisions()`/`DecisionsClient`. Chat Completions remains the other supported OpenAI surface |
| Hosted/builtin tools: `web_search`, `web_fetch`, `code_execution`, `file_search`, `image_generation`, `shell`, `memory`, `mcp_server`, `tool_search` | Carriers ported (D-047, map in §2): `github.com/luckystrker/ago/tools` `hosted.go`, mapped by anthropic, gemini and the Responses client (D-060); chat-completions + ollama reject every hosted schema (`config.ErrUnsupportedTool`). Still deferred: Gemini grounding/server-tool replay events |
| Ready-made tool content: `tools/{shell,code,sandbox,search,skills,dynamic}/`, `toolkits/filesystem.py` | Ported as a functional subset (D-062/D-063): `github.com/luckystrker/ago/toolpacks` — `NewShellTool(opts ...ShellOption)`, `NewLocalCodeRunner(opts ...CodeRunnerOption)` (`run_code`, local backend), `NewFilesystemToolkit(root).Tools()`, `NewDuckDuckGoSearch`/`NewTavilySearch(apiKey, ...)`/`NewPerplexitySearch(apiKey, ...)`, `NewSkillsToolkit(dir).Tools()`, `NewDynamicAgentFactory(cfg, pool, opts...)`. Deferred: Docker/Daytona sandboxes, the TypeScript runner, `update_file`/`delete_file`, per-call `Variable` kwargs of the search tools, the skills script runner |
| `ag2/mcp` server product (`MCPServer`, `mcp_tool`, sampling, elicitation) | Ported as a stateless subset (D-058, superseding the D-010 deferral): `github.com/luckystrker/ago/mcpserver` — `NewServer(ServerConfig{Ask, Tools, ...})` / `NewServerForAgent(a, opts...)`, transports `(*Server).Stdio()` and `(*Server).Handler()` (streamable HTTP), methods `initialize`/`tools/list`/`tools/call`/`ping`; one conversational `ask` tool plus optional deterministic tools. Not ported: sessions/conversations + `Mcp-Session-Id`, pause/`InputRequiredResult`, OAuth security, MCP Apps UI, stream_progress, resources/prompts, batch requests. The consume-side client is `github.com/luckystrker/ago/mcp` (D-029; `input_required` answering, discovery cache, protocol-era negotiation not ported) |
| Network wave 2: view policies (`views/`: FullTranscript, WindowedSummary, NamedTranscript, NamedWindowedSummary), hub arbiter, expectation sweepers + audit, task mirror, client LLM tools (`peers`/`channels`/`tasks`/`context`/`delegate`) | Ported (D-056, in `github.com/luckystrker/ago/network`): the four `ViewPolicy` impls over `ViewOptions{Channel, RenderEnvelope, NameFor}` (`Hub.NameFor`, `DefaultRenderEnvelope`), `HubArbiter` seam + `BaseArbiter` + `RuleBasedArbiter` (hub consults send/dispatch/channel-open/unknown-audience; `Register` and `AuthorizeInbox` are not consulted — Python parity), expectation evaluators (`acks_within`/`reply_within`/`max_silence`) + handlers (audit/notify/auto-close) + `AuditSink` (`NewFileAuditLog`/`NewSliceAuditLog`) + `NewExpectationSweeper` (owners opt in), `NewTaskMirror(hub, ownerID, channelID)`, `ClientTools(hc, self) ([]tools.Tool, error)`. Federation stays a stub (`network.RemoteAgentProxy`, no implementations — remote routing via `ResolveUnknownAudience`) |
| WebSocket link (`network/transport/ws.py`) | Ported as the optional module `github.com/luckystrker/ago/contrib/wslink` (D-057/D-069): `wslink.Dial(ctx, url, opts)` / `wslink.ServeWS(w, r, handle)` wrapping `transport.Link` over `coder/websocket` — own go.mod, one external dep |
| Knowledge: `SqliteKnowledgeStore`, `RedisKnowledgeStore` | Ported as optional modules (D-048/D-059, closing the D-011 deferral): `contrib/knowledgesqlite.NewStore(dbPath)` (pure-Go SQLite) and `contrib/knowledgeredis.NewStore(rdb, opts...)` — both `knowledge.Store`; Redis streams: `contrib/streamredis.NewStorage(rdb, opts...)` (`stream.Storage`). `bootstrap.py`/`polling.py`/`config.py` stay deferred (D-011 — cross-process change detection is a poll-the-store fallback; D-059) |
| Network core: envelope/channel/identity/rule/transitions/handoff, `Hub`, the four adapters, clients, transport | Ported single-host subset (D-010/D-031): envelope vocabulary (`ag2.*`), channel state machine, passports, rules, `TransitionGraph`, handoff/finish, `Hub`, Consulting/Conversation/Discussion/Workflow adapters, `HubClient`/`AgentClient`/`HumanClient`, WAL (memory/file/knowledge) with `Replay`, `transport.LocalLink`. Federation (`remote.py`) stays an interface stub (see the wave-2 row above) |
| Live/voice (`ag2/live/`, `events/voice.py`) | The HTTP cascade is ported (D-057/D-072): `github.com/luckystrker/ago/live` — `live.Cascade{STT Transcriber; LLM func(ctx, text) (string, error); TTS Synthesizer}` + `Handle(ctx, audio, mime, voice)`, ready stages `live.OpenAITranscriber`/`live.OpenAISynthesizer`, sentinels `ErrAPI`/`ErrIncomplete`. Deferred: the realtime-websocket stack (LiveAgent/`RealtimeConfig`, turn detection, barge-in, streaming TTS, `STTTranslationConfig`), the elevenlabs/gemini/mistral providers, `events/voice.py`, sound devices |
| `a2a/` — `A2AServer` (`server.py:38`), `build_card` (`card.py:31`), `AgentExecutor` (`executor.py`), the JSON-RPC transport (`transports/`), `A2AClient(LLMClient)` (`client.py:159`), task records (`tasks.py`) | Ported through wave 2 (D-049/D-050 + D-067, superseding the D-010 deferral; closure ledger D-070): `github.com/luckystrker/ago/a2a` — `NewServer(ServerConfig{Card, Handle, Store})` + `Handler()` (card discovery at `/.well-known/agent-card.json` + legacy `agent.json`; JSON-RPC 2.0 `message/send`, two-event SSE `message/stream`, `tasks/get` with `historyLength` trimming, `tasks/cancel`; `input-required` task state), push notifications (`ServerConfig.PushStore` + the `tasks/pushNotificationConfig/set|get|list|delete` methods, `DefaultPushSender`/`PushReceiver`, `Client.SetPushConfig`/…), the `urn:ag2:client-tools:v1` extension (`ServerConfig.HandleClientTools`/`HandleToolResults` server legs; `Client.RunClientTools`/`RunWithHumanInput` drivers, `AttachClientTools`/`ValidateExtensionActivation`), context-variable sync + HITL over `Message.metadata` (`ExtractContextUpdate`/`WithContextUpdate`/`StripReservedVariables`, `ServerConfig.SyncVariables`), the REST twin (`POST /message:send`, `POST /tasks`, `GET /tasks/{id}`, `POST /tasks/{id}:cancel`), declarative security schemes (security.py). `TaskStore` + `NewMemoryTaskStore`; protocol types with camelCase tags and the `kind` part discriminator; client `NewClient(baseURL, opts...)` — `AgentCard`/`SendMessage`/`SendMessageTask`/`TasksGet`/`TasksCancel`. Not ported: full history shipping (`ag2.history+json`), reconnect/resubscribe, card JWS signing/verification (no stdlib JWS — D-067), `validate_input_modes`, cluster mode, tenants, **gRPC — closed permanently (D-071)** |
| `acp/` — `ACPAgent` (`agent.py:118`) + executor, `ACPBridge` (`bridge.py:259`) + `make_bridge`, `ACPClient` (`client.py:77`), `permissions.py`, `elicitation.py`, `auth.py`, `tool_gateway.py` | Ported through wave 2 (D-052 + D-066; closure ledger D-070): `github.com/luckystrker/ago/acp` — both roles over newline-delimited JSON-RPC 2.0 stdio. Serve: `Serve(ServeConfig)` / `ServeAgent(a, cfg)` (`initialize` with truthful capabilities + `authMethods` + `loadSession: true`, `authenticate` + `Auth`/`acp.NewStaticTokenAuth(token)` gating session methods, `session/new` (uuid4 id, cwd/mcpServers recorded), `session/load` history replay (`ServeConfig.History`; `ServeAgent` replays its per-session conversations), `session/prompt` running `Handle(ctx, sessionID, userText, emit *Emitter)` — the full update vocabulary (`Message` chunks, `Thought` under `StreamThoughts`, `Plan`, `ToolCall`/`ToolCallUpdate`, `Usage` under `ContextWindow`), `session/cancel` stops the turn context and answers `{"stopReason":"cancelled"}`, never an error; `Session.RequestPermission`/`Session.Elicit` gated by the config policies (deny = synthesized locally); `Session.Gateway()` proxies client-declared MCP servers through `ago/mcp`. Client: `NewBridge(cmd, args, env)` + `SessionNew`/`SessionNewWithServers`/`Prompt`/`PromptTurn` (stop reason)/`Cancel`/`Load`/`Close`, `Bridge.FS` mediating `fs/read_text_file`/`fs/write_text_file` and `terminal/*` under the root guard (nil = disabled, `-32601`). Not ported: the multi-session store, cross-process `session/load` storage, the tool_gateway HOST direction (exposing local tools over HTTP), serving the agent's own tools through the gateway, `guard.py`, OAuth/terminal auth kinds, model pickers, remote transports |
| `ag_ui/` — `AGUIStream` (`stream.py:118`), `build_asgi` (`asgi.py:22`), `run_input.py` (`read_run_input`), `capabilities.py` (`served_capabilities`), `events.py` (`AGUIEvent`), `interrupts.py` | Ported through wave 2 (D-053 + D-065; closure ledger D-070): `github.com/luckystrker/ago/agui` — AG-UI 1.0 serving over `net/http`: `ServeAgent(a) http.Handler` and `Handler(HandlerConfig)` (GET capabilities, POST → SSE, 400 before any stream, protocol-major refusal, cancelled outcome), the event projection `NewStream(emit)` + `Handle`, `ReadRunInput` (dual-case keys, unknown-role strip, base64 refusal), `CapabilitiesFor`, typed events + `UnmarshalEvent` + `Raw`. Held turns (D-065): `agui.NewServer(opts...)` — `WithRetention` (default 900s/128), `WithResumeProof`, `WithStateSnapshots` — with `ServeAgent(name, prompt, opts...)`/`Handler(a)`: a `HumanInputRequest` pauses the run as RUN_FINISHED with an `interrupt` outcome (`InterruptOutcome{Interrupts}`); the next POST's `ResumeEntry{InterruptID, Status, Payload}` answers it (resolved → rebind to the new exchange, cancelled → abandon), refusals are single coded RUN_ERRORs before RUN_STARTED. Not ported: REASONING_ENCRYPTED_VALUE, the subagent suspension ledger, seeding the turn's variables from the client's state, prompt-level inputs as per-run fragments |
| `eval/` — `runtime/runner.py` (`run_agent`), `runtime/evaluate.py` (`evaluate_traces`), `trace.py` (`Trace`), `scorer.py` (`Scorer`, the `@scorer` decorator), `scorers/` (correctness, cost, threshold, tools, judge, pairwise_judge, attribution), `pairwise.py` (`PairwiseComparator`, `WinRate`), `results/` (`RunResult`, `store.dump`) | Ported produce-then-grade (D-051 + D-061; closure ledger D-070): `github.com/luckystrker/ago/eval` — `Run(ctx, agent, Suite, opts ...RunOption)` over `Case{Name, Input, Expected, Scorers}` (`WithConcurrency(n)`, default 4; 1 = strict order), `BuildTrace(evs)` reconstructing `Trace` **from the stream's event log — no OTEL** (the core deviation; `Trace.Err` = Python `Trace.exception`), scorers `Correctness`, `TokenBudget`, `ToolsCalled`, `Threshold`, `LLMJudge`, `Pairwise`, `FailureAttribution` + `DetectMechanical` (attribution.py), the scorer registry (`NewScorerRegistry`, name + params factories for `correctness`/`token_budget`/`tools_called`/`threshold`/`llm_judge`/`failure`, `SetClient`, `Register`), `SuiteFromJSONL(path, registry)` (input required, auto-names, scorer specs), `RunResult.SaveJSON`/`LoadJSON` + `Compare`, `Diff`/`DiffStrict`/`Summary`, `WilsonCI`/`(*RunResult).PassRateCI`/`CohenKappa`. Deferred: OTEL/Tempo/OTLP trace sources and `evaluate_traces`, repeats/per-task configs/`BudgetThresholds`/lifecycle events, suite name/source metadata + tags, pairwise single-order mode |
| `a2ui/` | Deferred (D-010) — protocol server out of scope |
| `mcp_ui/` (`resources.py`, `actions.py`) | Ported (D-045, supersedes the D-010 deferral for this slice): `github.com/luckystrker/ago/mcpui` — UI resources `RawHTML`/`ExternalURL`/`RemoteDOM` (text/blob encoding, `ui://` guard, `_meta` merge), UI actions `ToolCall`/`Prompt`/`Link`/`Intent`/`Notify`, `PostMessage` (json.dumps + html.escape byte parity), `mcpui.Marshal` for exact wire bytes, `WithUIResource` attaching blocks to `events.ToolResult.Parts` |
| `CronWatch` / `CadenceWatch` (`ag2/watch.py`) | Deferred (D-024); `EventWatch`/`IntervalWatch`/`DelayWatch`/`AllOf`/`AnyOf`/`Sequence` are ported |
| `MetricsMiddleware` | — | Deferred (D-013). `TelemetryMiddleware` is ported as `contrib/otelx` (row in §2, D-064) |
| `fast_depends.Depends` (factory-valued tool parameters; re-exported by `ag2`) | Not ported (D-036) — compute the factory value in Go before the call or wrap it as a separate tool; the DI dictionary (`Inject`/`Variable`/context parameter, D-016) covers the resolved-value cases |
| `textual.py` | Ported as a stdlib line REPL (D-056): `github.com/luckystrker/ago/tui` — `tui.REPL(a *agent.Agent, in io.Reader, out io.Writer, opts ...tui.Option) error`, `/help`//`exit`, `WithStreaming()` echoes `ModelMessageChunk` deltas; the textual App surface (widgets, markdown, the reasoning block) is deferred |
| `mypy.py`, `_import_utils.py`, `_telemetry_consts.py`, `types.py`, `utils.py` | Python infrastructure without a Go counterpart (typing helpers, telemetry constants, the extras-import block); `utils.build_model` → `response.SchemaOf`. `plugin.py` has a counterpart since D-044 (`github.com/luckystrker/ago/plugin`) |

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

Gates re-run for this document's final-wave sweep on 2026-10-30 (whole module, fresh
`-count=1`, from the repo root — `go.mod` sits there and `examples/` is part of the same
module; `contrib/` modules are excluded by construction — nested modules, they run their
own gates):

- `go build ./...` — success; `go vet ./...` — clean. (`gofmt -l` is not meaningful on
  this CRLF working-tree checkout — autocrlf flags every file; CI checks gofmt on the
  LF checkout.)
- `go test -count=1 ./...` — 46 packages `ok`, 0 failures, 0 skipped; no test files
  in `github.com/luckystrker/ago/internal/uuid` and the thirteen example programs.
- `go build ./examples/...` and `go vet ./examples/...` — clean;
  `go run ./examples/<name>` of the thirteen examples (`ask`, `tools`, `hitl`, `subagent`,
  `group`, `knowledge_memory`, `mcp_http`, `mcp_stdio`, `multi_turn`, `observers`,
  `policies_compact`, `safety_halt`, `structured_output`) — every one exits 0 offline.
- Contrib modules, from their directories: `go build ./...`, `go vet ./...`,
  `go test -count=1 ./...` — green in all five (`knowledgesqlite`, `knowledgeredis`,
  `streamredis`, `wslink`, `otelx`).

Symbol columns above were checked against the actual packages' source (constructors,
struct fields, method signatures), not from memory of the specs; the §5 rows for
`github.com/luckystrker/ago/a2a`, `github.com/luckystrker/ago/acp`, `github.com/luckystrker/ago/agui`
and `github.com/luckystrker/ago/eval` were re-checked the same way during this sweep, and
every code snippet in [docs/GUIDE.md](GUIDE.md) added in the final wave was compile-checked
against the tree (a scratch module importing the root module and the contrib modules the
snippets name — knowledgesqlite, knowledgeredis, streamredis, otelx — via `replace`).
