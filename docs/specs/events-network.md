# AG2 Spec: Events, middleware, policies, network, integration protocols (Python reference)

> Recon report from the AG2 Python clone (commit fa4e0fb, 2026-10-27). Package root: `C:\projects\ago2\ag2\ag2`. Tests: `C:\projects\ago2\ag2\test`.

## 1. Event Model (`ag2\events\`, 12 files)

**Base type.** `BaseEvent` (`events\base.py`, 342 lines): custom `dataclass_transform` metaclass `_ConditionMeta` + `Field()` descriptors; `__eq__`/`__lt__` etc. build a **condition DSL** (`events\conditions.py`): `Event.field == value` → `OpCondition`; `SomeEvent | OtherEvent` → `TypeCondition`. Every event auto-gets `created_at`. Flags:
- `__transient__` — ephemeral, not persisted by default (`ModelMessageChunk`, `ModelReasoning`, `TaskProgress`, `MessageEnqueued`).
- `__conversational__` — durable transcript replayed to the model (`UsageEvent` sets False).
- `ProviderReplay` mixin with `__replay_role__` (`"anchor"`/`"turn"`) — provider-native state kept by history shaping (`ag2\_replay.py` `replayable_span`).

Serialization via `events\_serialization.py` (`to_dict`/`from_dict`, JSON round-trip with `__enum__` wire marker); `events\sizing.py` gives `estimated_tokens`/`render_for_prompt`.

**Key subclasses:** input_events (see core spec), types.py (see core spec), tool_events (see core spec), task_events, lifecycle, alert (`ObserverAlert` source/severity/message/data, `Severity` INFO→FATAL; `HaltEvent`), voice (audio pipeline; skip in Go core).

**Event flow.** No global bus. Each conversation has a `Stream` (`ag2\stream.py`: Protocol, `MemoryStream`, `SubStream`) holding `history` (pluggable `Storage`) and `pending_messages` inbox. Agent loop emits via `context.send(event)`; `Stream.send` first runs **interrupters** (registered `interrupt=True`; may swallow/replace the event — e.g. halt checks), then all **subscribers** filtered by `Condition`. Subscribers are DI-resolved callables; persistence is itself a subscriber (`_FilteredStorage` skips transient unless `persist_all=True`). The LLM invocation is a subscriber on `ModelRequest`; tool execution subscribers on `ToolCallsEvent`/`ToolResultsEvent`. Consumers observe push (subscribe) or pull (`stream.where(...).get(...)`/`join`). `ag2\network\task_mirror.py` is a canonical subscriber example.

## 2. Middleware (`ag2\middleware\`, 12 files)

**Interface** (`middleware\base.py`): `BaseMiddleware` instantiated **per turn** via `MiddlewareFactory` (public `Middleware` wraps `(cls, **options)`); four onion hooks, each with `call_next`:
- `on_turn(call_next, event, context) -> ModelResponse` — wraps the whole agent turn.
- `on_tool_execution(call_next, ToolCallEvent, context) -> ToolResultEvent | ToolErrorEvent | ClientToolCallEvent`.
- `on_llm_call(call_next, events, context) -> ModelResponse` — wraps each LLM invocation (receives history list).
- `on_human_input(call_next, HumanInputRequest, context) -> HumanMessage`.

`ConditionalMiddleware` wraps any factory with an event `Condition`. `middleware\describe.py` — introspection (ADR 0013).

**Ordering** (wired in `Agent._execute`, agent.py ~692): outermost `AssemblerMiddleware(policies)` → compaction middleware → aggregation middleware → user middleware → innermost LLM client call. User factories from `Agent(..., middleware=...)`/`ask(..., middleware=...)`; plugins contribute too.

**Builtins** (`middleware\builtin\`): `HistoryLimiter` (keep last N events, `on_llm_call`), `TokenLimiter` (token-budget truncation), `RetryMiddleware` (`llm_retry.py`), `LoggingMiddleware`, `TelemetryMiddleware` (OTEL, extra), `MetricsMiddleware`, and `ApprovalRequired`/`approval_required` (`builtin\tools\approval.py`) — raises `ToolApprovalRequest` through HITL, honors "always" grants keyed on `ToolCallEvent.source`.

## 3. Policies (`ag2\policies\`, 7 files)

**Context-assembly policies** (not termination/retry). Protocol `AssemblyPolicy` (`ag2\assembly.py`): `async apply(prompts, events, context) -> (prompts, events)`; pure transforms composed left-to-right inside `AssemblerMiddleware.on_llm_call` before each LLM call; `validate_order` warns when reduction policies precede injection ones. The six:
- `ConversationPolicy` (`conversation.py`) — filter: only `ModelRequest/ModelResponse/Tool*` + `ProviderReplay` + `CompactionSummary` reach the LLM.
- `SlidingWindowPolicy` — keep last N events via `_replay.replayable_span` (never orphans tool-call pairs; widens rather than empties).
- `TokenBudgetPolicy` — char-estimated budget, retains contiguous suffix.
- `WorkingMemoryPolicy` — injects `/memory/working.md` from KnowledgeStore.
- `EpisodicMemoryPolicy` — injects recent conversation summaries (`CONVERSATIONS_PREFIX`).
- `AlertPolicy` (`alert.py`) — reads `ObserverAlert` events, dedups on `(source, severity, message)`, appends non-fatal as prompt text, emits `HaltEvent` for FATAL (consumed by `_HaltCheckMiddleware`, agent.py ~2029 → synthetic `"HALTED: <reason>"` response).

## 4. Network (`ag2\network\`, 58 files, ~14.3k lines)

Durable, protocol-driven multi-agent messaging over a **hub**, in-process or cross-process/host. Opt-in ("bare Agent works standalone"). Every exchange happens in a **channel**; wire unit is an `Envelope` (`network\envelope.py`): `channel_id`, `sender_id`, `audience` (None=broadcast), `event_type` (fixed vocabulary: `ag2.msg.text`, `ag2.packet`, `ag2.channel.*`, `ag2.task.cancel_request/cancelled`, `ag2.context.set`, `ag2.expectation.violated`), plus `causation_id`, `depth` (delegation cap), `ttl_seconds`, `idempotency_key`, `priority`.

Key types:
- `Hub` (`network\hub\core.py`, 3377 lines) — registries (Passports/Resumes/Rules/Skills), channel + task state machines, per-channel WAL persisted into KnowledgeStore, dispatch, adapter registry `(type, version)`, invite handshake (`EV_CHANNEL_INVITE` → acks → `EV_CHANNEL_OPENED`), TTL/expectation sweepers, audit log. Never imports tenant code.
- `network\channel.py` — `ChannelManifest` (type/version/participants/expectations/knobs), `ChannelMetadata`, `ChannelState` machine (PENDING→ACTIVE→CLOSING→CLOSED/EXPIRED), `ParticipantRole`, `Expectation`.
- `network\identity.py` — `Passport` (kind: agent/human/remote_agent, hub-stamped `agent_id`, `AuthBlock`, `CostProfile`), `Resume` (capability claims + stats), `AgentRuntime`.
- `network\rule.py` — `Rule` = `AccessBlock` (inbound/outbound globs, channel-type initiate/accept) + `LimitsBlock` (concurrency, TTLs, `RateBlock`, `delegation_depth`, `InboxBlock`); hub-enforced.
- **Transports** (`network\transport\`): `Frame` vocabulary (`frames.py`: Hello/Welcome handshake, Request/Response control plane, Notify/Receipt data plane, Ping/Pong), `LinkClient`/`LinkEndpoint` Protocols (`link.py`), ships: `LocalLink` (in-memory queues) and `WsLink`/`serve_ws` (WebSocket). Reconnect replay via `since_envelope_id`.
- **Adapters** (`network\adapters\`): `ChannelAdapter` Protocol (`base.py`) — stateless: `validate_create/validate_send/fold/on_accepted/initial_state`, envelope builders, per-turn LLM tools. Built-ins: `ConsultingAdapter` (1-question/1-reply, auto-close), `ConversationAdapter` (bidirectional multi-turn), `DiscussionAdapter` (multi-party round-robin), `WorkflowAdapter` (declarative `TransitionGraph` with `context_vars` and `EV_PACKET` rounds). Views (`network\views\`): `ViewPolicy` Protocol projecting WAL envelopes into `ModelRequest`/`ModelMessage`: `FullTranscript`, `WindowedSummary`, `NamedTranscript`, `NamedWindowedSummary`.
- **Clients** (`network\client\`): `NetworkClient` Protocol, `HubClient` (one connection per process; in-process direct or RPC over Link frames), `AgentClient` (per-registration; default notify handler auto-acks invites and runs `Agent.ask`), `HumanClient` (non-LLM participant), `Channel` handle, `NetworkPlugin` (LLM tools `peers`/`channels`/`tasks`/`context`/`delegate` — `client\tools\`), `delegate.py` (one-shot consulting channel), inject keys (`network\policies.py`: HUB_DEP, CHANNEL_DEP, …).
- `network\transitions.py` — data-only `TransitionGraph`: `TransitionTarget`s (`AgentTarget`, `RoundRobinTarget`, `StayTarget`, `RevertToInitiatorTarget`, `TerminateTarget`), `TransitionCondition`s (`Always`, `FromSpeaker`, `ToolCalled`, `ContextEquals`), priority-ordered `Transition`s, registries for serialization into channel knobs.
- `network\handoff.py` — `Handoff(target, reason)` / `Finish(summary, reason)` typed tool returns read from `ToolResultEvent` to set routing / close the channel.
- Hub gateways (`network\hub\`): `arbiter.py` (`HubArbiter`/`RuleBasedArbiter` Allow/Deny), `listener.py` (`HubListener`), `expectations.py` (`acks_within`, `reply_within`, `max_silence`; `audit`/`notify_channel`/`auto_close`), `audit.py` (`AuditLog` JSONL), `sweepers.py`, `telemetry.py`, `layout.py`.
- `network\remote.py` — `RemoteAgentProxy` Protocol (federation seam; no proxies ship). `network\task_mirror.py` bridges agent `Task*` events into `Hub.observe_task`. `network\auth.py` — `NoAuth`/`ApiKeyAuth` + `AuthAdapter`. `network\workflow_helpers.py` — `set_context`/`delete_context` over `EV_CONTEXT_SET`.

## 5. Integration protocols

- **`ag2\a2a\` (27 files)** — Agent-to-Agent protocol (Google a2a-sdk). `A2AServer` (executor, card, JSONRPC/gRPC/REST transports, streaming, push notifications, task stores, security) + `A2AClient` (drives remote A2A agents; private extension `urn:ag2:client-tools:v1`; context-var sync; HITL over the wire). Multi-tenant `A2AConfig.tenant`. *Go: optional; flagship interop surface; stub initially.*
- **`ag2\acp\` (20)** — Agent Client Protocol (Zed). Serving (`ACPAgent`, executor, guard, permissions, elicitation) + consuming (`ACPBridge` spawns/drives external CLI coding agents — Claude Code, Codex, OpenCode, KiloCode). *Go: optional; stub.*
- **`ag2\ag_ui\` (9)** — AG-UI serving: `AGUIStream` projects events into AG-UI core events; Starlette endpoint (GET capabilities / POST streaming run). *Go: optional; easy as HTTP SSE adapter.*
- **`ag2\a2ui\` (21)** — Agent-to-UI generative-UI protocol (surfaces, actions, ASGI server, REST/AG-UI/A2A transports). *Go: optional; stub.*
- **`ag2\mcp_ui\` (3)** — MCP-UI resource builders (`external_url`, `raw_html`, `remote_dom`; actions `intent/link/notify/post_message/prompt/tool_call`). *Go: trivial pure-data helpers; stub.*
- **`ag2\live\` (12)** — realtime voice (`LiveAgent`, RealtimeConfig, OpenAI Realtime/Gemini Live/ElevenLabs/Mistral configs, cascade STT→LLM→TTS, turn detection, sound device). *Go: skip for functional core.*
- **`ag2\knowledge\` (13)** — KnowledgeStore: virtual filesystem Protocol (`base.py`: read/write/list/delete/exists/append/read_range/on_change) over `MemoryKnowledgeStore`, `SqliteKnowledgeStore`, `DiskKnowledgeStore`, `RedisKnowledgeStore`; `LockedKnowledgeStore`; `bootstrap.py`; `log.py` `EventLogWriter` (hub WALs and agent event logs live here); `polling.py`. **Core dependency** (hub WAL, compaction/aggregation, memory policies). *Go: needed — Protocol + memory + disk backends minimum.*
- **`ag2\eval\` (36)** — offline evaluation: `runtime\runner.py` `run_agent` (produce-then-grade, OTEL in-memory exporter, Trace from spans), `runtime\evaluate.py` `evaluate_traces`, `scorer.py` `@scorer` DI, scorers (correctness, cost, tools, judge/pairwise, threshold, attribution), `dataset\` Suite/Task, results store + diffing, pairwise win-rate. *Go: not needed; stub or omit.*

**Adjacent core:** `ag2\hitl.py` (78 lines: `HumanHook` type, `wrap_hitl`, `default_hitl_hook` raises `HumanInputNotProvidedError`, `ElicitationPolicy = "ask"|"decline"`); `ag2\watch.py` (512: `Watch` Protocol, `EventWatch`, `CadenceWatch`/`CronWatch`, `IntervalWatch`, `DelayWatch`, composites `AllOf`/`AnyOf`/`Sequence`); `ag2\observers\` (`Observer` Protocol `register(stack, context)`, `StreamObserver`, `BaseObserver` (Watch-backed, emits `ObserverAlert`), `CompositeObserver`; built-ins `LoopDetector`, `TokenMonitor`). Alerts flow: Observer → `ObserverAlert` → `AlertPolicy` → prompt injection / `HaltEvent` → `_HaltCheckMiddleware`.

## 6. Test locations (Python)

`test\events\`, `test\stream\`, `test\middleware\` (+builtins/telemetry), `test\test_assembly.py`, `test\test_alert_halt.py`, `test\policies\`, `test\agent\test_hitl.py`, `test\test_watch.py`, `test\test_watch_advanced.py`, `test\observer\`, `test\network\` (46 files), `test\a2a\` (32), `test\acp\` (26), `test\ag_ui\`, `test\a2ui\`, `test\mcp_ui\`, `test\live\`, `test\knowledge\`, `test\mcp\`, `test\eval\`.

## 7. Go-port necessity summary

| Package | Verdict |
|---|---|
| `events` | **Needed** — backbone; condition DSL → predicate functions |
| `middleware` | **Needed** — 4 hooks, onion chain, ~5 builtins |
| `policies` | **Needed** (assembly protocol + conversation/sliding-window/token-budget; knowledge-backed optional) |
| `network` | Optional for single-process, but the distribution story; ports cleanly (pure data + one Protocol each) |
| `knowledge` | **Needed** (memory backend minimum) |
| `hitl` / `observers` / `watch` | Needed in reduced form (human input hook + alert flow load-bearing) |
| `a2a`, `acp`, `ag_ui`, `a2ui`, `mcp_ui`, `live`, `eval` | Optional — interfaces/stubs |
