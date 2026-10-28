# AG2 Spec: Core agent & chat execution model (Python reference)

> Recon report from the AG2 Python clone (commit fa4e0fb, 2026-10-27). This is the behavioral
> reference for the AGo port. Package root in the Python clone: `C:\projects\ago2\ag2\ag2`.

## 0. Classic API status: REMOVED

No `ConversableAgent`, `GroupChat`, `GroupChatManager`, `register_reply`, `initiate_chat`,
`generate_reply` anywhere. Replacements:

| Classic AutoGen | New AG2 (2026) equivalent |
|---|---|
| `ConversableAgent` | `Agent` (`ag2\agent.py:680`, `Generic[TResult]`, default `str`) |
| `initiate_chat` | `Agent.ask()` (blocking) / `Agent.run()` (observable) |
| reply-function chain | middleware (`on_turn` / `on_llm_call` / `on_tool_execution` / `on_human_input`) |
| GroupChat/Manager | `ag2\network` (Channel, Hub, TransitionGraph, RoundRobin/Terminate targets) + `Agent.as_tool()` |
| `human_input_mode` | `hitl_hook: HumanHook` (`ag2\hitl.py`) |
| `update_system_message` | `Agent.prompt()` decorator (dynamic prompt hooks) / static prompt fragments |
| `register_hook` etc. | `observer()` decorator / `tools=` + `Tool.register()` / `Plugin` |

## 1. Primary user-facing API

Public surface (`ag2\__init__.py`): `Agent, AgentReply, AgentRun, AgentSpec, Task, TaskConfig,
TaskInject, TaskSpec, Context, Inject, Variable, Depends, Middleware, Plugin, observer,
ResponseSchema, PromptedSchema, response_schema, MemoryStream, Toolkit, ToolResult, tool,
TextInput/ImageInput/AudioInput/VideoInput/DocumentInput/DataInput/BinaryInput, FilesAPI,
KnowledgeConfig`.

```python
Agent(name, prompt: str | Iterable[str | async-hook], *,
      config: ModelConfig | None, hitl_hook: HumanHook | None,
      tools: Iterable[Callable | Tool], middleware: Iterable[MiddlewareFactory],
      observers: Iterable[Observer], dependencies: dict, variables: dict,
      response_schema: type | ResponseProto | None,
      plugins: Iterable[Plugin], knowledge: KnowledgeConfig | None,
      tasks: TaskConfig | Literal[False] = False,
      assembly: Iterable[AssemblyPolicy] = ())
```

A bare `Agent(name, config=cfg)` is a plain LLM loop; all harness features are opt-in.

Key methods (`ag2\agent.py`):
- `async ask(*msg, stream=None, dependencies, variables, prompt, config, tools, plugins, middleware, observers, response_schema, hitl_hook) -> AgentReply` (line 1060)
- `run(*msg, ...) -> AgentRun` (line 1163): async CM — `async with agent.run("hi") as run: reply = await run.result()`; also `run.start()`, `run.stream` (event bus), `run.enqueue(*msg)` (mid-turn inbox).
- `AgentReply` (line 132): `.body -> str | None`, `await .content(retries=0) -> TResult` (schema-validated with retry-by-re-asking loop), `.files -> list[BinaryResult]`, `.history -> History`, `await .usage() -> UsageReport`, continuation `.ask(...)` / `.run(...)`.
- `resume(*events, ...) -> AgentReply` (line 1275): replay trajectory — all but last event seed history, last event is re-entry trigger.
- `as_tool(*, description, name=None, stream=None, middleware=()) -> SubagentTool` (line 1730).
- `task(title, *, description, payload, capability, ttl_seconds, context, checkpoint_store, resume_from) -> Task` (line 939).

`ag2\testing.py` provides `TestConfig`/`TrackingConfig` fake clients.

## 2. Core types

### Event base — `ag2\events\base.py`
`BaseEvent` (metaclass `_ConditionMeta`, `Field()` descriptors): field `created_at: float` (auto
Unix timestamp, compare=False); class flags `__transient__: bool` (do not persist — streaming
chunks, lifecycle bookkeeping) and `__conversational__: bool` (default True; False for
`UsageEvent`). Methods `to_dict()`/`from_dict()`. Condition DSL (`events\conditions.py`):
`ModelResponse == ...`, `HumanMessage.parent_id == request.id`, `StreamA | StreamB`, `~Cond`.

### Input events — `ag2\events\input_events.py`
- `Input(BaseEvent)` — `metadata: dict[str, Any]`; static `ensure_input(content)`: str→TextInput, else DataInput.
- `ModelRequest(Input)` — `parts: list[Input]`; `ensure_request(msgs)`.
- `DrainedModelRequest(ModelRequest)` — marker emitted after inbox drain; the LLM-trigger subscriber skips it (prevents recursive LLM call).
- `MessageEnqueued` (transient) — announces inbox append.
- `TextInput(Input)` — `content: str`.
- `DataInput(Input)` — `data: SendableMessage`.
- `BinaryInput(Input)` — `data: bytes`, `media_type`, `vendor_metadata: dict`, `kind: BinaryType`.
- `FileIdInput(Input)` — `file_id: str`, `filename: str|None`.
- `UrlInput(Input)` — `url: str`, `kind: BinaryType`.
- `enum BinaryType(str)`: BINARY, IMAGE, AUDIO, DOCUMENT, VIDEO.
- Factories `ImageInput/DocumentInput/AudioInput/VideoInput` — from (url | file_id | data+media_type | path), extension→media-type inference.

### Model / human events — `ag2\events\types.py`
- `Usage` (frozen dataclass): `prompt_tokens, completion_tokens, total_tokens, cache_read_input_tokens, cache_creation_input_tokens, thinking_tokens` — all `float|None`; field-wise `+` (None+None stays None).
- `BinaryResult` (frozen dataclass): `data: bytes`, `metadata: dict` (`.name` from `metadata["filename"]`).
- `ModelMessage(ModelEvent)` (transient): `content: str`, `metadata: dict`.
- `ModelMessageChunk(ModelEvent)` (transient): `content: str`. `ModelReasoning(ModelEvent)` (transient): `content: str`.
- `ModelResponse(ModelEvent)` — the turn's final answer: `message: ModelMessage|None`, `tool_calls: ToolCallsEvent`, `usage: Usage`, `response_force: bool` (True = a `final=True` tool forced this response, ends the loop), `files: list[BinaryResult]`, tracing `model/provider/finish_reason/response_id: str|None`; properties `content`, `metadata`; `to_api()`.
- `UsageEvent(BaseEvent)` (`__conversational__=False`): `usage: Usage`, `kind: str` (`"model_call"|"subtask"|"compaction"|"aggregation"`), `model/provider/finish_reason/label: str|None`, `parts: list[UsageEvent]`.
- `HumanInputRequest(BaseEvent)`: `id: str (uuid)`, `content: str`, `timeout: float|None`, `task_id: str|None`.
- `ToolApprovalRequest(HumanInputRequest)`: `+ tool_call_id: str`.
- `HumanMessage(BaseEvent)`: `parent_id: str`, `content: str`.

### Tool events — `ag2\events\tool_events.py`
- `ToolResult` (dataclass): `parts: list[Input]`, `final: bool`, `metadata: dict`; ctor takes `*inputs`.
- `ToolCallsEvent(BaseEvent)`: `calls: list[ToolCallEvent]`.
- `ToolResultsEvent(BaseEvent)`: `results: list[ToolResultEvent]` — drives the next model call.
- `ToolCallEvent(ToolEvent)`: `id: str (uuid)`, `name: str`, `arguments: str` (JSON), `vendor_metadata: dict`, `source: str|None`.
- `ClientToolCallEvent(ToolEvent)` (id/name/arguments) — delegated to the client application.
- `BuiltinToolCallEvent` / `BuiltinToolResultEvent` — provider-hosted tools.
- `ToolResultEvent(ToolEvent)`: `parent_id: str`, `name: str|None`, `result: ToolResult`.
- `ToolErrorEvent(ToolResultEvent)`: `+ error: Exception`. `ToolNotFoundEvent(ToolErrorEvent)`.

### Lifecycle / task / alert events
`events\lifecycle.py` (all transient): `ObserverStarted/Completed(name)`, `CompactionStarted/Completed/Failed`, `AggregationStarted/Completed/Failed`, `EventLogFailed`, `UnknownEvent`.
`events\task_events.py`: `TaskStarted(task_id, agent_name, objective, spec, expires_at)`, `TaskProgress`, `TaskCompleted(..., result, task_stream)`, `TaskFailed(..., error)`, `TaskExpired`, `TaskCancelled(..., reason)`.
`events\alert.py`: `enum Severity(str)`; `ObserverAlert(BaseEvent, severity)`; `HaltEvent(BaseEvent, reason)`.

### Stream & history
- `Stream` Protocol (`ag2\context.py:33`): `id: UUID`, `history: History`, `pending_messages: list[ModelRequest]`, `send(event, context)`, `enqueue(*content)`, `spawn_background(coro)`, `where(condition) -> Stream`, `join(max_events=None)`, `subscribe(func, *, interrupt=False, sync_to_thread=True, timeout=None, condition=None) -> SubId`, `unsubscribe`, `sub_scope(...)`, `get(condition) -> Future[BaseEvent]`.
- `MemoryStream` (`ag2\stream.py:129`): in-memory pub/sub; `send()` runs `interrupt` subscribers first (a subscriber returning falsy stops delivery), then normal subscribers in registration order; persistence via `Storage` Protocol (`save_event/get_history/set_history/drop_history`, `ag2\history.py:21`) — non-transient events only unless `persist_all=True`. `SubStream` = filtered view sharing parent's id/history. Redis alternative in `ag2\streams\redis\`.
- `History` (`ag2\history.py:48`): `stream_id`, `storage`; `get_events()`, `replace(events)`. Helpers: `unanswered_tool_calls(events)`, `close_unanswered_tool_calls(history, result=...)` (repairs mid-flight-killed transcripts with sentinel `HUMAN_INPUT_ABANDONED_TOOL_RESULT`).

### Context — `ag2\context.py:130`
`ConversationContext` (dataclass, aliased `Context`): `stream: Stream`, `dependency_provider: Provider|None`, `variables: dict[str, Any]` (reserved prefixes `"ag:"`, `"a2a:"` stripped on the wire), `dependencies: dict[Any, Any]` (DI: agent instance, model config, KnowledgeStore, active Task), `prompt: list[str]` (system-prompt fragments), `tool_call_id: str|None`. Methods: `pending_messages` view, `enqueue(*content)` (append + announce `MessageEnqueued`, thread-safe), `spawn_background(coro)`, `async input(message, timeout) -> str` / `async ask(HumanInputRequest) -> str` (HITL; failures normalize to `HumanInputError` subclasses), `async send(event)`.

### Run / result / usage
- `AgentRun` (`ag2\agent.py:473`): `stream`, `enqueue()`, `start()`, `async result() -> AgentReply` (idempotent, re-raises same failure); turn scope dies with the `async with` block.
- `AgentReply` (`ag2\agent.py:132`): `response: ModelResponse`; `body`, `content(retries)`, `files`, `history`, `usage()`, `ask()`, `run()`.
- `UsageReport` / `UsageRecord` (`ag2\usage.py`): `total: Usage`, `records: tuple[UsageRecord(usage, kind, model, provider, finish_reason, label)]`, `by_model / by_provider / by_kind`; built solely from `UsageEvent`s (`from_events`).

### Task — `ag2\task.py`
`TaskState` enum (`created/running/completed/failed/expired/cancelled`), `TERMINAL_TASK_STATES`; `TaskSpec(title, description="", payload={}, capability=None)`; `TaskMetadata(task_id, owner_id, spec, state, created_at, started_at, completed_at, expires_at, last_progress_at, progress: dict, result, error, channel_id)` with `to_dict`/`from_dict`; `Task` async CM (`progress()`, `complete(result)`, `fail(error)`, `expire()`, `cancel(reason)`, `checkpoint(state)`, `resumed_state`) auto-completes on clean exit / auto-fails on exception; `CheckpointStore` Protocol (`write(task_id, state)`, `read(task_id)`); `TaskInject = Annotated[Task | None, Inject("ag2.task")]`.

### Response schemas — `ag2\response\`
`ResponseProto[T]` protocol (`proto.py`): `name`, `description`, `json_schema`, `system_prompt`, `async validate(response, context, provider) -> T`. `ResponseSchema[T]` (`schema.py`) wraps pydantic `TypeAdapter`; scalars embedded into `{"data": ...}`. `RawSchema` = raw JSON schema. `PromptedSchema` (`prompted.py`), `response_schema` decorator (`callable.py`), `DecisionSpec/Question/DecisionOption` (`decision.py`). `AgentSpec` (`ag2\spec.py:87`): `name`, `prompt: list[str]`, `tool_names: list[str]`, `response_schema: ResponseSchemaSpec|None`; `from_agent()` / `to_agent(...)`.

### Config-related value types
`TaskConfig` (`ag2\agent.py:106`): `config`, `prompt="You are a task agent..."`, `include_tools`, `exclude_tools=()`, `extra_tools=()`, `max_concurrency`. `SendableMessage` (`ag2\types.py:105`): JSON-decodable | dataclass | dict/list thereof | Decimal | datetime. `MediaType` literal unions per modality (`ag2\types.py`).

### Exceptions — `ag2\exceptions.py`
`AG2Error` base; `ConfigNotProvidedError`, `ToolConflictError`, `ToolResolutionError`, `ToolNotFoundError`, `UnsupportedToolError`, `UnsupportedInputError`; `HumanInputError` base with `HumanInputNotProvidedError` / `HumanInputFailedError(cause)` / `HumanInputTimeoutError(timeout)`; skill-loading errors.

## 3. Control flow: from `ask`/`run` to final result

1. **Entry** — `ask` is the degenerate `run`: both funnel through `_open_run` (`agent.py:1200`) → build a `Context` over a stream (default fresh `MemoryStream`, per-call `stream=` reuses state) → `_make_run` (`agent.py:1238`) returns `AgentRun`.
2. **Scope open** (`AgentRun.__aenter__` → `Agent._turn_scope`, `agent.py:1415`): acquire the **per-stream turn lock** (keyed by `stream.id`, weak registry — serializes concurrent asks on one stream); apply per-invocation plugin defaults (dependencies/variables, rolled back on exit); `_prepare_turn` (`agent.py:1352`) resolves `ModelConfig` (raises `ConfigNotProvidedError`), stamps it into dependencies, seeds `context.prompt` from static system-prompt fragments + dynamic prompt hooks; plugin prompt fragments appended with identity-preserving markers and removed on exit (ADR 0022).
3. **Drive scope** (`_drive_scope`, `agent.py:1474`): knowledge-context bootstrap/event-log write; resolve the tool set (`self.tools` + auto-injected `_additional_tools` + call-time tools, through `resolve_tools` precedence); instantiate the **middleware chain** (per turn; folded so `agent_turn = mw.on_turn∘...∘_execute_turn` and `llm_call = mw.on_llm_call∘...∘client(...)`); register stream subscribers: (a) `_call_client` on `ModelRequest | ToolResultsEvent` (the LLM trigger), (b) the **HITL hook** on `HumanInputRequest` with `interrupt=True` (or `default_hitl_hook` → `HumanInputNotProvidedError`), (c) the `ToolExecutor` on `ToolCallsEvent`, (d) observer lifecycle bracketing.
4. **The loop** (`_execute_turn`, `agent.py:1807`): drain stream inbox into the initial `ModelRequest`; then `await stream.get(ModelResponse)` while `context.send(event)` fires `_call_client` → drain pending again (re-emitted as `DrainedModelRequest`), read `history.get_events()`, run middleware-wrapped LLM call, emit `UsageEvent` then `ModelResponse`. While `message.tool_calls and not message.response_force`: send `ToolCallsEvent` → `ToolExecutor` (`ag2\tools\executor.py`) runs all calls in parallel (`asyncio.gather`, cancels siblings on `HumanInputError`), emits `ToolResultsEvent` (or, for a `final=True` tool, a forced `ModelResponse(response_force=True)` directly) → `_call_client` re-fires → next `ModelResponse`. Finally, drain any leftover enqueued input into one extra model call, then return the `ModelResponse`.
5. **Termination**: no max-turn counter in the core loop — the turn ends when the model stops requesting tools (or a `final=True` tool forces a response, or `_HaltCheckMiddleware` returns a synthetic `"HALTED: <reason>"` response after an `AlertPolicy`-emitted `HaltEvent`). `max_turns` exists only in `ag2\network\transitions.py`. `HumanInputError` escaping the turn triggers `close_unanswered_tool_calls` history repair, then propagates.
6. **Result**: final `ModelResponse` wrapped in `AgentReply`. Continuation via `reply.ask(...)`/`reply.run(...)` (same context/stream/client).
7. **Extended flows**: compaction (`_CompactionMiddleware` + `CompactTrigger`/`CompactStrategy` in `ag2\compact.py`) and aggregation (`ag2\aggregate.py`) fire post-turn when thresholds are exceeded; assembly policies (`AssemblyPolicy` in `ag2\assembly.py`; impls in `ag2\policies\`) transform `(prompts, events)` before each LLM call, outermost in the chain; subtasks (`tasks=TaskConfig`) inject `run_subtask`/`run_subtasks` tools spawning bare child `Agent`s (no recursion by construction); multi-agent routing lives in `ag2\network`.

## 4. Essential vs optional

**Essential**: `Agent` + `AgentRun`/`AgentReply`; event model (`ag2\events\`); `Stream`/`MemoryStream`/`SubStream` + `History`/`Storage`; `ConversationContext` + Stream protocol; annotations `Context`/`Inject`/`Variable`; middleware (`BaseMiddleware.on_turn/on_llm_call/on_tool_execution/on_human_input`); `ModelConfig`/`LLMClient` protocols; `Tool`/`@tool`/`Toolkit`/`ToolExecutor`; HITL; exceptions; the stream turn lock + inbox drain semantics.

**Optional/extended**: Task lifecycle and subtasks; knowledge store + compaction/aggregation; assembly policies; built-in middleware (history/token limiters, retry, logging, metrics, telemetry); observers (LoopDetector, TokenMonitor); Plugins; `AgentSpec` serialization; usage reporting; Redis streams; multi-agent network/hub; protocol servers/clients (`ag2\a2a`, `acp`, `mcp`, `ag_ui`, `live`, `a2ui`, `mcp_ui`); files API, eval, textual/watch/voice.

## 5. Tests (Python reference)

`C:\projects\ago2\ag2\test\`: `test\agent\` (test_run.py — the Agent.run contract, test_hitl.py, test_context_variables.py, test_tools_di.py, test_hooks_di.py, test_observer.py, test_response_schema.py, test_concurrent_asks.py, test_input.py, test_sysprompt.py, test_usage.py), `test\events\`, `test\stream\`, `test\middleware\`, plus `test_task.py`, `test_history.py`, `test_aggregate.py`, `test_compact.py`, `test_assembly.py`, `test_alert_halt.py`, `test_agent_composition.py`, `test_agent_resume.py`, `test_usage.py`.

Supporting references: ADRs in `docs\adr\` (0008, 0009, 0022, 0014, 0026) and `docs\context\agent-invocation.md`.
