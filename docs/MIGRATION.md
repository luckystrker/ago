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

## Breaking changes — v1.3.0 sync

The scoped-variables port (upstream `fad570ca19`, D-090) changes the context
variable API. Migrate external code as follows.

**`Context.Variables` is a store, not a map.** A Go map cannot intercept
`m[k] = v`, so a map field cannot declare a scope or rebind a SHARED value.
Before/after:

```go
// before
cx.Variables["tone"] = "terse"
tone := cx.Variables["tone"]
delete(cx.Variables, "tone")

// after
err := cx.SetVariable("tone", "terse")       // returns *agocontext.ScopeError on conflict
tone, ok := cx.Variable("tone")
cx.VariableStore().Delete("tone")
```

Declare scopes with `agocontext.ScopedValue`:

```go
agent.New("root", "", agent.WithVariables(map[string]any{
	"findings": agocontext.ScopedValue{Value: []string{}, Scope: agocontext.ScopeShared},
	"secret":   agocontext.ScopedValue{Value: "root only", Scope: agocontext.ScopeLocal},
	"depth":    0, // COPIED (default): each delegated child gets a deep copy
}))
```

**`NewWithDeps` returns `(*Context, error)`.** A malformed declaration is now
an error instead of being silently ignored. `NewWithVariables(s, deps, store)`
takes an already-built `*agocontext.Variables` (the delegation path).

```go
// before
cx := agocontext.NewWithDeps(s, deps, vars)

// after
cx, err := agocontext.NewWithDeps(s, deps, vars)
if err != nil { return err }
// or, with a prebuilt store:
cx = agocontext.NewWithVariables(s, deps, store)
```

**`Context.SetVariable` returns `error`.** Existing call statements still
compile; code that wants to surface a scope conflict must now check it.
`Context.Variable`/`VariablesSnapshot` stay the read path.

**`AgentReply.Context()` is new** — additive, Python `reply.context` parity.

**`a2a.ReservedVariablePrefixes` is deprecated.** The control-plane prefixes
have a single source in `context`: use `agocontext.ReservedVariablePrefixes()`
(returns a copy) or `agocontext.IsReservedVariable(key)`. The old
`a2a.ReservedVariablePrefixes` variable remains as a snapshot alias for source
compatibility (mutating it does not affect the context contract);
`a2a.StripReservedVariables` and agui's state export already delegate to the
context helper.

`Context.Clone()` keeps the existing cross-turn isolation for branch values and
keeps one SHARED binding table for the continuation; a delegated child instead
gets a derived branch (COPIED deep-copied, LOCAL/reserved dropped, SHARED
reused). Full contract: `docs/specs/core.md` §2 and D-090.

## 1. Core API

### Agent and turn lifecycle (`ag2/agent.py` → `github.com/luckystrker/ago/agent`)

| Python | Go | Differences |
|---|---|---|
| `Agent(name, prompt=..., config=..., hitl_hook=..., tools=..., middleware=..., dependencies=..., variables=..., response_schema=..., tasks=..., stream=...)` | `agent.New(name, prompt string, opts ...agent.Option)` | Dataclass kwargs → option funcs: `WithConfig`, `WithHitlHook`, `WithTools`, `WithMiddleware`, `WithResponseSchema`, `WithDependencies`, `WithVariables`, `WithStreamBase`, `WithTasks`, `WithPromptFragments`, `WithPromptHook`, `WithPlugin` (D-044). Static prompt is one string; extra fragments via `WithPromptFragments`. No `observers`/`knowledge`/`assembly` kwargs (see §5) |
| `await agent.ask(*msg, config=, tools=, middleware=, observers=, plugins=, hitl_hook=, dependencies=, variables=, prompt=, response_schema=, stream=)` | `a.Ask(ctx, msgs ...agent.Sendable)`; per-call overrides via `a.AskWith(ctx, agent.RunOptions, msgs...)`, `a.RunWith`, `a.ResumeWith` | `Sendable = any` (string, `events.Input`, JSON value). `RunOptions` carries Stream/Config/Tools/Middleware/Observers/Plugins/HitlHook/Dependencies/Variables/Prompt/ResponseSchema/FreeText; zero value keeps the agent's settings (D-093a, D-097). `FreeText` is the explicit `response_schema=None` vs "not passed" distinction. `Ask`/`Run`/`Resume` remain the zero-option wrappers |
| `async with agent.run(...) as run: reply = await run.result()` | `run, _ := a.Run(ctx, msgs...); run.Start(ctx); defer run.Close(); reply, err := run.Result(ctx)` | Python async context manager → explicit `Start`/`Result`/`Close`; `Close` ends the turn scope (unsubscribe, release per-stream lock) |
| `run.enqueue(*msg)`, `run.stream` | `run.Enqueue(parts ...events.Input)`, `run.Stream() stream.Stream` | Mid-turn inbox parity |
| `AgentReply.body` | `reply.Body() string` | `""` when the model returned no text (Python `None`) |
| `await reply.content(retries=N)` (schema-validated) | `agent.ContentAs[T](ctx, reply, retries) (T, error)` | Package-level generic instead of a method (Go generics cannot be methods); re-asks on validation failure with the error appended, same semantics |
| `reply.files`, `reply.history`, `await reply.usage()` | `reply.Files()`, `reply.History() *stream.History`, `reply.Usage() *usage.Report` | `Usage()` is a synchronous aggregation of the stream's `UsageEvent`s — no await |
| `reply.ask(...)` / `reply.run(...)` | `reply.Ask(ctx, ...)` / `reply.Run(ctx, ...)` | Continuation on the same stream/context |
| `resume(*events, ...)` | `a.Resume(ctx, evs ...events.Event) (*AgentReply, error)` | All but last event seed history; last is the re-entry trigger |
| `as_tool(description=..., name=..., stream=, middleware=)` | `a.AsTool(opts ...agent.AsToolOption) tools.Tool` | Options `WithToolName` (default `task_<name>`, Python `subagent_tool.py` parity — D-040), `WithToolDescription`, `WithToolStream`, `WithToolStreamFactory` and `WithToolMiddleware`; `agent.PersistentStream()` is the shared-stream factory (D-097); the tool takes `objective` + optional `context` and each call runs the delegation machinery (`runTaskOn`): `TaskStarted`/`TaskCompleted`/`TaskFailed`/`TaskCancelled` and the child's source `UsageEvent` records (with `path`/`task_id`/`agent_name`) on the caller's stream — **no subtask rollup**; child variables are a scoped branch (COPIED deep-copied, LOCAL/reserved `ag:`/`a2a:` dropped, SHARED reused — D-090), dependencies are copied, HITL bridges to the parent's hook (request and answer reach the parent only as `ChildEvent` observations — D-041/D-092). A failed sub-task is the text `Sub-task '<name>' failed: …`; a HITL failure ends the calling turn (subtask tools `run_subtask`/`run_subtasks` live in `agent` too). `TaskCompleted.Usage` is a cumulative child-stream snapshot — never sum it with source usage records |
| `agent.task(title, ...)` | `a.Task(ctx, title string, opts ...task.Option) (*task.Task, error)` | Checkpoint store / TTL / resume via `task.WithCheckpointStore`, `task.WithTTL`, `task.WithResumeFrom`; `agent.TaskWithCheckpointStore`, `agent.TaskWithTTL` forwards; the task always opens its own private stream (D-030) |
| `PromptHook` (async prompt hook), `update_system_message` | `agent.WithPromptHook(fn agent.PromptHook)`, `agent.WithPromptFragments(...)` | `PromptHook func(*agocontext.Context) ([]string, error)` |
| `Plugin` / `PluginTarget._apply_plugin` (`ag2/plugin.py`), `Agent(plugins=[...])` | `plugin.NewBundle(name)` builder (`.WithTool`, `.WithMiddleware`, `.WithPrompt`, `.WithPromptHook`) handed to `agent.WithPlugin(p plugin.Plugin)` | Applied once at agent construction — Python's turn-scoped `additional_plugins` attach/detach is not ported (D-044). Carried legs: tools append after the agent's own, middleware appends after the agent's own (innermost), fragments extend the collectors, hooks run like `WithPromptHook`. Not carried (no agent slot): plugin HITL hook, dependencies, variables, observers, assembly policies. A nil plugin contributes nothing |
| `AgentReply.response` | `reply.Response() *events.ModelResponse` | Accessor instead of attribute |
| `ConfigNotProvidedError` on missing config | `agent.ErrConfigNotProvided` | Sentinel; `errors.Is` |

### Context, stream, HITL (`ag2/context.py`, `ag2/stream.py`, `ag2/history.py`, `ag2/hitl.py`)

| Python | Go | Differences |
|---|---|---|
| `ConversationContext` (alias `Context`) | `agocontext.Context` (package `github.com/luckystrker/ago/context`) | Struct with exported fields `Stream`, `Dependencies`, `Variables *agocontext.Variables`, `Prompt`, `ToolCallID`; mutation via methods (`SetDependency`, `SetVariable`, `AddPrompt`, `PromptLines`). Safe for concurrent use; `Clone()` shares the stream, branches the variable store (SHARED bindings persist across the continuation) and copies dependencies/prompt. Variables support delegation scopes (`ScopedValue`, D-090) |
| `context.enqueue/input/ask/send/spawn_background` | `Enqueue/Pending/Drain`, `Input(ctx, msg, timeout)`, `Ask(ctx, req)`, `Send`, `SpawnBackground` | `Drain()` returns and clears pending input (Python drains implicitly in the turn loop) |
| `dependency_provider` / DI annotations `Inject`, `Variable`, `Context` | `Dependency(id)`, `MustDependency(id)`, `SetDependency`; tool-side: `tools.NewInject`, `tools.NewVariable`, a `*agocontext.Context` parameter (§2) | DI values keyed by string id (Python keys are arbitrary objects) |
| `hitl_hook: HumanHook`, `default_hitl_hook` | `hitl.HumanHook`, `hitl.DefaultHook` (`→ hitl.ErrNotProvided`), `ctx.SetHitlHook(h)` | `HumanHook func(ctx context.Context, req *events.HumanInputRequest) (*events.HumanMessage, error)`; typed errors `NotProvidedError`/`FailedError`/`TimeoutError` match the Python `HumanInputError` family |
| `Stream` Protocol, `MemoryStream`, `SubStream` | `stream.Stream`, `stream.MemoryStream`, `stream.SubStream` | Interfaces instead of protocols; `NewMemoryStream(opts ...MemoryOption)` with `WithStorage`/`WithStreamID`/`WithPersistAll`/`WithLogger`; `stream.LoadHistory(id, storage, evs)` to replay a saved transcript |
| `stream.subscribe(..., interrupt=, condition=, timeout=)` | `Subscribe(fn stream.Subscriber, opts stream.SubscribeOptions) stream.SubID` | `Subscriber func(events.Event) error` (no context arg); `SubscribeOptions{Interrupt, Condition, Timeout}` |
| `stream.where(cond)`, `stream.get(cond)`, `stream.join()` | `Where(c events.Condition) Stream`, `Get(c events.Condition) (events.Event, error)`, `stream.Join(ctx, s, JoinOptions{Condition, MaxEvents})` (also `(*MemoryStream).Join`/`(*SubStream).Join`) | `Get` scans history + incoming. `Join` is a pull iterator (`Next`/`Close`, `ErrJoinDone`/`ErrJoinClosed`); it is a function plus concrete methods, not a `Stream` interface method, so third-party streams keep compiling (D-098) |
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
| `response_schema` decorator / `ResponseProto` protocol | `response.Proto` interface | `Name/Description/JSONSchema/SystemPrompt/Validate(response, ctx)`; the Python `provider` parameter of `validate` is dropped (D-007). The decorator form is `response.NewCallableSchema[T](func(ctx, value T) (any, error), opts...)` (D-107, see §9) |
| `DecisionSpec` / `Question` / `DecisionOption` | `response.DecisionSpec` / `response.Question` / `response.DecisionOption` | Same enum-schema shape |
| validation exception | `*response.ValidationError{Err, Response}`, `response.ErrInvalidResponse` | The agent's `ContentAs[T]` retry loop keys off `*ValidationError` |

### Usage, task, spec, testing

| Python | Go | Differences |
|---|---|---|
| `UsageReport` / `UsageRecord` / `Usage.from_events` (`ag2/usage.py`) | `usage.Report` / `usage.Record` / `usage.FromEvents(evs)` | Totals + `ByModel`/`ByProvider`/`ByKind` buckets; only `UsageEvent`s with reported tokens count (`usage.Reported`) |
| `TaskState`, `TaskSpec`, `TaskMetadata`, `CheckpointStore`, `Task` async CM (`ag2/task.py`) | `task.State`, `task.Spec`, `task.Metadata`, `task.CheckpointStore`, `task.MemoryCheckpointStore`, `task.Task` | Python's async context manager (auto-complete/auto-fail) → explicit terminal calls `Complete/Fail/Cancel/Expire` (each returns `error`); `task.Start(ctx, owner, spec, opts...)` opens; `task.DepKey = "ag:task"` dependency key preserved |
| `TaskConfig` (`ag2/agent.py`) | `agent.TaskConfig` + `agent.WithTasks(cfg)` | Fields `Config`, `Prompt`, `IncludeTools`, `ExcludeTools`, `ExtraTools`, `MaxConcurrency`; `IncludeTools []string` — nil = all parent tools (Python `include_tools=None` default), empty slice = none, allowlist of names otherwise (`ExcludeTools` applies after it). Breaking vs earlier AGo builds where `IncludeTools bool` defaulted to "no parent tools" |
| `AgentSpec.from_agent() / to_agent()` (`ag2/spec.py`) | `spec.FromAgent(a)`, `(*spec.AgentSpec).ToAgent(available map[string]tools.Tool, opts ...agent.Option)` | Wire keys stay snake_case (`tool_names`, `response_schema`); missing tool → `tools.ErrResolution`; since D-107 `FromAgent` captures the response schema and `ToAgent` restores it as a `RawSchema` |
| `TestConfig` / `TrackingConfig` (`ag2/testing.py`) | `agotest.NewTestConfig(responses ...*events.ModelResponse)`, `agotest.NewTrackingConfig(inner config.ModelConfig)` | Import alias `agotest "github.com/luckystrker/ago/testing"`; scripted full responses with a shared cursor and recorded call snapshots (D-018; the partial-turn vocabulary is `agotest.NewTestScript(agotest.Text/ToolCall/Fail/Publish/Reply)` since D-107, see §9) |
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
| `MCPToolkit` (`tools/toolkits/mcp_server/`) | `mcp.New(config) (*mcp.Client, ...)` + `mcp.Tools(cl) ([]tools.Tool, error)`; since D-108 also `mcp.NewToolkit`/`mcp.NewToolkitFunc` (discovery cache, §9) | A helper converts remote tools to `tools.Tool` with `DeclaredInCode=false`; stdio + streamable HTTP, paginated `ListTools`, content mapping (D-029, D-034). Python's `mcp_tool` `Variable` indirection (and `Depends`/`Inject` parameters) is not ported — remote tools map their declared schemas directly (D-029, D-036) |
| Builtin/hosted tools (`tools/builtin/`: `web_search`, `web_fetch`, `code_execution`, `file_search`, `image_generation`, `shell`, `memory`, `mcp_server`, `tool_search`, `retrieval`) | `tools.NewWebSearch` / `NewWebFetch` / `NewCodeExecution` / `NewFileSearch` / `NewImageGeneration` / `NewShell` / `NewMemory` / `NewMCPServer(url, label, …)` / `NewToolSearch(mode, tools…)` / `NewRetrieval(knowledgeID, opts…)` (`github.com/luckystrker/ago/tools`, `hosted.go`) | Schema carriers (D-047): constructor options = Python kwargs, `Schemas` return the provider-neutral `ToolSchema` (Python wire field names), `Register` subscribes nothing (provider executes). `NewToolSearch` defers member schemas with `defer_loading` and registers the members (their calls are answered client-side). Provider mappers map what the ported Python clients map — anthropic: web_search/web_fetch/code_execution/memory/mcp_server/tool_search; gemini: googleSearch/urlContext/codeExecution/fileSearch; **openai: via the Responses client (D-060)** — web_search/file_search/code_interpreter/image_generation/shell/mcp/tool_search; **zai: retrieval (D-094)**; chat-completions + ollama reject every other hosted schema (`config.ErrUnsupportedTool`). The tool **content** packages are `github.com/luckystrker/ago/toolpacks` (row below) |

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
| `MetricsMiddleware` | `middleware.Metrics(reg *MetricsRegistry, opts...)` | Ported in wave 3 (D-106, see §9); stdlib registry, no OpenTelemetry |
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
| `OpenAICompletionsConfig` (and deprecated alias `OpenAIConfig`) | `config/openai.OpenAICompletionsConfig` — `NewOpenAICompletions(model)` | **full** | Chat Completions: SSE streaming with tool-call delta merge, tools, strict structured output (`openai.StrictifySchema`, `openai.ResponseFormat`), usage incl. cached/reasoning tokens; env `OPENAI_API_KEY`. Typed request options (`tool_choice`, `reasoning_effort`, penalties, `n`, `logprobs`/`top_logprobs`, `logit_bias`, `metadata`, `modalities`, `prediction`, `prompt_cache_key`/`prompt_cache_options`, `safety_identifier`, `service_tier`, `store`, `verbosity`, `web_search_options`), `extra_body` (merges last), `default_query`, `http_client` (D-094) |
| `OpenAIResponsesConfig` (+ `containers.py`) | `config/openai.OpenAIResponsesConfig` — `NewOpenAIResponses(model)`; client `ResponsesClient` | **full** (D-060; closes the D-008 deferral — D-068) | Responses API over raw HTTP/SSE: input mapping incl. `FileIdInput` → `input_file.file_id`, hosted tools on the wire (web_search/file_search/code_interpreter/image_generation/shell container auto|reference/mcp/tool_search with `defer_loading`), hosted output items → builtin call/result events with second-call item replay, strict `text.format`, usage incl. cached/reasoning tokens. Fields: APIKey/BaseURL/Temperature/TopP/MaxOutputTokens/MaxToolCalls/Store(nil=default true)/ParallelToolCalls(nil=default true)/TopLogprobs/Metadata/PromptCacheKey/PromptCacheOptions/Reasoning{Effort,Summary}/ServiceTier/User/Truncation/Timeout/MaxRetries/Organization/Project/DefaultHeaders/ExtraBody; `Clone` takes Go names + snake_case aliases, nil clears. Shell command/output delta chunks (`openai.ShellCommandChunk`/`ShellOutputChunk`) were added in D-110. Deferred: containers, prompt-cache diagnostics events, reasoning-item replay, `websocket_base_url` |
| `OpenAIDecisionsConfig` | `config/openai.OpenAIDecisionsConfig` — `NewOpenAIDecisions()` (model `DefaultDecisionsModel` "gpt-6-luna", `BooleanThreshold` 0.5); client `DecisionsClient` | **full** (D-060/D-068) | Decision-only schemas (bool, string enum, int enum from 0, 0..1 number — otherwise the schema-guidance error), one question per request, thresholded answers rendered through the schema, refusal as `*DecisionRefusedError{Name}`, usage normalization; `Descriptions`/`QuestionInstructions`/`SafetyIdentifier`/`ExtraBody` carried. The sequence form of the question instructions is `QuestionInstructionParts []any` (D-110) |
| `ag2/files/` (`FilesAPI`, `FilesClient` protocol), `config/openai/files.py`, `config/anthropic/files.py` | `github.com/luckystrker/ago/files`: `files.NewAPI(client files.Client) *files.API`, types `UploadedFile`/`FileContent`/`FileProvider`; provider clients via `(*openai.OpenAICompletionsConfig).CreateFilesClient()` and `(*anthropic.AnthropicConfig).CreateFilesClient()` | **full** (D-046) | `API` delegates `Upload/Read/List/Delete` and keeps Python's two upload validations (`files.ErrNoData`, `files.ErrFilenameRequired`); OpenAI: multipart upload (empty purpose → `assistants`), `has_more`/`after` pagination; Anthropic: `/v1/files` under `files-api-2025-04-14`, no purpose on upload, `next_page`/`page` pagination, 600s default timeout; non-2xx → per-provider `ErrFilesAPI`. Python's `created_at`/`media_type` survive in `Raw`. `containers.py` deferred; a file reference as a *message input* is mapped since D-110: `events.FileIdInput` → `{"type":"file","file":{"file_id"}}` on the OpenAI Chat Completions dialect and an `image`/`document` file source on Anthropic (other dialects of the shared engine still fail with `config.ErrUnsupportedInput`, D-017) |
| `AnthropicConfig` | `config/anthropic.AnthropicConfig` — `NewAnthropic(model)` | **full** | Messages API over raw HTTP/SSE: `tool_use`/`tool_result` replay with orphan repair, prompt caching (`cache_control` injection on the last system block, the last tool and — D-110 — the last block of the last user turn), typed `ThinkingConfig` (enabled+budget/adaptive/disabled; sampling params dropped while on) with durable `ThinkingBlockEvent`/`RedactedThinkingEvent` replay before `tool_use`, `default_headers` (incl. `anthropic-beta` merged for messages and Files), per-input `cache_control`/`citations` via `VendorMetadata`, `CacheDiagnostics` with `previous_message_id` chaining, `metadata`/`service_tier`/`speed`, `extra_body` (merges last), `http_client`, usage incl. cache tokens; env `ANTHROPIC_API_KEY`. `Clone` keys match Go names and Python snake_case (D-095) |
| `GeminiConfig` | `config/gemini.GeminiConfig` — `NewGemini(apiKey, model, opts...)` | **full** | `generateContent` + SSE streaming, function declarations, `usageMetadata` normalization (tool-use prompt tokens, thinking tokens), `WithHTTPClient` (D-094); wave 3 adds cached content, response modalities, image config, raw thinking config, input `vendor_metadata` (D-111) and the Maps tool mapping (D-112), see §9; env `GEMINI_API_KEY` |
| `VertexAIConfig` | — (only the `ProviderVertexAI` enum value) | deferred (D-022) | Google auth chain not ported |
| `BedrockConfig` (Converse) | `config/bedrock.BedrockConfig` — `NewBedrock(model)` | **stub** (D-008) | Config validates the model; `Create()` → `config.ErrNotImplemented` (needs SigV4 + botocore credential chain) |
| `MistralConfig` | `config/mistral.MistralConfig` — `NewMistral(model)` | **full** (chat-compat subset) | OpenAI-compatible engine; typed sampling/routing knobs (`stop`, `random_seed`, presence/frequency/repetition penalty, `top_k`, `min_tokens`, `n`, `top_logprobs`, `service_tier`, `metadata`, `parallel_tool_calls`, `tool_choice`, `reasoning_effort`, `prompt_cache_key`), `http_headers`, `http_client`, `timeout`; `guardrails`, `prediction`, `prompt_mode`, `logprobs` ported in wave 3 (D-111, §9); Files not ported (D-021); env `MISTRAL_API_KEY` |
| `OllamaConfig` | `config/ollama.OllamaConfig` — `NewOllama(model, opts...)` | **full** (D-022, streaming D-111) | `/api/chat` with tools + usage; `WithFrequencyPenalty`/`WithPresencePenalty` CreateOptions and `WithHTTPClient` (D-094); streaming is NDJSON (D-111). Default host `http://127.0.0.1:11434`; env `OLLAMA_API_KEY` for Ollama Cloud |
| `DashScopeConfig` | `config/dashscope.DashScopeConfig` — `NewDashScope(model)` | **full** (compatible-mode) | OpenAI-compatible dialect; `stop`, `seed`, frequency/presence penalty and `http_client` (D-094); default base URL is the `compatible-mode` root (AGO-DEVIATION, D-021); env `DASHSCOPE_API_KEY` |
| `ZAIConfig` | `config/zai.ZAIConfig` — `NewZAI(model)` | **full** (chat-compat subset) | Request knobs (`stop`, `seed`, `tool_choice`, `user_id`, `request_id`, `do_sample`, `meta`), `extra_headers`, `extra_body` (typed fields win, Python `_merge_extra_body`), `http_client`, `timeout`; the knowledge-base `retrieval` hosted tool is mapped (`tools.NewRetrieval`, D-094). Thinking flags, token-cache opt-out, sensitive-word check, `watermark_enabled`, `tool_stream` ported in wave 3 (D-111, §9); Files API not ported (D-021); env `ZAI_API_KEY` |
| `XAIConfig` (gRPC SDK) | `config/xai.XAIConfig` — `NewXAI(model)` | **stub** (D-008) | `Create()` → `config.ErrNotImplemented` (gRPC transport out of scope) |
| `TypeSafeConfig` | `config/typesafe.TypeSafeConfig` — `NewTypeSafe(model)` | **stub** (D-008) | Same stub pattern (niche SDK out of scope) |
| `input_acceptance.py` (`accepts_input`, `input_modalities`, …) | `config.AcceptsInput` / `config.DescribedDialect` (`config/input_acceptance.go`) | **ported** (W1-B, wave 1; recorded with D-096) | Static per-provider table over (position, source, kind) plus media-type filters; the provider-neutral dialect is inferred from the config type. Callers pre-check a part instead of letting the mapper fail the call, and the AG-UI server drops an unaccepted attachment with a warning (`agui/serve.go`, Python parity: `_accepted_parts`) |
| `ag2/mcp/sampling.py` pseudo-client (implements `ModelConfig` + `LLMClient`) | `mcpserver.WithClientModel()` | **ported as behavior** (D-109) | A served agent samples on the *client's* model through `sampling/createMessage` (checked against the client's advertised capability; without it the agent's own model is used). There is no Go type that implements `ModelConfig` for it, and the back-channel exists only on a stateful SSE transport (§9) |

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
| `ProviderReplay` mixin + provider events (`OpenAIReasoningEvent`, `AnthropicRedactedThinkingEvent`) | `config/anthropic/ThinkingBlockEvent`/`RedactedThinkingEvent`/`CacheDiagnostics` (registered in the `events` registry and replayed by `ConvertMessages`) | Anthropic signed/redacted thinking is durable and joined with the following `ModelResponse` into one assistant turn (D-095). OpenAI reasoning items still travel in the client's `VendorMetadata` (D-060); anchor/turn roles have no Go equivalent |
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
| `Agent(..., knowledge=KnowledgeConfig)` | Ported (D-075, compact/aggregate fields + middleware wiring in D-077): `agent.WithKnowledgeConfig(cfg knowledge.Config)` — the store registers in the dependencies under `knowledge.DepKey`, `cfg.ExposeTool` injects the `knowledge` tool (`read`/`write`/`list`/`delete`, Python answer strings verbatim), `cfg.WriteEventLog` rewrites `logs/<streamID>.jsonl` at turn end (failures → transient `EventLogFailed`), the bootstrap (`cfg.Bootstrap`, default `knowledge.DefaultBootstrap`) runs once per store, and `cfg.Compact`/`cfg.CompactTrigger` plus `cfg.Aggregate`/`cfg.AggregateTrigger` arm the post-turn compaction/aggregation middlewares (aggregation needs both halves). `knowledge.NewConfig(store)` applies the Python defaults; a `Config` with a nil `Store` contributes nothing. Policies still take the store in their constructors (D-030) |
| `FilesAPI` / `FilesClient` (`ag2/files/`) | Ported (D-046): `github.com/luckystrker/ago/files` (`files.NewAPI` over any `files.Client`), provider clients via `CreateFilesClient()` on `config/openai` / `config/anthropic` (map in §3). File references as message inputs are mapped by OpenAI Chat Completions (OpenAI dialect) and Anthropic since D-110; the other dialects of the shared engine still fail with `config.ErrUnsupportedInput` (D-017) |
| OpenAI Responses API, Decisions API | Ported (D-060/D-068, closing the D-008 deferral): `config/openai` — `NewOpenAIResponses(model)`/`ResponsesClient` (map in §3) and `NewOpenAIDecisions()`/`DecisionsClient`. Chat Completions remains the other supported OpenAI surface |
| Hosted/builtin tools: `web_search`, `web_fetch`, `code_execution`, `file_search`, `image_generation`, `shell`, `memory`, `mcp_server`, `tool_search`, `retrieval` | Carriers ported (D-047, map in §2): `github.com/luckystrker/ago/tools` `hosted.go`, mapped by anthropic, gemini, the Responses client (D-060) and zai (`retrieval`, D-094); chat-completions + ollama reject every other hosted schema (`config.ErrUnsupportedTool`). Per-call `Variable` kwargs and the Google Maps carrier were ported in wave 3 (D-112, §9). Still deferred: Gemini grounding/server-tool replay events |
| Ready-made tool content: `tools/{shell,code,sandbox,search,skills,dynamic}/`, `toolkits/filesystem.py` | Ported as a functional subset (D-062/D-063): `github.com/luckystrker/ago/toolpacks` — `NewShellTool(opts ...ShellOption)`, `NewLocalCodeRunner(opts ...CodeRunnerOption)` (`run_code`, local backend), `NewFilesystemToolkit(root).Tools()`, `NewDuckDuckGoSearch`/`NewTavilySearch(apiKey, ...)`/`NewPerplexitySearch(apiKey, ...)`, `NewSkillsToolkit(dir).Tools()`, `NewDynamicAgentFactory(cfg, pool, opts...)`. Wave 3 (D-112/D-115, §9) added `update_file`/`delete_file`, per-call `Variable` kwargs of the search tools, the skills script runner, the execution seam and the Docker/Daytona sandboxes (`contrib/sandboxdocker`, `contrib/sandboxdaytona`). Deferred: the Tenki sandbox, the TypeScript runner |
| `ag2/mcp` server product (`MCPServer`, `mcp_tool`, sampling, elicitation) | Ported as a stateless subset (D-058, superseding the D-010 deferral): `github.com/luckystrker/ago/mcpserver` — `NewServer(ServerConfig{Ask, Tools, ...})` / `NewServerForAgent(a, opts...)`, transports `(*Server).Stdio()` and `(*Server).Handler()` (streamable HTTP), methods `initialize`/`tools/list`/`tools/call`/`ping`; one conversational `ask` tool plus optional deterministic tools. Wave 3 (D-109, §9) added sessions/conversations + `Mcp-Session-Id`, SSE answers with resume, resources/prompts, the elicitation/sampling back-channel, progress and argument validation. Not ported: the modern protocol era on the server, pause/`InputRequiredResult`, OAuth security, MCP Apps UI, a stdio back-channel, batch requests (refused like the SDK). The consume-side client is `github.com/luckystrker/ago/mcp` (D-029; since D-108 it answers `input_required`, negotiates the protocol era and has the `Toolkit` discovery cache) |
| Network wave 2: view policies (`views/`: FullTranscript, WindowedSummary, NamedTranscript, NamedWindowedSummary), hub arbiter, expectation sweepers + audit, task mirror, client LLM tools (`peers`/`channels`/`tasks`/`context`/`delegate`) | Ported (D-056, in `github.com/luckystrker/ago/network`): the four `ViewPolicy` impls over `ViewOptions{Channel, RenderEnvelope, NameFor}` (`Hub.NameFor`, `DefaultRenderEnvelope`), `HubArbiter` seam + `BaseArbiter` + `RuleBasedArbiter` (hub consults send/dispatch/channel-open/unknown-audience; `Register` and `AuthorizeInbox` are not consulted — Python parity), expectation evaluators (`acks_within`/`reply_within`/`max_silence`) + handlers (audit/notify/auto-close) + `AuditSink` (`NewFileAuditLog`/`NewSliceAuditLog`) + `NewExpectationSweeper` (owners opt in), `NewTaskMirror(hub, ownerID, channelID)`, `ClientTools(hc, self) ([]tools.Tool, error)`. Federation stays a stub (`network.RemoteAgentProxy`, no implementations — remote routing via `ResolveUnknownAudience`) |
| WebSocket link (`network/transport/ws.py`) | Ported as the optional module `github.com/luckystrker/ago/contrib/wslink` (D-057/D-069): `wslink.Dial(ctx, url, opts)` / `wslink.ServeWS(w, r, handle)` wrapping `transport.Link` over `coder/websocket` — own go.mod, one external dep |
| Knowledge: `SqliteKnowledgeStore`, `RedisKnowledgeStore` | Ported as optional modules (D-048/D-059, closing the D-011 deferral): `contrib/knowledgesqlite.NewStore(dbPath)` (pure-Go SQLite) and `contrib/knowledgeredis.NewStore(rdb, opts...)` — both `knowledge.Store`; Redis streams: `contrib/streamredis.NewStorage(rdb, opts...)` (`stream.Storage`). The rest of `ag2/knowledge/` is ported in `github.com/luckystrker/ago/knowledge` — next row |
| `knowledge/bootstrap.py` (`StoreBootstrap`, `DefaultBootstrap`), `knowledge/polling.py` (`PollingChangeWatcher`), `knowledge/config.py` (`KnowledgeConfig`) | Ported (D-075, closing the D-011 deferral; `compact`/`aggregate` fields and the agent's middleware wiring in D-077): `knowledge.StoreBootstrap` + `knowledge.DefaultBootstrap{MentionTool}` + `knowledge.BootstrapOnce(store, actorName, b)` (sentinel `knowledge.SentinelPath` = `/.initialized`, written before the layout); `knowledge.VersionSource` (`VersionsUnder(prefix) (map[string]string, error)`) + `knowledge.NewPollingWatcher(src, prefix, onChange, interval)` → `(*PollingWatcher).Start(ctx)`/`Close()` (default 500ms, floor 50ms); `knowledge.Config{Store, ExposeTool, WriteEventLog, Compact, CompactTrigger, Aggregate, AggregateTrigger, Bootstrap}` / `knowledge.NewConfig(store)` wired via `agent.WithKnowledgeConfig`. Deviations: the bootstrap's log-directory SKILL.md lives at `/logs/` (AGo's `LogsDir` pin, D-027); `VersionsUnder` exists only on `MemoryStore` (Python's sqlite/redis backends carry version columns); the dependency key is the string `"ag:knowledge"` (Python keys by the store type); `NewConfig` carries the Python flag defaults — the Go zero value disables both; the strategy fields hold the transcript-shaped `compact.Strategy`/`aggregate.Strategy` (D-026/D-028) — Python's store-aware strategy protocols (`persist_dropped`, store-writing aggregation) are not ported (D-077) |
| Network core: envelope/channel/identity/rule/transitions/handoff, `Hub`, the four adapters, clients, transport | Ported single-host subset (D-010/D-031): envelope vocabulary (`ag2.*`), channel state machine, passports, rules, `TransitionGraph`, handoff/finish, `Hub`, Consulting/Conversation/Discussion/Workflow adapters, `HubClient`/`AgentClient`/`HumanClient`, WAL (memory/file/knowledge) with `Replay`, `transport.LocalLink`. Federation (`remote.py`) stays an interface stub (see the wave-2 row above) |
| Live/voice (`ag2/live/`, `events/voice.py`) | The HTTP cascade is ported (D-057/D-072): `github.com/luckystrker/ago/live` — `live.Cascade{STT Transcriber; LLM func(ctx, text) (string, error); TTS Synthesizer}` + `Handle(ctx, audio, mime, voice)`, ready stages `live.OpenAITranscriber`/`live.OpenAISynthesizer`, sentinels `ErrAPI`/`ErrIncomplete`. Deferred: the realtime-websocket stack (LiveAgent/`RealtimeConfig`, turn detection, barge-in, streaming TTS, `STTTranslationConfig`), the elevenlabs/gemini/mistral providers, `events/voice.py`, sound devices |
| `a2a/` — `A2AServer` (`server.py:38`), `build_card` (`card.py:31`), `AgentExecutor` (`executor.py`), the JSON-RPC transport (`transports/`), `A2AClient(LLMClient)` (`client.py:159`), task records (`tasks.py`) | Ported through wave 2 (D-049/D-050 + D-067, superseding the D-010 deferral; closure ledger D-070): `github.com/luckystrker/ago/a2a` — `NewServer(ServerConfig{Card, Handle, Store})` + `Handler()` (card discovery at `/.well-known/agent-card.json` + legacy `agent.json`; JSON-RPC 2.0 `message/send`, two-event SSE `message/stream`, `tasks/get` with `historyLength` trimming, `tasks/cancel`; `input-required` task state), push notifications (`ServerConfig.PushStore` + the `tasks/pushNotificationConfig/set|get|list|delete` methods, `DefaultPushSender`/`PushReceiver`, `Client.SetPushConfig`/…), the `urn:ag2:client-tools:v1` extension (`ServerConfig.HandleClientTools`/`HandleToolResults` server legs; `Client.RunClientTools`/`RunWithHumanInput` drivers, `AttachClientTools`/`ValidateExtensionActivation`), context-variable sync + HITL over `Message.metadata` (`ExtractContextUpdate`/`WithContextUpdate`/`StripReservedVariables`, `ServerConfig.SyncVariables`), the REST twin (`POST /message:send`, `POST /tasks`, `GET /tasks/{id}`, `POST /tasks/{id}:cancel`), declarative security schemes (security.py). `TaskStore` + `NewMemoryTaskStore`; protocol types with camelCase tags and the `kind` part discriminator; client `NewClient(baseURL, opts...)` — `AgentCard`/`SendMessage`/`SendMessageTask`/`TasksGet`/`TasksCancel`. Not ported: full history shipping (`ag2.history+json`), reconnect/resubscribe, card JWS signing/verification (no stdlib JWS — D-067), `validate_input_modes`, cluster mode, tenants, **gRPC — closed permanently (D-071)** |
| `acp/` — `ACPAgent` (`agent.py:118`) + executor, `ACPBridge` (`bridge.py:259`) + `make_bridge`, `ACPClient` (`client.py:77`), `permissions.py`, `elicitation.py`, `auth.py`, `tool_gateway.py` | **`ACPClient`/`ACPConfig` (the ACP agent as a model, `client.py:77`, `config.py:56`) are ported in gap-audit wave 2 as `acp.Config` + presets — see §8 and D-105** (until 2026-11-10 this row wrongly implied they were). Ported through wave 2 (D-052 + D-066; closure ledger D-070): `github.com/luckystrker/ago/acp` — both roles over newline-delimited JSON-RPC 2.0 stdio. Serve: `Serve(ServeConfig)` / `ServeAgent(a, cfg)` (`initialize` with truthful capabilities + `authMethods` + `loadSession: true`, `authenticate` + `Auth`/`acp.NewStaticTokenAuth(token)` gating session methods, `session/new` (uuid4 id, cwd/mcpServers recorded), `session/load` history replay (`ServeConfig.History`; `ServeAgent` replays its per-session conversations), `session/prompt` running `Handle(ctx, sessionID, userText, emit *Emitter)` — the full update vocabulary (`Message` chunks, `Thought` under `StreamThoughts`, `Plan`, `ToolCall`/`ToolCallUpdate`, `Usage` under `ContextWindow`), `session/cancel` stops the turn context and answers `{"stopReason":"cancelled"}`, never an error; `Session.RequestPermission`/`Session.Elicit` gated by the config policies (deny = synthesized locally); `Session.Gateway()` proxies client-declared MCP servers through `ago/mcp`. Client: `NewBridge(cmd, args, env)` + `SessionNew`/`SessionNewWithServers`/`Prompt`/`PromptTurn` (stop reason)/`Cancel`/`Load`/`Close`, `Bridge.FS` mediating `fs/read_text_file`/`fs/write_text_file` and `terminal/*` under the root guard (nil = disabled, `-32601`). Not ported: the multi-session store, cross-process `session/load` storage, the tool_gateway HOST direction (exposing local tools over HTTP), serving the agent's own tools through the gateway, `guard.py`, OAuth/terminal auth kinds, model pickers, remote transports |
| `ag_ui/` — `AGUIStream` (`stream.py:118`), `build_asgi` (`asgi.py:22`), `run_input.py` (`read_run_input`), `capabilities.py` (`served_capabilities`), `events.py` (`AGUIEvent`), `interrupts.py` | Ported through wave 2 (D-053 + D-065; closure ledger D-070): `github.com/luckystrker/ago/agui` — AG-UI 1.0 serving over `net/http`: `ServeAgent(a) http.Handler` and `Handler(HandlerConfig)` (GET capabilities, POST → SSE, 400 before any stream, protocol-major refusal, cancelled outcome), the event projection `NewStream(emit)` + `Handle`, `ReadRunInput` (dual-case keys, unknown-role strip, base64 refusal), `CapabilitiesFor`, typed events + `UnmarshalEvent` + `Raw`. Held turns (D-065): `agui.NewServer(opts...)` — `WithRetention` (default 900s/128), `WithResumeProof`, `WithStateSnapshots` — with `ServeAgent(name, prompt, opts...)`/`Handler(a)`: a `HumanInputRequest` pauses the run as RUN_FINISHED with an `interrupt` outcome (`InterruptOutcome{Interrupts}`); the next POST's `ResumeEntry{InterruptID, Status, Payload}` answers it (resolved → rebind to the new exchange, cancelled → abandon), refusals are single coded RUN_ERRORs before RUN_STARTED. Not ported: REASONING_ENCRYPTED_VALUE, the subagent suspension ledger, seeding the turn's variables from the client's state, prompt-level inputs as per-run fragments |
| `eval/` — `runtime/runner.py` (`run_agent`), `runtime/evaluate.py` (`evaluate_traces`), `trace.py` (`Trace`), `scorer.py` (`Scorer`, the `@scorer` decorator), `scorers/` (correctness, cost, threshold, tools, judge, pairwise_judge, attribution), `pairwise.py` (`PairwiseComparator`, `WinRate`), `results/` (`RunResult`, `store.dump`) | Ported produce-then-grade (D-051 + D-061; closure ledger D-070): `github.com/luckystrker/ago/eval` — `Run(ctx, agent, Suite, opts ...RunOption)` over `Case{Name, Input, Expected, Scorers}` (`WithConcurrency(n)`, default 4; 1 = strict order), `BuildTrace(evs)` reconstructing `Trace` **from the stream's event log — no OTEL** (the core deviation; `Trace.Err` = Python `Trace.exception`), scorers `Correctness`, `TokenBudget`, `ToolsCalled`, `Threshold`, `LLMJudge`, `Pairwise`, `FailureAttribution` + `DetectMechanical` (attribution.py), the scorer registry (`NewScorerRegistry`, name + params factories for `correctness`/`token_budget`/`tools_called`/`threshold`/`llm_judge`/`failure`, `SetClient`, `Register`), `SuiteFromJSONL(path, registry)` (input required, auto-names, scorer specs), `RunResult.SaveJSON`/`LoadJSON` + `Compare`, `Diff`/`DiffStrict`/`Summary`, `WilsonCI`/`(*RunResult).PassRateCI`/`CohenKappa`. Deferred: OTEL/Tempo/OTLP trace sources and `evaluate_traces`, repeats/per-task configs/`BudgetThresholds`/lifecycle events, suite name/source metadata + tags, pairwise single-order mode |
| `a2ui/` — `A2UIServer` (`server.py:25`), `A2UIAction`/`A2UIEventAction` (`actions.py`), `SchemaManager` (`schema_manager.py`), `parser.py`, `incoming.py`, `serialize.py`, `request.py` | Ported v1.0-only subset (D-076, supersedes the D-010 deferral for this slice; stable wire in D-089): `github.com/luckystrker/ago/a2ui` — the `Message` union + flat `Component` (verbatim camelCase, golden-tested against the running Python; the stable wire version is `"1.0"` while the protocol selector stays `v1.0`) and the canonical `ToJSONL` serializer; `NewParser(mgr).Parse(response)` (the `<a2ui-json>` tags, fence stripping, JSONL, the exactly-one-payload envelope + root-component rules) with `Validate`/`FormatValidationError` over the vendored v1.0 basic catalog (hand-rolled JSON-Schema subset — unsupported keywords pass; `TextField.value` required, restricted catalog `$ref`s enforced at load); `SanitizeForPrompt` plus the exact unregistered-click/error/function-response rewrites; `Action{Name, Description, ExampleContext, Fn ActionFunc}` + `NewActionRegistry()`; `NewServer(opts...)` — an `http.Handler` (SSE or NDJSON, the rest.py request contract and error strings) that answers registered clicks and every `callAgentFunction` itself and hands the rest to `WithPromptHandler` (`Turn{Text, Messages}`). Not ported: v0_9/v0_9_1 spec and capacity keys, the A2A transport (and its MIME-tagged parts), Starlette transports/ASGI app, events/middleware stream integration, capabilities negotiation, custom catalogs, `dispatch.py`'s agent-turn execution |
| `mcp_ui/` (`resources.py`, `actions.py`) | Ported (D-045, supersedes the D-010 deferral for this slice): `github.com/luckystrker/ago/mcpui` — UI resources `RawHTML`/`ExternalURL`/`RemoteDOM` (text/blob encoding, `ui://` guard, `_meta` merge), UI actions `ToolCall`/`Prompt`/`Link`/`Intent`/`Notify`, `PostMessage` (json.dumps + html.escape byte parity), `mcpui.Marshal` for exact wire bytes, `WithUIResource` attaching blocks to `events.ToolResult.Parts` |
| `CronWatch` / `CadenceWatch` (`ag2/watch.py`) | Ported in wave 3 (D-106): `watch.CronWatch`, `watch.CadenceWatch` (§9); `EventWatch`/`IntervalWatch`/`DelayWatch`/`AllOf`/`AnyOf`/`Sequence` as before |
| `MetricsMiddleware` | Ported in wave 3 as `middleware.Metrics` (D-106, §9). `TelemetryMiddleware` is ported as `contrib/otelx` (row in §2, D-064) |
| `fast_depends.Depends` (factory-valued tool parameters; re-exported by `ag2`) | Not ported (D-036) — compute the factory value in Go before the call or wrap it as a separate tool; the DI dictionary (`Inject`/`Variable`/context parameter, D-016) covers the resolved-value cases |
| `textual.py` | Ported as a stdlib line REPL (D-056): `github.com/luckystrker/ago/tui` — `tui.REPL(a *agent.Agent, in io.Reader, out io.Writer, opts ...tui.Option) error`, `/help`//`exit`, `WithStreaming()` echoes `ModelMessageChunk` deltas; the textual App surface (widgets, markdown, the reasoning block) is deferred |
| `mypy.py`, `_import_utils.py`, `_telemetry_consts.py`, `types.py`, `utils.py` | Python infrastructure without a Go counterpart (typing helpers, telemetry constants, the extras-import block); `utils.build_model` → `response.SchemaOf`. `plugin.py` has a counterpart since D-044 (`github.com/luckystrker/ago/plugin`) |

## 6. Semantics cheat-sheet (things that look different but are parity)

**A2UI wire version (D-089, upstream `1b0c399012`).** v1.0 stabilized: a
message's `version` and a version-keyed capabilities object carry the bare
`"1.0"`, not the protocol selector `"v1.0"`. AGo exposes `a2ui.WireVersionV1`
(`"1.0"`) and keeps `a2ui.VersionV1` (`"v1.0"`) for the protocol/spec names;
`Message.Version` defaults to the wire version, `SchemaManager.VersionString()`
and `Parser.VersionString()` report it, and `ProtocolVersion()` reports the
selector. Serialized messages and the golden fixtures now carry `"1.0"`; an
operation declaring `"v1.0"` is rejected by the re-vendored schema.
`TextField.value` is required in the basic catalog, and the restricted catalog
`$ref` rule is enforced when the vendored catalog loads (custom catalogs are
still not ported).

- **Async → goroutines + `context.Context`:** every cancellable op takes `ctx` first; stream
  pub/sub dispatches synchronously in the caller's goroutine (D-004).
- **Errors:** exception classes → package sentinels; check with `errors.Is`/`errors.As`.
- **Optional fields:** `X | None` → `*X` (notably `events.Usage` is `*int64`, D-006).
- **Turn termination:** no max-turn counter in the core loop — parity with Python; the loop
  ends when the model stops requesting tools, a `final=true` tool forces a response, or a
  fatal alert produces `"HALTED: <reason>"`. `max_turns` exists only in network transitions.
- **HITL failure path:** a `HumanInputError` escaping the turn triggers
  `events.CloseUnansweredToolCalls` history repair before propagating — same as Python.
- **Delegation usage:** operational child events reach the parent as `ChildEvent`
  observations; source `UsageEvent`/`Task*` records travel as themselves with
  `path`/`task_id`/`agent_name`. There is no subtask rollup: reports deduplicate
  source records by `event_id`, and `TaskCompleted.Usage` is a cumulative snapshot
  that must not be added to them (D-092).

## 7. Verification status

Gates re-run for this document's final-wave sweep on 2026-10-30 (whole module, fresh
`-count=1`, from the repo root — `go.mod` sits there and `examples/` is part of the same
module; `contrib/` modules are excluded by construction — nested modules, they run their
own gates), re-verified 2026-10-31 after the D-075/D-076 knowledge+a2ui wave:

- `go build ./...` — success; `go vet ./...` — clean. (`gofmt -l` is not meaningful on
  this CRLF working-tree checkout — autocrlf flags every file; CI checks gofmt on the
  LF checkout.)
- `go test -count=1 ./...` — 47 packages `ok`, 0 failures, 0 skipped; no test files
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
and `github.com/luckystrker/ago/eval` were re-checked the same way during this sweep, the
`github.com/luckystrker/ago/knowledge` and `github.com/luckystrker/ago/a2ui` rows were
checked the same way on 2026-10-31, and
every code snippet in [docs/GUIDE.md](GUIDE.md) added in the final wave was compile-checked
against the tree (a scratch module importing the root module and the contrib modules the
snippets name — knowledgesqlite, knowledgeredis, streamredis, otelx — via `replace`); the
D-075/D-076 snippets (the polling watcher, the A2UI server) were compile-checked the same
way on 2026-10-31.

## 8. Gap-audit wave 1 (2026-11-09/10)

New API from the wave-1 tasks, grouped by area. Symbols are the exported Go names in
this tree; the decision records are D-095…D-104.

### Agent, stream and runtime

| Python | Go |
|---|---|
| `ask/run/resume(config=, tools=, middleware=, observers=, plugins=, hitl_hook=, dependencies=, variables=, prompt=, response_schema=, stream=)` | `AskWith`/`RunWith`/`ResumeWith` + `agent.RunOptions` |
| `agent.task(description=, payload=, capability=)` | `(*Agent).TaskWithSpec(ctx, task.Spec, opts...)` |
| `subagent_tool(stream=, middleware=)` / `persistent_stream()` | `AsTool(WithToolStream, WithToolStreamFactory, WithToolMiddleware)` / `agent.PersistentStream()` |
| `background_agent_tool()` | `(*Agent).AsBackgroundTool()`; settle the goroutine with `Stream.WaitBackground()` |
| `Agent(observers=...)` | `agent.WithObservers(obs...)` |
| `subscribe(timeout=)` | `stream.SubscribeOptions.Timeout` + `stream.ErrSubscriberTimeout` |
| `stream.join(max_events=, condition=)` | `stream.Join(ctx, s, stream.JoinOptions{Condition, MaxEvents})`, `Next`/`Close`, `ErrJoinDone`/`ErrJoinClosed` |
| `collect_usage_events` | `usage.NewCollector` (`Collect`, `Records`, `Report`) |
| `RetryMiddleware(max_retries, retry_on)` | `middleware.RetryMaxRetries` + `middleware.WithRetryOn` |
| `ApprovalRequired(message, denied_message, allow_always)` | `middleware.WithApprovalMessage`, `WithDeniedMessage`, `WithAllowAlways` |
| `chars_per_token` | `WithCharsPerToken` on `middleware.TokenLimiter`, `policies.TokenBudgetPolicy`, `compact.TokenTrigger` |
| `observer(interrupt=, timeout=)` | `observers.WithInterrupt()`, `observers.WithTimeout(d)` |
| `TailWindowCompact(target)` | `compact.NewTailWindowCompact(target)` / `compact.NewTailWindow(trigger, target)` |

### Providers and media

| Python | Go |
|---|---|
| Anthropic `thinking=` (via `extra_body`) | `anthropic.ThinkingConfig` (`Enabled`+`BudgetTokens`/`Adaptive`/`Disabled`), `ErrInvalidThinking` |
| Anthropic thinking replay events | `anthropic.ThinkingBlockEvent`, `anthropic.RedactedThinkingEvent`, `anthropic.CacheDiagnostics` in the `events` registry |
| `default_headers`, `anthropic-beta`, cache diagnostics | `AnthropicConfig.DefaultHeaders`, `CacheDiagnostics bool` |
| `accepts_input` / `input_modalities` / `input_media_types` / `binary_kind_of` | `config.AcceptsInput`, `config.InputModalities`, `config.InputMediaTypes`, `config.BinaryKindOf`, `config.InputDialect` |
| Chat Completions `input_audio`, `file`, `image_url.detail` | shared engine (`config/openaicompat.go`), `ErrUnsupportedInput` for invalid values |
| Mistral `document_url`, array `content`/`ThinkChunk` | shared engine mapping to `ModelReasoning` (stream and non-stream) |

### Tools, toolpacks and skills

| Python | Go |
|---|---|
| `FilesystemToolkit(read_only=True)` | `NewFilesystemToolkit(root, WithFilesystemReadOnly())` |
| `SandboxShellTool(allowed=, blocked=, ignore=, readonly=)` | `WithShellAllowed`, `WithShellBlockPatterns`, `WithShellIgnore`, `WithShellReadonly`; `ReadonlyCommands()` |
| `CodeAdapter(runners=)` / `LanguageRunner` | `LanguageRunner{InlineArgv}` or `{FileExtension, FileRunnerArgv}`, `WithCodeRunners`, `DefaultLanguageRunners` |
| `middleware=` on shell/filesystem/search/dynamic/code tools | `WithShellMiddleware`, `WithFilesystemMiddleware`, `WithDDGMiddleware`, `WithTavilyMiddleware`, `WithPerplexityMiddleware`, `WithDynamicMiddleware`, `WithCodeRunnerMiddleware` |
| `SkillLoader.validate_skill_metadata`, `resources_truncated` | `toolpacks.ValidateSkillMetadata`, `SkillInfo.ResourcesTruncated` |
| `MemorySkill` / `MemoryRuntime` | `toolpacks.NewMemorySkill`, `toolpacks.NewMemoryRuntime`, `NewSkillsToolkitFromRuntime` |
| `SkillPlugin(...)` | `toolpacks.NewSkillPlugin(toolkit)` (catalog + capability-gated tools) |
| `ClientTool(middleware=)` | `tools.WithClientToolMiddleware` |

### AG-UI, MCP and ACP

| Python | Go |
|---|---|
| `RunAgentInput.tools` (per-ask tools) | automatic: `ServeAgent`/`Server.begin` pass them as `agent.RunOptions.Tools`; no construction-time option |
| `mcp_tool(title=, annotations=, output_schema=, meta=)` | `mcpserver.ServerConfig.ToolMeta` / `mcpserver.WithToolMeta(name, ToolMeta)`; `ToolMeta.Resolve` |
| `MCPExecutionContext` / `MCP_REQUEST_CONTEXT_DEP` / `MCPRequestContextField` | `mcpserver.RequestDependency` + `*RequestContext`, `ServerConfig.Variables`/`Dependencies` |
| `context_provider(get_access_token())` | `mcpserver.WithContextProvider(func(ctx, token, *RequestContext) (RequestScope, error))`; `RequestScope{Variables, Dependencies, Tools, Prompt}` |
| `current_mode_update` / `available_commands_update` | `acp.ModeChange`, `acp.AvailableCommandsEvent`, `acp.EventOf`; `Emitter.Mode`/`AvailableCommands` |

### Network, A2A, eval and live

| Python | Go |
|---|---|
| envelope `task_id`/`trace_id`; manifest `labels`/`default_view_policy`/`knobs_schema`/`participant_schema` | `network.Envelope.TaskID`/`TraceID`; `network.ChannelManifest.Labels`/`DefaultViewPolicy`/`KnobsSchema`/`ParticipantSchema` |
| passport/resume fields, `record_observation`, `agents_with_capability` | `network.Passport`, `network.Resume`, `AddResumeExample`, `Hub.RecordObservation`, `Hub.AgentsWithCapability` |
| `LimitsBlock.inbox` (pending count decremented on outbound, `hub/core.py:2172`), `LimitsBlock.rate` | `network.InboxBlock`, `Hub.AckInbox` (explicit acknowledgement), `Hub.InboxPending`, `Rule.Rate`, `Hub.SetRule` |
| `round_robin` / `sequence` graph builders | `network.NewRoundRobinGraph`, `network.NewSequenceGraph` |
| auth adapters, `set_context`/`delete_context` | `network.AuthAdapter`/`NoAuth`/`ApiKeyAuth`/`AuthRegistry`/`WithHubAuth`; `SetContext`/`DeleteContext` and client methods |
| hub listeners, health, queries, checkpoints, peer cancel, pending turns | `network.HubListener`/`BaseHubListener`, `Hub.Health`, `QueryChannels`/`ReadWALRange`/`QueryAgents`/`CanSend`, `CheckpointTask`/`ReadTaskCheckpoint`/`HubBackedCheckpointStore`, `ResumePendingTurns`/`WaitForChannelEvent`, `NewNetworkPlugin`, `ClientTask`, `ToolsFor`/`ExpectedNext` |
| `A2AConfig(prefer=)` / `preset_card` / `interceptors` / `history_length` | `(*a2a.Client).Connect` + `a2a.WithTransport`, `WithPresetCard`, `WithInterceptors`, `WithHistoryLength` |
| `card_modifier` / `card_cache_control` | `a2a.ServerConfig.CardModifier` / `CardCacheControl` |
| `no_tool_errors()` / `console_reporter` / `store_dir`/`load_run` | `eval.NoToolErrors{}`, `eval.NewConsoleReporter`+`WithReporter`, `WithStoreDir`/`StoreRun`/`LoadRun` |
| `voice_to_wav_buffer` | `live.PCMToWAV` (automatic in `OpenAITranscriber`) |

### ACP agent as a model (wave 2, D-105)

| Python | AGo |
|---|---|
| `ACPConfig(command=..., args=..., env=..., cwd=..., model=...)` | `acp.Config{Command (argv), Env, CWD, ModelName, ...}` (a `config.ModelConfig`) |
| `ClaudeCodeConfig` / `CodexConfig` / `OpenCodeConfig` / `KiloCodeConfig` | `acp.ClaudeCodeConfig` / `acp.CodexConfig` / `acp.OpenCodeConfig` / `acp.KiloCodeConfig` |
| `aclose()` / `async with config` | `Config.Close()` (required; no finalizer) |
| `allow_terminal=False` / `expose_tools=False` | `DisableTerminal: true` / `DisableTools: true` (zero value = Python default) |
| `ACPTransportError` / `MCPCapabilityError` | `acp.ErrTransport` + `*acp.TransportError` / `acp.ErrMCPCapability` |
| returned files (`turn_files`) | `ModelResponse.Files` |


## 9. Gap-audit wave 3 (2026-11-10)

Python → AGo mappings for the API added in wave 3 (D-106..D-115). Everything is additive:
no existing signature changed, and every new field's zero value keeps the old behavior
unless a row says otherwise. Symbols were grepped in the tree; "(opt-in)" marks a place
where AGo's default differs from Python's on purpose.

### Core: watches, observers, middleware, policies (D-106)

| Python | Go | Differences |
|---|---|---|
| `CronWatch(expression)` | `watch.CronWatch(expression string, opts ...CronOption) (Watch, error)`; `watch.WithCronNow(func() time.Time)` for tests | Own five-field parser (`*`, `a-b`, `*/step`, `start/step`, lists, `SUN..SAT`, 7 = Sunday). Out-of-range and reversed fields are **rejected at construction** (Python fails or silently wraps at search time); one-year horizon, `now+1h` fallback |
| `CadenceWatch(stream, n, max_wait)` | `watch.CadenceWatch(s stream.Stream, n int, maxWait time.Duration, opts ...CadenceOption) (BatchWatch, error)`; `watch.WithCadenceCondition` | Pull model: the batch comes from `BatchWatch.WaitBatch`; the subscription lives for one wait |
| `BaseObserver(name, process, batch=, window=, timeout=)` | `observers.NewBaseObserver(name, process ProcessFunc, opts ...)`; `WithBatchSize`, `WithBatchWindow`, `WithProcessTimeout`, `WithObserverCondition` | Batches inline in the stream subscription; `process` errors and timeouts are logged, never raised from `Send` |
| `LoopDetector(repeats, window_size=, name=, timeout=)` | `observers.NewLoopDetector(repeats int, opts ...)`; `WithLoopWindowSize`, `WithLoopName`, `WithLoopTimeout` | Detects repeated tool calls (name + arguments) as Python does, **and keeps** the earlier model-text detection. Default name `loop_detector` (Python: `loop-detector`) |
| `TokenMonitor(warn=, alert=)` | `observers.NewTokenMonitorThresholds(warn, alert int64, opts ...)`; `WithTokenMonitorName`, `WithTokenMonitorTimeout`; `NewTokenMonitor(budget)` unchanged | `>=` comparison, warning/critical, each fired once; default name `token_monitor` (Python: `token-monitor`) |
| `MetricsMiddleware` | `middleware.Metrics(reg *middleware.MetricsRegistry, opts ...)`, `middleware.NewMetricsRegistry()`, `middleware.WithMetricsAgentName(name)` | Nine instruments, Python names/labels; stdlib registry, read with `reg.Samples()` (no OpenTelemetry — use `contrib/otelx` for spans). `error_type` is the Go type name; a tool failure is labelled `tool_error`. The agent label comes from the `ago:agent` dependency (D-107) |
| `middleware.describe()` | `middleware.Describe(m Middleware)`, `middleware.DescribeFactory(f Factory)` → `MiddlewareDescription` | Describes a per-turn instance (a Go `Factory` is a closure); a custom retry filter gives `retry_on: "custom"`, `Complete: false` |
| `transparent=` on window/budget/episodic policies | `policies.WithWindowTransparency(bool)`, `WithBudgetTransparency(bool)`, `WithEpisodicTransparency(bool)` | Counts the conversational subset (D-030). **Episodic is off by default** (Python: on) so existing prompts do not change |
| `validate_order(policies)` | `policies.ValidateOrder(ps []AssemblyPolicy) []string` | Same warning text |

### Core: halt, schemas, spec, testing, compaction, knowledge, agent (D-107)

| Python | Go | Differences |
|---|---|---|
| `HaltCheckMiddleware` (`middleware/halt.py`) | `middleware.HaltCheck()` | Place it after the layer that sends `HaltEvent`; its subscription lives for one turn |
| `@response_schema(...)` decorator | `response.NewCallableSchema[T](func(ctx *agocontext.Context, value T) (any, error), opts...)`; options `WithCallableName`, `WithCallableDescription`, `WithCallableJSONSchema`, `WithCallableEmbed` | The answer is decoded into `T` like `Schema[T]`; dependencies come from the conversation context, not `fast_depends`; `SystemPrompt()` is empty. `response.NewRawCallable` passes the raw text (Go-only) |
| `ResponseSchema(T, embed=False)` | `response.NewSchema[T](name, description, response.WithSchemaEmbed(false))` | A scalar is validated and prompted without the `{"data": …}` envelope; for `string` the answer is still a JSON-quoted string |
| `AgentSpec.from_agent()` / `to_agent()` with a response schema | `spec.FromAgent`, `(*spec.AgentSpec).ToAgent(available, opts...)`, `(*spec.SchemaSpec).ResponseProto()` | Restored as a `RawSchema` (no validation); an explicit `agent.WithResponseSchema` wins |
| `TestConfig(*turns, raise_tool_errors=)` with `Turn` | `agotest.NewTestScript(agotest.Text(...), agotest.Reply(resp), agotest.ToolCall(calls...), agotest.Fail(err), agotest.Publish(evs...))`; `(*TestConfig).SetRaiseToolErrors(bool)` | `Text` does not also publish a `ModelMessage` (the Go agent publishes the returned response). **`SetRaiseToolErrors` is off by default** (Python: on); it sees the tool-error events in the history but cannot re-raise the original exception type |
| compaction keep window | `compact.New(trigger, strategy, compact.WithKeepRecent(n))` | The cut moves to a replayable boundary: a tool result never loses its call |
| `await store.on_change(path, cb)` → `ChangeSubscription` | `store.Subscribe(path, fn) *knowledge.ChangeSubscription` on `MemoryStore`/`DiskStore`/`LockedStore`; `knowledge.SubscribeTo(store, path, fn)`; interface `knowledge.ChangeSubscriber` | `Close()` is idempotent and nil-safe; a path matches itself and anything below; callbacks fire in registration order; `DiskStore` sees only this process's writes |
| the agent under `__ag2_agent__` | `middleware.AgentDependencyKey` (`"ago:agent"`), published by the run | Feeds the `Metrics` agent label |

### MCP client (D-108, D-109)

| Python | Go | Differences |
|---|---|---|
| `MCPAnswerPolicy` | `mcp.AnswerPolicy{Elicitation, Sampling, SamplingHandler, Roots, MaxRounds}` + `mcp.WithAnswering(p)` | Each capability is advertised only when enabled; `ErrMaxRounds` past `MaxRounds`; `ErrAnswering` when a request cannot be answered |
| `await tool_call(...)` answering server requests | `(*mcp.Client).CallToolWith(ctx, cx, name, args) (ToolOutcome, error)` | `cx` carries the conversation (HITL channel, the agent's model). Elicitation without a conversation **fails** instead of being declined; answers to one round are sequential |
| `MCPServerConfig(protocol_mode=)` / `MCPStdioConfig(protocol_mode=)` | `MCPServerConfig.ProtocolMode` / `MCPStdioConfig.ProtocolMode`: `mcp.ProtocolLegacy` (zero value), `mcp.ProtocolAuto` | `ProtocolAuto` probes `server/discover` and falls back to `initialize` like `negotiate_auto` |
| `MCPServerConfig(proxy=)` | `MCPServerConfig.Proxy` (HTTP(S) or SOCKS5 URL) | |
| `MCPServerConfig(verify=True)` | `MCPServerConfig.InsecureSkipVerify bool` | Inverted so the zero value is the safe one |
| `MCPServerConfig(description=)`, `MCPStdioServerConfig(description=)` | `MCPServerConfig.Description`, `MCPStdioConfig.Description` | Never reaches the wire; `Toolkit` reports it |
| `MCPToolkit` with its per-turn `Variable` config and discovery cache | `mcp.NewToolkit(config any, opts ...Option) (*Toolkit, error)`, `mcp.NewToolkitFunc(resolve mcp.ConfigResolver, opts ...Option) *Toolkit` | One function over the whole config instead of `Variable` fields; a changed config invalidates the cache, a failed discovery does not; a superseded client is closed after the next replacement |
| embedded resource in a tool result | `TextInput` (text) / `BinaryInput` (blob) | Previously a JSON dump |
| client GET stream and notifications | `mcp.WithNotifications(func(mcp.Notification))` | Re-entrant client calls from the callback deadlock (documented) |

### MCP server (D-109)

| Python | Go | Differences |
|---|---|---|
| `MCPServer(resources=, resource_templates=, prompts=)` | `mcpserver.WithResources(...)`, `WithResourceTemplates(...)`, `WithPrompts(...)` | Fields are fixed at registration (no per-request `Variable`); `Read` sees the request through `cx` |
| `SessionConfig` | `mcpserver.ConversationConfig{MaxConversations, TTL}` + `mcpserver.WithConversations(cc)` | A handle is the sole credential (no principal binding) |
| `TransportConfig(stateless=, json_response=, event_store=, ...)` | `mcpserver.TransportConfig{Stateful, StreamResponses, SessionIdleTimeout, MaxSessions, MaxRequestBodySize, EventStore, SSERetryInterval}` + `mcpserver.WithTransport(tr)` | **Default stays stateless + JSON** (Python: stateful + SSE); set `Stateful`/`StreamResponses` for Python's mode |
| `EventStore` | `mcpserver.EventStore`, `mcpserver.NewMemoryEventStore(maxPerStream)`, `(*MemoryEventStore).WithMaxStreams(n)`, `mcpserver.StreamForgetter` | The in-memory store is bounded (default 10,000 streams) and forgets a session's streams when it ends |
| `client_model=True` | `mcpserver.WithClientModel()` | Sampling on the client's model; without the capability the agent's own model is used |
| `elicitation_policy` | `mcpserver.WithElicitationPolicy(p)` | Back-channel only on a stateful SSE transport |
| `stream_progress=False` | `mcpserver.WithoutProgress()` | Progress/log go on the request's own stream |
| argument validation error | tool result `isError: true`, text `Input validation error: …` | Not a JSON-RPC error. A stdlib validator: `unevaluated*`, `$dynamicRef`, external `$ref`, non-RE2 patterns and the draft-07 array `items` are not enforced |

### Providers (D-110, D-111, D-112)

| Python | Go | Differences |
|---|---|---|
| `FileIdInput` on Chat Completions | `events.FileIdInput` → `{"type":"file","file":{"file_id"}}` (OpenAI dialect only) | Other dialects of the shared engine still refuse it; a file uploaded through another provider is rejected by the API, not pre-checked |
| `FileIdInput` on Anthropic | `image`/`document` block with `source.type = "file"` + automatic `files-api-2025-04-14` beta header (merged with yours) | An image is told by the file extension |
| prompt caching (`cache_control` injection) | `AnthropicConfig.PromptCaching` | Now also marks the last block of the last user turn; a caller-set `cache_control` there is kept (Python overwrites it, losing `ttl: "1h"`) |
| `output_config` (always sent) | `AnthropicConfig.NativeStructuredOutput bool` | **Opt-in** — older models answer 400 |
| `pause_turn` continuation | automatic, up to 5 hops | Usage is summed; `extra_body["messages"]` does not override the continuation; a paused answer without content is returned as is |
| server-tool blocks | `BuiltinTool*Event` call/result events; the block lives in `VendorMetadata["block"]` / `Result.Metadata["block"]` and is replayed | MCP connector blocks are mapped (Python drops them); `container_upload` is dropped |
| `OpenAIDecisionsConfig(question_instructions=[...])` (sequence form) | `OpenAIDecisionsConfig.QuestionInstructionParts []any` (a string, an `events.Input` or a ready map) | |
| `OpenAIShellCommandChunk` / `OpenAIShellOutputChunk` | `openai.ShellCommandChunk` / `openai.ShellOutputChunk` | Transient events; the `delta` of `shell_call_output_content` is an object |
| `OllamaConfig(streaming=True)` | `ollama.OllamaConfig{Streaming: true}` | NDJSON, `ModelMessageChunk`/`ModelReasoning`; tool calls and usage from the `done` chunk |
| `GeminiConfig(cached_content=, response_modalities=, image_config=, thinking_config=)` | `gemini.WithCachedContent(name)`, `WithResponseModalities(...)`, `WithImageConfig(ImageConfig)`, `WithThinkingConfig(map[string]any)` | Generated images → `ModelResponse.Files`; the raw thinking object overrides level/budget |
| input `vendor_metadata` (`media_resolution`, `video_metadata`, `display_name`) | the same keys on the input's vendor metadata | `video_metadata` takes `startOffset`/`endOffset`/`fps` |
| `ZAIConfig(thinking=, reasoning_effort=, sensitive_word_check=, watermark_enabled=, tool_stream=, extra=, disable_token_cache=)` | `ZAIConfig.Thinking *bool`, `ReasoningEffort`, `SensitiveWordCheck`, `WatermarkEnabled *bool`, `ToolStream`, `Extra`, `DisableTokenCache *bool` | `DisableTokenCache == nil` is Python's default (true: the raw key is the bearer); `false` signs a fresh HS256 token per request (`<id>.<secret>` key) |
| `x-source-channel` header | `ZAIConfig.SourceChannel` (default `zai.DefaultSourceChannel` = `"ago"`) | An `ExtraHeaders` entry of that name wins |
| `MistralConfig(guardrails=, prediction=, prompt_mode=, logprobs=)` | `MistralConfig.Guardrails []map[string]any`, `Prediction map[string]any`, `PromptMode string`, `Logprobs *bool` | An explicit `false` for `Logprobs` is sent |

### Tools and toolpacks (D-112)

| Python | Go | Differences |
|---|---|---|
| `FilesystemToolkit` `update_file` / `delete_file` / `read_file(raw=True)` | default members of `toolpacks.NewFilesystemToolkit`; `WithFilesystemReadOnly()` drops every mutating one | The default toolkit now has 7 members, not 5; `delete_file` never follows a symlink |
| Tavily tool parameters (topic, time range, dates, days, country, `auto_parameters`, …) | `toolpacks.WithTavilyTopic`, `WithTavilyTimeRange`, `WithTavilyStartDate`, `WithTavilyEndDate`, `WithTavilyDays`, `WithTavilyCountry`, `WithTavilyAutoParameters`, `WithTavilyIncludeFavicon`, `WithTavilyIncludeAnswerMode`, … | Depths `fast` and `ultra-fast` accepted |
| `Variable` kwargs of Tavily / DuckDuckGo | `toolpacks.WithTavilyVariable(param, v)`, `toolpacks.WithDDGVariable(param, v)` | Tavily binds any request parameter; DDG binds `max_results`, `region`, `safesearch`; a nil value drops the parameter |
| `SkillsToolkit(runtime, ...)` over several runtimes | `toolpacks.NewSkillsToolkitFromRuntime(rts ...SkillRuntime)` (variadic), `toolpacks.NewSkillsToolkitFromDirs(dirs ...string)` | The last runtime wins on a name clash; `NewLocalSkillRuntime(dir, opts...)` takes options |
| `run_skill_script` | `toolpacks.WithSkillScripts()`, `WithSkillScriptTimeout`, `WithSkillScriptMaxOutput`, `WithSkillScriptBlocked`, `WithSkillScriptSandbox` on a `LocalSkillRuntime` | **Opt-in** (Python always offers it): any `WithSkillScript*` enables it |
| `value \| Variable` kwargs of hosted tools | `tools.WithWebSearchVariable(param, v)`, `WithWebFetchVariable`, `WithFileSearchVariable`, `WithImageGenerationVariable`, `WithShellVariable`, `WithMCPServerVariable`, `WithRetrievalVariable`, `WithGoogleMapsVariable` | Bound by the schema's JSON field name, resolved per `Schemas` call; an unknown name or a mistyped value is a `Schemas` error; no `default_factory` |
| `GoogleMapsTool` | `tools.NewGoogleMaps(opts ...)`; `WithGoogleMapsLocation(lat, lng)`, `WithGoogleMapsLanguageCode(code)`, `WithGoogleMapsWidget(bool)` | Gemini sends `googleMaps` and, only with both coordinates, `toolConfig.retrievalConfig` |
| sandbox backend protocol (`tools/sandbox/`) | `toolpacks.Sandbox` (`Exec`, `PutFile`, `GetFile`, `Close`), `toolpacks.SandboxFactory`, `toolpacks.SingletonFactory`, `toolpacks.LocalSandbox`, `toolpacks.CodeEnvironment`, `toolpacks.CodeAdapter` | `GetFile` is beyond Python |
| shell and code tools running on a sandbox backend | `toolpacks.WithShellSandbox(factory)`, `toolpacks.NewSandboxCodeTool(env, opts...)`, `toolpacks.NewSandboxCodeToolFromFactory(factory, opts...)` | Local-only options are refused next to a backend; with a non-host factory the ignore guard is POSIX-lexical and touches no host files (D-115) |

### Contrib modules (D-113, D-114, D-115)

Three new optional modules, each with its own `go.mod` (`replace` to the root, stdlib only);
add them with `go get github.com/luckystrker/ago/contrib/<name>`.

| Python (`ag2/extensions/`) | Go | Differences |
|---|---|---|
| `ExaToolkit(api_key=)` | `searchtools.NewExaToolkit(apiKey, opts ...ExaOption)` (`EXA_API_KEY`) | 4 tools. Python's dead kwargs (`user_location`, `moderation`, `exa_find_similar(category=)`) are not exposed |
| `FirecrawlToolkit(api_key=)` | `searchtools.NewFirecrawlToolkit(apiKey, opts ...)` (`FIRECRAWL_API_KEY`) | 2 tools. **A 200 answer without `"success": true` is now an error** (`ErrAPIResponse`) |
| `GlasserToolkit(api_key=)` | `searchtools.NewGlasserToolkit(apiKey, opts ...)` | The key is required (pay-per-call); a relative `result_url` is accepted; `charge_usd` is a string |
| `KeenableSearchToolkit()` | `searchtools.NewKeenableSearchToolkit(apiKey, opts ...)` | Keyless by default (`/v1/search/public`) |
| `SerplySearchToolkit(api_key=)` | `searchtools.NewSerplySearchToolkit(apiKey, opts ...)` | Keeps the `ag2-serply-extension` User-Agent |
| `TinyFishSearchToolkit(api_key=)` | `searchtools.NewTinyFishSearchToolkit(apiKey, opts ...)` (`TINYFISH_API_KEY`) | |
| `XquikSearchToolkit(api_key=)` | `searchtools.NewXquikSearchToolkit(apiKey, opts ...)` | |
| `middleware=` / `Variable` kwargs / key from a variable | `With<Provider>Middleware(...)`, `With<Provider>Variable(param, variable)`, `With<Provider>APIKeyVariable(name)` | Defaults can be bound to conversation variables |
| SDK retry settings | `searchtools.WithTinyFishMaxRetries(n)` = **retries** (1 + n attempts, default 2); `searchtools.WithFirecrawlMaxRetries(n)` = **total attempts** (default 3) | Parity with each SDK, but the same word means two things. TinyFish retries 408/429/5xx and network errors; Firecrawl only 502 and network errors; **a call can now take longer** because of retries and the 600 s default timeouts (Exa, TinyFish; Firecrawl capped at 600 s — was 60 s). A timed-out attempt is not retried. Firecrawl `scrape` also resumes a `408 SCRAPE_TIMEOUT` with `processing_continues` up to 5 times |
| an empty query to Firecrawl/TinyFish | `searchtools.ErrRejected` before any request | Was a request to the API |
| `httpx.HTTPStatusError` | `*searchtools.HTTPError` (`errors.Is` with `ErrHTTPStatus`; also `ErrAPIResponse`, `ErrMissingKey`) | Carries provider, method, URL, status, body, provider message |
| `DockerSandbox` (`extensions/docker`) | `sandboxdocker.NewEnvironment(opts ...)` → `toolpacks.SandboxFactory`; `sandboxdocker.NewSandbox(cfg)`; options `WithHost`, `WithImage`, `WithEnvVars`, `WithTimeout`, `WithNetworkMode`, `WithMemLimit`, `WithCPUQuota`, `WithUser`, `WithAutoRemove`, `WithHostPath`, `WithWorkdir`, `WithMaxOutput`, `WithImageVariable`, `WithEnvVarsVariable`, `WithNetworkModeVariable` | Docker Engine API over the unix socket (`DOCKER_HOST`) or plain `tcp://` — no TLS/ssh; defaults network `none`, memory `512m`; output capped (default 100k characters); a `..` escape is refused |
| `DaytonaSandbox` (`extensions/daytona`) | `sandboxdaytona.NewEnvironment(opts ...)` → `toolpacks.SandboxFactory`; `sandboxdaytona.NewSandbox(cfg)`; options `WithAPIKey`, `WithAPIURL`, `WithTarget`, `WithSnapshot`, `WithImage`, `WithEnvVars`, `WithResources(Resources)`, `WithTimeout`, `WithCreateTimeout`, `WithWorkdir`, `WithMaxOutput`, `WithPollInterval`, `WithHTTPClient`, `WithVariable(param, v)` | Key/URL from options or `DAYTONA_API_KEY`/`DAYTONA_API_URL`/`DAYTONA_TARGET`; sends `cwd` and per-call `envs` (Python sends neither); toolbox paths unverified against a live account |
| `extensions/tenki`, NLIP, mi4afa, `compaction_verifier`, `tealtiger` | — | Not ported; the owner has not chosen them |

### Protocols and network (D-116)

| Python | Go | Differences |
|---|---|---|
| `list_tasks(config, context_id=, status=, page_size=, page_token=, history_length=)` → `ListedTasks` | `Client.TasksList(ctx, a2a.ListTasksParams{...}) (*ListTasksResult, error)` | JSON-RPC `tasks/list` or REST `GET /tasks`; the page token is an offset; `tasks`/`nextPageToken`/`pageSize`/`totalSize` |
| `A2AServer(extended_card=, extended_card_modifier=)` | `ServerConfig.ExtendedCard`, `ServerConfig.ExtendedCardModifier(func(r *http.Request, card *AgentCard) (*AgentCard, error))` | The modifier receives the request (like `CardModifier`); JSON-RPC `agent/getAuthenticatedExtendedCard` + `GET /extendedAgentCard`; `Client.ExtendedCard(ctx)` |
| `A2AServer(validate_input_modes=True)` | `ServerConfig.ValidateInputModes` + `ServerConfig.InputModesError()` | `NewServer` returns no error, so the card check is a separate method; media-type refusal on every message surface |
| `A2ATaskAuthRequiredError` / `A2ATaskRejectedError` | `a2a.ErrTaskAuthRequired` / `a2a.ErrTaskRejected` (wrapped in the handler's error) | `auth-required` (non-terminal) and `rejected` (terminal); the answer is the Task record |
| `guard.serve(agent, reader, writer)` / `guard_router` | built into `acp.Serve` (`acp/guard.go`) | No separate wrapper: a `_meta` collision → `-32602`, a notification is dropped |
| `AuthMethodAgent` / `EnvVarAuthMethod` / `TerminalAuthMethod` | `acp.NewAgentAuthMethod` / `NewEnvVarAuthMethod` / `NewTerminalAuthMethod`, `acp.DeclaredAuth` | env-var/terminal validation is the application's (`Validate`); `StaticTokenAuth` is the `agent` kind |
| `encrypted_signature_of` / `signature_event` / `restore_tool_call` | `agui.EncryptedSignatureOf` / `agui.SignatureEvent` / `agui.RestoreToolCall` | The signature lives in `ToolCallEvent.VendorMetadata[agui.ThoughtSignatureKey]` |
| `is_same_provider` / `provider_of` | internal to `agui` (`agui/provider.go`) | The vendor tag (`google`) and the API name (`gemini`/`vertexai`) count as the same provider |
| `STTTranslationConfig(model, api_key=)` | `live.OpenAITranslator{APIKey, Model, BaseURL, Client, PCMSampleRate, PCMChannels}` | `POST /audio/translations`, `response_format=text`; implements `Transcriber` |
| `BudgetThresholds(max_tokens_per_task=, max_seconds_per_task=)` | `eval.BudgetThresholds{MaxTokensPerTask, MaxSecondsPerTask}` + `eval.WithBudgets` | Observational: `RunResult.BudgetViolations`; cases are not aborted |
| `Suite(name=, source=)`, `Task(tags=, metadata=)` | `eval.WithSuiteName` / `WithSuiteSource`, `eval.Case.Tags` / `Metadata` | `Suite` is a slice of cases, so name/source travel as run options into `RunResult`; tags reach `CaseResult.Tags` |
| `agent_judge(retries=, response_schema=Verdict)` | `eval.LLMJudge{StructuredOutput: true, Retries: n}` | Both opt-in (zero value = the old single plain call) |
| `pairwise_judge(swap=False)` | `eval.Pairwise{SingleOrder: true}` | One judge call, no swap and no position-bias guard |
| `parse_hub_urn(s)` | `network.ParseHubURN(s) (hubID, agentID string)`, `network.HubURN(hubID, agentID)` | A malformed URN → `("", s)` |
| `AccessBlock.channel_types` | `network.Rule.ChannelTypes` (`Initiate`/`Accept`) | Enforced at `AuthorizeChannelOpen` (Python only declares it); empty = any type |
| `LimitsBlock.max_concurrent_tasks` | `network.Rule.MaxConcurrentTasks` | Enforced on the first `ObserveTask` (`ErrAccessDenied`) |
| `ContextEquals(key=, value=)` | `network.ContextEquals{Key, Value}` | Deep comparison; a missing key = nil |
| `Transition(priority=)`, `TransitionGraph(default_target=)` | `network.Transition.Priority`, `network.TransitionGraph.DefaultTarget` | Lower priority first, ties in list order; a nil default → the old `no_transition` |
| `peers(action="find", sort_by="name"\|"cost"\|"track_record")` | the same tool, `sort_by` argument | Peer summaries now carry `cost` and `observed_success_rate`; without `sort_by`, the hub order |

## 10. Gap-audit wave 4 (2026-11-11)

Python → AGo mappings for the API added in wave 4 (D-117..D-123). Everything is
additive unless a row says otherwise: existing signatures did not change, a hub
built without a store behaves as before, and a streaming server without
`ServerConfig.HandleStream` keeps the two-event `message/stream`. The invariants
behind the `network` changes are in `docs/acceptance/wave4-invariants.md`.

### Network: persistence and the idempotent post (D-118)

| Python | Go | Differences |
|---|---|---|
| `Hub.hydrate()` / `Hub.open(...)` on an existing store | `network.OpenHub(network.WithHubStore(store))` | `OpenHub` now calls `Hydrate()` (registry, then WAL). **It can return an error on a corrupted store where it used to start** — that is the point; previously only channels were restored, so a reconnecting client could register a second passport under a name already on disk |
| `Hub(store=...)` and `store_wal` | `network.WithHubStore(knowledge.Store)`, `network.NewStoreWAL(store)` | Python's per-entity layout: `/agents/<id>/{passport,rule,resume}.json`, `inbox.cursors.json`, `inbox_nacks.jsonl`, `/registry/by_capability.json`, `/channels/<id>/{metadata.json,wal.jsonl}`, `/tasks/<id>/checkpoint.json`. The passport file is the commit point; a directory without one is deleted on hydrate (Python leaves it) |
| `inbox_cursor(agent, channel)` / `_advance_cursor` | `Hub.InboxCursor(agentID, channelID)`, `Hub.AdvanceInboxCursor(agentID, channelID, envelopeID)` | Forward-only, per channel; the store is written before memory (Python: memory first). No envelope-existence check here — the wire does that (D-119) |
| `inbox_nacks.jsonl` | `Hub.RecordNack(...)`, `Hub.Nacks(agentID) []Nack` | A nack never moves the cursor |
| `find_envelope_by_causation(channel, sender, causation_id)` | `Hub.Send` now dedupes on the same key; the lookup still exists | A retried post returns the accepted envelope and appends/dispatches nothing; the index is rebuilt from the WAL by `Replay`/`Hydrate` and pruned when the channel ends |
| `checkpoint.json` + `checkpoint_writers.json` | `/tasks/<id>/checkpoint.json` | State and claimed writer share one file |

### Network: the wire endpoint and remote mode (D-119)

| Python | Go | Differences |
|---|---|---|
| `Hub.attach_endpoint(addr, link)` / `LinkEndpoint` | `Hub.ServeLink(ctx, link transport.Link)` | No address argument: one call serves one connection and the endpoint id is minted by the hub. `Frame.From`/`Frame.To` are never read, so a frame cannot act as another connection's agent |
| `LocalLink(hub).client()` | `transport.NewMemListener(n)` + `Hub.Serve(ctx, l)`, client through `ListenerDialer(l)` | `transport.NewPipe(n)` gives an unaddressed pair for tests. The old address-addressed `LocalLink` stays for in-process use |
| `HubClient(WsLink(url))` (cross-process) | `ConnectHub(ctx, network.RemoteConfig{Dial, RPCTimeout})`, then `Attach`/`Detach`/`Reconnect`/`Close` | `Dialer` opens one fresh link per call; `RPCTimeout` defaults to 45 s (a `create_channel` request spans the invite ack). `HubClient.Hub()` is nil remotely |
| `HelloFrame.since_envelope_id` | `network.HelloBody.Since` (per channel) + `HelloBody.SinceEnvelopeID` (legacy) | `null` skips replay, `""` replays everything; the per-channel map wins when both are sent |
| `ResponseFrame.ok` / `error_code` | `Frame.Code == ""` = success; `Code`/`Message` = error | Codes are strings (`name_taken`, `channel_closed`, `access_denied`, …) so `errors.Is` works across the link |
| `add_resume_example` (get+set in Python) | the `add_resume_example` op, `AgentClient.AddExample` | Atomic: 24 concurrent calls keep all 24 examples. No Python op of that name; `Hub.AddResumeExample` is the hub-side method |
| `get_skill` / `set_skill` | answered by the wire as `null` / `protocol_error` | AGo has no SKILL.md registry |
| remote `remote_agent` Hello | refused (`auth_failed`/protocol error) | The owner-tracking that makes a re-attach safe is not ported |

### A2A: history, tenants, JWS and streaming (D-120..D-123)

| Python | Go | Differences |
|---|---|---|
| `build_user_message(history_events=)`, `build_tool_result_message(history_events=)`, `events_to_payload`, `payload_to_events` | `a2a.WithHistory(events)`, `a2a.AttachHistory`, `a2a.ParseHistory`, `a2a.EventsToPayload`/`PayloadToEvents`, `ServerConfig.RestoreHistory` | `application/vnd.ag2.history+json`; all seven event kinds and five input-part types; restore runs once per turn on every binding; a malformed part is `-32602`. Non-text tool results are JSON, a failed result inside a batch keeps its error text in the content, and an empty `arguments` comes back as `"{}"` |
| `tenant=` / `_maybe_tenant` / `a2a:tenant` in the context | `a2a.WithTenant(name)` on the client; `ServerConfig.Tenant`, `ServerConfig.StoreForTenant`, `ServerConfig.TenantResolver`, `ServerConfig.AuthorizeIdentity`, `a2a.IdentityFrom(ctx)` on the server | JSON-RPC `params.tenant` and the REST prefix `/{tenant}/message:send`, `/{tenant}/tasks[/{id}[:cancel]]`; `//tasks` and `/acme/` are not tenant paths. An unknown tenant (the store resolver returns nil) is 400/`-32602`; a resolver refusal is 403 (REST) or `-32602` (JSON-RPC/SSE). The resolver sees `TenantRequest{Request, Tenant, Identity}` and runs even when the client named no tenant; `Authorize` runs before `AuthorizeIdentity` and both must pass |
| `sign_card` / `CardSigner` / `card_signature_verifier` / `A2ACardSignatureError` | `a2a.SignCardJWS`, `a2a.NewCardSigner`/`CardSigner`, `a2a.NewCardSignatureVerifier`/`WithCardVerifier`, `a2a.NewServerChecked`, `ErrCardSignature`, `ErrStaleCardSignature` | Detached JWS, ES256/384/512, RS*, PS*, EdDSA, HS*; payload = the a2a-sdk 1.3.0 proto-JSON form (defaults dropped, `signatures` removed, `_clean_empty`, RFC 8785, depth 128, integers ±(2^53−1)). A stale signature is compared on the payload; `NewServer` answers 500 on the card routes, `NewServerChecked` refuses at construction |
| `message/stream` client consumption; `tasks/resubscribe`; `client.subscribe` | `Client.MessageStream(ctx, msg) (*TaskStream, error)`, `Client.Resubscribe`, `WithMaxReconnects`, `WithReconnectBackoff`, `WithPollingInterval`, `WithStreaming(false)`, `ErrReconnect` | JSON-RPC SSE and REST `POST /message:stream`; resubscribe opens with the current task and closes at the terminal state; a failed open is retried inside the budget; polling fallback on a clean EOF, `ErrUnsupportedOperation` and `ErrTaskNotFound` |
| server artifact streaming (`chunk_to_text_artifact`) | `ServerConfig.HandleStream` + `StreamEmitter.Artifact`/`Status`, `a2a.ApplyArtifactUpdate`, `KindArtifactUpdate`, `ErrStreamClosed` | Each chunk folds into the task record, so `tasks/get` agrees with the stream. `Status` is progress-only; terminal/interrupted states come from the handler's return value. A subscriber more than 32 events behind is disconnected, not blocked |

### Network: offline delivery and reconnect (D-124)

| Python | AGo | Differences |
|---|---|---|
| `_replay_for_recipient` (active channels, after `max(cursor, since_envelope_id)`) | replay inside the per-(connection, agent) sender (`network/delivery.go`) | Per-channel since marks; a Hello without marks replays from the cursor; ended channels replay too |
| `ReceiptFrame` ack / nack | the remote client acks every delivered envelope | A nack stops that channel until reconnect; the client re-acks its marks after reconnect |
| `ws.py` reconnect | `RemoteConfig.Reconnect` (`ReconnectPolicy`); `contrib/wslink` `Dialer` / `RemoteConfig` / `HubHandler` | Backoff and attempt limit; `HubClient.Close` stops every pump |

### Card wire and semantics changes to check before upgrading

- **The served agent card.** An unset `streaming: false` is no longer written, so a zero
  card now serializes as `{"pushNotifications":false}` where it used to carry both
  keys; a client that reads an absent key as `false` sees no difference. A card with
  `PushNotifications: true` and no `PushStore` is now served with `false` (the server
  always declares the flag, `server.py:218`), and `extendedAgentCard` is declared only
  when a second card exists. Presence is state: `reflect.DeepEqual` between a decoded
  and a literal card now distinguishes `false`-present from unset — compare the
  marshalled JSON, or use `SetStreaming`/`SetPushNotifications`/`SetExtendedAgentCard`
  to state intent.
- **`OpenHub` may fail.** A corrupted store is reported instead of silently producing a
  half-restored hub (see the network table above). A hub without a store is unaffected.
- **Remote mode is not federation.** `ServeLink`/`ConnectHub` connect one client to one
  hub over a `transport.Link`; `RemoteAgentProxy` is still a stub. `ClientTools` and
  `AgentClient.ChannelTools` work remotely, but `ChannelTools` rebuilds the channel state
  with the built-in adapters only — a custom hub-side adapter gives an empty tool set on
  the client.

### Warnings — read before exposing a hub or a tenant server

- **Tenants are a client's claim unless you resolve them.** A server with no
  `TenantResolver` and no `StoreForTenant` is single-tenant; a server with only
  `StoreForTenant` serves whatever tenant the caller asks for. The tenant is *not*
  derived from the caller. Configure `TenantResolver` and, to bind the tenant to the
  caller, `AuthorizeIdentity`; the same warning is on the fields and on `WithTenant`.
- **`NoAuth` agents are taken over by name on the wire.** A Hello for an unauthenticated
  agent displaces the current holder (Python parity). Any deployment that serves
  `Hub.ServeLink` outside a trusted boundary must register agents with `ApiKeyAuth`/an
  `AuthAdapter` — the boundary no longer has to invent a "link → allowed AgentID" map
  (that is now built in), but it must not run unauthenticated agents.
