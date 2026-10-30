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
