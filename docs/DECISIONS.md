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
  (`_replay.snap`) is not ported: the brief pins a plain keepRecent cut, and provider
  mappers already repair orphan tool results on replay (D-020).
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
  retained span that opens with a tool result (ToolResultEvent, ToolErrorEvent,
  ToolNotFoundEvent, BuiltinToolResultEvent) whose call lies outside the span
  widens back until the pair closes; the span never widens past the transcript
  start, so it grows rather than empties. Python's ProviderReplay anchor/turn
  items do not exist in the Go event vocabulary.
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
