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
Closed 2026-10-30: both APIs are ported in `config/openai` (`OpenAIResponsesConfig`/
`ResponsesClient`, `OpenAIDecisionsConfig`/`DecisionsClient`) — D-060, closure record
D-068. The stub rows above (xai/bedrock/typesafe) are unchanged.

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

## D-013 — 2026-10-29 — middleware package: brief-driven simplifications vs Python builtins

Implemented against the wave-1 brief (events-network.md §2 + the porting ask), which
simplifies the Python builtins. All marked `// AGO-DEVIATION` in code:
- **If/Conditional**: the OnLLMCall condition is evaluated on the last event of the
  history it receives (≈ the turn trigger), not on a captured initial event — a Go
  `Factory func() Middleware` has no event argument.
- **HistoryLimiter / TokenLimiter**: plain suffix of the history (transient events are
  dropped and uncounted in HistoryLimiter); no ModelRequest anchor re-attachment and no
  `replayable_span` widening — tool-pair repair stays with the agent. Limits/budgets
  below 1 clamp to 1 (Python raises ValueError; a Factory has no error return).
- **Retry**: linear backoff (`retryBaseDelay` var, tests may shorten); context
  cancellation is detected through the error value only (middleware hooks carry no
  stdlib context), so the backoff sleep is not interruptible; the Python
  "attempt published events → don't retry" guard is omitted (agent owns stream
  consistency).
- **Approval**: answers y/yes/approve/always (Python also takes "1"); denied calls
  return a `ToolErrorEvent` "denied by human" (Python returns a ToolResultEvent);
  "always" grants live on the middleware instance, i.e. one turn (Python stores them
  in context.variables, conversation-scoped); grant key = ToolCallEvent.Source falling
  back to Name (Python parity). The request goes through `ctx.Ask` on
  context.Background() as the embedded *events.HumanInputRequest — a Go-embedding
  limitation; the concrete ToolApprovalRequest (ToolCallID = call ID) is what the
  middleware constructs.
- **Chain** (additive): `Chain(fs ...Factory) Factory` composes factories outermost-
  first; needed for onion ordering tests and reusable by the agent turn loop.

## D-014 — 2026-10-29 — usage & task: aggregation rule, dependency key, explicit terminal transitions

Deviations marked `// AGO-DEVIATION` in `usage/` and `task/`:

- **usage**: `FromEvents` aggregates top-level `UsageEvent`s only; the `Parts` of a
  subtask rollup are never visited (the rollup's own Usage already sums its children —
  Python parity). Records whose Usage reports no non-zero token count are skipped
  (Python `Usage.__bool__` treats zero-valued fields as unset); a reported zero
  survives summation (`_add_tokens(None, 0.0) → 0.0` parity, D-006).
- **task dependency key**: `"ag:task"` (AGo's reserved framework prefix per the context
  package) instead of Python's `"ag2.task"`.
- **no async context manager**: Python auto-completes on a clean CM exit and auto-fails
  on an exception. Go has no exception capture, so `Start` performs the `__aenter__`
  side (TaskStarted, state=running, dependency stamping) and the owner calls the
  terminal method explicitly; the first terminal transition performs the `__aexit__`
  dependency rollback. Mutators return `error` (Python raises through `await send`).
- **dependency rollback**: the context package cannot delete keys, so when no previous
  `ag:task` value existed the rollback writes `nil`; readers must treat a nil
  dependency value as absent. Python pops the key.
- **TaskCompleted event result**: the AGo event vocabulary carries `Result string`;
  non-string results are JSON-encoded into the event, the full value lives in
  `Metadata.Result` (Python `TaskCompleted.result` is Any).
- **Metadata times**: Go `time.Time` instead of Python's ISO strings; JSON renders
  RFC3339 (`null` when unset) and `UnmarshalJSON` is lenient — missing state defaults
  to `created`, unparseable times and a malformed spec collapse to zero values, never
  raising (Python `from_dict` defensiveness parity).

## D-015 — 2026-10-29 — response: schema envelope and validation semantics in Go

The `response` package normalizes Python's `make_adapter` embedding rules and adapts
validation to stdlib `encoding/json`:

- **Scalar envelope**: top-level bare scalars — string, all int/uint kinds, floats,
  bool — are wrapped into `{"type":"object","properties":{"data":…},"required":["data"]}`;
  structs, maps and arrays are never wrapped. Python differs at the edges: a bare `str`
  schema is plain text there (no schema at all) and top-level lists get wrapped through
  a `GenericAlias` quirk; AGo follows the ARCHITECTURE contract instead.
- **time.Time**: schema `{"type":"string","format":"date-time"}`, never wrapped (Python
  wraps `datetime` as a "primitive").
- **Validation strictness**: `encoding/json` is strict where pydantic coerces ("5" → 5
  fails in Go). What the stdlib cannot enforce — required object members and enum
  membership — is checked by a schema walk (`checkRequired`) before the typed unmarshal.
- **decision.py**: `DecisionSpec/FromResponse` ports the predicate/choice/score
  classification; there is no Annotated/Enum introspection in Go, so option names
  default to the value as text, `Question` is a plain descriptor passed to the new
  `QuestionSchema` helper (the Go equivalent of the `Annotated` marker), and the
  `response_schema` decorator (callable validators over fast_depends) is not ported.
- **JSONSchema() returns deep copies** so provider strict-mode mappers can rewrite their
  copy without racing on the shared immutable schema.

## D-016 — 2026-10-29 — tools: DI annotations, argument binding, and executor semantics in Go

Deviations marked `// AGO-DEVIATION` in `tools/`. Python binds tool parameters through
fast_depends with named parameters; Go reflection exposes no parameter names, and its
`Annotated`-style metadata does not exist, which forces the shapes below:

- **Arguments bind by struct field**: a tool function takes a single struct argument
  whose fields are the model-facing parameters (json tag or field name). Flat
  non-struct parameters and multiple struct arguments are construction errors; a
  signature with no model-facing parameters yields `{"type":"object","properties":{}}`.
  Parameter schemas are generated with `response.SchemaOf` over a rebuilt struct that
  drops DI fields; per-property `title` entries of the Python output are not produced
  (the AGo contract forbids titles in Parameters).
- **Inject/Variable are exported struct types** (`tools.Inject`, `tools.Variable`) with
  `NewInject`/`NewVariable` constructors. The contract sketch `func Inject(...) inject`
  cannot work: the returned unexported type cannot be named in a user's function
  signature, and a func and a type cannot share the `Inject` name. A DI slot is an
  argument-struct field of those types or a field tagged `aginject:"id"` /
  `agvariable:"name"`; the key falls back to the field's JSON name (Python parity for
  `Inject()` without an explicit name). A standalone `Inject`/`Variable` function
  parameter is rejected — there is no name to resolve it by.
- **Requiredness**: every model-facing argument field is required unless it is a
  pointer (Optional[X] parity) or carries its own `agschema` tag (then the tag rules
  entirely, `required` included).
- **Defaults**: `agdefault:"<literal>"` tags declare scalar defaults (parsed per field
  kind); missing dependencies/variables wrap `ErrNotFound` into a ToolErrorEvent.
  Python materializes a declared default into the conversation-scoped dependency
  registry on first use; AGo materializes into the per-call context copy only
  (`Context.Clone` copies its maps, and the package cannot reach the conversation from
  inside the tool chain).
- **Typed DI slots deliver `Inject`/`Variable` values** with the resolved payload in
  `Default` (`HasDefault` true) — the framework must pass the declared parameter type.
- **Middleware order**: for tool execution, `mws[0]` (agent middleware, the list the
  executor passes to `Tool.Register`) is the outermost layer, then the tool's own
  middleware (first entry outermost), then timeout, then the function — matching the
  net Python order (the Python agent pre-reverses its instances; AGo keeps the
  repo-wide `middleware.Chain` convention of first = outermost).
- **Hitl cancellation**: a hitl failure abandons the batch and propagates; sibling
  goroutines cannot be interrupted, so a sibling that already finished may have sent
  its result event, but the batch never settles (no `ToolResultsEvent`) and its
  outcome is discarded — Python's `task.cancel()` guarantee narrows to "nothing is
  recorded", not "no side effect landed" (Python documents the same limit for sync
  tools).
- **Batch result typing**: `ToolResultsEvent.Results` is `[]events.ToolResultEvent`, so
  a `ToolErrorEvent` outcome is appended as its embedded `ToolResultEvent`; the error
  text travels in the result parts (set by `events.FromErrorCall`), and the typed
  Error field survives only on the event the tool sent to the stream.
- **ClientTool wire schema** is the flat AGo shape (`name`, `description`,
  `parameters`, `defer_loading`), not Python's nested `{"function": …}` envelope;
  a top-level `title` is stripped (Python `__post_init__` parity).
- **Source**: `WithSource(s)` overrides, default `function:<runtime func name>:<tool
  name>` (Python: `function:{module}:{qualname}:{name}`); the executor stamps handled
  calls through `HandledBy` inside the tool's subscriber, so the sent call event keeps
  the model's unstamped copy.
- **Known-but-unregistered names** (hosted tool types) never trigger the not-found
  fallback, mirroring Python; a call for such a name with no handler would wait
  indefinitely, exactly as Python's `stream.get` does — hosted calls are settled by
  the provider, never on this stream.

## D-017 — 2026-10-29 — config core, OpenAI-compatible engine, internal/httpx & internal/sse

Wave-1 config brief implemented (`config/config.go`, `config/client.go`,
`config/openaicompat.go`, `internal/httpx`, `internal/sse`). Deviations, marked
`// AGO-DEVIATION` in code:

- **CompatParams carries `Provider`**: the brief's field list omitted it, but the
  engine must stamp `ModelResponse.Provider` for usage accounting and has no other
  way to know the vendor. An unset Provider defaults to `openai` (the dialect the
  engine speaks).
- **DataInput parts travel as JSON text**: the brief lists only text/binary/url
  parts; spec §6 and the Python serializer rule (DataInput → encoded text) are
  followed for both user turns and tool results.
- **FileIdInput → ErrUnsupportedInput**: per the brief. Python's OpenAI client
  accepts Files-API references; the shared engine has no Files API, so they fail
  fast instead of silently degrading.
- **Tool results are one string**: parts are concatenated with "\n" (brief:
  "конкатенирован"). Python sends an array of text parts when a result has
  several parts.
- **Assistant replay keeps raw arguments** (`ToolCallEvent.ToAPI`); Python
  re-serializes the parsed arguments dict. The raw form preserves the model's
  exact JSON across the round trip.
- **`_strictify_schema` ported** (`strictifySchema`, unexported): the shared
  engine serves openai too, and the ARCHITECTURE contract requires strict schema
  coercion on that path (`response_format.type=json_schema`, `strict: true`,
  `required` = all properties, `additionalProperties: false`).
- **stream_options.include_usage is set automatically** when streaming (Python
  config parity: usage arrives only with it); an `Extra["stream_options"]`
  override wins.
- **Validation also requires `model`** (brief named key/base URL only) — an empty
  model would otherwise reach the wire as `"model": ""`.
- **Nil conv tolerated**: a nil conversation context means a headless call —
  system prompt empty, no progress events sent (streaming chunks still not
  produced). The LLMClient contract allows any caller to pass nil.
- **Empty tool description omitted** from the wire; Python sends the empty
  string. Equivalent on the wire (description is optional).
- **httpx error surface**: `PostJSON` returns every completed HTTP response as
  `(status, body, nil)` — errors are reserved for transport failure after
  retries and ctx cancellation; callers build API errors from the status.
  `Retry-After` is honored (seconds or HTTP-date) but capped at one minute so a
  broken server cannot stall a turn; the exponential backoff has no jitter
  (deterministic tests over SDK-identical timing).
- **httpx streaming timeout**: `Client.Timeout` caps only non-streaming requests;
  a stream may be long-lived, so it relies on ctx cancellation
  (`http.Client.Timeout` would kill the whole body read).
- **sse Scanner robustness**: the trailing block of a stream without a final
  blank line is still delivered, and a block carrying no data lines is not
  dispatched (chat-completions streams always carry data; this keeps half-flushed
  keepsalives from becoming empty chunks). `data: [DONE]` passes through as data.


## D-018 — 2026-10-29 — testing: scripted doubles in Go (TestConfig/TrackingConfig)

Package `testing` (package name `agotesting`, import alias `agotest "ago/testing"` —
avoids clashing with the stdlib testing package). Deviations vs Python
`ag2/testing.py`, marked `// AGO-DEVIATION` in code:

- **Turn vocabulary collapses to full ModelResponse scripts**: the brief scripts
  `*events.ModelResponse` values only. Python's `Turn` forms — plain strings
  (auto-wrapped into ModelMessage + published), raised exceptions, single/iterable
  `ToolCallEvent` shortcuts, and mid-turn published events (chunks, reasoning,
  builtin tool activity) — are not ported, and neither is the
  `raise_tool_errors` history inspection. A Go caller that needs tool-call turns
  scripts a ModelResponse with `ToolCalls.Calls` populated; it needs a failing
  call, it returns the error from its own client wrapper (e.g. TrackingConfig over
  a custom config).
- **TestConfig is its own client; Create() returns the receiver**: Python's
  `create()` returns a fresh TestClient with an independent script iterator per
  call; AGo follows the brief, so the script cursor is shared by every client
  handed out.
- **Provider/Model never raise**: Python raises NotImplementedError when
  provider/model were not given; `Provider()` returns the fixed `ProviderTest`
  ("test") and `Model()` returns a stable default "test", changeable through
  Clone's "model" override (the only supported override; unknown keys error per
  the ModelConfig.Clone contract).
- **Clone semantics**: Python `copy()` returns self for both configs. AGo
  follows the ModelConfig.Clone contract: TestConfig.Clone returns a fresh copy
  sharing the script but with its own cursor and fresh call recording;
  TrackingConfig.Clone clones the inner config (overrides applied) and wraps it,
  sharing the call recorder so recordings accumulate across clones (preserving
  the Python copy()-returns-self accumulation behavior).
- **Recording shape**: TestConfig records call history snapshots (Python
  TestClient records nothing; TrackingClient records ModelCall{prompt, tools,
  dependencies, variables} and mocks the last message). Both AGo configs expose
  `Calls() [][]events.Event` — deep JSON-round-trip copies taken at call time,
  re-copied on every read; events outside the marshal registry (they would
  degrade to UnknownEvent) are kept as-is. A call that finds the script
  exhausted is recorded too.
- **NewTrackingConfig returns an error** for a nil inner config instead of
  panicking at call time.

## D-019 — 2026-10-29 — config/openai: Chat Completions provider on the shared engine

`config/openai` (OpenAICompletionsConfig, Client, StrictifySchema, ResponseFormat) is a thin
provider skin over `config.RunChatCompat`; deviations, marked `// AGO-DEVIATION` in code:

- **model is a private field with a `Model()` accessor**: the `config.ModelConfig` contract
  requires the method and Go forbids a field and a method sharing one name, so the Python
  public dataclass field `model` cannot be kept as declared in the wave brief.
  `NewOpenAICompletions(model)` is the only way to set it — which also guarantees the
  constructor defaults (MaxRetries=4, Streaming=false, Timeout=0) always apply; `Clone`
  overrides the model by key.
- **StrictifySchema strips title/examples/default**: the porting brief mandates removal;
  Python's `_strictify_schema` keeps them (the strict API ignores them). The shared engine's
  unexported `strictifySchema` stays Python-parity (it serves zai/mistral/dashscope too);
  the openai Client builds the per-call response_format itself via the exported
  `ResponseFormat`, so the stripped form is what reaches the wire. Precedence is the
  engine's: an explicit config `ResponseFormat` wins over the per-call schema.
- **MaxCompletionTokens/Seed/User travel through the engine's Extra body map**:
  `CompatParams` types only the fields every OpenAI-compatible provider shares; the
  OpenAI-specific create options are merged as plain body fields (Python parity: regular
  create options), with `ExtraBody` applied last (SDK extra_body parity). Empty `User`
  is omitted (Python omit parity).
- **Clone keys**: Go field names with Python snake_case aliases; values must keep the Go
  field types (per the ModelConfig contract, e.g. `*float64` for temperature). Unknown
  keys and mismatched types are errors; the original config is never mutated.
- **MaxRetries=0 means no retries** (SDK parity for max_retries=0); the default 4 is set
  by the constructor since a Go struct literal cannot express "unset".
- **API key resolution happens at Create() time** (config → `OPENAI_API_KEY` env →
  `config.ErrNotProvided`), mirroring the SDK reading the environment when the client is
  constructed; the engine re-validates at call time.

## D-020 — 2026-10-29 — config/anthropic: Messages API provider on raw HTTP/SSE

Wave-1 provider brief implemented (`config/anthropic/`: config.go, mappers.go, client.go).
Parity with `ag2/config/anthropic/` over `internal/httpx` + `internal/sse` (no SDK).
Deviations, marked `// AGO-DEVIATION` in code:

- **The model travels in a private field** behind the required `Model()` accessor: Go
  forbids a field and a method with one name on one type (same resolution as
  config/openai and config/gemini). Set via `NewAnthropic` or the `Clone` override
  `"Model"`; `NewAnthropic` applies the defaults (BaseURL, MaxTokens 4096, MaxRetries 2,
  PromptCaching true), and a zero-valued `MaxTokens` falls back to 4096 at Create() (the
  API requires the field; a Go struct literal cannot express "unset").
- **Temperature/TopP are kept and sent when set**: the 2026 Python config deprecates
  them (the newer API version it targets rejects them); the ported wire version
  (anthropic-version 2023-06-01) accepts them, so they map to the request body.
- **Prompt caching places cache_control per the brief** (last system block + last
  tool), not Python's `inject_cache_breakpoint` on the last user turn; with caching off
  the system prompt is a plain string (SDK parity).
- **Structured output is prompt-driven only**: the schema's `SystemPrompt()` is appended
  to the system prompt and the response text is returned unvalidated (validation lives
  in response.Proto above). Python additionally sends a native `output_config`; the
  brief scopes the port to the prompt path.
- **FileIdInput fails fast** (config.ErrUnsupportedInput): AGo has no Files client, while
  Python references uploads through the files-api beta.
- **Unknown response content blocks are dropped silently** (server_tool_use,
  redacted_thinking, container_upload, ...): the AGo event vocabulary has no
  provider-replay carrier for them (Python re-emits typed Anthropic* events and replays
  them as assistant blocks). text/tool_use/thinking are mapped.
- **pause_turn continuations are not ported**: Python re-issues up to 5 follow-up calls
  when the server pauses a turn; the brief omits it, and pause_reason passes through
  MapStopReason unchanged.
- **Tool-use integrity repair is ported** (orphan tool_use / orphan tool_result
  dropping, and loose ToolResultEvent/ToolErrorEvent dedup against the
  ToolResultsEvent wrapper — AGo needs it more than Python here: AGo tools send their
  own result events AND the executor sends the batch, so both land in history).
- **Tool-call arguments replay as parsed JSON** in the tool_use `input` block (Python
  `json.loads(call.arguments or "{}")`); invalid arguments fail the call. On the way in,
  `input` is kept as the raw JSON the API sent (Arguments string).
- **TotalTokens is reported only when input or output tokens were** (nil = not
  reported, D-006); Python always computes input+output (0+0 = 0). thinking_tokens from
  `output_tokens_details` maps to ThinkingTokens (a decomposition of output_tokens).
- **Clone keys** are Go field names, matched case-insensitively ("Model"/"model"); for
  pointer fields the plain element type is accepted (float64 → *float64) and nil clears
  the field; unknown keys and mismatched types are errors. The original is never mutated.
- **API-key resolution happens at Create()** (config → `ANTHROPIC_API_KEY` env →
  `config.ErrNotProvided`), mirroring the SDK raising at client construction; streaming
  is Anthropic's native SSE event stream (message_start/content_block_*/message_delta),
  not a Chat-Completions dialect, so it does not reuse the shared openaicompat engine.

## D-021 — 2026-10-29 — provider packages zai/mistral/dashscope (chat-compat) and stubs xai/bedrock/typesafe

Wave-1 provider brief implemented as `config/zai`, `config/mistral`,
`config/dashscope`, `config/xai`, `config/bedrock`, `config/typesafe`. The
three chat-compat providers bind their config onto the shared engine
`config.RunChatCompat` (fresh client per `Create()`, Python parity); the
stubs exist as configs only. Deviations, marked `// AGO-DEVIATION` in code:

- **The model is a private field behind the `Model()` accessor** in every
  provider config: the `config.ModelConfig` contract requires a `Model()`
  method and Go forbids a field and a method sharing one name, so the brief's
  public `Model` field cannot compile. The constructor (`NewZAI(model)` etc.)
  is the only way to set it — the same resolution `config/openai` adopted.
- **DashScope default base URL is the compatible-mode root**
  (`https://dashscope-intl.aliyuncs.com/compatible-mode/v1`, per the brief) —
  Python defaults to the DashScope-native REST root (`.../api/v1`) spoken
  through the dashscope SDK; AGo talks the OpenAI-compatible dialect, so the
  native root would not serve `/chat/completions`.
- **Mistral default base URL folds `/v1` into the base**
  (`https://api.mistral.ai/v1` + engine path `/chat/completions`), landing on
  the same endpoint as Python's `MISTRAL_DEFAULT_SERVER_URL` plus the SDK's
  version segment.
- **`SafePrompt` is sent only when true**: Python's field is tri-state
  (None = omit), the Go bool cannot express "explicitly false", so false
  omits the wire field.
- **API-key env fallback happens at `Create()`** (zai: `ZAI_API_KEY`,
  mistral: `MISTRAL_API_KEY`, dashscope: `DASHSCOPE_API_KEY`) — Python
  parity: the SDK resolves the key when the client is constructed, i.e. once
  per `create()`, not per call. An empty-but-set variable counts as missing.
- **Stubs validate the model first, then report `ErrNotImplemented`**: the
  `ModelConfig.Create` contract ("missing model or key → error wrapping
  `ErrNotProvided`") binds stubs too; the wrapped error names the provider
  (`xai`, `bedrock`, `typesafe`). Stubs do not require a key — no client
  exists to consume one.
- **TypeSafe's model is required** via `NewTypeSafe(model)`: Python defaults
  it from `typesafe_sdk.constants.DEFAULT_MODEL`, an SDK absent from the
  Python clone, so the default value is unknowable. `Criteria` is
  `map[string]string` (Python `Mapping[str, str]`), `Timeout` is
  `time.Duration`.
- **`MaxRetries` int: zero = single attempt** (Go zero value); the zai
  constructor applies the Python default of 3.
- **`Clone` typing**: keys are Go field names, matched case-insensitively
  (same convention as config/anthropic); unknown keys and mismatched types
  are errors; a `*T` field also accepts a `T` (wrapped) and `nil` (cleared,
  Python `None` parity); slice/map fields are copied so a clone never aliases
  the receiver's memory. `Clone` never mutates the receiver.
- **Empty `BaseURL` on a direct struct literal falls back to the default** at
  `Create()` time (Python parity: `base_url=None` → SDK default) instead of
  failing the `RunChatCompat` base-URL check.

## D-022 — 2026-10-29 — config/gemini & config/ollama: raw-REST providers, field visibility, usage mapping

Wave-1 provider brief for gemini + ollama implemented over raw HTTP (`config/gemini/`,
`config/ollama/`), on `internal/httpx`. Deviations, marked `// AGO-DEVIATION` in code:

- **Config fields are unexported, set through the constructor and Option funcs**: the
  `ModelConfig` contract requires a `Model()` accessor and Go forbids a field and a method
  with the same name on one type, so the brief's "type GeminiConfig{Model, ...}" field list
  cannot carry exported fields and still implement the interface. Realized as
  `NewGemini(apiKey, model, opts...)` / `NewOllama(model, opts...)` with `With*` options;
  `Clone(overrides)` keys stay the Python field names, nil clears optional fields.
- **gemini usage mapping**: `candidatesTokenCount` maps straight to CompletionTokens and
  `thoughtsTokenCount` lands only on ThinkingTokens — the porting brief pins this, while the
  Python mapper additionally folds thinking into completion_tokens so prompt+completion==total
  (they diverge for thinking models). `toolUsePromptTokenCount` folds into the prompt and
  `cachedContentTokenCount` → CacheReadInputTokens (Python parity).
- **gemini provider stamp**: ModelResponse.Provider is `"gemini"` (the AGo ModelConfig
  vocabulary, consistent with usage reports); Python stamps `"google"`.
- **gemini FileIdInput → ErrUnsupportedInput**: Python resolves file references through the
  Gemini Files API; this port has no Files client, so they fail fast. BinaryInput
  `.vendor_metadata` is dropped (no raw-REST mapping this wave).
- **ollama Streaming=true → Create() error** (ErrUnsupportedInput), per the brief — streaming
  is deferred (Python streams); a streaming config refuses to create a client instead of
  silently degrading to non-streaming.
- **ollama default host** `http://127.0.0.1:11434` (brief) vs Python's
  `OLLAMA_DEFAULT_HOST` (`http://localhost:11434`) — the address Ollama actually binds,
  immune to an IPv6 localhost resolution. A missing API key is legal (local server): the
  Authorization header is simply omitted.
- **Tool-call ids synthesized** (`uuid`): neither API returns one on the wire, but tool
  results must reference a call id; Python parity — its ToolCallEvent dataclass defaults
  id to uuid4 as well.
- **Response schemas** (beyond the brief's mapping list, Python parity): gemini sends
  `generationConfig.responseMimeType=application/json` + `responseJsonSchema`, ollama sends
  the raw JSON schema as `format`; both append the schema's SystemPrompt to the prompt.
- **gemini streaming is implemented** although the brief names only the non-streaming
  endpoint: `streamGenerateContent?alt=sse` with the same chunk parsing — text parts emit
  ModelMessageChunk, thought parts ModelReasoning, one ModelMessage at the end.
- **No transport retries** for either provider (Python SDK parity: both post plain httpx
  without retry logic; MaxRetries 0). Callers cancel via ctx.
- **gemini UrlInput → fileData** with a MIME guessed from the URL extension, and
  BinaryInput → inlineData for every binary kind (Python parity).

## D-023 — 2026-10-29 — spec: AgentSpec serialization (FromAgent / ToAgent)

Package `spec` (wire copy of Python `ag2/spec.py`). Deviations, marked
`// AGO-DEVIATION` in code:

- **FromAgent leaves ResponseSchema nil**: the AGo Agent does not expose its
  response schema publicly (no getter; the field is private in `agent.options`),
  so the schema Python reads from `agent._response_schema` cannot be captured.
  To wire a schema back, pass `agent.WithResponseSchema(...)` to ToAgent.
- **ToAgent does not rebuild the response schema**: Python's `to_agent` applies
  the spec's schema unless an explicit argument overrides it. AGo options are
  opaque funcs, so "explicit argument wins over the spec" is unobservable —
  applying the spec's schema unconditionally could clobber a caller's explicit
  `WithResponseSchema`. The field stays wire data; reconstruction is the
  caller's choice.
- **No UnknownSpecFieldWarning**: Python warns (non-fatally) on unknown spec
  keys; a Go library cannot warn, so unknown JSON keys are dropped silently by
  encoding/json (same load result, no diagnostic).
- **available pool is a flat map, no toolkit flattening**: Python's `to_agent`
  walks `Toolkit` members while building its name index. The AGo pool is
  already a name→tool index supplied by the caller, so flattening is the
  caller's job (a Toolkit under its own name resolves only as that name).
- **Prompt joins with "\n"** into a single `agent.New` prompt string; the
  fragments keep their order and re-enter the turn as one static fragment.

## D-024 — 2026-10-29 — watch: pull-model Wait replaces the push arm/disarm protocol

Package `watch` ports `ag2/watch.py` minus the cron variants (CronWatch and the
CadenceWatch size-or-time batching are deferred — no consumer needs them yet and
the cron parser is its own chunk of work). Deviations, marked `// AGO-DEVIATION`
in `watch/`:

- **Push → pull**: Python arms a watch on a stream with a callback and disarms it
  later; the AGo `Watch.Wait(ctx) (events.Event, error)` contract
  (ARCHITECTURE.md) makes one Wait call one armed watch. There is no
  `id`/`is_armed`/`disarm` surface — repeated or concurrent Waits are
  independent, so the implementations stay stateless and concurrency-safe.
- **Signal semantics**: time-based watches report the tick as `(nil, nil)` — the
  event is nil, the tick itself is the payload (Python invokes the callback with
  an empty batch). Composites report the **last non-nil** event across their
  sub-watches (Python passes the combined batches; one Wait carries one event).
- **EventWatch** subscribes for the duration of one Wait only (subscribe →
  channel → unsubscribe on every return path; no leak) and sees only
  post-subscription events (Python parity: fires after arming, no history
  replay).
- **DelayWatch** is stateless: Python disarms itself forever after its single
  fire; here each Wait arms a fresh single-shot delay. IntervalWatch(0) and
  DelayWatch(≤0) fire immediately.
- **Composite edge cases**: empty AllOf/Sequence fire immediately with
  (nil, nil) (AllOf parity: 0 of 0 fired; Python's empty Sequence never fires —
  a Wait that could never return is a deadlock trap, so it completes instead);
  empty AnyOf can never fire and blocks until ctx is done. AnyOf cancels the
  losing waits and abandons a sub-watch that ignores cancellation (its result is
  discarded; the goroutine ends when it returns on its own). AllOf/AnyOf/Sequence
  reject nil sub-watches with the `ErrNilWatch` sentinel instead of panicking
  inside a spawned goroutine.

## D-025 — 2026-10-29 — observers: direct stream subscription, contract-pinned built-ins

Package `observers` ports `ag2/observers/` to the brief's AGo contract. Deviations,
marked `// AGO-DEVIATION` in `observers/`:

- **No ExitStack, no Watch**: Python registers observers under a caller-owned
  ExitStack (`register(stack, context)`) and the BaseObserver family buffers
  events through an armed Watch. The AGo `Observer` interface is
  `Name() string; Register(ctx) error` — one direct `Stream.Subscribe` per
  observer, events processed inline in the dispatching Send, subscription lives
  with the stream. ObserverStarted/ObserverCompleted lifecycle events stay the
  agent's job (Python parity: emitted by `_observer_lifecycle` in agent.py).
- **LoopDetector keys on ModelResponse text**, not ToolCallEvent `(name,
  arguments)` pairs, per the AGo contract. Consecutive identical normalized
  contents (whitespace collapsed, `strings.Fields` join) fire once; the dedup
  set persists until `Reset()` (Python `_flagged` parity). A clamp
  `repeats < 2 → 2` replaces Python's unvalidated threshold. Responses whose
  normalized text is empty are not counted: tool-call rounds legitimately emit
  contentless ModelResponses back to back, which would false-positive.
- **TokenMonitor has one budget → one error alert**, not Python's
  warn/critical threshold pair, per the AGo contract; the comparison is strict
  (`total > budget`, contract wording) where Python uses `>=`. Billable count is
  `TotalTokens` when reported, else prompt+completion: Python's
  `_billable_tokens` takes `max(total, prompt+completion)` — the max is dropped,
  the unset-total fallback is kept so hand-assembled usage cannot silently read
  zero.
- **`TypeOf[T]` value-form conditions** (`TypeOf[events.ModelResponse]()` not
  `TypeOf[*events.ModelResponse]()`): the events arrive pointer-wrapped from the
  agent but `TypeOf[*T]()` compiles to a pointer-only matcher, while value-form
  `T` matches both value and pointer events (`events.AsValue` normalizes in the
  handler).
- **StreamObserver callback has no error result** (`func(events.Event)`, brief
  shape) and is wrapped into a `stream.Subscriber` that always returns nil;
  only the built-ins that send alerts surface send errors to the dispatcher.


## D-026 — 2026-10-29 — aggregate: aggregation collapses to transcript merging (brief-driven)

The porting brief redefines aggregation against Python (`ag2/aggregate.py` writes
knowledge-store files from raw events and never touches the transcript). AGo (`aggregate/`)
reduces it to transcript shaping: after a successful turn, the aggregation middleware
(`New(trigger, strategy)`) folds every `CompactionSummary` plus the conversational events
older than the keepRecent=2 tail into ONE new `CompactionSummary` via the Strategy,
replacing the history, bracketed by transient
`AggregationStarted`/`AggregationCompleted`/`AggregationFailed`. Choices:

- Triggers are constructor funcs returning `Trigger` (`SummaryCountTrigger(n)`,
  `UsageTrigger(total)`), matching the brief's notation and the middleware-builtin style.
  The usage handed to a trigger is `usage.FromEvents(history).Total` — the tokens reported
  on the stream, which already include the finished turn's `model_call` UsageEvent.
- `LLMStrategy{Client}` makes one LLM call per aggregation and reports its tokens as
  `UsageEvent(kind="aggregation")` on the conversation stream (Python parity:
  `_emit_aggregation_usage` — without it the aggregation call's tokens would never reach
  the usage report). `AggregationCompleted.Usage`/`LLMCalls` come from an optional
  unexported seam (`usageSource`); hand-written strategies report zero.
- A strategy failure emits `AggregationFailed`, leaves the history untouched (Replace
  happens only after success) and does NOT fail the turn — `OnTurn` returns the turn's own
  result. A summarization failure must not kill the conversation.
- Non-conversational persisted events (`UsageEvent` and the like) in the superseded span
  are preserved verbatim ahead of the merged summary: usage reports read the history
  (`AgentReply.Usage`), so dropping them would silently lose telemetry.
- Summaries are collected from the whole history (that is what `SummaryCountTrigger`
  counts); conversational events only from before the keepRecent tail, which stays
  verbatim even if it itself contains a summary.
- `aggregate` imports `middleware` for the `middleware.Factory` return type the brief
  pins. The ARCHITECTURE dep-graph line names only `events, context`; the Factory contract
  requires the type, and `middleware` sits strictly below `aggregate`, so the import stays
  acyclic.

## D-027 — 2026-10-29 — knowledge: virtual Store with memory/disk backends, brief-shaped EventLogWriter

Parity with `ag2/knowledge/` (base/memory/disk/locked/log); deviations marked
`// AGO-DEVIATION` in `knowledge/`:

- **Store error surface is Go-idiomatic**: Python returns `None`/`""`/raises;
  AGo `Read` returns `ErrNotFound` (wrapped with the path), `ReadRange` returns
  empty with no error for a missing file, `Delete` of a missing path is a no-op,
  `List` of a missing directory is empty — each mirroring its Python counterpart
  where one exists.
- **`Append` drops the Python byte-offset return** (the brief pins
  `Append(path, data) error`); the WAL cursor use case is served by replaying the
  whole file via `Replay`, so no consumer needs the offset. `ReadRange` takes
  `(off, n)` instead of Python's `(start, end)`: `n <= 0` reads to EOF.
- **`OnChange(fn)` is a global callback list**, not Python's per-path
  subscription with a closable handle: the brief's signature carries no path and
  no subscription object. Callbacks run synchronously in the mutating goroutine
  after the store's internal lock is released — re-entering the store itself is
  safe; re-entering a `LockedStore` wrapper is not (documented deadlock, Python
  has the same hazard inside its named write lock).
- **DiskStore.OnChange observes only in-process mutations** through the same
  store instance: the stdlib has no filesystem watcher and external deps are
  forbidden (D-003). Python's watchdog backend is replaced by the same fallback
  path Python itself ships for capability-less backends (polling by the caller).
- **LockedStore is one RWMutex** (mutations exclusive, reads shared) instead of
  Python's per-path named async locks with a 30s TTL; it also closes the
  read-modify-write window the per-path locks leave open.
- **DiskStore works on every platform** (the tests run on Windows); Python
  declares macOS/Linux-only and guards filesystem-illegal characters. AGo passes
  path bytes through — framework-controlled paths (`/memory/...`, `/logs/...`)
  are always legal. Traversal protection is `filepath.Rel` against the absolute
  root after cleaning (`..`, and anything resolving outside root → ErrTraversal).
- **EventLogWriter lines are bare `MarshalEvent` JSON**, not Python's
  `{"type", "data"}` envelope: `events.MarshalEvent/UnmarshalEvent` already carry
  the `@type` discriminator, so a second envelope would be redundant. Log path is
  `logs/<streamID>.jsonl` (brief) vs Python's `/log/<streamID>.jsonl`. Broken
  lines are skipped on replay (brief); unknown event types still decode to
  `events.UnknownEvent` (Python parity). The Python persist/persist_dropped
  segment machinery (compaction snapshots) is not in the brief; `Streams()` lists
  logged stream ids instead.

## D-028 — 2026-10-29 — compact: brief-shaped triggers/strategy, spend carried through the rewrite

Wave-3 brief implemented (`compact/`: trigger.go, strategy.go, compact.go) against
core.md §3.7 and the Python `_CompactionMiddleware` (ag2/agent.py) + `compact.py`.
Deviations, marked `// AGO-DEVIATION` in code:

- **Constructor functions, not structs**: `EventCountTrigger(n)` / `TokenTrigger(budget)`
  return the `Trigger` interface (the `middleware.HistoryLimiter(n)` precedent); the
  impls are unexported. n/budget <= 0 disables (Python 0 = disabled parity) — a Go
  constructor has no error return.
- **Trigger's usage argument** is the usage summed over the `ModelResponse` events of
  the history snapshot (the conversation's reported spend); both built-ins ignore it
  (count / `events.EstimatedTokens` over conversational events only) — it exists for
  custom spend-budget triggers. Python's `CompactTrigger` had no usage input at all.
- **The cut lives in the middleware**: `Strategy.Summarize` receives only the old
  conversational span (all conversational events before the first of the last
  `KeepRecent = 2`); Python strategies received the whole history and cut themselves
  (`_cut_for_target`). Interleaved telemetry after the cut rides along with the fresh
  range, telemetry before it is dropped. The tool-cycle boundary widening
  (`_replay.snap`) is not ported: the brief pins a plain keepRecent cut.
  Correction (2026-10-29): the original rationale — "provider mappers already
  repair orphan tool results on replay (D-020)" — holds only for
  config/anthropic; the openai-compatible, gemini and ollama mappers replay
  tool results verbatim, so a keepRecent cut that splits a tool round still
  reaches those wires as an orphan `role:"tool"` message. Round-closing repair
  lives at assembly time (policies widening, D-030); a round-aware compact cut
  remains open for providers without mapper repair.
- **LLMStrategy is headless**: one `config.LLMClient.Call` with a nil conversation
  context (Python summarizes over a throwaway `MemoryStream` — same isolation; nil
  conv drops provider progress chunks, which nothing consumes). No stdlib context in
  middleware hooks → `context.Background()` (same limitation as middleware.Retry,
  D-013). Its spend is reported as a live `UsageEvent(kind=compaction)` on the
  conversation stream (Python `_summarize` parity, same as aggregate's LLMStrategy).
- **Summarization-window spend survives the rewrite**: the middleware subscribes a
  collector for the duration of `Summarize` and appends the collected `UsageEvent`s
  after the fresh range in the `History.Replace` payload; `CompactionCompleted.Usage`
  is their field-wise sum and `LLMCalls` their count (Python: `spent_during` +
  `strategy.last_usage`). AGO-DEVIATION: Python's `_with_usage_events` additionally
  re-attaches every UsageEvent from before the compaction, so spend outside the
  retained window stays reportable on `AgentReply.usage()`; the brief's contract
  replaces history with the summary plus the fresh range only, so those records are
  dropped — bounding that belongs with the ADR-0014-style decision, not here.
- **No double-compaction guard**: Python's `_last_compact_event_count` per-instance
  counter is omitted — a Factory builds a fresh instance per turn (at most one
  compaction per turn by construction) and the trigger itself governs refiring.
- **Replace and lifecycle-send errors fail the turn** (Python parity: those awaits
  sit outside the try/suppress blocks); only the strategy's own failure is
  swallowed into `CompactionFailed` with the history untouched. `error_type` /
  `strategy` labels come from `reflect` type names (`type(exc).__name__` parity;
  anonymous types fall back to the full reflect string).

## D-029 — 2026-10-29 — mcp: minimal client over one persistent session

Wave-3 brief implemented as `mcp/` (config.go, jsonrpc.go, transport.go, http.go,
stdio.go, client.go, content.go, tools.go). JSON-RPC 2.0 over streamable HTTP
(POST per message, `Accept: application/json, text/event-stream`, answers parsed
from either content type, `Mcp-Session-Id` honored, 404 → session rebuilt with one
retry) or over a subprocess's newline-delimited stdio. Deviations, marked
`// AGO-DEVIATION` in code:

- **One persistent session per Client**: the Python proxy opens a fresh
  short-lived session per tool call because its sessions are async context
  managers; the brief's contract (`Initialize`/`ListTools`/`CallTool` on one
  Client) implies the Client owns its process/connection. Initialize is
  idempotent, the first operation initializes lazily, and a lost HTTP session is
  re-established transparently with a single retry.
- **No context.Context on the exported operations**: the brief pins
  `Initialize()`/`ListTools()`/`CallTool(name, args)` signatures; they run on
  `context.Background()`. Operations serialize under the Client lock (one
  exchange in flight), which also keeps the stdio read loop single-owner.
- **MCPStdioConfig carries the filter fields** (`AllowedTools`, `BlockedTools`,
  `ToolNamePrefix`) beyond the brief's struct sketch: `Tools()` must filter both
  transports, and Python's `MCPStdioServerConfig` has them.
- **Embedded resource → TextInput carrying the block's JSON** (brief-pinned);
  Python maps text contents to `TextInput(text)` and blobs to `BinaryInput`.
  Image **and audio** blocks decode base64 into `BinaryInput` (the ARCHITECTURE
  contract names both; the brief named only image). Unknown future blocks are
  preserved as JSON text, never dropped.
- **Pagination bound 100 pages** (brief) vs Python's `_MAX_TOOL_PAGES=1000`;
  the repeated-cursor abort is kept.
- **`input_required` answering (spec §5.5) not ported** and **no discovery cache
  keyed by resolved config** (spec §5.3): the brief omits both, and AGo configs
  are plain values without `Variable` indirection — one Client is one resolved
  config.
- **No protocol-era negotiation**: the client offers `2024-11-05` and records
  whatever version a server that answered reports; `protocol_mode`, proxy,
  verify, connection_timeout, encoding, server_label and description fields from
  Python's configs are dropped (the brief's structs omit them).
- **Tool call failures vs tool errors**: transport/JSON-RPC failures wrap
  `ErrProtocol`; a result the server flagged `isError` wraps `ErrToolCall` with
  the server's text blocks as the message. Both become `ToolErrorEvent`s at the
  `remoteTool` boundary (the executor records them as results); a success maps
  through `events.NewToolResult`.
- **Garbled stdio lines are skipped**, not fatal: a chatty banner must not kill
  the session, and the awaited reply may still follow (HTTP/SSE notifications are
  skipped the same way; server-initiated requests are never answered — the
  minimum client declares no client capabilities).

## D-030 — 2026-10-29 — policies: assembly over the Conversational flag, brief-pinned formats

Package `policies` (spec events-network.md §3; Python `ag2/policies` +
`ag2/_replay.py`). Deviations, marked `// AGO-DEVIATION` in code:

- **ConversationPolicy filters on `Conversational()`**, not on a type list:
  the Go event vocabulary already encodes replay-worthiness. CompactionSummary
  is conversational in AGo (events/lifecycle.go), matching Python's explicit
  inclusion; the ToolCallsEvent batch container drops out while the individual
  ToolCallEvent and the results carry the pair.
- **Reductions (sliding window, token budget) operate on the conversational
  subset** (brief: "последние n разговорных событий"): the agent hands the full
  stream history to every LLM call (agent/turn.go), so telemetry must not count
  toward a window or budget. When no trim is needed the input passes through
  unchanged (Python parity), so an upstream ConversationPolicy is optional.
- **Orphan repair is targeted widening, not full `replayable_span`**: a
  retained span that opens with an event carrying required call ids widens
  back until the span answers them all; the span never widens past the
  transcript start, so it grows rather than empties. Required ids come from
  the tool results — the flat ToolResultEvent / ToolErrorEvent /
  ToolNotFoundEvent / BuiltinToolResultEvent and the ToolResultsEvent batch
  container the executor actually sends — and from standalone
  ToolCallEvent / ClientToolCallEvent, which map to nothing on the wire:
  provider mappers replay calls only from a ModelResponse's tool_calls, so a
  valid span holds the announcing response, not just the call event (only
  config/anthropic additionally repairs orphan pairs on replay, D-020).
  Builtin calls carry their own provider item and need nothing. Python's
  ProviderReplay anchor/turn items do not exist in the Go event vocabulary;
  where Python drops orphaned events from the tail (_replay._prune), AGo
  widens the whole round back onto the wire instead.
- **AlertPolicy prompt format is one `alert(<severity>) from <source>:
  <message>` line per alert** (brief) instead of Python's joined
  "[OBSERVER MONITORING ALERTS]" block. A fatal alert sends a single
  HaltEvent with reason `FATAL: <message>` (Python's `source`/`alerts` fields
  do not exist on the Go HaltEvent) and adds no prompt line (Python added a
  "[FATAL ALERT]" note).
- **Memory policies take the knowledge.Store in the constructor** (brief) where
  Python resolves it from context dependencies; the episodic directory is
  `conversations/` (brief) vs Python's `/memory/conversations/`, and the
  episodic transparency-note fragment is dropped (the brief names one fragment).
- **`validate_order` is not ported**: it exists to warn Python users about
  policy ordering. The recommended order (filters and injections first,
  reductions last) is documented in the package doc; the Python `name`
  attribute and the transparency options go with it.

## D-031 — 2026-10-29 — network: functional single-host subset, hub/adapters/clients in Go

Wave-3 network brief implemented (`network/` + `network/transport/`): Envelope/Channel/
Passport/Rule/Transitions/Handoff, Hub (registries, pending→invite→ack→opened handshake,
send gates, WAL + Replay), Consulting/Conversation/Discussion/Workflow adapters,
HubClient/AgentClient/HumanClient, Frame vocabulary + LocalLink. WebSocket links,
federation (RemoteAgentProxy), view policies, expectation sweepers, task mirror and the
client LLM-tool set (peers/channels/tasks/context/delegate) are out of the subset (D-010).
Deviations, marked `// AGO-DEVIATION` in code:

- **Envelope shape is brief-driven, not Python-wire**: single-id `Audience` ("" =
  broadcast) instead of a list, `Payload []byte` (raw JSON) instead of the `event_data`
  dict, `Priority` int, hub-stamped `ID`/`CreatedAt`. The `ag2.*` event-type vocabulary
  is kept verbatim (D-010); the ack type is `ag2.channel.ack` per the brief (Python:
  `ag2.channel.invite.ack`), and `ag2.channel.invite.reject` / `ag2.channel.expired` are
  added beyond the brief's constant list because the all-or-nothing handshake and TTL
  need them. Lifecycle broadcasts (opened/closed/expired) address the full participant
  list in Python; with a single-id audience the self-inclusion resolves at dispatch.
- **WAL seam + binding**: the brief pins `knowledge.EventLogWriter`, but knowledge
  landed mid-wave, so the Hub speaks a local `WAL` interface (channel snapshots +
  envelope records) with `MemoryWAL`/`FileWAL` backends and `KnowledgeWAL` binding
  `knowledge.EventLogWriter` (records travel as DataInput events, one log stream).
  Replay restores channels + adapter fold state; it does NOT restore passports
  (identities re-register with fresh ids after a restart).
- **Adapter interface collapsed to the brief's four methods**:
  `validate_create/validate_send/fold/on_accepted/initial_state` merge into
  `ValidateCreate` + `OnEnvelope` (validate-then-fold, state untouched on error) +
  `InitialState`. The brief's `ch *ChannelState` parameter is the hub's channel-state
  record (`*ChannelHandle`); the literal `ChannelState` type is the lifecycle enum. An
  optional `StateInitializer` extension seeds discussion rotation / workflow graph from
  the channel record — the brief's zero-arg `InitialState()` cannot see it.
- **Transitions are simplified per the brief**: no per-transition priorities (list
  order decides), no `default_target` (fall-through terminates with `no_transition`),
  no `ContextEquals` condition, and no (de)serialization registries — a fixed
  name-based `ToDict`/`GraphFromDict` covers the built-ins for channel knobs.
  `(state, envelope)` becomes `*TurnContext`; terminating targets write `CloseReason`
  and return "".
- **Agent integration is callback-based**: the brief forbids `network → agent`, so
  `AgentClient` bridges through `MessageFunc func(msg string) (string, error)`
  / `EnvelopeFunc` / `RoundFunc` instead of calling `Agent.ask` (the spec's default
  notify handler). No view-policy projection — the inbound text is the rendered
  envelope (packets: handoff signal line + body).
- **Hub dispatch is synchronous notifier calls** (D-004 style): invites are acked
  inline so `CreateChannel`'s handshake completes deterministically; substantive
  envelopes go through each client's buffered mailbox + pump goroutine (overflow
  drops). Adapter-requested closes run after dispatch (Python order); trailing replies
  racing the close are rejected by the hub gates (terminal state / workflow
  `expected == ""` after termination), never invented.
- **Handoff/Finish recognition** covers typed DataInput values (a live Go tool
  returning `*Handoff`/`*Finish`) and JSON text/marked-map encodings
  (`{"handoff": {...}}` / `{"finish": {...}}`) for results that crossed a wire;
  `RoundFromEvents` scans ToolResultsEvent/ToolResultEvent with Finish winning over
  Handoff. Handoff targets resolve Passport name → agent id at post time (Python:
  packet-finalize).
- **Lazy TTL**: no background sweeper — the first send past the channel `ExpiresAt`
  (knob `ttl_seconds`) expires the channel and broadcasts `ag2.channel.expired`;
  per-envelope TTL is sender-asserted (checked only when the sender supplied
  `CreatedAt`).
- **LocalLink** is an address-addressed switchboard (`Dial(addr)` + `Accept` for
  server loops, delivery by `Frame.To`) with bounded inboxes (`ErrLinkOverflow`)
  instead of Python's unbounded asyncio queues.

## D-030 — 2026-10-29 — Agent.Task always runs on a private stream

Python's `agent.task(...)` accepted a `context=` argument letting observers watch the
task's stream. The AGo signature `Agent.Task(ctx, title, opts...)` has no context
parameter, so the task always runs on a private `*agocontext.Context` over a fresh
stream seeded with the agent's dependencies. An unexported `taskStreamProbe` seam
(tests only) provides the observer window in tests. Recorded by the orchestrator on
behalf of the wave-3 agent (D-030 replaces the agent's placeholder numbering).

## D-031 — 2026-10-29 — Wave gate policy: final gate decides, transient conflicts are resolved

During wave 3, parallel package agents reported "partial" because sibling agents'
in-progress edits broke the whole-repo gate mid-wave. The workflow's final gate
(build/vet/test) ran after all agents settled and is green; those transient findings
are considered resolved. Rule going forward: a package's status is set by the final
whole-repo gate, not by mid-wave snapshots.

## D-032 — 2026-10-29 — Tool subscriptions are per-turn; Register returns them and the agent unsubscribes on Close

Review (confirmed by reproduction) found that the subscriptions created by
`tools.Executor.Register` (batch handler, per-tool handlers, not-found fallback) were
never removed: Register discarded the `stream.SubID`s and the agent's `Close` only
dropped its own LLM-trigger and halt subscriptions. A turn continuing on the same
stream (`AgentReply.ask`) therefore ran under the previous turn's still-live handlers:
duplicate tool executions, duplicate `ToolResultsEvent`, extra model calls until the
script/limit ran out. Python avoids this by registering through a per-turn ExitStack
`sub_scope` (ag2/tools/executor.py) that unwinds at turn end; the Python agent
docstring warns precisely about "duplicate tool execution" when subscriptions leak.

Fix: `Tool.Register` and `Executor.Register` return the ids of the subscriptions they
create — on error nothing stays subscribed (unwind parity with the ExitStack) — and
the agent collects them into the run's subscription set, unsubscribed by `Close`.
The per-stream turn lock serializes turns, so `Close` of turn N always precedes
`Register` of turn N+1 and registration never stacks duplicates. This makes real the
contract formerly phrased as "Go cleanup is via TurnScope (agent-owned); Register is
idempotent per turn" — the returned ids are that turn scope's substance. No new
deviation from Python parity: the subscription lifetime now matches the ExitStack
semantics.

## D-032 — 2026-10-29 — Ask routes questions through the stream; hooks are abandoned, not cancelled

Review found Context.Ask invoking the hook directly and taking the answer from its
return value, contradicting the ARCHITECTURE.md contract ("Ask sends
HumanInputRequest, waits for the matching HumanMessage (parent_id == req.ID) via
Stream.Get") and Python (ag2/context.py:186-266, ag2/hitl.py wrap_hitl). The old
hitl package comment referenced a DECISIONS entry that did not exist. Fixed, not
deviated:

- **Routing**: Ask publishes the question on the stream and a per-request interrupt
  subscription hands it to the installed hook; the hook's answer is sent back on
  the stream and consumed via `Stream.Get` on `HumanMessage.parent_id`, so an
  external participant (transport, test, replay) can answer instead of the hook.
  The question event is swallowed (`stream.ErrHandled`) — Python parity: the
  interrupt wrapper returns None and dispatch stops before the persistence
  subscriber, so the question is not transcribed and normal subscribers never see
  it. MemoryStream.Send correspondingly runs interrupters before persisting.
- **Timeout**: `req.Timeout` covers both the asking and the waiting; the hook runs
  on its own goroutine and Ask selects on the deadline, so a hook that never
  returns cannot pin the stream or the turn (Python: `asyncio.wait_for`). AGO
  cannot cancel a goroutine — an abandoned hook's eventual answer is discarded
  (Python cancels the in-flight hook). The agent passes Ask's per-request context
  down to the user hook; the middleware chain carries no stdlib context, so it is
  bound into the chain head per invocation.
- **Error classification** (Python parity): no hook → NotProvidedError; the hook
  failing on its own → FailedError even at the deadline; a hook surfacing the
  Ask-imposed cancellation (`ctx.Err()` → DeadlineExceeded) and the deadline
  itself → TimeoutError; caller cancellation → FailedError carrying
  context.Canceled.
- **Conversational flags** (same review): HumanMessage, ToolCallsEvent and
  TaskStarted/Completed/Failed/Expired/Cancelled are Conversational() == true,
  matching the Python `__conversational__` default — compaction counts, policies
  and aggregation had silently excluded them. ModelMessage stays false (transient;
  its durable copy is ModelResponse), TaskProgress and UsageEvent stay false.

## D-033 — 2026-10-29 — network: envelope WAL failures surface through Send; lifecycle records stay swallowed

Review (confirmed) found `recordLocked` swallowing every WAL append error while
its own comment promised the opposite ("surfaced for envelopes"): a failing
backend let `Send` report success for an envelope that `Replay` would never
see. Python parity: `post_envelope` awaits `_wal_append` without try/except
(ag2/network/hub/core.py), so a failed store fails the send.

Contract now: `recordLocked` returns the append error. The envelope record in
`postLocked` propagates it — `Send` fails after stamping, nothing is
dispatched, and the envelope never enters the in-memory log (the append moved
before the log write, so `ReadWAL` shows only accepted envelopes). Lifecycle
records (channel snapshots in create/activate/expire/close) stay swallowed so
the send path keeps working while the backend is down for content.
AGO-DEVIATION: Python surfaces the terminal close envelope's append failure
through `close_channel`; AGo's `closeChannel` is void and also runs as a
post-send follow-up where no caller could receive the error, so that one stays
swallowed (marked in code). Regression: `TestSendSurfacesWALAppendFailure`
(fails against the swallow behavior — verified by reverting the propagation).

## D-034 — 2026-10-29 — mcp: HTTP exchanges carry a 30s default timeout, configurable via MCPServerConfig.Timeout

Review (confirmed) found the HTTP transport built on `&http.Client{}`: a
hung/silent server blocked `CallTool` forever and, through the client's
exchange lock, every later operation and the agent turn. Python bounds the
exchange with `connection_timeout: float = 30.0`
(ag2/tools/toolkits/mcp_server/types.py). Fix: `MCPServerConfig.Timeout`
(time.Duration; zero = the 30s default, negative = no cap) applied to the
transport's `http.Client`; the stdio transport is untouched (Python's stdio
has no timeout either). This supersedes the D-029 bullet listing
`connection_timeout` among the dropped config fields.
AGO-DEVIATION: `http.Client.Timeout` caps the whole exchange including reading
an SSE response body, while httpx's timeout is per-phase (connect/read/write);
a slow-but-steady server is cut off at the cap. Regression:
`TestHTTPTimeout` (defaults, override, negative, hung-server fast failure
freeing the client lock).

## D-035 — 2026-10-30 — config/gemini: presence/frequency penalties, seed and thinking reach Clone and generationConfig

The Gemini config accepted only model/api_key/base_url/sampling/stop/streaming;
`presence_penalty`, `frequency_penalty`, `seed`, `thinking_level` and
`thinking_budget` were silently dropped by `Clone` and never reached the
request body. Python parity: `GeminiBaseConfig`
(ag2/ag2/config/gemini/config.py) and `_resolve_thinking_config` — penalties
and seed pass through to the generation config; the two thinking knobs
collapse into one object (`thinkingConfig.thinkingLevel` / `thinkingBudget` on
the REST wire, one or both). Fields are `*float64`/`*int64`/`*string`, set via
`WithPresencePenalty`/`WithFrequencyPenalty`/`WithSeed`/`WithThinkingLevel`/
`WithThinkingBudget`, deep-copied in `Clone`, nil clears. `cached_content`,
`response_modalities`, `image_config` and the raw `thinking_config` SDK object
stay out of the port's scope (SDK-typed in Python).
AGO-DEVIATION: `Clone` rejects a `thinking_level` outside
"low"/"medium"/"high"; Python's `ThinkingLevel` literal is compile-time only
and accepts any string at runtime. Regression: `TestGeminiClonePenaltiesSeedThinking`,
`TestGeminiPenaltiesSeedThinkingInRequest`, `TestGeminiThinkingConfigResolution`.

## D-036 — 2026-10-30 — tools: fast_depends `Depends` (factory-valued tool parameters) is not ported

Python re-exports `Depends` from fast_depends (`ag2/ag2/__init__.py`) for tool
signatures: a `Depends(fn)` parameter is excluded from the model-facing JSON schema
and its value is computed by calling `fn` right before the tool body, memoized per
call; `fn` itself may declare `Inject`/`Variable`/`Depends`. Porting it to Go is not
mechanical: reflection does not yield parameter names, so a factory parameter needs a
new convention (an `agdepends`-style tag plus a registry), a schema-exclusion rule
and per-call memoization — a new API surface of 150+ lines. The resolved-value cases
it covers are already served by the AGo DI dictionary (`tools.Inject`/`tools.Variable`
and the `*agocontext.Context` parameter, D-016). Its Python consumers — a2ui
actions/server_action, mcp tools/apps, skills runtime — are themselves deferred
subsystems (migration map §5).
Decision: do not port. Compute the factory value in Go before the call, or wrap the
factory as a separate tool. Revisit together with the deferred subsystems that
consume it.

## D-037 — 2026-10-30 — agent: per-call ask(dependencies=, variables=, prompt=) overrides are not ported

Python `ask`/`run` accept per-call `dependencies=`, `variables=` and `prompt=`
(ag2/ag2/agent.py:272-294, applied as `_ContextOverrides` on top of the agent's own
state). AGo `Ask`/`Run` take only `(ctx, msgs...)` (agent/agent.go); dependencies,
variables and prompt fragments are constructor options
(`WithDependencies`/`WithVariables`/`WithPromptFragments`). A caller-built
`*agocontext.Context` cannot be handed into a run either — `newRunOn` is unexported.
Replacement: vary at construction (a fresh `agent.New` per variant — the struct is
cheap) or mutate the shared context between turns. Revisit if a consumer needs
call-scoped overrides.

## D-038 — 2026-10-30 — context: reserved "ag:"/"a2a:" variable prefixes are convention only; no wire stripping

Python reserves the variable prefixes `ag:`/`a2a:` for control-plane state (the
approval bypass key `ag:approval_required:always`, A2A bookkeeping, `a2a:tenant`)
and enforces it with `strip_reserved_variables` (ag2/ag2/context.py:289-316): every
wire-originated merge drops reserved keys so a remote peer cannot author them —
writing the bypass key would pre-approve a gated tool and the human would never be
asked. AGo declares the same prefixes reserved but ships no stripping code: variables
never leave the process today (variables sync is part of the deferred federation
surface, D-010/D-031), so there is no wire to strip at, and Python's safety goal
cannot arise. The one crossing that exists is deliberate and filters by key, not by
prefix: a sub-task inheriting the parent's variables drops exactly the approval
bypass key (`agent/subtask.go` `bypassKey`; run_task parity). Gate: introduce
stripping when a transport that forwards variables lands.

## D-039 — 2026-10-30 — agent: TaskConfig.IncludeTools changed from bool to []string

`IncludeTools` was a bool ("child inherits the parent's tools, minus
ExcludeTools"); it is now `[]string`, matching Python's
`include_tools: list[str] | None` (`_filter_subtask_tools` shape): nil (the zero
value) inherits all parent user tools — Python's `include_tools=None` default; an
empty slice inherits none; otherwise it is a name allowlist, and `ExcludeTools`
applies after it. Breaking against earlier AGo builds, where the zero value
(`false`) gave the child no parent tools while the new zero value (`nil`) gives all
of them; accepted before v1.0. Regression: `TestSubtaskToolFilters`
(nil / empty allowlist / allowlist / exclude-after-allowlist).

## D-040 — 2026-10-30 — agent: the default AsTool tool name is task_<name>, not ask_<name>

Python parity: subagent_tool.py `tool_name = name or f"task_{agent.name}"`. The
default name generated by `Agent.AsTool()` changed from `ask_<name>` to
`task_<name>`; an explicit `WithToolName` override is untouched (the subagent
example pins `ask_specialist` and keeps working). Regression: `TestAsToolSurface`
(name and schema).

## D-041 — 2026-10-30 — delegation AGO-DEVIATION: a forwarded HITL question is transcribed on the parent stream

When a sub-task without its own hook asks through the parent's hook, the bridge
sends a copy of the question — stamped with the task id — to the parent stream
before invoking the hook (`agent/subtask.go`); the answer still lands only in the
child's transcript. Python swallows the forwarded question with a per-turn
interrupter; AGo subscribers receive events only (no context), so per-turn wiring
cannot be replicated on a stream another agent owns (D-013, D-032 — Ask routes
questions through the stream, and the answer is matched by the original request id,
which the stamped copy keeps). Observers of the parent conversation therefore see
the delegation asking. Without a hook on either side the child's question fails
with `hitl.NotProvidedError`, carried through the tool layer as a turn-ending hitl
error (Python parity: run_task re-raises HumanInputError). The AGO-DEVIATION comment
at the bridge cites "D-040" — the number this decision held when the fix landed;
numbering shifted after D-035 was taken by the gemini entry. Regressions:
`TestSubtaskBridgesHitlToParent`, `TestSubtaskHitlRefusedWithoutHook`.

## D-042 — 2026-10-30 — stream: SubStream.Send forwards unconditionally — a filtered view filters reading, not writing

`SubStream.Send` dropped events that did not match the view's condition (silently,
returning nil). Python parity: stream.py `SubStream.send` is a bare `parent.send` —
the view's condition filters what the view shows (`Subscribe`/`Get`), never what a
caller pushes through it. Behavior change against earlier AGo builds: an event sent
through a filtered view now reaches the parent stream (its subscribers and
transcript) even when it does not match the view condition. Regression:
`TestSubStreamSubscribeAndSend`.

## D-043 — 2026-10-30 — events: the sentinel tool result is Python's human-readable sentence, not the constant name

`SentinelToolResult` — the content appended for tool calls abandoned by an
unanswerable human-input request — carried the literal string
`"HUMAN_INPUT_ABANDONED_TOOL_RESULT"`; it now carries the Python sentence itself
(ag2/ag2/history.py:64): "This tool call was not completed: the turn ended because
a human-input request could not be answered." Behavior change against earlier AGo
builds: that sentence is what the model reads on the next call, so transcripts and
provider replays differ from earlier builds and now match Python. Regressions:
the events suite pins the sentence verbatim;
`TestHitlMissingHookRepairsHistory` exercises the repair path end to end.

## D-044 — 2026-10-30 — plugin: plugins apply at agent construction; turn-scoped attach/detach is not ported

Python carries plugins two ways: constructor-level (`Agent(plugins=...)`, applied permanently
via `PluginTarget._apply_plugin`, ag2/ag2/plugin.py:337-340) and invocation-level
(`additional_plugins`, applied inside `_turn_scope` and removed at turn end through
AsyncExitStack callbacks with identity-preserving `_PluginPrompt` markers —
ag2/ag2/agent.py:1432-1453). AGo ports the constructor path only: `agent.WithPlugin(p)`
expands the plugin into the existing options once, at `agent.New`. The turn-scoped path
— attaching and detaching plugins mid-conversation, including their per-turn context
contributions (dependencies, variables, prompt fragments added and retracted per turn)
— is deferred; nothing in the Go surface can add or retract contributions after
construction.

The Go Plugin carries the tool/middleware/prompt legs only. Python plugins also collect
HITL hooks, dependencies, variables, observers and assembly policies
(plugin.py:220-254); the AGo surface has no slot for them in this wave.

Ordering parity holds where the ported legs land: plugin tools append after the agent's
own; plugin middleware appends after the agent's own, which places it innermost
(Python add_middleware: appended runs closest to the model); plugin prompt fragments
extend the collectors, and plugin prompt hooks run through the same seeding pipeline
as `agent.WithPromptHook` hooks. Regressions: the `ago/plugin` suite pins the
Bundle→Plugin accessors, a tool round trip through `WithPlugin`, middleware order,
static and dynamic fragments via `PromptLines` capture, and empty/nil plugins being
harmless.

## D-045 — 2026-10-30 — mcpui: the ag2/mcp_ui data constructors are ported, stdlib-only

Supersedes the `mcp_ui` deferral in D-010 for this slice (owner directive): `ago/mcpui` ports
ag2/ag2/mcp_ui/resources.py and actions.py — the pure data constructors, no protocol code.
Python delegates to the mcp-ui-server SDK; AGo reproduces its wire shapes with stdlib only
(the SDK adds no behavior the constructors need). Shapes were verified against
mcp-ui-server 1.0.0 + mcp 2.x by running the reference code: a resource renders as
`{"type":"resource","resource":{"uri",…,"mimeType","_meta"?,"text"|"blob"}}` (by_alias,
exclude_none), an action as `{"type","payload"}`.

Deviations from Python parity, all marked in code:

- Map key order: Go maps have no insertion order, so `params`/`_meta` object keys serialize
  in sorted order where Python keeps dict insertion order. The JSON data is identical; only
  byte order of keys differs. PostMessage stays deterministic because of this.
- Floats render via Go's shortest representation (`1`), not Python's repr (`1.0`).
- Exact mcp wire bytes require `mcpui.Marshal`: encoding/json HTML-escapes <, > and &
  (`\u003c`…) at every entry point, and re-escapes even a Marshaler's output, so no custom
  MarshalJSON can opt out. Through a JSON parser both spellings decode identically — plain
  `json.Marshal` is shape-equivalent.
- `WithUIResource` is a Go-side helper: Python only documents the "include the UIResource in
  the toolResult content array" convention (mcp_ui_server core.py docstring) and ships no
  function; AGo expresses it by appending DataInput parts to `ToolResult.Parts`.
- The PostMessage serializer (pythonJSON) handles the JSON-basic value set natively with
  json.dumps separators (`": "`, `", "`) and ensure_ascii escaping; unknown value types fall
  back to compact encoding/json. Named slices expand element-wise to keep the separator.

Regressions: the ago/mcpui suite pins every constructor against the captured Python goldens
(text/blob encodings, both frameworks, prefixed+verbatim `_meta`), all five actions (wire
JSON and PostMessage bytes, including the apostrophe breakout test), ensure_ascii surrogate
pairs, the uri-before-framework guard order, and the attach helper round trip.

## D-046 — 2026-10-30 — files: the Files API moves out of deferred — ago/files plus OpenAI and Anthropic clients

`ag2/ag2/files/` and the two provider `files.py` modules port to `ago/files` and to
`FilesClient`/`CreateFilesClient` in `ago/config/{openai,anthropic}`. The package direction
is fixed by the cycle risk: `ago/files` imports no config package; `config/<provider>`
returns a `files.Client`.

Typed surface follows the wave brief, which drops two Python fields: `created_at`
(UploadedFile, inherited from FileIdInput) and `media_type` (FileContent, set by the
Anthropic read). Both survive in `Raw` (the provider's decoded response object); no
timestamp parsing is ported (`_created_at_to_float` has no destination field).

Upload keeps Python's two validations as sentinels (`ErrNoData` for nil data — AGo has no
`path=` parameter, callers read disk themselves; `ErrFilenameRequired` for the empty
filename). Python's provider options kwargs (`**provider_options`) have no Go counterpart.

Transport lives in the provider files, not in `internal/httpx`: the Files API needs
multipart, GET and DELETE, while httpx speaks POST+JSON only and was outside this wave's
file set. The retry loop mirrors httpx semantics (connection errors and 429/5xx,
exponential backoff capped at 8s, Retry-After capped at a minute). Non-2xx answers wrap a
per-provider `ErrFilesAPI` sentinel.

Wire details pinned against the AG2 tests (test/config/{openai,anthropic}/test_files.py),
which run against the real SDKs: OpenAI list pagination is `has_more`+`last_id` with
`?after=` on the following request; Anthropic list pagination is `next_page` with `?page=`
— not the `has_more`/`after` pair. OpenAI upload defaults an empty purpose to
`assistants`; Anthropic upload sends no purpose field at all — the argument only fills the
returned metadata (files.py:33-45 never forwards it). The Anthropic client targets
`{base}/v1/files` under `anthropic-beta: files-api-2025-04-14` (the Python client reaches
the API through the SDK's `beta.files` namespace; the SDK itself is not vendored in the
repo, so the header value is taken from the documented beta flag, not verified against SDK
source). An unset Anthropic timeout becomes 600s (files.py: `timeout if timeout is not
None else 600.0`).

`FileIdInput` message mapping is untouched: the Python files client has no direct mapper
binding — the only link is that `UploadedFile` subclasses `FileIdInput`, so a Python
uploaded file can be passed straight into message inputs. The Go struct deliberately
drops that inheritance (brief contract), so mappers keep failing fast with
`ErrUnsupportedInput` (D-017/D-020). Python's `OpenAIDecisionsConfig.create_files_client`
raises NotImplementedError and has no Go config to attach to — nothing to port.

Regressions: `ago/files` pins the wrapper validations and pass-through delegation;
`config/openai` and `config/anthropic` suites pin the multipart upload bodies (purpose
field present/absent, part content types, auth/beta headers), the two-call read order,
both pagination shapes (0/1/3 pages), delete, non-2xx → `ErrFilesAPI`, and
`CreateFilesClient` key resolution (explicit → env → `ErrNotProvided`).

## D-047 — 2026-10-30 — tools: hosted builtin tools become schema carriers; anthropic/gemini mappers map them

The nine builtin tools (`ag2/ag2/tools/builtin/`: web_search, web_fetch,
code_execution, file_search, image_generation, shell, memory, mcp_server,
tool_search) port to `ago/tools/hosted.go` as schema carriers: each is a `Tool`
whose `Schemas` returns one `ToolSchema` impl (provider-neutral type string =
Python's dataclass `type`), whose `Register` subscribes nothing, and whose options
carry the Python constructor kwargs. Provider mappers translate only what the
ported Python clients translate: Anthropic maps web_search (minus
`search_context_size`, which the Python mapper also drops), code_execution,
web_fetch (version gates `use_cache`/`response_inclusion` + the request-wide
`url_sources` tool check), memory, mcp_server (the `mcp_toolset` tools[] entry
plus `ExtractMCPServers` into the request's `mcp_servers` parameter — Python's
`extra_body` shape) and tool_search (regex/bm25 variants); shell is rejected with
the client-side explanation; file_search/image_generation fail fast (OpenAI
Responses-only, D-008). Gemini maps web_search→`googleSearch` (excludeDomains
only), web_fetch→`urlContext` (every field ignored), code_execution→
`codeExecution` and file_search→`fileSearch` (`store_names` required); wire keys
follow the google-genai SDK's camelCase REST aliases — the shape the ported
Python client actually sends. The Chat-Completions engine keeps rejecting every
hosted schema (Python's Completions `tool_to_api` does the same); documented at
the `ToolToAPI` branch.

Deviations. Python resolves `Variable`-annotated tool kwargs per turn
(`builtin/_resolve.py`); a Go carrier takes concrete values at construction and
`Schemas` ignores the context. Go's zero-value schema stands in for the Python
dataclass's default instance: an empty version/mode field resolves to the Python
default literal in both the carriers and the mappers (`WebSearchToolSchema{}`
plays `WebSearchToolSchema()`). Gemini-side, Python builds a ToolConfig with
`include_server_side_tool_invocations` and replays server-side invocations as
`GeminiServerToolCallEvent`; the AGo client does not parse grounding metadata, so
no ToolConfig is emitted and grounding travels only as ordinary response text —
request-side mapping is still meaningful, response-side replay is not ported.
Anthropic `server_tool_use` response blocks were already dropped (client comment);
unchanged. `ToolSearchTool` copies each member function schema with
`defer_loading` set (Python rebuilds `FunctionToolSchema` the same way) and
registers the members so their expanded calls are answered client-side.

Regressions: `ago/tools/hosted_test.go` (golden schema JSON, defaults, snapshot
isolation, tool_search deferral/registration), `config/anthropic/hosted_test.go`
(golden server-tool blocks, version gates, url_sources resolution incl. the
mcp_toolset configs path, `ExtractMCPServers`, httptest wire body with
`mcp_servers`), `config/gemini/hosted_test.go` (golden blocks, mixed-request
ordering, unsupported set, httptest wire body), `config` `TestToolToAPIUnsupported`
extended with a real carrier.

## D-048 — 2026-10-30 — Dependency policy: optional subsystems may take dependencies, but only outside the core

Owner decision, extending D-003. The core module `ago` keeps **zero external
dependencies** — unchanged hard rule; every package under `ago/` (including the newer
`ago/files`, `ago/plugin`, `ago/mcpui` slices and the hosted builtin tools) is
stdlib-only in this wave: `ago/go.mod` and `examples/go.mod` carry no `require`
entries (verified 2026-10-30), the Files clients speak raw `net/http` multipart and
`ago/mcpui` reproduces the mcp-ui-server wire shapes without the SDK (D-045/D-046).

What is new is the escape hatch and its shape, mirroring AG2's extras policy
(`optional_import_block`, ag2/ag2/_import_utils.py:93 — third-party-SDK features live
behind an optional import and ship as an extra, never in the required install): a
subsystem that genuinely needs a dependency it cannot reproduce over the stdlib (a
vendor SDK, a SQLite/Redis driver — the D-011 blockers) may take it, but is packaged
**outside the core** — a separate Go module with its own go.mod, `replace`-tied from
the repo root like `examples/` already is. The core's dependency surface stays empty
for everyone who does not import the optional module. A `require` inside any core
`ago/*` package still demands a DECISIONS entry per AGENTS.md rule 2; none exists
today.

Completes the hosted-tool provider matrix of D-047 (checked against the mappers):
**anthropic** maps web_search, web_fetch, code_execution, memory, mcp_server and
tool_search, and rejects shell (client-side bash explanation),
file_search/image_generation (OpenAI Responses-only)
(config/anthropic/mappers.go:426-491); **gemini** maps web_search→googleSearch,
web_fetch→urlContext, code_execution and file_search (store_names required), and
rejects every other hosted type (config/gemini/mappers.go:250-295); the shared
chat-completions engine (openai/zai/mistral/dashscope) rejects every hosted schema
(config/openaicompat.go:362-383); **ollama** rejects every hosted schema through its
own mapper — "Ollama speaks only function calling" (config/ollama/mappers.go:168-180);
the stub providers (xai/bedrock/typesafe) never reach a mapper — `Create()` fails
first. Every rejection wraps `config.ErrUnsupportedTool`.

## D-049 — 2026-10-30 — a2a: transport-level subset — the conversational A2AClient machinery is not ported

Supersedes the `a2a/` deferral in D-010 for this slice (owner directive): `ago/a2a`
ports the A2A wire surface — protocol types with camelCase tags and the `kind`
part discriminator, agent-card discovery at `/.well-known/agent-card.json` plus the
legacy `/.well-known/agent.json` alias, JSON-RPC 2.0 `message/send` /
`tasks/get` / `tasks/cancel`, a `TaskStore` interface with the memory
implementation, a `Client` (AgentCard/SendMessage/TasksGet/TasksCancel), and a
local JSON-RPC helper (not shared with `ago/mcp`). The served card always
carries the discovery fields a 1.x client resolves a transport through —
`protocolVersion` and `supportedInterfaces`; when `ServerConfig.Card` leaves
them unset the server fills in one JSONRPC interface over the card URL stamped
with the current protocol version "1.0" (parity: `build_card` always emits
`supported_interfaces` via `_build_interfaces` at `PROTOCOL_VERSION_CURRENT`,
and an SDK 1.x / AG2 client fails card resolution on an empty list —
`select_interface` raises `A2AInvalidCardError`). Config-declared interfaces
are served verbatim. (Blocking review fix, 2026-10-30: the field reduction the
first revision shipped silently was not a recorded deferral; the `tenant`
routing feature itself stays deferred — the field is wire shape only.)

Not ported (honest deferrals, each an `// AGO-DEVIATION` in code): Python's
`A2AClient` is an `LLMClient` (ag2/ag2/a2a/client.py:159) that ships full AG2
history per turn, drives streaming/polling to a terminal task, resumes
`input_required` continuations, syncs context variables, and executes
client-side tools through the `urn:ag2:client-tools:v1` extension — none of that
turn machinery exists in Go, so `ago/a2a.Client` is a transport-level client and
no `Tools()` adapter ships (Python's A2AClient is not a tool either, it is an
LLM provider). Also deferred: the REST and gRPC transports, push notifications,
card signing/verification, `validate_input_modes`, cluster mode
(`VersionedTaskStore`/`TaskEventStream`), tenants, and the reduced task-state
machine (submitted/working/completed/failed/canceled only — no
input-required/auth-required/rejected).

Supplemented 2026-10-30 (D-067, same day, owner directive): the second a2a wave
closed most of this entry's deferrals — push notifications, the
`urn:ag2:client-tools:v1` extension with client-side execution drivers,
context-variable sync and HITL over the wire, the REST transport, and the
declarative security schemes of security.py. Two amendments to the paragraphs
above: the task-state machine now also carries `input-required` — the
client-tools and HITL continuations are meaningless without a state that says
"answer pending on the caller", Python parks there too (executor.py
`requires_input`); and the conversational machinery is only partially closed —
`Client.RunClientTools`/`RunWithHumanInput` drive input-required continuations
through callbacks, but full AG2 history shipping and reconnect/resubscribe stay
deferred. Still deferred, and **gRPC stays deferred permanently**: JSON-RPC +
REST already carry the full protocol semantics — the gRPC binding is the same
method vocabulary over a different wire encoding, and serving it requires
protobuf codegen (protoc) plus the `google.golang.org/protobuf` dependency,
which the zero-dependency core forbids (D-048); there is no AG2 behavior
reachable through gRPC that JSON-RPC/REST do not also reach. Also still
deferred: `validate_input_modes`, cluster mode, tenants, card JWS
signing/verification (D-067 — no stdlib JWS/JWT), and the `a2a:tenant` /
`TENANT_VARIABLE_KEY` per-call routing (wire shape only).

## D-050 — 2026-10-30 — a2a: message/stream is a two-event SSE; client-side streaming is not ported

The server implements `message/stream` as Server-Sent Events with exactly two
JSON-RPC-wrapped `status-update` results: `working` (`final=false`) then the
terminal `completed`/`failed` (`final=true`, reply attached on
`status.message` — the shape AG2's client absorbs from the completed
transition, ag2/ag2/a2a/client.py:561-566). Chunked text artifacts, artifact
updates, `tasks/resubscribe` reconnects, and mid-stream cancels are not
modeled: the Go `Server` handler contract is one synchronous
`Handle(ctx, *Message) (*Message, error)` call, so the stream can only report
its terminal outcome. The `Client` does not consume streams at all — it uses
`message/send` only. `message/send` records the exchange as a Task
(completed/failed) in the shared store before answering, which is what makes
`tasks/get`/`tasks/cancel` meaningful against a synchronous handler.

Supplemented 2026-10-30 (D-067): the two-event shape now also carries a third
outcome — `input-required` as the terminal event, when the configured
`HandleClientTools` parks the turn for client execution or a HITL question
(the `working` event still precedes it; the task record is stored in the
parked state, so the continuation can resume it by id). The continuation loop
itself still rides `message/send`, not the stream — the stream stays a
one-shot projection of one synchronous turn.

## D-051 — 2026-10-30 — eval: the Trace is event-sourced, not OTEL-reconstructed (ago/eval supersedes the D-010 deferral for this slice)

`ago/eval` ports the eval harness as produce-then-grade: `Run(ctx, agent, suite)`
asks the agent per `Case`, rebuilds a `Trace` from the reply's stream history
(`BuildTrace` — `ModelResponse` → `LLMCalls` paired with the `model_call`
`UsageEvent` emitted just before each, `ToolCallEvent`-family → `ToolCalls`
with results/errors matched by call id), scores it, and tallies a `RunResult`
(`SaveJSON`/`LoadJSON`/`Compare`).

The core architectural difference from Python: **AGo builds the Trace directly
from the event log, with no OpenTelemetry anywhere.** Python's `run_agent`
wraps the turn in `TelemetryMiddleware` → in-memory span exporter →
`readable_spans_to_trace`, and `evaluate_traces` grades traces from OTEL /
Tempo / OTLP sources — one span→Trace path on purpose. AGo's event log is
already the durable, ordered record of exactly what those spans captured
(`agent/turn.go` emits `UsageEvent` before the response; tool events land on
the stream), so reconstructing from events removes a dependency and a lossy
projection instead of porting it: stream-only events Python's spans never see
are visible here too. Trace stores the raw `Events`, so `Trace.Tokens()` sums
`UsageEvent`s through `ago/usage` (Python parity: response usage excluded to
avoid double counting), and — unlike Python's store, which drops events on
load — `SaveJSON`/`LoadJSON` round-trip events through the `@type` registry,
keeping token math alive on loaded runs.

Scorers: `Scorer{ Name; Score(ctx, trace) (Score, error) }` with
`Score`-as-data (`Name/Value/Passed/Details`); boolean scorers carry 1.0/0.0 so
`Threshold` and run math treat them uniformly. `Correctness` (final-answer vs
expected; exact/casefold/contains, zero = casefold, the Python default),
`TokenBudget` (per-trace input+output ≤ max — Python's `token_budget`; the
Python cost scorer is this budget), `ToolsCalled` (any/all over a name list),
`Threshold` (numeric gate ≥ min, inner-name preserving, ungradeable → fail),
`LLMJudge` (one headless client call, JSON verdict `{score, reason}`, clamped
to [0,1], pass = ≥ 0.5), `Pairwise` (dual-order position swap, conservative
tie on flip). A scorer error never aborts a run: the runner records it as a
failing score (Python: `Feedback(score=None)`, "scorer raised").

Deferred (Python parity gaps, all recorded here): OTEL/Tempo/OTLP trace
sources and `evaluate_traces` (superseded by the event path above); the
`@scorer` DI decorator and injectable-params introspection (a typed Go
interface replaces function-signature reflection); the `attribution` scorer;
run-level machinery — repeats, per-task model configs, `BudgetThresholds`,
concurrency (AGo runs cases sequentially), eval lifecycle events on a Stream,
`RunDiff` regression diffing (AGo ships the pairwise win-rate tally
`Compare`, ties counting 0.5, Python parity); `Suite.from_jsonl`, tags and
metadata; the judge's `response_schema` structured output + validation retries
(a bare client call with JSON parsing instead, so a non-JSON verdict is a
recorded fail rather than a re-ask); pairwise single-order mode, Wilson CI and
Cohen's kappa.

## D-052 — 2026-10-30 — acp: both protocol roles over newline JSON-RPC; the session store, guard and mediated filesystem stay deferred

`ago/acp` ports the Agent Client Protocol as two stdio roles in one package
(supersedes the D-010 deferral for this slice, owner directive), stdlib-only,
transport identical to `ago/mcp` stdio — JSON-RPC 2.0, one message per line,
no Content-Length framing, 8 MiB scan cap.

**Serve role** (`Serve(ServeConfig)`, Python parity `ag2/ag2/acp/agent.py`
ACPAgent + executor): `initialize` (version answered as `min(client, ours)`,
truthful capabilities — text-only `promptCapabilities`), `session/new`
(uuid4 id; `cwd`/`mcpServers` recorded as context, never acted on), and
`session/prompt` running `cfg.Handle(sessionID, userText, emit)` with each
`emit` pushing one `agent_message_chunk` `session/update`; the handler's final
text goes out as one more chunk only when it differs from what was already
streamed (executor parity: `_forward_updates` + `_send_final_text`), so a
non-streaming handler still delivers. A handler error is a JSON-RPC internal
error carrying the reason; an unknown session is `-32002` "acp-session:<id>"
(parity: `resource_not_found`). `ServeAgent(a *agent.Agent, cfg)` builds the
handler: per-session conversation continuity (first prompt `Run`, later
prompts continue the stored reply), `ModelMessageChunk` deltas streamed live.

**Client role** (`NewBridge/SessionNew/Prompt/Close`, Python parity
`ag2/acp/bridge.py` + `client.py`): subprocess launch + initialize handshake,
one serialized exchange at a time under the Bridge lock; `Prompt` returns the
`agent_message_chunk` text accumulated during the turn. Agent→client requests
are answered inline from the policies — `Permission(toolCall, input)` selects
the agent's first `allow_once`/`allow_always` option (parity:
`permissions.resolve_permission_option_id`), `Elicit(message)` accepts with the
answer or declines; a nil policy or false denies/declines and nothing is ever
fabricated. The serve side mirrors this: `Session.RequestPermission` consults
`cfg.Permission` first and synthesizes the reject decision locally (deny never
touches the wire), `Session.Elicit` gates the question on `cfg.Elicit` and the
client's accept carries the answer.

Wire dialect — the SDK's spelling is what this package emits: the
session/update block under **`content`** (parity: `mappers.py`
`block_text(update.content)`) and the session/request_permission answer nested
**`{"outcome": {"outcome": "selected"|"cancelled", "optionId": …}}`** (parity:
`bridge.py` `RequestPermissionResponse(outcome=AllowedOutcome(...))` /
`DeniedOutcome`). An earlier revision of this package emitted `contentBlock`
and a flattened `{outcome, optionId}` with `outcome_selected`/
`outcome_cancelled` values, citing a porting-brief pin; a blocking review
found the brief pins no dialect and the shapes are unreadable to standard ACP
peers (an SDK client reads `content` — chunks arrive empty otherwise — and a
real agent refuses a flat answer by schema), so the emission moved onto the
SDK forms (2026-10-30). Input stays tolerant: `contentBlock` updates and flat
answers are still understood, and a zero `Decision` emits the cancelled
outcome — an absent selection denies, never allows. Deny answers select a
`reject_once`/`reject_always` option the agent actually offered (Python
parity: `resolve_permission_option_id` returns the reject option; a
synthesized id the agent never offered would be refused too). Elicitation is
text-only (no form/url modes, no per-property rendering or coercion, no
MAX_ATTEMPTS loop — parity: `elicitation.py` `_resolve_form`).

Deferred (recorded here as the honest gaps): `session/cancel` and the
`cancelled` stop reason; `session/load` + history replay (`history_to_session_updates`);
the multi-session store (LRU cap, idle TTL, retention, per-session queues —
Serve runs one request at a time; per-session state is a plain map); auth
(`auth.py`); thought/plan/tool-call/usage updates and `stream_thoughts`;
the mediated filesystem (`fs/*`), terminals (`terminal/*`) and `tool_gateway.py`;
`guard.py`; `session.py`/`sessions.py` model pickers and high-water-mark prompt
shipping; `config.py`'s remote/transport variants (AGo's Bridge is subprocess-stdio
only).

Tests are network-free: the Bridge suite re-execs the test binary as a fake
ACP agent (`TestMain` process-helper pattern, `mcp/stdio_test.go` template —
two-chunk prompt, outbound permission + elicitation round-trips, policies on
and off); the serve suite runs `serveIO`/`serveAgentIO` over `os.Pipe` against
an in-process fake client (initialize/new/prompt shapes, final-text-once
dedup, handler-error mapping, permission deny-synthesized/allow/reject/
cancelled, elicitation gate/accept/decline, ServeAgent streaming via a
chunk-publishing client double and non-streaming via `agotest.NewTestConfig`).

Note (docs sweep, 2026-10-30): the deferral/dialect comments in `ago/acp`
cited "D-049" for a while — D-049 is the a2a supersession, the same numbering
drift the D-041 entry documents for the earlier wave; corrected to cite this
entry when the wire dialect was moved onto the SDK forms.

## D-053 — 2026-10-30 — ag_ui: one synchronous exchange per run; the held-turn interrupt machinery is not ported (ago/agui supersedes the D-010 deferral for this slice)

Owner directive, same wave as D-049/D-051/D-052. `ago/agui` ports the serving side
of `ag2/ag_ui/` against the ag-ui-protocol **1.0** wire vocabulary (read off the
SDK's generated models, `ag_ui/_generated/models.py` — the same source the Python
module builds on; THINKING_* left the protocol in 1.0, reasoning travels as
REASONING_*). Ported: the event structs with camelCase tags and the SDK's
omit-when-unset serialization (`events.go`); `RunAgentInput` parsing with
unknown-role stripping and pre-run base64 refusal (`run_input.py`); the projection
(`stream.py` `map_events_to_ag_ui`): ModelMessageChunk → TEXT_MESSAGE_*,
ModelReasoning → REASONING_* spans closed by the next non-reasoning event,
ModelResponse → TEXT_MESSAGE_END or the whole-answer TEXT_MESSAGE_CHUNK,
ToolCallEvent → TOOL_CALL_START/ARGS/END closed before execution,
ToolResultEvent → TOOL_CALL_RESULT (lone text as a string, else parts),
ClientToolCallEvent → TOOL_CALL_CHUNK, task lifecycle → SUBAGENT_*, usage grouped
per (provider, model) with the anthropic/bedrock cache correction and recomputed
totals (`usage.py`), pendingToolCallIds on the success outcome; `Handler`/
`ServeAgent` over `net/http` (GET capabilities, POST → SSE `data: <json>\n\n`,
400 before any stream on an unparseable body), protocol-major refusal
(`UNSUPPORTED_PROTOCOL_VERSION` before RUN_STARTED), cancelled-run outcome on
server-side cancellation (`drive_run`'s CancelledError → cancelled outcome).

**Not ported — the held-turn machinery (`interrupts.py`, ~1200 lines):** a served
turn in AGo lives inside its HTTP exchange; there is no `ServedTurns` registry,
no retention/eviction, no RUN_FINISHED interrupt outcome, no resume entries, no
per-interrupt proofs, no tool-approval or subagent-suspension wiring, no
capability declaration of `humanInTheLoop.interrupts`. An agent that asks a human
a question fails the run and the client reads RUN_ERROR. REASONING_ENCRYPTED_VALUE
(Gemini thought signatures) is deferred with it — AGo has no Gemini
thought-signature plumbing to project. STATE_SNAPSHOT/STATE_DELTA: the state
merge (`client state | agent variables | run variables`) needs agent internals
AGo does not expose (variables, config, hitl hook, tasks config are all
unexported on `agent.Agent`), so ServeAgent emits no snapshots and declares no
`state` capability; handlers with their own `Run` callback can emit
STATE_SNAPSHOT through the event vocabulary. Subsequent deferrals of the same
shape, each with an AGO-DEVIATION comment in the code: prompt-level inputs
(system/developer messages, the shared-context block from `RunAgentInput.context`)
fold into the opening transcript request instead of per-run prompt fragments
(Python passes them to `agent.ask` as prompt; AGo's fragments are fixed at agent
construction); content-part metadata is dropped (ago inputs have no metadata
field); provider-tagged file handles are always skipped, since the served agent's
provider is not readable (Python skips only foreign-tagged ones); the
client-tool-name suppression of Python's projection is replaced by call-id
suppression, because AGo's executor sends the model's ToolCallEvent before the
client tool answers it — the reverse of Python's dispatch order — and the
gathered `ToolResultsEvent` is not projected (its results already went out
individually; Python has no such batch on its stream). Input parsing accepts the
camelCase wire spellings only, where the SDK also accepts snake_case attribute
names. ServeAgent documents that the served agent must be usable with a fresh
stream per turn: a `WithStreamBase` agent would have every request's history
seeded over the shared transcript.

Identity.Type says **"ago"** where Python says "ag2" — the serving framework's
own name. Imports: `ago/agent` and `ago/events` per the brief, plus `ago/stream`
(the projection subscribes through `stream.SubscribeOptions` — the same edge
`agent` already has, no new cycle) and `ago/internal/uuid` for message ids (the
same helper `ago/events` uses).

Tests are network-free (`httptest` + `agotest.NewTestConfig`): GET capabilities
(shape and the omitted sections), POST streaming run over a chunk-publishing
client double (RUN_STARTED → TEXT_MESSAGE_START/CONTENT×N/END → RUN_FINISHED with
camelCase frames), agent failure → RUN_ERROR, tool-call projection with the
not-found answer, usage on RUN_FINISHED, input history seeding asserted through
`TestConfig.Calls`, 400 on an unparseable body, protocol-major refusal,
cancelled outcome, handler-callback passthrough (STATE_SNAPSHOT/CUSTOM/Raw) and
the not-ended-run RUN_ERROR repair; plus unit coverage of the event JSON forms,
transcript round-trip of `AGUIEvent`, and every projection branch.

## D-054 — 2026-10-30 — release: tagged asset names, redirect-based latest resolution, checksums; install.sh + release.yml

The release pipeline is two files: `.github/workflows/release.yml` (a `v*` tag push
builds `./cmd/ago` for linux/darwin/windows × amd64/arm64 and publishes the release)
and `install.sh` (curl|sh-safe POSIX installer). The asset contract the installer
depends on:

- `ago_<tag>_<os>_<arch>.tar.gz` on unix, `ago_<tag>_<os>_<arch>.zip` on windows;
  the archive root holds the binary as `ago` / `ago.exe` (the installer moves it to
  the target name, no path juggling inside archives);
- `checksums.txt` in `sha256sum` output format beside the archives; a missing file
  degrades to a warning, a mismatch aborts the install.

Two deliberate readings of the brief. First, `/releases/latest/download/<asset>`
cannot resolve "latest" for assets whose name carries the version — GitHub requires
the exact asset name — so the installer resolves the latest tag by following the
redirect of `.../releases/latest` (the final URL contains `/releases/tag/<tag>`).
No API call, no token, which is what the brief actually asked for; the asset URL
itself stays `.../releases/download/<tag>/ago_<tag>_<os>_<arch>.<ext>` as sketched.
Second, the release is published by a separate job that collects per-target build
artifacts: a matrix shard cannot compute `checksums.txt` over all six archives, and
six concurrent `gh release create` calls would race on the same release. The
installer carries one test hook, `AGO_INSTALL_REPO_URL` (release base URL override),
used to exercise download, checksum verification and both archive layouts against a
local server that mimics the GitHub layout; normal use always hits GitHub. When
curl and wget are both absent, or the download fails, it falls back to
`git clone --depth 1` (+ `--branch <tag>` when the tag is known) and
`CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.Version=<tag>" ./cmd/ago`
— the same stamp contract as the workflow (`main.Version` in `cmd/ago/main.go`;
the installer's `dev` default matches the source default).

## D-055 — 2026-10-30 — module: canonical Go module path `github.com/luckystrker/ago`; go.mod at the repo root

Owner directive: the module moves from the bare path `ago` to the canonical
`github.com/luckystrker/ago`, with `go.mod` at the repository root. The motivation is
distribution: a module path without a domain cannot be fetched — `go get` and `go install`
resolve a module path through the Go module proxy to a hosted repository, and pkg.go.dev
does not index non-domain paths — so under the old layout a consumer had to clone the repo
and wire a `replace` (plus a `go.work`) by hand. With the canonical path the standard flows
work: `go get github.com/luckystrker/ago`, `go install github.com/luckystrker/ago/cmd/ago@latest`,
and a pkg.go.dev reference page for the module.

What moved:

- Framework packages from `ago/<pkg>` to directories at the repo root; import paths become
  `github.com/luckystrker/ago/<pkg>` — the earlier "import paths are unaffected" statement
  is void, every consumer import changes. The port itself is unaffected: package names,
  exported APIs and the wire vocabulary are untouched; a module-path rename moves code and
  changes import strings, nothing else.
- Examples from the separate `agoexamples` module into `examples/` inside the same module;
  `go.work` and the `replace` directive are gone. All gates run from the repo root:
  `go build ./...`, `go vet ./...`, `go test ./...` (42 packages `ok`, 0 failures, 0
  skipped), plus `go build ./examples/...` / `go vet ./examples/...` and an offline `go run`
  of the thirteen examples — re-verified for this entry.
- New `cmd/ago` CLI helper (`version` / `doctor` / offline `demo`) gives `go install` a
  concrete target; its `-ldflags "-X main.Version=..."` stamp contract is shared with
  install.sh and release.yml (D-054).

Docs sweep in the same change: README (Install section, pkg.go.dev badge, Quick start
imports, status bullets), root AGENTS.md, docs/ARCHITECTURE.md, docs/GUIDE.md,
docs/MIGRATION.md (incl. a re-run of its §7 gates), docs/PORT_STATUS.md (incl. its Gates
block) and .github/workflows/ci.yml. Historical decision records keep the old `ago/...`
spellings — they are records of the layout they described.

## D-056 — 2026-10-30 — tui: textual.py ported as a stdlib line REPL (ago/tui)

Owner directive: the TUI ships without a TUI framework. `ag2/ag2/textual.py` is a
textual `App` (Header / ScrollableContainer / Input / Markdown widgets); AGo has zero
external dependencies in the core module (D-003), so `github.com/luckystrker/ago/tui`
rebuilds the same conversation surface as a plain line loop: `REPL(a *agent.Agent,
in io.Reader, out io.Writer, opts ...Option) error` reads lines, sends each as the
next user turn and prints the answer. The first turn goes through `Agent.Ask`, every
later turn continues the same conversation through `AgentReply.Ask` — Python parity
of `self.conversation.ask(text) if self.conversation else self.agent.ask(text)`, and
the scripted test asserts the second model call already sees the first user turn.
Commands `/help` and `/exit` (plus end-of-input) end the loop; a failed turn is
reported on the output and the loop continues instead of dying.

Streaming (Python parity: the `ModelMessageChunk` / `ModelReasoning` sub_scopes
appending into the assistant block): the `WithStreaming()` option drives the turn
through `AgentRun` and subscribes `ModelMessageChunk` on the run's stream, writing
each delta to the output as produced; the final body is printed only when it differs
from the joined deltas — the same final-text-once rule `acp`'s ServeAgent uses. A
non-streaming client simply produces no deltas and the final body prints as usual.

Deferred: the textual App surface itself (widgets, markdown rendering, the
"Thinking…" block that `ModelReasoning` deltas fill before the answer replaces it) —
no TUI framework in the core; a reasoning-echo option can be added later without an
API change.

## D-057 — 2026-10-30 — live: the HTTP cascade slice ported (ago/live); the realtime stack stays deferred

Owner directive: port `ag2/ag2/live/cascade.py` as the plain STT→LLM→TTS HTTP chain,
without the realtime-websocket machinery. `github.com/luckystrker/ago/live` ships
`Cascade{STT Transcriber; LLM func(ctx, text) (string, error); TTS Synthesizer}` with
`Handle(ctx, audio, mime, voice) (replyText, audioOut, mimeOut, err)`: transcribe,
answer, synthesize, with stage failures wrapped `live: transcribe` / `live: llm` /
`live: synthesize` over the cause. A whitespace-only transcript ends the turn
silently (Python parity: cascade.py `_turn`'s `if not text.strip(): return`).

Scope deltas against the Python `CascadeConfig`:

- The Python class implements `RealtimeConfig` and is driven by `LiveAgent` inside a
  conversation context: turn detector (`SilenceTurnDetector`), barge-in, playback
  start/complete events, a capped tool loop and streaming TTS via
  `StreamingTTSConfig`. The Go `Cascade` is one synchronous function per spoken turn
  per the brief; those realtime pieces stay deferred — they need websocket clients
  and sound-device I/O, both outside the stdlib-only core (extends the live/ deferral
  D-010 carried).
- The LLM stage is `func(ctx, text) (string, error)` instead of a `ModelConfig`:
  any driver fits (an agent reply body, a config client, a stub in tests). A thin
  adapter over `agent.Agent` can be added later without an API change.
- Ready-made OpenAI stages over `net/http`, no SDK: `OpenAITranscriber` (POST
  `/audio/transcriptions`, multipart with the part's real Content-Type and a
  filename extension derived from the mime — the endpoint infers the container from
  it; `response_format=text`; optional `language`). AGO-DEVIATION: Python streams
  transcript deltas (`TranscriptionChunkEvent` / `TranscriptionCompletedEvent`) onto
  the context; here one blocking request — the cascade shape has no live event stream
  to publish deltas to. `OpenAISynthesizer` (POST `/audio/speech`, JSON body).
  AGO-DEVIATION: Python pins `response_format="pcm"` for its realtime player; here
  `Format` is a field (default `"mp3"`, the API default) and the reply reports the
  media type (response Content-Type, else the type implied by the format). Python's
  `STTTranslationConfig` and the elevenlabs/gemini/mistral providers are not ported.

Provider errors surface as a shared sentinel `live.ErrAPI` (status + body snippet);
an incomplete `Cascade` (any stage missing) fails fast with `live.ErrIncomplete`.
`events/voice.py` stays unported alongside the realtime stack.

## D-059 — 2026-10-30 — contrib: the SQLite/Redis knowledge and stream backends port as optional modules (contrib/knowledgesqlite, contrib/knowledgeredis, contrib/streamredis)

(Numbering correction, same drift the D-041 entry documents: this entry briefly held
"D-058" concurrently with the mcpserver entry, which kept the number via its earlier
citations — the contrib entry moves to the next free number.)

Supersedes the D-011 deferral for the sqlite/redis knowledge backends and the
`streams/redis` deferral for this slice (owner directive), under the D-048 packaging
policy: each backend is its own Go module in `contrib/<name>/` —
`module github.com/luckystrker/ago/contrib/<name>`, `require github.com/luckystrker/ago
v0.0.0` + `replace github.com/luckystrker/ago => ../..`, external dependencies allowed
only there. The root module stays at zero `require` entries; `contrib/` is invisible to
the root gates (`./...` skips nested modules).

- `contrib/knowledgesqlite` — `knowledge.Store` over `modernc.org/sqlite` (pure Go, no
  cgo — the machine has no C toolchain). One `files(path, data, updated_at)` table,
  prefix matching via `substr(path, 1, ?) = ?` (never LIKE — Python's own rationale).
  Append runs read-modify-write inside a `BEGIN IMMEDIATE` transaction (`_txlock=
  immediate` DSN), the exact atomicity Python's `_sync_append` pins: appends from
  concurrent goroutines, other instances and other processes never lose entries.
- `contrib/knowledgeredis` — `knowledge.Store` over `redis/go-redis/v9`, client
  injected (`NewStore(rdb redis.UniversalClient, ...)`). Content keys
  `{prefix}:{path}`, companion sorted-set index `{prefix}:__index` + version counter
  (Python `ag2:knowledge` defaults); Append is the Python `_APPEND_SCRIPT` Lua port —
  APPEND+INCR+ZADD in one server-side step, atomic across connections on one node.
- `contrib/streamredis` — `stream.Storage` over go-redis: one list per stream at
  `{prefix}:{stream_id}` (default `ag2:stream`), SaveEvent RPUSH, SetHistory a
  transactional DEL+RPUSH pipeline, DropHistory DEL — the Python storage.py shape.
  Events round-trip through `events.MarshalEvent/UnmarshalEvent` ("@type" JSON).

AGO-DEVIATION (all three, one decision): Python's `on_change` attaches a
`PollingChangeWatcher` (500 ms) and the sqlite/redis backends expose
`list_versions_under` for it. AGo's `Store.OnChange` contract is in-process callbacks
(MemoryStore/DiskStore parity), so the contrib backends notify only mutations through
the same instance and carry no version column — the sqlite table uses `updated_at`
metadata instead of Python's `version`. Not ported for the same reason: Python's
pickle serializer (streamredis is "@type"-JSON only — pickle is arbitrary-code
execution against anyone with Redis write access), the URL-based constructor
overloads (client injection only). The core protocol methods carry no `context`;
the Redis clients run on `context.Background()`.

Tests are stdlib `testing`, offline: `github.com/alicebob/miniredis/v2` stands in for
the Redis server (including Lua EVAL), SQLite tests run on `t.TempDir()` files —
including Python's cross-instance atomic-append scenario with 8/20 concurrent
appenders. Gates: root suite green with the root `go.mod` untouched, plus
`go build ./... && go test ./...` inside each contrib module.

## D-058 — 2026-10-30 — mcp: the serving side ported as a stateless subset (ago/mcpserver supersedes the D-010 deferral for this slice)

Owner directive, same wave as D-049/D-052/D-053. `ago/mcpserver` ports the serving
side of `ag2/ag2/mcp/` (server.py, executor.py, tools.py, transport.py) as a
stateless MCP server exposing one Ask bridge as the conversational `ask` tool,
with optional deterministic tools (`ServerConfig.Tools []tools.Tool` — the
`mcp_tool` parity: schema plus function) served next to it. `NewServerForAgent`
bridges a `*agent.Agent` directly (every ask is one blocking `Run`/`Start`/
`Result` turn on a fresh stream, reply body as text content — the agent must be
usable with a fresh stream per call, same caveat as agui.ServeAgent).

Wire subset: `initialize` (revision negotiation over the single-element support
set `{2024-11-05}` — the same revision the ago/mcp client offers; an unknown
requested revision is answered with this one), `tools/list` (fixed set, ask
first, no cursor), `tools/call` (unknown name → JSON-RPC invalid-params, the
executor's MCPError arm; empty `message` guard and the `conversation`-argument
refusal mirror the executor's sessions=False path), `ping`, and
`notifications/initialized` (as notification — never answered — or as request,
confirmed empty). The tools methods refuse before a handshake ran on the server
process; the ask tool's inputSchema carries `message` (required) and `context`
(folded in front of the message as `Context:\n…\n<message>`, the two-input
_build_inputs shape collapsed into the one string the Ask bridge takes).

Transports: `Stdio()` — newline-delimited JSON-RPC on stdin/stdout, the same
no-framing dialect as the ago/mcp stdio client (garbled lines skipped; one
request at a time); `Handler()` — streamable HTTP (POST, notifications answered
202, GET held open as a silent server-to-client SSE stream with a keepalive
comment frame, DELETE refused alongside other methods).

Result mapping (Python parity: mappers.py): reply text / function-tool result
parts → content blocks — TextInput→text, BinaryInput→image/audio (media-type
prefix, base64) or an embedded `resource` blob, UrlInput→`resource_link` (the
serving tool's name fills the required `name`), DataInput and future variants →
JSON text, empty result → one empty text block; failures become isError content
with the message only (tool_error / the executor's exception arm). Deterministic
tools are invoked through the Tool machinery itself: registered on a scratch
memory stream, the call event sent, the result event collected from the
synchronous dispatch — **AGO-DEVIATION**: Python calls the handler function
directly; Go's `tools.Tool` interface has no direct call.

**Not ported — sessions/conversations** (`sessions.py`): the server is
stateless, no conversation handle is minted or accepted, `Mcp-Session-Id` is
never issued and an incoming one ignored. **Pause/resume** (`pause.py`): a
served turn lives inside its exchange and cannot come back as
`InputRequiredResult`. **Security** (`security.py`): no OAuth 2.1 resource-
server routes, no bearer middleware, no DNS-rebinding protection (TransportConfig's
validation deltas defer with it). **MCP Apps UI** (`apps.py`): no `_meta` ui
bindings, no SEP-2133 extensions. **stream_progress**: the string-in/string-out
Ask bridge carries no stream to forward ModelMessageChunk/ToolCall events from,
so no progress/log notifications. **resources/prompts** (`resources.py`,
`prompts.py`): not advertised. **Argument-schema validation** (`mappers.py
input_validation_error`): deferred; the executor's empty-message guard stands
in. **POST answers are always application/json** (Python's `json_response=True`
mode) — the per-request SSE answer branch is not served; batch requests are
refused. Tool-name conflicts and hosted (non-function) schemas fail
construction (`ErrConflict`, parity with MCPToolNameConflictError).

Tests are network-free: the real ago/mcp client drives the real server over
`httptest` (HTTP) and over a re-exec'd test binary on stdio (the
mcp/stdio_test.go TestMain helper pattern with the roles flipped — the child is
the system under test): handshake info, tools/list order and schema, ask text
and context folding, echo through the function-tool machinery, fail →
ErrToolCall, unknown tool → ErrProtocol, negotiation echo/downgrade, the
before-initialize gate, ping/202-notifications/batch/garbage refusals, the GET
stream shape, construction refusals, the never-answering-tool guard, and the
content-mapping branches (unit).

## D-060 — 2026-10-30 — config/openai: the Responses API and the Decisions API are ported; hosted tools have their OpenAI client

Supersedes the D-008 deferral of `openai_responses_client.py` /
`openai_decisions_client.py` for this slice (owner directive), and closes the
hosted-tool gap D-047/D-048 documented: the Chat-Completions engine rejects
every hosted schema with "hosted tools need the Responses API, which is not
ported" (config/openaicompat.go) — that client now exists, so the hosted
schemas of `ago/tools/hosted.go` reach the wire.

**Responses** (`config/openai/responses.go`): `OpenAIResponsesConfig`
(NewOpenAIResponses + Provider/Model/Create/Clone/CreateFilesClient; defaults
mirror the Python dataclass: store and parallel_tool_calls on and always sent,
4 transport retries, OPENAI_API_KEY fallback) and `ResponsesClient`
(config.LLMClient) POSTing `{base}/v1/responses` over ago/internal/httpx with
SSE via ago/internal/sse. Input mapping (`responsesInput`) replays
ModelRequest (one user message per part; FileIdInput → `input_file.file_id`
with no filename — the API mutual-exclusion), ModelResponse (assistant text +
`function_call` items), ToolResultsEvent (`function_call_output`, single
input_text collapsing to a string), CompactionSummary, image/document
BinaryInput (data URLs, `detail` via VendorMetadata whitelisted
auto/low/high/original) and UrlInput (`file_url` without filename). Tools
(`toolToResponsesAPI`): function (top-level name, `defer_loading`),
web_search (search_context_size/user_location/filters.allowed_domains, plus
the historical `max_uses` the Python mapper keeps sending), file_search
(vector_store_ids/max_num_results/filters), code_interpreter (container auto),
shell (container_auto with network_policy / container_reference; a shell
without a hosted environment is rejected — client-executed bash stays out),
image_generation, mcp (`require_approval:"never"`, blocked_tools rejected,
AuthorizationToken → bearer header under the Headers-precedence rule) and
tool_search. Response side: `message` → ModelMessage, `reasoning` → summaries,
`function_call` → ToolCallEvent, hosted items (web_search_call,
code_interpreter_call, image_generation_call, file_search_call, mcp_call,
mcp_list_tools, shell_call + shell_call_output paired by call_id) →
BuiltinToolCallEvent/BuiltinToolResultEvent with the item's own outcome parts
and metadata; image_generation_call results also land in ModelResponse.Files.
Usage normalization lifts input/output/total tokens and the cached /
cache-write / reasoning detail tokens (normalize_responses_usage parity).
Response schemas become `text.format` (json_schema, strict, StrictifySchema);
`store:false` adds include reasoning.encrypted_content.

**Decisions** (`config/openai/decisions.go`): `OpenAIDecisionsConfig`
(NewOpenAIDecisions: model gpt-6-luna, boolean_threshold 0.5,
safety_identifier omitted unless set) and `DecisionsClient` POSTing
`{base}/v1/decisions`: the response schema is read as one DecisionSpec
(response.FromResponse) and becomes the single question (predicate / choice
with config-keyed descriptions / score levels requiring the 0..n-1 rubric);
instructions assemble as prompt + question_instructions + schema question
(string forms only); every tool is rejected before the request; the answer
(predicate thresholded by boolean_threshold, choice string, score snapped to
the nearest level) renders through DecisionSpec.Render into the ModelMessage,
whose Metadata carries the answer object minus type/name; refusals raise
DecisionRefusedError; usage normalizes like Responses; ModelResponse.Provider
is "openai-decisions".

**Replay carrier.** Python replays hosted items through vendor event types
(OpenAIServerToolCallEvent.item, OpenAIReasoningEvent.item, persisted
ProviderReplay anchors). AGo's event vocabulary has no vendor item slot, so
the raw wire item travels in BuiltinToolCallEvent.VendorMetadata["item"] and
(in line with Python, which keeps `item` only for shell outputs) a shell
result in BuiltinToolResultEvent.Result.Metadata["item"]; `responsesInput`
replays them (minus output-only fields such as `created_by`, skipping a
shell_call whose shell_call_output never arrived). **Deviations, all recorded
as such in code:** reasoning summaries surface as transient ModelReasoning and
reasoning items are not stored/replayed (events.ModelReasoning is transient
and carries no item slot — with store:false and reasoning models the API may
ask for the encrypted anchor AGo cannot resend); shell command/output delta
chunks are not surfaced (the finished items carry the content); prompt-cache
diagnostics are deferred (the vendor diagnostics event has no AGo carrier —
PromptCacheKey/PromptCacheOptions still travel; the client keeps no
last-response-id chaining state, so it stays stateless and concurrency-safe);
skills extraction/merge into the shell environment is deferred (no SkillsTool
in AGo); question_instructions carries strings only (the sequence-of-parts
form is deferred); websocket_base_url is not modeled (no websocket transport);
`Containers` (containers.py ContainerManager) stays deferred — nothing in the
ported responses client references it.

Regressions: `config/openai/responses_test.go` (config defaults, clone
semantics, non-stream and SSE round trips with a tool call, hosted web_search
wire schema + call/result events + second-call replay, input mapping incl.
FileIdInput, audio fail-fast, shell mapping, unsupported hosted schemas,
defer_loading, strict text.format, store:false include, usage normalization,
streaming reasoning + code_interpreter events), `config/openai/decisions_test.go`
(defaults, no-Files-API, choice/predicate/score round trips with wire
assertions on /v1/decisions, threshold semantics, refusal, unsupported schema,
tools rejected before the request, instruction assembly + input flattening,
inline-image mapping, needs-question guard). Whole-module gates re-run (see
PORT_STATUS).


## D-061 — 2026-10-30 — eval: attribution, scorer registry, Suite JSONL, RunDiff, Wilson/kappa stats, parallel cases (the D-051 deferrals close for this slice)

Owner directive, same wave as D-049/D-051/D-052. Six `ago/eval` additions, all
stdlib-only:

**Failure attribution** (`attribution.go`, Python parity:
`scorers/attribution.py`). `DetectMechanical` names the failures a Trace
carries on its face — the ask raised, a tool errored terminally with no answer
produced, or the run finished without an answer — and `FailureAttribution`
exposes them as a Scorer whose `Details["error_mode"]` is the categorical
verdict (Python: `Feedback.value`), so a run's failure-mode distribution reads
off the scores. For the semantic modes (incorrect answer, ignored constraint,
…) the LLM attributor reads the numbered trajectory over one headless client
call, under the same JSON-verdict discipline as `LLMJudge` (non-JSON verdict is
a recorded fail; a verdict with no `error_mode` degrades to "other" — Python:
`content()` returning None). To see the crash at all, `Trace` gained an `Err`
field (`json:"err"`, Python parity: `Trace.exception`), stamped by `Run` from
the ask failure; `CaseResult.Err` stays as-is. Deviations: the attributor sees
only the trace, so the first model call's input stands in for the task prompt
and the reference section of the Python prompt is dropped; `response_schema`
structured output + retries are not ported (D-051's judge deviation applies
verbatim). Note: the brief's phrase "shares of tool calls that influenced the
final answer" does not describe `attribution.py` — that scorer is failure
attribution, and its Python semantics are what shipped.

**Scorer registry** (`registry.go`, the `@scorer` DI stand-in). Where Python
introspects a scorer function's parameters and injects by name, AGo resolves a
scorer *name* plus a params map: `ScorerRegistry` maps names to
`ScorerFactory func(params map[string]any, scope CaseScope) (Scorer, error)`,
pre-registered with the built-ins (`correctness`, `token_budget`,
`tools_called`, `threshold`, `llm_judge`, `failure`); `SetClient` feeds the
client-backed ones. Built-ins reject unknown param keys (typos must not
swallow); user-registered factories validate their own. `CaseScope.Expected`
lets a data-driven `correctness` fall back to the case's expected answer — the
data twin of the in-code `Correctness{Expected: c.Expected}` idiom.

**Suite from JSONL** (`dataset_jsonl.go`, Python parity: `Suite.from_jsonl`,
reduced to the AGo Case fields). One case per line: `input` required, `name`
auto-fills `task-0000`… in dataset order, `expected` feeds the case and the
scorers' scope, `scorers` entries are a registry name or
`{"name", "params"}`. Blank lines skip; a UTF-8 BOM is tolerated
(`utf-8-sig` parity); errors carry file and line. Deviation: the Python
suite's name/source metadata and per-task tags have no slot on `Suite []Case`
and are dropped.

**RunDiff** (`diff.go`, Python parity: `results/diff.py`, reduced to what an
AGo `RunResult` records). `Diff(current, baseline)` lines the runs up by case
name and classifies each shared case regression/improvement/unchanged, lists
the only-in-* cases, and tallies per-scorer pass rates over the shared cases;
`DiffStrict` errors on mismatched suites (Python: `strict=True` raising
`RunsNotComparableError`); `Summary` renders the printable form. Deviation:
Python keys flips by (scorer key, task id) and detects content drift from the
tasks' `inputs`/`reference_outputs`; the AGo RunResult persists neither, so
the diff is per case and content drift is undetectable until per-case inputs
ride on the result.

**Stats** (`stats.go`, Python parity: `pairwise._wilson_ci` with z = 1.96 and
`pairwise._cohen_kappa` over the a/b/tie vocabulary, general labels welcome).
`WilsonCI` clamps to [0,1] and yields (0,0) for n = 0; `RunResult.PassRateCI`
wraps the run's pass rate; `CohenKappa` returns the raw rate plus the
chance-corrected kappa, with the degenerate expected-agreement-of-1 case
reporting 1 instead of dividing by zero. Golden tests pin values computed with
the Python reference formulas run verbatim.

**Parallel cases** (`run.go`, Python parity: `runner.py` `concurrency=4`).
`Run` takes variadic `RunOption`s; `WithConcurrency(n)` caps in-flight cases
(default `DefaultConcurrency` = 4, clamped to >= 1). Each goroutine writes its
own `CaseResult` slot — the result needs no locks and stays in suite order
regardless of completion order. `WithConcurrency(1)` runs inline on the
calling goroutine in strict suite order — the deterministic mode a per-case
scripted model double needs, since a shared script queue cannot serve
overlapping asks. Agent sharing is safe: every case runs on its own stream and
streams serialize their turns independently (`agent.Agent.locks`).

Regressions: `eval/attribution_test.go` (mechanical detector table, the
deterministic and LLM-attributed scorer paths, Trace.Err JSON round trip),
`eval/registry_test.go` (built-in factories incl. nested threshold and client
backing, param/typo rejection, user factories, concurrent use),
`eval/dataset_jsonl_test.go` (loading, scope fallback, end-to-end run,
error table, BOM/blank handling), `eval/diff_test.go` (movements, only-in
exclusion, strict gate, scorer rates, summary),
`eval/stats_test.go` (Wilson/kappa goldens, PassRateCI),
`eval/run_test.go` (overlap via an in-flight-peak tool, concurrency-1
serialization, clamp, Trace.Err stamping), plus the pre-existing suite.

## D-062 — 2026-10-30 — toolpacks: the ready-made tool packs port as ago/toolpacks (shell, local code runner, filesystem, search, skills)

Owner directive. `ago/toolpacks` builds the ready-made tools of ag2/ag2/tools/ on
the ago/tools framework; every constructor returns a `tools.Tool` implemented over
`FunctionTool`/`Toolkit` (the FunctionTool delegation Python uses). The exec
result shape is Python's verbatim: combined stdout+stderr, whitespace-trimmed,
truncated at max_output with a trailing notice; a non-zero exit appends
`[exit code: N]`, a timeout reports 124 with Python's sentence, a missing command
127 (sandbox/local.py `_run_subprocess`, adapter/shell.py `_format`).

- **Shell** (`shell.go`, shell/tool.py SandboxShellTool over sandbox/local.py):
  `NewShellTool` runs commands through the platform shell (`cmd /c` on Windows,
  `sh -c` elsewhere; Python hardcodes `sh -c` — the port covers both hosts),
  with workdir (auto temp dir; `Close` deletes only the auto one), timeout, env
  merge, max output. Deviation: `WithShellBlockPatterns` matches the raw
  command line case-insensitively instead of Python's argv word match — the Go
  tool has no restricted (shell-less) mode whose argv could be matched, and a
  word match behind `sh -c` would check what the shell has not expanded yet;
  the Python caveat (advisory filter, not a security boundary) carries over.
- **Local code runner** (`coderunner.go`, code/tool.py SandboxCodeTool over
  sandbox/local.py + adapter DEFAULT_RUNNERS): `NewLocalCodeRunner` is the
  explicitly-named local backend (Python requires the backend to be passed for
  the same reason — no accidental unsandboxed execution), `run_code(code,
  language)` with python/bash inline runners and a javascript file runner;
  languages are restricted via `WithCodeLanguages`. Deviations: `sh` is an
  accepted bash alias and the bash runner falls back to `sh`; the TypeScript
  (ts-node) runner and the Docker/Daytona backends are deferred.
- **Filesystem** (`filesystem.go`, toolkits/filesystem.py, abbreviated to the
  brief's five): read_file, write_file (UTF-8 pinned, parents created),
  list_directory, make_directory, find_files (Python 3.13 `**` semantics). The
  traversal guard resolves the deepest existing prefix through symlinks and
  re-checks containment. Deviations: Python's update_file and delete_file and
  the `raw=true` base64 read are not ported.
- **Search** (`search.go`, search/{duckduckgo,tavily,perplexity}.py):
  `NewDuckDuckGoSearch` replaces the ddgs SDK with a direct fetch of the HTML
  endpoint and a regex parser (the brief's simplified variant), unwrapping the
  `/l/?uddg=` redirect hrefs; `NewTavilySearch` and `NewPerplexitySearch`
  (a toolkit: perplexity_search + perplexity_answer) call the REST APIs. The
  API key never enters the tool schema (Python parity: constructor-only); where
  Python resolves several request parameters as Variables, the port carries a
  per-conversation variable override for the key (`tavily_api_key` /
  `perplexity_api_key`) instead, and the optional request parameters beyond
  max_results/depth/topic/include_answer (Tavily) and max_results (Perplexity)
  are deferred — Python's Variable-resolvable kwargs per tool do not map onto
  the argument-struct schema one to one.
- **Skills** (`skills.go`, skills/toolkit.py over runtime/local, abbreviated):
  `NewSkillsToolkit(dir)` discovers `<dir>/<name>/SKILL.md` with a
  `key: value` frontmatter parser (the YAML subset the toolkit reads — no YAML
  dependency in the core module) and serves list_skills, load_skill (the
  wrapped `<skill_content>` body with the resource list) and
  read_skill_resource; the name argument is enum-constrained to the discovered
  skills (the `Literal[name, …]` parity, via WithToolSchema). Deviations: one
  runtime (no multi-runtime last-to-first delegation chain), the script runner
  and the install/extract path are not ported; a skill without a frontmatter
  name falls back to its directory name instead of being skipped by the lenient
  loader.

Tests are stdlib-only: httptest fakes for the three search APIs, temp-dir
fixtures for filesystem/skills, real subprocesses for shell/code (python when
on PATH, the bash/sh runner otherwise — the brief's skip-compatible fallback;
the timeout and exit-code paths exercise without an interpreter too). Google
Maps, X-search and image generation stay deferred per the brief.

## D-063 — 2026-10-30 — toolpacks/dynamic: the factory composes delegation through Agent.AsTool; the child-failure wording follows subagent_tool, not handler.py

`NewDynamicAgentFactory` (dynamic/factory.py + handler.py resolve_and_run)
resolves the model-built spec against the pool via `spec.AgentSpec.ToAgent`,
attaches the spec's response schema as `response.RawSchema` (Python:
`to_agent`'s spec-schema branch), then delegates through the child's
`AsTool()` rather than a run_task reimplementation — the shared machinery
(TaskStarted/Completed/Failed, the rolled-up subtask UsageEvent, the
dependency/variable copy, the HITL bridge) cannot be reached from outside the
agent package, and duplicating it would fork the delegation semantics. Two
consequences: a child that fails after starting surfaces as AsTool's
"Sub-task '<name>' failed: …" success-text instead of handler.py's
"Error: {result.error}" (the spec-resolution failure keeps the handler's exact
"Error: unknown tools […]. Available: […]" wording with Python list reprs), and
the delegation rides a synthetic per-call tool call on the parent stream under
a unique name (`task_<child>_<8 hex>`), because the framework routes calls to
tools by name and two concurrent same-named children must not answer each
other's calls.

## D-056 — 2026-10-30 — network wave 2: views, arbiter, expectation sweepers, task mirror, client tools, federation stub

The six deferred pieces of the D-031 network subset land in `github.com/luckystrker/ago/network`
(owner directive; each was listed as deferred in the D-031 row of PORT_STATUS).

- **views.go** — `ViewPolicy` + `FullTranscript` / `WindowedSummary` / `NamedTranscript` /
  `NamedWindowedSummary` (Python `views/base.py` + `builtin.py`). Pure projections: WAL slice +
  participant + renderer + name resolver → `[]events.Event` (`ModelRequest`/`ModelMessage`,
  head `events.CompactionSummary` for the windowed views). `DefaultRenderEnvelope` folds the
  adapters' `default_render_envelope` and the packet handoff-shape; `Hub.NameFor` is the
  passport-backed resolver. AGO-DEVIATION: the AGo `CompactionSummary` carries `Content` only
  (no `event_count` field) — the count rides in the summary line. Adapters do not call views
  automatically; the call pattern stays handler-owned (adapter folds), documented on the type.
- **arbiter.go** — `HubArbiter` seam (six gates) + `BaseArbiter` (no-op base) +
  `RuleBasedArbiter` (default). The hub consults it at the exact pre-commit points Python does:
  `Register` is *not* gated (Python parity — the reference hub never calls `authorize_register`),
  `CreateChannel` runs `AuthorizeChannelOpen` instead of the inline concurrency/inbound
  pre-flight, `Send` runs `AuthorizeSend` (explicit audiences only, broadcasts skip) before the
  WAL append, dispatch runs `AuthorizeDispatch` per recipient (Deny = silent skip) and
  `ResolveUnknownAudience` for unknown audience ids (federation hook). `Decision` collapses the
  Python `Allow`/`Deny` dataclasses into one struct; the depth-cap Deny carries
  `ErrDepthExceeded` so the pre-existing sentinel tests keep their meaning, everything else
  defaults to `ErrAccessDenied`. AGO-DEVIATIONS: gates are synchronous (the hub holds its lock —
  arbiters must not block); `AuthorizeInbox` exists on the seam but the hub never consults it —
  delivery inboxes live client-side and the AGo `Rule` carries no inbox block.
- **expectations.go / audit.go** — `ChannelManifest.Expectations` (the D-031 data-only
  `Expectation` grows `OnViolation`/`AppliesTo`) evaluated by three pure evaluators
  (`acks_within`, `reply_within`, `max_silence`) against channel + WAL + clock, deduplicated per
  (channel, expectation index, violator) in an in-memory cache cleared on terminal transitions.
  Handlers: `audit` (no-op), `notify_channel` (posts `ag2.expectation.violated`; on a pending
  channel the post fails and is suppressed — Python parity), `auto_close` (close with reason
  `expectation_violated:<kind>`). `Hub.SweepExpectations` runs one deterministic tick;
  `ExpectationSweeper` wraps it in an interval loop (default none — hub owners opt in, unlike
  Python's auto-started sweeper). Every *fire* reaches the optional `AuditSink`
  (`WithAuditSink`; `FileAuditLog` JSONL / `SliceAuditLog`) before the handler, even when the
  handler lookup misses (unknown `on_violation` values stay silently unconsumed — Python
  parity), and sink failures are swallowed. Task store rides along: `Hub.ObserveTask` /
  `UpdateTask` / `Task` / `Tasks` over `task.Metadata` (in-memory only, no
  `max_concurrent_tasks` cap — the AGo Rule has no task-limits block).
- **taskmirror.go** — `TaskMirror` subscribes a `stream.Stream` for the six `Task*` events and
  forwards them into the hub task store; `ErrTaskNotFound` on progress/completion is swallowed
  (never crashes the agent turn), the agent-side `ExpiresAt` carries through, re-observe
  updates in place.
- **clienttools.go** — `ClientTools(hc, self)` builds `peers` / `channels` / `tasks` /
  `context` / `delegate` as `[]tools.Tool` (reflection `FunctionTool`s; bound by closure over
  the `HubClient` + calling `AgentClient` — AGO-DEVIATION vs the Python `AgentClientInject`
  DI, same shape one notify handler produces). `tasks progress/complete` act on the context
  dependency `task.DepKey` (`ag:task`) — the TaskInject analogue. `delegate` opens a consulting
  channel (handshake awaited inside `CreateChannel`), suppresses the local handler for the
  channel's lifetime (`AgentClient.SuppressHandler`/`ResumeHandler`), posts the prompt one hop
  deep, and waits by polling `Hub.ReadWAL` for the respondent's text or a terminal channel
  event (AGO-DEVIATION vs `wait_for_channel_event`'s inbox — polling is deterministic for the
  in-process subset), with the `functions.`-prefix target fallback and the available-targets
  hint on a miss. `peers find/describe` reads the new `Hub.Registrations()` directory view
  (skills/SKILL.md and the cost-profile sorting are not ported — no skills registry in the
  subset). The `network → tools` import is a deliberate extension of the ARCHITECTURE graph
  (no cycle: tools does not import network).
- **remote.go** — `RemoteAgentProxy` as an interface stub: no implementations ship and the hub
  does not consult the seam (Python parity: no proxies ship there either); remote-audience
  routing is the arbiter's `ResolveUnknownAudience` hook until a proxy registry lands.

Gates re-run from the repo root after the change: `go build ./...` and `go vet ./...` clean;
`go test ./... -count=1` exit 0 (46 packages `ok`, zero failures) with `go test ./network/...`
green including the new suites (views golden projections, arbiter allow/deny + federation
reroute, sweeper violations → audit + envelope + dedup + interval loop, task mirror lifecycle,
client tools end-to-end through the real `FunctionTool` stream path, delegate consult against a
live peer); `go build ./examples/...` / `go vet ./examples/...` clean. The optional
`contrib/wslink` module runs its own gates from its directory (D-057): `go build ./...`,
`go vet ./...`, `go test ./... -count=1` — all green.

## D-057 — 2026-10-30 — contrib/ layout: optional modules with their own go.mod

First `contrib/<name>` module: `contrib/wslink` (`module
github.com/luckystrker/ago/contrib/wslink`), the WebSocket `transport.Link` port of
`ag2/network/transport/ws.py`. Pattern, for every future contrib module:

- own `go.mod` at `contrib/<name>/`, `require github.com/luckystrker/ago v0.0.0` plus
  `replace github.com/luckystrker/ago => ../..`; external dependencies are allowed there and
  forbidden in the root module (D-003 stands);
- `go get` runs only inside the module's own directory;
- gates for a contrib module run from its directory (`go build ./...`, `go vet ./...`,
  `go test ./...`) in addition to the root gates, which stay clean because the root module
  never imports contrib (the dependency points one way: contrib → core).

`wslink` itself: `wslink.Conn` implements `transport.Link` over `github.com/coder/websocket`
(pure Go, no cgo), one JSON text message per frame via `transport.MarshalFrame` /
`UnmarshalFrame`; `Dial` is the client side, `ServeWS` the `http.HandlerFunc` upgrade path.
Malformed inbound messages are skipped (ws.py parity); heartbeat rides the library's control
frames; Close wakes blocked Recv with an error wrapping `transport.ErrLinkClosed` and Send
with `ErrClosed`. AGO-DEVIATION: no auto-reconnect and no `since_envelope_id` replay — Python
`ws.py` does one connect per open and leaves reconnect to the tenant ("callers that need
backoff wrap it"), so there is nothing to port; replay, when a tenant needs it, is an
application-level Hello-frame field the hub answers from its WAL, not a transport concern.
Tests run over `httptest` + a real `coder/websocket` dial: echo through the Link semantics,
server→client push, malformed-frame skipping, context cancellation, Close waking Recv,
unknown-frame-kind rejection, plain-HTTP refusal.

## D-064 — 2026-10-30 — contrib/otelx: the agent-side telemetry middleware ported as an optional module (closes the TelemetryMiddleware deferral)

Owner directive, same contrib wave as D-059, under the D-057 packaging. `contrib/otelx`
ports `ag2/middleware/builtin/telemetry.py` (the D-013 "telemetry builtin not ported"
note) as its own module: the core stays dependency-free (D-003); the module requires
go.opentelemetry.io/otel + sdk, plus the stdout and OTLP/HTTP exporters for
InstallDefault only, pinned at v1.41.0 — the newest release whose whole module graph
still declares go ≤ 1.24 (otel v1.42+ and the 2026-era indirects of the first @latest
attempt require go ≥ 1.25; the attempt was discarded for exactly that).

Contract: `Middleware(tp trace.TracerProvider) middleware.Factory` — OnTurn/OnLLMCall/
OnToolExecution wrap next with spans `ago.turn` (internal), `ago.llm_call` (client) and
`ago.tool_execution` (internal), each parented under its instance's turn span (a Factory
builds one instance per turn; Go has no ambient context, so the parenting is explicit
under a mutex instead of `start_as_current_span`). `ago.llm_call` stamps model/provider
(`gen_ai.request.model`, `gen_ai.response.model`, `gen_ai.provider.name`), the usage
tokens (`gen_ai.usage.input/output/total_tokens` + cache_read/cache_creation/thinking)
and finish_reasons off the ModelResponse, only for reported fields; `ago.tool_execution`
stamps the tool name/call id/type. Pipeline errors record an exception event and set the
span status to Error; a `ToolErrorEvent` answer (value or pointer) does the same when
next() itself succeeded (Python parity). OnHumanInput passes through.
`InstallDefault(ctx, opts...)` wires the SDK provider (batcher) and returns
(factory, shutdown): WithStdout/WithStdoutWriter (JSON lines) or WithCollector
(OTLP/HTTP; host:port and http:// → plaintext, https:// → TLS, "" → plaintext
localhost:4318), registering the process-global provider.

AGO-DEVIATION: span names `ago.*` instead of Python's "invoke_agent <agent>"/"chat"/
"execute_tool <name>"; tracer scope `github.com/luckystrker/ago/contrib/otelx` instead
of `opentelemetry.instrumentation.ag2` (same schema URL 1.11.0); InstallDefault speaks
only OTLP/HTTP — a gRPC user builds their own provider and hands it to Middleware.
Deferred: content capture (message bodies, tool arguments/results, the 8192-char
truncation), the constructor knobs (agent/provider/model names, span_attributes), the
`await_human_input` span, the `record_usage` subscription spans (spend that never
becomes an LLM span — subtask rollups, compaction — stays untraced; the Go UsageEvent
surface lives in ago/usage without a stream-subscription API for this), and the
traceparent dependency relaying from network envelopes (the hub-side network.envelope
tracing has no Go counterpart).

Tests are network-free: sdktrace/tracetest SpanRecorder in-memory (nesting on one trace
under the turn span, attribute stamping, empty-usage omission, error status via the
pipeline-error and ToolErrorEvent value/pointer paths, per-turn instance isolation,
concurrent tool execution under one turn, nil-provider global fallback, human-input
pass-through) plus InstallDefault over a buffer and against an OTLP endpoint on
httptest.

## D-065 — 2026-10-30 — agui: held-turn interrupts, state snapshots, snake_case input (closes the D-053 deferrals for interrupts.py)

Owner directive: port the remaining `ag2/ag_ui` surface — interrupts.py's
held-turn machinery, the STATE_SNAPSHOT emitters, and dual-case run-input
parsing. All of it lands in `github.com/luckystrker/ago/agui`.

What was built:

- `Server` (`agui/server.go`) serves one agent with turns owned by the server
  rather than by an HTTP request (`NewServer` + `Options()`/`ServeAgent` +
  `Handler`). A `HumanInputRequest` on a served run — question or tool
  approval — parks the turn: the exchange ends on a `RUN_FINISHED` whose
  outcome is the new `InterruptOutcome` (`agui/events.go`, wire shape from the
  protocol schema's `RunFinishedInterruptOutcome`/`Interrupt`), never a
  `RUN_ERROR`. The answer arrives as the `resume` entries of a later POST on
  the same thread: `resolved` rebinds the turn to the new exchange (fresh
  RUN_STARTED, then the events kept while paused, then the continuation),
  `cancelled` abandons it (kept events + closings for open entities, then
  RUN_FINISHED cancelled). Refusals are lone coded `RUN_ERROR`s before
  RUN_STARTED: `INTERRUPT_NOT_COVERED`, `INTERRUPT_PAYLOAD_REFUSED`,
  `INTERRUPT_NOT_PROVEN` — with the turn restored unchanged, so a legitimate
  answer in time still resumes it.
- `Retention` (default 900s / 128 held) bounds unanswered questions in an
  in-memory registry keyed by thread; expiry releases the turn, and the hook's
  own timer releases it even with no traffic. Registry entries are per-process:
  resumes must be routed back to this server, nothing survives a restart.
- Resume proofs: every interrupt carries `metadata["ago"]["proof"]` (32 random
  bytes, per interrupt, constant-time compare). A proof that is present must be
  the one issued; refusing absent proofs is opt-in (`WithResumeProof`).
  AGO-DEVIATION: the envelope key is `"ago"` where Python uses `"ag2"`.
- The mechanism rides the existing HITL plumbing: `Options()` installs
  `hitl.DefaultHook` behind an `interrupter` middleware (`agui/interrupts.go`)
  that intercepts by the asking context's stream id, so runs outside the server
  keep their own arrangements. The run scope is opened on
  `context.WithoutCancel` — a paused turn must not die with the response it
  paused on (Python: the task lives outside the request scope).
- `WithStateSnapshots` emits a `STATE_SNAPSHOT` at the start of every run: the
  client's `state` keys and the conversation's variables together, reserved
  `ag:`/`a2a:` keys stripped, declared in capabilities as
  `state.snapshots=true`; `humanInTheLoop.interrupts=true` is now declared by
  the served answer.
- Run-input parsing (`agui/run_input.go`) decodes through an alias struct
  accepting both camelCase (the SDK's spelling) and snake_case keys — top
  level, messages, tool calls, resume entries — camelCase winning when a body
  carries both; `state`/`forwardedProps`/tool parameters stay untouched values.
  A resume entry of unknown status is stripped with a warning (Python parity:
  `_known_entries`).

AGO-DEVIATION / deferred (the honest edges of the minimal mechanism):

- AGo's `Context.Ask` hands middleware the request stripped of its
  `ToolApprovalRequest` wrapper (`middleware/approval.go` documents this), so
  the served interrupt cannot name `toolCallId` or say
  `reason=tool_call`; every interrupt reads `input_required`, and approvals
  travel through the same text channel — a boolean resume payload is
  translated to the words the approval middleware reads (`true`→`y`,
  `false`→`n`) for any interrupt, where Python refuses booleans for
  non-approvals.
- Subagent suspension is deferred: a paused run does not close its open
  invocations as suspended and re-announce them on resume (interrupts.py's
  TurnOutput re-announcement ledger); open invocations are only closed when a
  turn is abandoned. `SubagentRunID` attribution on an interrupt uses the
  projection's open-invocation tracking instead of the full ledger.
- Seeding the turn's variables from the client's `state` is deferred (the
  served run cannot write the conversation variables from outside the agent
  package); the client's state is merged into the snapshot representation only.
- Opening snapshots are emitted unconditionally when configured, not only when
  they differ from what the client holds.

Gates re-run for this entry: `go build ./...`, `go vet ./...`,
`go test ./...` (all packages green), `go build ./examples/...` — plus
`go test ./agui/ -count=5` for stability. Tests are network-free (httptest):
the interrupt/resume cycle (question → partial SSE → resume POST →
continuation), state snapshot, dual-case input, refusals with the turn kept
answerable, cancelled-resume abandonment, retention expiry, resume proofs,
boolean payload translation, shutdown release. The race detector could not run
in the authoring environment (no C compiler for cgo).

## D-066 — 2026-10-30 — acp: session/cancel, session/load, the full update vocabulary, mediated fs/terminal, the MCP gateway and token auth (closes the D-052 deferrals for this slice)

Owner directive, same wave as D-065. `ago/acp` closes the deferrals D-052
recorded ("Not ported": `session/cancel` + the cancelled stop reason,
`session/load` replay, auth, the thought/plan/tool-call/usage updates, the
mediated `fs/*` + `terminal/*`, `tool_gateway.py`) as a functional subset of
the Python modules — session.py/agent.py/executor.py/mappers.py (serve),
bridge.py (client), auth.py. Wire shapes were pinned against the SDK the
reference itself pins (`agent-client-protocol` 0.12.1, extracted from the
wheel): `usage_update` carries `{used, size, cost?}` (the stabilized context
shape — NOT per-call token fields), `tool_call`/`tool_call_update` carry
`toolCallId/title/kind/status/content/rawInput`, `plan` carries `entries
[{content, priority, status}]`, the mediated methods are `fs/read_text_file`,
`fs/write_text_file`, `terminal/create`, `terminal/output`,
`terminal/wait_for_exit`, `terminal/kill`, `terminal/release`, and the error
codes are the ACP ones (`-32000` auth required, `-32002` resource not found —
the previous implementation-defined "session not found" code already matched).

**Serve role.** Reading moved to a dedicated goroutine (replies to a channel,
notifications dispatched immediately, requests queued to the serve loop — one
request runs at a time, unchanged), which is what makes a cancel deliverable
mid-turn. `session/cancel` stops the named session's turn context and is
ignored for unknown sessions (agent.py parity: a notification has no reply
channel); a cancelled turn answers `{"stopReason": "cancelled"}` — never a
protocol error, whatever the handler returned. `Handle` now receives the
turn's `context.Context` plus an `*Emitter` (`Message/Thought/Plan/ToolCall/
ToolCallUpdate/Usage`) — a signature change over D-052, documented here
because a handler that ignores its context still gets the cancelled stop
reason. `session/load` reattaches to a session this connection issued and
replays its stored history before the response (user turns as
`user_message_chunk` with `messageId`, replies as `agent_message_chunk`, tool
calls started once and settled once — mappers.history_to_session_updates
parity); a plain `Serve` has no history source, `ServeAgent` replays its
per-session conversations; cross-process storage stays deferred.
`authenticate` + an `Auth` provider gate every session method
(`_require_session_scope` parity); `initialize` advertises `loadSession:
true` and the provider's `authMethods`. `ServeAgent`'s live projection maps
`ModelReasoning` → `agent_thought_chunk` (only when `StreamThoughts` is on —
Python's `stream_thoughts` default), the per-call tool events → `tool_call` /
`tool_call_update` (the batch `ToolCallsEvent`/`ToolResultsEvent` are the
loop's own bookkeeping and would double every call on the wire — executor
parity), and `UsageEvent` → `usage_update` only when `ServeConfig.
ContextWindow` is set, because the shape's required `size` (context window)
has no truthful value otherwise; `used` is the session's running token total.

**Client role.** `Cancel` writes the `session/cancel` notification without
taking the Bridge lock (the lock is held by the parked `Prompt` — taking it
would deadlock against the very answer the cancel unblocks); `closed` became
atomic for the same reason. `PromptTurn` returns the stop reason next to the
text (`Prompt` keeps its D-052 signature). `Load` drives `session/load` and
the replay reaches the new `OnUpdate` hook — which receives every
`session/update` the agent streams, chunk or not. `SessionNewWithServers`
announces HTTP MCP servers in `session/new` and refuses stdio entries
(`ErrUndeclarableServer`): a command only the declarer can run is useless to
a subprocess agent (the reference's `session.ensure` takes `HttpMcpServer`
only). The mediated `fs/*`/`terminal/*` are answered under a root guard
(bridge.py `_confine` parity: `confine()` resolves symlinks in existing
components and refuses anything outside the root; terminals run confined to
the same root, output capped with the head kept and a truncated flag —
bridge.py `_Terminal`). `Bridge.FS == nil` is the disable option: every
mediated request answers `-32601`, the answer a client owes for a method it
never advertised, and `initialize` advertises `clientCapabilities.fs` only
when enabled — truthful on the deny side too.

**MCP gateway** (`tool_gateway.go`): `ToolGateway` proxies tool calls to
declared servers through the `ago/mcp` client — one connection per server,
dialed lazily on the first call (a client's declaration must not spawn
subprocesses on the agent's behalf until its handler asks). The serve role
records declared servers on the Session and exposes `Session.Gateway()` for
handlers; deviations recorded below.

AGO-DEVIATION / deferred:

- Python's `tool_gateway.py` hosts the AG2 agent's local tools on an
  in-process HTTP MCP server and hands the URL to the CLI agent — the
  opposite direction. The Go subset ports the consume direction only (proxy
  through the `ago/mcp` client, per the porting brief); hosting local tools
  over HTTP for the agent is deferred (it needs an HTTP MCP server on
  net/http — doable, but a separate slice).
- The served AGo agent's own tools are not routed through the gateway:
  `ServeAgent` runs plain `Ask` turns (D-052) whose tools resolve through the
  AGo tool framework; a handler that wants client-declared servers calls
  `Session.Gateway()` itself.
- `guard.py` (the `_meta` merge guard) is not ported because the behavior it
  guards does not exist here: the Go dispatcher decodes params into typed
  structs and drops `_meta` — nothing merges metadata over request fields.
- `list_directory` and `run_terminal` from the brief's method list are not in
  the reference's method set (bridge.py / SDK 0.12.1 carry no such methods);
  the mediated set is exactly the reference's.
- The serve side never CALLS `fs/*`/`terminal/*` (Python's ACPAgent never
  does either); only the answering side is implemented.
- The batch result events flatten error identity (the executor embeds
  `ToolResultEvent` in `ToolResultsEvent`), so a live `tool_call_update`
  settles errors as `failed` only via the loose error events; batch results
  are emitted `completed` with the error text as content. A signal-terminated
  terminal reports a negative exit code, no signal name.
- `StaticTokenAuth` is the auth subset ported (the env-key/secret case); the
  terminal/OAuth auth-method kinds are deferred. `PromptResponse`'s unstable
  `usage` field is not emitted.
- The session store (LRU/TTL/queues), cross-connection session/load and
  model pickers stay deferred (D-052).

Tests: the re-exec suite grows one child role per mechanism —
`acp-cancel-agent` (parks until the cancel notification, answers the
cancelled stop reason), `acp-load-agent` (replays two chunks on session/load),
`acp-fs-probe-agent` (line/limit read, escape refusal, write-through-
mkdirs), `acp-term-probe-agent` (create → wait → output → release of a
re-exec'd one-shot echo role), `acp-fake-mcp-server` (stdio MCP the gateway
dials) — plus in-process fake-client tests for the serve side (cancel stops
the turn context and answers cancelled, stray cancels ignored, load replay
with messageIds, the update vocabulary and its StreamThoughts/ContextWindow
gates, the static-token flow, the gateway against an httptest MCP server,
`confine` unit cases).

Gates re-run for this entry: `go build ./...`, `go vet ./...`,
`go test ./...` (all packages green, 0 skipped), `go build ./examples/...`,
`go vet ./examples/...`; `go test ./acp/ -count=1` re-run after the docs
sweep. The race detector could not run in the authoring environment (no C
compiler for cgo), same note as D-065.

## D-067 — 2026-10-30 — a2a wave 2: push notifications, the client-tools extension, context/HITL over the wire, REST, declarative security schemes; card JWS signing stays deferred

Owner directive, the same wave as the D-049/D-050 supplements above. `ago/a2a`
closes the remaining a2a deferrals that do not need new dependencies. What
shipped:

- **Push notifications** (`push.go`, parity push.py + server.py): a
  `PushConfigStore` (`MemoryPushConfigStore`) behind the four JSON-RPC methods
  `tasks/pushNotificationConfig/set|get|list|delete` — the v0.3 wire spellings
  (a2a-sdk `src/a2a/compat/v0_3/jsonrpc_adapter.py:60-63`), consistent with the
  `message/send`-style method names the package already serves; the 1.x
  dispatcher renames them to the gRPC-derived `CreateTaskPushNotificationConfig`
  family, a rename-only difference. Config wire shape is the a2a-sdk
  `TaskPushNotificationConfig` proto (url, token,
  authentication{scheme, credentials}, server-issued id). Configuring a store
  flips `capabilities.pushNotifications` on the served card (parity server.py:218)
  and arms dispatch: on every terminal commit the server fires each registered
  webhook asynchronously (`context.WithoutCancel` — the RPC has its answer
  already; a webhook outage cannot fail it). `DefaultPushSender` POSTs the task
  record as `application/a2a+json` with the token on `X-A2A-Token`;
  `PushReceiver` is the matching webhook endpoint (POST, constant-time token
  check, Task decode, callback). AGO-DEVIATION: the token header spelling is
  this package's contract between its own sender and receiver — the a2a-sdk
  leaves the sender to an app-supplied `PushNotificationSender`, so there is no
  upstream spelling to match. Client side: `SetPushConfig`/`GetPushConfig`/
  `ListPushConfigs`/`DeletePushConfig` (parity push.py create/get/list/delete).
  An optional `PushURLValidator` refuses webhook URLs at set time (parity
  server.py `push_url_validator`).
- **The `urn:ag2:client-tools:v1` extension** (`extension.go`, parity
  extension.py + mappers/tools.py): the URI and the four `vnd.ag2.*` MIME
  constants; `ToolSchema`/`ToolCall`/`ToolResultEntry` with the Python payload
  dicts (schemas `{"tools":[...]}`, calls `{"id","name","arguments"}` —
  arguments stays the JSON string AG2 holds; results
  `{"results":[{"id","name","content","error"}]}` — only the error string
  survives the wire, as in Python). Server: `ServerConfig.HandleClientTools`
  replaces `Handle` for a turn carrying tool-schemas parts; returning calls
  parks the task `input-required` with one tool-call data part per call on the
  status message (parity executor.py's `requires_input`), returning none
  completes. `HandleToolResults` resumes a task parked by id — the same two
  legs as executor.py `_build_initial_event` (ToolResultsEvent vs ModelRequest).
  Client: `AttachClientTools` stamps the URI on `Message.extensions` and
  appends the schemas part (parity `build_user_message` advertise_extension);
  `RunClientTools` loops park → execute-callback → results continuation until
  the task is terminal. `ValidateExtensionActivation` reconciles activated URIs
  with the card in both directions (parity extension.py
  `validate_extension_activation`, including the native-URI exemption).
- **Context variables + HITL over the wire** (`contextwire.go` + driver, parity
  mappers/messages.py extract/build + executor.py bracket): the
  `ag2.context_update` metadata key; `ExtractContextUpdate` returns an empty
  map on absence/non-object (callers merge unconditionally);
  `StripReservedVariables` drops the `ag:`/`a2a:` prefixes on both wire
  directions (parity context.py:289 `RESERVED_VARIABLE_PREFIXES` — declared
  locally, an AGO-DEVIATION: the Go core does not export the strip helper and
  a2a stays a leaf package). The server engages the sync only when
  `ServerConfig.SyncVariables` is set: inbound delta (stripped) keyed by task
  id, returned final set (stripped) attached to the reply. HITL: a parked task
  whose status message is the question is answered by `Client.RunWithHumanInput`
  — the text continuation goes out under the same task id (parity
  `build_input_response_message`), extensions preserved because activation is
  per-request. The drivers cap the loops at 16 wire rounds (`wireRoundLimit`) —
  Python relies on `input_required_timeout` instead; a fixed cap is the honest
  equivalent against a misbehaving server.
- **REST transport** (`rest.go`, parity transports/rest.py mounting the a2a-sdk
  `create_rest_routes`): `POST /message:send`, `POST /tasks` (the same send
  under the wave's alias path), `GET /tasks/{id}` with `?historyLength=`, and
  `POST /tasks/{id}:cancel` — the send/get/cancel trio of the wave's scope;
  payloads `application/a2a+json` (a2a-sdk constants.py
  `A2A_JSON_MEDIA_TYPE`), errors as JSON-RPC error objects with HTTP mappings
  (400 invalid, 404 unknown task, 409 terminal cancel, 500 handler/store).
  Not ported from the SDK route table: `tasks/list` (GET /tasks),
  `/extendedAgentCard`, the `/{tenant}` mount (tenants stay deferred), and the
  `tasks/{id}/pushNotificationConfigs` REST routes (the push surface speaks
  JSON-RPC only).
- **Declarative security schemes** (`security.go`, parity security.py):
  `NamedScheme`/`SecurityRequirement` and the six factories
  (bearer/http/api-key/oauth2/oidc/mtls). Wire shapes verified against the
  official Go SDK (a2aproject/a2a-go a2a/auth.go, a2a/agent.go): the scheme is
  the nested ProtoJSON oneof (`httpAuthSecurityScheme`, ...), scopes render as
  `{}` / `{"list":[...]}` inside `{"schemes": {...}}` (legacy bare arrays
  accepted on decode), the card carries `securitySchemes` +
  `securityRequirements`.
- **Card/extension wire fields** (`types.go`): `Message.extensions` +
  `Message.metadata`, `DataPart.mediaType` (+part metadata), `AgentExtension`
  on capabilities with `extendedAgentCard`, and the `input-required` task
  state (the D-049 amendment above). The client stamps `A2A-Extensions`
  (comma-separated, the A2A spec spelling; parity extension.py
  `extension_call_context`) via `WithExtensions`, and `SendMessageTask` /
  the drivers absorb both message/send result shapes (Message for immediate
  completions, Task for parks — the probe AG2's client runs, client.py).

**Deferred — card JWS signing/verification stays closed**: `sign_card`,
`CardSigner`, `card_signature_verifier` and `A2ACardSignatureError` (errors.py)
are real JWS over the card payload; the stdlib has no JWS/JWT (no HS256/RS256
primitives short of rolling the JOSE stack by hand, which is exactly the
crypto-not-invented-here territory the zero-dependency rule exists to avoid).
security.py itself is pure card metadata and is fully ported above; the
signing layer around it is not. Also still deferred: full history shipping on
continuations (`ag2.history+json` parts), reconnect/resubscribe,
`validate_input_modes`, cluster mode, tenants, gRPC (permanently — see the
D-049 supplement).

Gates for this entry: `go build ./...`, `go vet ./...` (clean);
`go test -count=1 ./a2a/ -v` — 62 tests ok; full `go test -timeout 300s` over
`$(go list ./... | grep -v /acp)` — 46 packages ok, 0 failures; `./acp` was
excluded because a concurrently-authored test in that package
(`TestServeCancelStopsTheTurn`, acp/serve_extend_test.go — another agent's
in-flight work, not this wave's) hung past the timeout; `go build ./examples/...`
and `go vet ./examples/...` clean. The race detector could not run in the
authoring environment (no C compiler for cgo), same note as D-065/D-066.

## D-068 — 2026-10-30 — Final wave: the D-008 Responses/Decisions deferral is closed (config/openai)

Closes the last open line of D-008's provider surface: `OpenAIResponsesConfig` and
`OpenAIDecisionsConfig` exist as full clients, not config stubs. The detail record is
D-060; this entry is the closure ledger against the deferral.

- `NewOpenAIResponses(model)` → `Create() (config.LLMClient, error)` — the
  Responses API over raw HTTP/SSE on `internal/httpx` + `internal/sse` (no SDK, D-003):
  `POST {base}/v1/responses`, field parity with the Python dataclass
  (Temperature/TopP/MaxOutputTokens/MaxToolCalls/Store(nil = default true)/
  ParallelToolCalls(nil = default true)/TopLogprobs/Metadata/PromptCacheKey/
  PromptCacheOptions/Reasoning{Effort,Summary}/ServiceTier/User/Truncation/Timeout/
  MaxRetries(default 4)/Organization/Project/DefaultHeaders/ExtraBody); `Clone`
  accepts Go field names with snake_case aliases, nil clears. `CreateFilesClient`
  delegates to the existing Files client (D-046).
- Hosted builtin tools reach the wire through this client — the D-047 carrier gap on
  the OpenAI side is closed: web_search, file_search, code_interpreter, image_generation,
  shell (container auto/reference), MCP, tool_search (`defer_loading`). Hosted output
  items map back to `BuiltinToolCallEvent`/`BuiltinToolResultEvent` with second-call
  item replay. `FileIdInput` maps to `input_file.file_id` (the Chat-Completions engine
  still rejects it, D-017/D-020).
- `NewOpenAIDecisions()` → `DecisionsClient` — decision-only schemas (bool, string
  enum, int enum numbered from 0, 0..1 number; anything else is refused with the
  schema guidance), thresholded answers (`BooleanThreshold`, default 0.5), refusal as
  `*DecisionRefusedError{Name}`, default model `DefaultDecisionsModel` ("gpt-6-luna").

Still deferred (D-060's honest remainder): OpenAI Containers (nothing in the ported
client references them), prompt-cache diagnostics events, reasoning-item replay, shell
delta chunks, question-instruction parts (Decisions), `websocket_base_url`. The
D-008 stubs (xai/bedrock/typesafe) are unchanged.

## D-069 — 2026-10-30 — Final wave: the contrib "extras" policy is applied across all five optional modules

The D-048 policy (itself the extension of D-003; the extras shape mirrors AG2's
`optional_import_block`, ag2/ag2/_import_utils.py:93) materialized through the D-057
layout. Five modules ship under `contrib/<name>/`, each its own Go module —
`module github.com/luckystrker/ago/contrib/<name>`, `require github.com/luckystrker/ago
v0.0.0` + `replace github.com/luckystrker/ago => ../..` — the only places in the
repository where external dependencies are allowed:

| module | dependency | carries | record |
|---|---|---|---|
| `contrib/knowledgesqlite` | `modernc.org/sqlite` v1.34.1 (pure Go) | `knowledge.Store` | D-059 |
| `contrib/knowledgeredis` | `redis/go-redis/v9` v9.5.1 (+ miniredis test dep) | `knowledge.Store` | D-059 |
| `contrib/streamredis` | `redis/go-redis/v9` v9.5.1 (+ miniredis test dep) | `stream.Storage` | D-059 |
| `contrib/wslink` | `github.com/coder/websocket` v1.8.15 | `transport.Link` | D-057 |
| `contrib/otelx` | `go.opentelemetry.io/otel` v1.41.0 + sdk/stdout/OTLP exporters | telemetry `middleware.Factory` | D-064 |

Verified 2026-10-30: the root `go.mod` is `module github.com/luckystrker/ago` + `go 1.24`
with **zero `require` entries** — `contrib/` is invisible to the root `./...` gates
(nested modules), and the dependency points one way (contrib → core). Gates re-run from
each module directory (`go build ./...`, `go vet ./...`, `go test -count=1 ./...`): all
five green. CI runs a dedicated `contrib` job that loops `contrib/*/` (added this wave).

## D-070 — 2026-10-30 — Final wave: closure ledger for network/a2a/acp/agui/eval against D-049..D-053

Each protocol wave closed its D-049..D-053 slice with its own record (network: D-056
wave 2; eval: D-061; agui: D-065; acp: D-066; a2a: D-067). This entry is the
project-level statement of what "closed" means per zone and what honestly remains.

- **network** (D-031 + D-056): closed — view policies, the `HubArbiter` seam with the
  `RuleBasedArbiter` default, expectation evaluators/handlers + `AuditSink` +
  `ExpectationSweeper` (owners opt in), `TaskMirror`, `ClientTools` (peers/channels/
  tasks/context/delegate as `[]tools.Tool`), `Hub.NameFor`/`Registrations`. Remains:
  no `AuthorizeInbox` consultation (client-side inboxes), federation has no
  implementations (`RemoteAgentProxy` stays an interface stub; remote routing goes
  through `ResolveUnknownAudience`), the sweeper never auto-starts, no skills/SKILL.md
  registry behind the peers tool.
- **a2a** (D-049/D-050 + D-067): closed — push notifications (store + four JSON-RPC
  methods + async webhook dispatch + sender/receiver), the `urn:ag2:client-tools:v1`
  extension with server legs and client drivers, context-variable sync and HITL over
  `Message.metadata`, the REST twin, declarative security schemes, `input-required`
  task state. Remains: full history shipping (`ag2.history+json`),
  reconnect/resubscribe, card JWS signing/verification (no stdlib JWS — D-067),
  `validate_input_modes`, cluster mode, tenants. gRPC is closed permanently — D-071.
- **acp** (D-052 + D-066): closed — both roles, `session/cancel` (cancelled stop
  reason, never an error), `session/load` with per-session replay, the full
  `session/update` vocabulary (thoughts under `StreamThoughts`, plan, tool calls,
  `usage_update` under `ContextWindow`), token auth (`Auth`/`StaticTokenAuth`),
  mediated `fs/read_text_file`/`fs/write_text_file` under the root guard, the
  `terminal/*` family confined to the same root, the MCP gateway (consume direction).
  Remains: the multi-session store, cross-process `session/load` storage, the
  tool_gateway HOST direction (exposing local tools over HTTP), serving the agent's
  own tools through the gateway, `guard.py` (its `_meta` merge target does not exist
  in the Go dispatcher), OAuth/terminal auth kinds, model pickers, remote transports.
- **agui** (D-053 + D-065): closed — the held-turn `Server` (a question pauses the run
  as RUN_FINISHED with an `interrupt` outcome; the answer rides the next POST's resume
  entries; resolved rebinds, cancelled abandons), in-memory retention (default
  900s/128), optional resume proofs, STATE_SNAPSHOT of conversation variables,
  snake_case run-input keys. Remains: REASONING_ENCRYPTED_VALUE, the subagent
  suspension ledger (open invocations close on abandon, are not re-announced on
  resume), seeding the turn's variables from the client's state, prompt-level inputs
  folded into the opening transcript request.
- **eval** (D-051 + D-061): closed — failure attribution (mechanical + one-call LLM
  attributor, `Trace.Err`), the scorer registry (name + params factories, built-ins,
  `SetClient`), `SuiteFromJSONL`, `Diff`/`DiffStrict`/`Summary`, `WilsonCI`/`CohenKappa`,
  parallel cases (`WithConcurrency`, default 4). Remains: OTEL/Tempo/OTLP trace sources
  and `evaluate_traces` (the trace stays event-sourced — the core D-051 deviation),
  repeats/per-task configs/`BudgetThresholds`/lifecycle events, suite name/source
  metadata + tags (no slot on `Suite []Case`), pairwise single-order mode.

## D-071 — 2026-10-30 — a2a: the gRPC transport is closed permanently, not deferred

Decision: AGo will not carry an A2A gRPC binding — not "later", ever. Upgrades the
D-049/D-067 "deferred permanently" note to a positive closure so the ledger states the
reasons, not just the verdict:

1. **No lost behavior.** The gRPC binding is the A2A method vocabulary over a different
   wire encoding. The Go server already serves every ported method over JSON-RPC 2.0
   (message/send, message/stream, tasks/get, tasks/cancel, the push-config family, the
   client-tools and HITL continuation legs) and over the REST twin
   (`POST /message:send`, `POST /tasks`, `GET /tasks/{id}?historyLength=`,
   `POST /tasks/{id}:cancel` — a2a/rest.go:16-19). There is no AG2 behavior reachable
   only through gRPC.
2. **Dependency cost is structural.** Serving gRPC requires protobuf codegen (protoc)
   plus `google.golang.org/protobuf`. That is an external dependency — forbidden in the
   core (D-003/D-048) — and unlike the contrib cases (a driver, an SDK), the payload is
   generated artifacts that must track a2a proto revisions forever, for a transport no
   ported behavior needs. A contrib module buys nothing here.
3. **Clients resolve to JSONRPC by construction.** The served card always carries
   `supportedInterfaces`, and when the config leaves them unset the server fills in
   exactly one JSONRPC interface at protocol version "1.0" (D-049) — the path 1.x
   clients take; REST covers non-RPC callers.

The a2a row in PORT_STATUS lists gRPC as the honest permanent remainder of an otherwise
ported slice; nobody needs to re-open this.

## D-072 — 2026-10-30 — Final wave: live keeps the HTTP cascade; the realtime stack and sound devices stay deferred

`github.com/luckystrker/ago/live` is the ported slice (D-057): `Cascade{STT Transcriber;
LLM func(ctx, text) (string, error); TTS Synthesizer}` with `Handle(ctx, audio, mime,
voice) (replyText, audioOut, mimeOut, err)` — transcribe, answer, synthesize; a
whitespace-only transcript ends the turn silently (cascade.py `_turn` parity); stage
failures wrap the cause; an incomplete cascade fails fast with `ErrIncomplete`, provider
errors surface as `ErrAPI` (status + body snippet). Ready OpenAI stages over net/http:
`OpenAITranscriber` (Model default "whisper-1", optional Language) and
`OpenAISynthesizer` (Model default "tts-1", Voice default "alloy", Format default
"mp3", Speed).

Still deferred, deliberately — the honest remainder of `ag2/live/`:

- the realtime-websocket stack (LiveAgent-driven `RealtimeConfig`, turn detection,
  barge-in, streaming TTS, `STTTranslationConfig`) — websocket clients plus
  sound-device I/O, both outside the stdlib-only core (D-057);
- the elevenlabs/gemini/mistral live providers;
- `events/voice.py` (the voice event vocabulary has no consumer without the stack);
- **sound devices** (OS audio capture/playback): no stdlib surface exists; a Go port
  would need a platform audio dependency, i.e. a contrib module — there is nothing to
  put behind `Cascade`'s plain-byte `Transcriber`/`Synthesizer` interfaces until an
  application brings its own audio source, which the interfaces already allow.

## D-073 — 2026-10-30 — mcpserver, toolpacks, tui: the three new packages and why the Go layout splits them

Three packages land without a 1:1 Python module counterpart; each carves one role out
of a Python module that bundled several:

- **`mcpserver`** — the *serving* half of `ag2/mcp/` (server.py, executor.py, tools.py,
  transport.py; record D-058). Python kept client and server in one module; AGo splits
  them because the roles pull the dependency graph in different directions: the client
  (`ago/mcp`, D-029) stays a leaf (events/stream/tools/middleware only), while the
  server imports `agent` (`NewServerForAgent` builds one blocking agent turn per ask)
  and the client itself (verified imports, 2026-10-30). One package per role keeps an
  MCP consumer from pulling the whole agent. Statelessness is the second cut: every
  `tools/call` is an independent exchange; sessions/paused-runs/OAuth/MCP-Apps are
  deferred (D-058).
- **`toolpacks`** — the ready-made tool *content* of `ag2/tools/` (shell, code +
  sandbox/local, toolkits/filesystem.py, search, skills, dynamic/factory.py; records
  D-062/D-063). Python shipped them beside the tool framework; AGo separates the
  framework (`ago/tools`: schema, binding, executor) from concrete tools
  (`ago/toolpacks`: `NewShellTool`, `NewLocalCodeRunner`, `NewFilesystemToolkit`,
  `NewDuckDuckGoSearch`/`NewTavilySearch`/`NewPerplexitySearch`, `NewSkillsToolkit`,
  `NewDynamicAgentFactory`) so the framework stays dependency-free and the packs can
  grow without touching it. Deferred: Docker/Daytona sandboxes, the TypeScript runner,
  parts of the skills runtime (D-062).
- **`tui`** — `ag2/textual.py` without the textual framework (record D-056-tui):
  `REPL(a, in, out, opts...)` with `/help`//`exit` and `WithStreaming()` chunk echo.
  The textual App surface (widgets, markdown, the reasoning block) stays deferred —
  no TUI framework in the zero-dependency core.

## D-074 — 2026-10-30 — a2a: atomic task state transitions (TaskUpdater + per-task fallback)

`TaskStore.Get` returns a deep copy and `Save` replaces the record, so every
read-modify-write cycle the server runs — a turn committing its outcome,
`tasks/cancel` flipping a task to canceled — was a lost-update race: two
continuations resuming one parked task (the client-tools and HITL flows do
exactly that) clobbered each other's history entry and status, and a cancel
landing while a turn ran was overwritten by the turn's finishing `Save` — a
canceled task resurrected as completed. Python never sees this because the
interpreter's event loop serializes the interleavings; Go's concurrent HTTP
handlers do not.

The fix makes the transition atomic instead of widening any lock around the
agent turn (a lock held across the whole `Handle` call would also stall
`tasks/cancel` for the turn's full duration):

- **`TaskUpdater`** — optional `TaskStore` extension (`Update(id, mutate)`):
  re-read, mutate and write under one store-lock hold. `MemoryTaskStore`
  implements it; its results come back by deep copy, keeping the store's
  callers-never-share-state invariant.
- **Fallback** — stores without `Update` get a per-task mutex in `Server`
  across the `Get`→`Save` pair. Default flows never enter it.
- **History merges at commit** — the turn appends its incoming message to the
  history the store holds at commit time, not to the snapshot it loaded at
  begin: a concurrent continuation's entry survives.
- **Refusal policy** — a stored `canceled` record refuses a turn's commit (the
  cancel won; the turn's outcome is dropped wholesale and the caller is
  answered with the stored Task record — a canceled task never resurrects).
  `completed`/`failed` records still continue on an explicit new message with
  the same task id, matching the executor's append-to-whatever-id-arrives
  behavior the REST tests pin; `tasks/cancel` refuses over every terminal
  state, as before (-32002).

AGO-DEVIATION: the cancel-wins-over-a-running-turn ordering has no Python
counterpart — the single-threaded loop makes the race unrepresentable there
(D-050 covers the related store-before-answer deviation).
