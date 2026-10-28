# Decision records (AGo)

Format: what was decided, when, why. Newest at the bottom.

## D-001 — 2026-10-28 — Repo layout: Go repo at root, Python clone ignored

The Go repository lives at `C:\projects\ago2` (module `ago`). The Python reference clone
`ag2/` sits inside the working tree but is git-ignored — it's a read-only reference, never
part of the Go repo. Specs distilled from it are copied into `docs/specs/`.

## D-002 — 2026-10-28 — Which AG2 to port: the 2026 rewrite, not classic AutoGen

The current AG2 main has fully removed `ConversableAgent`/`GroupChat`/`initiate_chat`
(verified: zero hits in the clone). The port target is the event-sourced `Agent`/`ask`/`run`
architecture with middleware, HITL hooks, tools-as-subscribers, and the network hub.
Group-chat behavior is expressed via `network` (channels + TransitionGraph) and `Agent.as_tool`.

## D-003 — 2026-10-28 — Zero external dependencies for core

Core packages use only the Go stdlib (net/http, encoding/json, crypto/rand, os/exec).
Reason: build reliability, no dependency churn, and provider SDKs in Python map to ~300-600
LOC of raw HTTP/SSE per provider in Go. UUIDs come from a tiny internal helper
(crypto/rand, RFC 4122 v4). Any external dep requires a DECISIONS entry.

## D-004 — 2026-10-28 — asyncio → goroutines; stream dispatch is synchronous

`async def` becomes plain functions taking `context.Context`. `MemoryStream.Send` runs
interrupt subscribers then normal subscribers synchronously in the caller's goroutine under
a lock (parity with Python's awaited subscribers). Background work: `SpawnBackground`
(goroutine + panic recovery + WaitGroup). Per-stream turn lock: `sync.Mutex` keyed by
stream ID (serializes concurrent asks on one stream — Python parity).

## D-005 — 2026-10-28 — Condition DSL → predicate functions

Python's descriptor condition DSL (`Event.field == value`, `A | B`, `~c`) becomes
`events.Condition = func(Event) bool` with `AllOf/AnyOf/Not/TypeOf[T]/Equal` combinators.
Reason: Go has no operator overloading; predicates are idiomatic and equally composable.

## D-006 — 2026-10-28 — Usage token counts are *int64 (not *float64)

Python uses `float | None`; token counts reported by providers are integers. AGo uses
`*int64` (nil = not reported) with field-wise `Add`. If a provider reports fractional values,
they are rounded (documented in that provider's mapper).

## D-007 — 2026-10-28 — response.Proto.Validate drops the provider parameter

Python passes `provider` to `validate(response, context, provider)`; builtin implementations
don't use it. AGo: `Validate(response string, ctx *Context) (any, error)`. Revisit if a
provider-aware validator appears.

## D-008 — 2026-10-28 — Provider surface for wave 1

Full implementations over raw HTTP: **openai** (chat completions + SSE streaming + tools +
structured output + usage normalization), **anthropic** (messages + SSE + tools + prompt
caching), **gemini** (generateContent + tools), **ollama** (chat + tools).
Medium: **zai**, **mistral**, **dashscope** (OpenAI-compatible-ish REST).
Stubs (config structs exist, Create returns "not yet implemented"): **xai** (Python uses a
gRPC SDK), **bedrock** (needs AWS SigV4/credentials chain), **typesafe** (niche SDK).
OpenAI Responses/Decisions APIs: deferred (config structs noted, client later).

## D-009 — 2026-10-28 — Tools DI: reflection-based binding

Python resolves tool parameters through fast_depends + Pydantic. AGo: `NewFunctionTool`
uses reflection — JSON args map to typed params; `*context.Context` param and
`tools.Inject(id)` / `tools.Variable(name)` (or struct tags) resolve from
`Context.Dependencies` / `Context.Variables`. Schema generated from the func signature via
the shared reflection JSON-schema generator (package `response`).

## D-010 — 2026-10-28 — Network and protocols: functional subset, wire vocabulary preserved

Port a functional single-host subset of `network`: Envelope/Channel/Passport/Rule,
LocalLink transport, Hub core (registries, channels, dispatch, WAL via knowledge.EventLogWriter),
HubClient/AgentClient/HumanClient, the four adapters (Consulting, Conversation, Discussion,
Workflow) + TransitionGraph + Handoff/Finish tools. `ag2.*` envelope event-type vocabulary is
kept verbatim for wire compatibility. WebSocket link, federation (RemoteAgentProxy), a2a/acp/
ag_ui/a2ui/mcp_ui/live/eval: interfaces + stubs, deferred.

## D-011 — 2026-10-28 — knowledge: memory + disk backends first

`Store` protocol + MemoryStore + DiskStore (filesystem) + Locked wrapper + EventLogWriter
(JSONL). SQLite/Redis backends deferred (would need external deps).

## D-012 — 2026-10-28 — Porting is wave-based with dynamic workflows

Wave 0 (foundation: events/stream/context) is hand-written — it fixes contracts. Waves 1/3
fan out over packages via dynamic workflows, one subagent per package with build+test gates
(`go build ./... && go test ./<pkg>/`). Wave 2 (`agent` turn loop) hand-written — highest
subtlety. Wave 4 is a whole-repo fix loop to green.
