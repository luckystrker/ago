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
