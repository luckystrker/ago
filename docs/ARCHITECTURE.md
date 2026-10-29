# AGo — Architecture (Go port of AG2)

AGo is a Go rewrite of [AG2](https://github.com/ag2ai/ag2) (commit fa4e0fb, 2026-10-27).
The Python reference lives in `./ag2` (git-ignored clone) and its behavior specs in `./docs/specs/`.
This document is the **binding contract** for the Go implementation: package layout, public APIs,
and the translation rules Python→Go. Every porting agent must implement against these contracts.

## Design principles

1. **Zero external dependencies for core.** stdlib only: `net/http`, `encoding/json`, `crypto/rand`,
   `os/exec`, `sync`. No OpenAI SDK — providers are implemented over raw HTTP/SSE.
   (Deviation is allowed only with a DECISIONS.md entry.)
2. **Parity where it matters, idiomatic Go where it doesn't.** Names, semantics and event flow
   follow the Python specs; signatures follow Go idiom. Every deviation is marked
   `// AGO-DEVIATION: <reason>` in code and recorded in docs/DECISIONS.md.
3. **asyncio → goroutines.** `async def` → plain funcs taking `context.Context` for cancellation.
   Stream pub/sub is synchronous (in the caller's goroutine, under a lock); background work uses
   `go func` with panic recovery.
4. **Errors are values.** Sentinel errors per package (`errors.Is`/`errors.As`), wrapping with `%w`.
5. **Every package compiles + has tests.** Gate (from `ago/`, the framework module):
   `go build ./... && go vet ./... && go test ./...`.
6. Tests are table-driven, stdlib `testing` only, no testify.

## Module layout

Module name: `ago` (framework name "AGo"). The framework module lives in the `ago/`
directory of the repository (so the repo root holds docs, examples and CI); import paths
are unaffected (`ago/agent`, ...). Examples are a separate module (`agoexamples`) in
`examples/` with `replace ago => ../ago`; a `go.work` at the repo root ties them together.
Go packages (dependency order, arrows = imports):

```
events  (leaf)                       — event vocabulary, Usage, conditions
stream  → events                     — Stream/MemoryStream/SubStream, History, Storage
context → stream, events             — ConversationContext
hitl    → events                     — HumanHook, HITL errors
response → events, context           — ResponseProto, Schema[T], JSON-schema generation
tools   → events, context            — Tool, FunctionTool, Toolkit, ToolExecutor, precedence
config  → events, tools, response    — Provider, ModelConfig, LLMClient, per-provider clients
middleware → events, context         — 4-hook onion chain, Conditional, builtins
usage   → events                     — Report aggregation from UsageEvents
task    → stream, events             — Task lifecycle, CheckpointStore
knowledge → events                   — KnowledgeStore protocol, memory/disk backends, EventLog
policies → events, context, knowledge — assembly policies
compact/aggregate → events, context  — triggers/strategies + middlewares
observers → events, stream, context  — Observer protocol, LoopDetector, TokenMonitor
watch   → events, stream             — Watch protocol, Interval/Event/AllOf/AnyOf/Sequence
agent   → everything above           — Agent, AgentRun, AgentReply, turn loop, subtasks
spec    → agent, tools               — AgentSpec from/to agent
network → agent, events, stream, knowledge, task — hub, channels, envelope, links, adapters
mcp     → tools, context             — MCP client (stdio+HTTP JSON-RPC), MCPToolkit
testing → config, events, stream     — TestConfig, TrackingConfig, fake clients
examples/                            — runnable programs (offline ones use testing pkg)
```

Forbidden: any import cycle; `agent` must never import `network` (network imports agent);
`config` must never import `agent`.

## Package contracts

### package `events` (see docs/specs/core.md §2, events-network.md §1)

```go
// Event is the base interface. Each concrete event is a struct; type identity via EventType().
type Event interface {
    EventType() string                    // Python class name, e.g. "TextInput", "ModelResponse"
    CreatedAt() time.Time
    Transient() bool                      // __transient__
    Conversational() bool                 // __conversational__
}

type Base struct { CreatedAt time.Time `json:"created_at"` }  // embed; zero CreatedAt → time.Now() on first send
```

Concrete events (fields per specs; JSON tags = Python field names):
`TextInput{Content}`, `DataInput{Data any}`, `BinaryInput{Data []byte, MediaType string, Kind BinaryType, VendorMetadata map[string]any}`,
`UrlInput{URL string, Kind BinaryType}`, `FileIdInput{FileID, Filename string}`,
`ModelRequest{Parts []Input, Metadata map[string]any}`, `DrainedModelRequest`, `MessageEnqueued`,
`ModelMessage{Content string, Metadata map[string]any}`, `ModelMessageChunk{Content string}`, `ModelReasoning{Content string}`,
`ModelResponse{Message *ModelMessage, ToolCalls ToolCallsEvent, Usage Usage, ResponseForce bool, Files []BinaryResult, Model, Provider, FinishReason, ResponseID string}`,
`UsageEvent{Usage Usage, Kind string, Model, Provider, FinishReason, Label string, Parts []UsageEvent}`,
`HumanInputRequest{ID, Content string, Timeout float64, TaskID string}`, `ToolApprovalRequest{ToolCallID string}` (embeds HumanInputRequest), `HumanMessage{ParentID, Content string}`,
`ToolCallsEvent{Calls []ToolCallEvent}`, `ToolResultsEvent{Results []ToolResultEvent}`,
`ToolCallEvent{ID, Name, Arguments string, VendorMetadata map[string]any, Source string}`,
`ToolResultEvent{ParentID, Name string, Result ToolResult}`, `ToolErrorEvent{Error string}`, `ToolNotFoundEvent`,
`TaskStarted/TaskProgress/TaskCompleted/TaskFailed/TaskExpired/TaskCancelled`,
`CompactionSummary{Content string}`, `ObserverAlert{Source string, Severity Severity, Message string, Data map[string]any}`, `HaltEvent{Reason string}`,
lifecycle events (`ObserverStarted/Completed`, `CompactionStarted/Completed/Failed`, `AggregationStarted/Completed/Failed`, `UnknownEvent`).

```go
type Input interface { Event; inputMarker() }
type BinaryType string  // "binary","image","audio","document","video"
type Severity string    // "info","warning","error","critical","fatal"

type ToolResult struct {
    Parts    []Input `json:"parts"`
    Final    bool    `json:"final,omitempty"`
    Metadata map[string]any `json:"metadata,omitempty"`
}

type Usage struct {
    PromptTokens, CompletionTokens, TotalTokens            *int64
    CacheReadInputTokens, CacheCreationInputTokens         *int64
    ThinkingTokens                                         *int64
}
func (u Usage) Add(o Usage) Usage   // field-wise; nil+nil=nil

type BinaryResult struct { Data []byte; Metadata map[string]any }

// Condition DSL → predicates.
type Condition func(Event) bool
func AnyOf(c ...Condition) Condition; func AllOf(c ...Condition) Condition; func Not(c Condition) Condition
func TypeOf[T Event]() Condition    // type match incl. embedded/base kinds
func Equal[T comparable](get func(Event) T, want T) Condition

// Serialization: JSON with a "@type" discriminator.
func MarshalEvent(ev Event) ([]byte, error)
func UnmarshalEvent(data []byte) (Event, error)   // registry keyed by EventType()
func RegisterEvent(ev Event)                       // idempotent; done in init() of the defining file

// Input helpers.
func EnsureInput(content any) Input                // string → TextInput, else DataInput
func EnsureRequest(msgs []any) *ModelRequest
// factories: ImageInput/DocumentInput/AudioInput/VideoInput (from URL, file path, or data+mediaType)

// History-repair helper (used by agent on HumanInputError):
func CloseUnansweredToolCalls(...) // sentinel result: Python's human-readable abandoned-call sentence (events.SentinelToolResult, D-043)
func EstimatedTokens(ev Event, charsPerToken int) int  // text len/4; image/audio 1000, video/doc 2000
```

All event structs implement `EventType()` and embed `Base`; `Transient()`/`Conversational()` are
methods on each type (per the flags in the specs). JSON round-trip via `MarshalEvent/UnmarshalEvent`
must be lossless for every exported event (test it).

### package `stream` (specs: core.md §2 Stream/history, events-network.md §1)

```go
type Storage interface {
    SaveEvent(streamID string, ev events.Event) error
    GetHistory(streamID string) ([]events.Event, error)
    SetHistory(streamID string, evs []events.Event) error
    DropHistory(streamID string) error
}
// ships: MemoryStorage (map+mutex); DiskStorage later (knowledge pkg owns file-based EventLog)

type Subscriber func(ev events.Event, ctx *contextpkg.Context) error   // ctx = *ago/context.Context (import: ago/context)
type Condition = events.Condition
type SubID string

type SubscribeOptions struct {
    Interrupt  bool       // runs first; return false or non-nil error to stop delivery of ev
    Condition  Condition
    Timeout    time.Duration
}

type Stream interface {
    ID() string
    History() *History
    Pending() []*events.ModelRequest            // snapshot of inbox
    Send(ev events.Event, ctx *contextpkg.Context) error
    Enqueue(parts ...events.Input) error        // appends ModelRequest to inbox + MessageEnqueued
    Subscribe(fn Subscriber, opts SubscribeOptions) SubID
    Unsubscribe(id SubID)
    Where(c Condition) Stream                   // filtered view sharing id/history/pending
    Get(c Condition) (events.Event, error)      // first matching event: checks history+incoming until match
}

type MemoryStream struct{...}  // implements Stream; sync dispatch under RWMutex
type SubStream struct{...}     // filtered view
type History struct{...}       // GetEvents() []events.Event; Replace([]events.Event); backed by Storage
func NewMemoryStream(opts ...MemoryOption) *MemoryStream   // WithStorage(s), WithID(id), PersistAll(bool)
func (m *MemoryStream) SpawnBackground(ctx context.Context, fn func(context.Context)) // go func + recover
```

Send semantics (parity with Python): interrupt subscribers first (in registration order; a
subscriber returning an error or `false`-equivalent stops delivery of the event to everyone),
then normal subscribers filtered by their condition, in registration order. Persist non-transient
events (all if PersistAll). Errors from normal subscribers: log + continue (Python swallows into
`EventLogFailed`-style telemetry; AGo logs via the package-level `Logger`).

### package `context`

```go
type Context struct {
    Stream        stream.Stream
    Dependencies  map[string]any          // DI values, keyed by string id
    Variables     map[string]any          // serializable; reserved prefixes "ag:", "a2a:"
    Prompt        []string                // system-prompt fragments
    ToolCallID    string
    // private: hitl hook, mutex, background wg
}
func New(s stream.Stream) *Context
func NewWithDeps(s stream.Stream, deps map[string]any, vars map[string]any) *Context
func (c *Context) Enqueue(parts ...events.Input) error
func (c *Context) Pending() []*events.ModelRequest
func (c *Context) Send(ev events.Event) error
func (c *Context) SpawnBackground(ctx context.Context, fn func(context.Context))
func (c *Context) Input(ctx context.Context, message string, timeout time.Duration) (string, error) // HITL
func (c *Context) Ask(ctx context.Context, req *events.HumanInputRequest) (string, error)
func (c *Context) SetHitlHook(h hitl.HumanHook)
func (c *Context) Dependency(id string) (any, bool)
func (c *Context) MustDependency(id string) (any, error)
func (c *Context) Clone() *Context   // shares Stream, copies maps
```
Concurrency-safe (mutex around Variables/Prompt mutation). `Ask` sends `HumanInputRequest`, waits
for the matching `HumanMessage` (parent_id == req.ID) via `Stream.Get`, normalizes all failures to
hitl errors (see `hitl`).

### package `hitl` (specs: events-network.md §5 adjacent)

```go
type HumanHook func(ctx context.Context, req *events.HumanInputRequest) (*events.HumanMessage, error)
func DefaultHook(ctx context.Context, req *events.HumanInputRequest) (*events.HumanMessage, error)
    // returns ErrNotProvided
var ErrNotProvided = errors.New("hitl: no human input hook provided")
type NotProvidedError / FailedError{Cause error} / TimeoutError{Timeout time.Duration}  // wrap ErrNotProvided etc.
```

### package `response` (specs: core.md §2, config-llm.md §2)

```go
type Proto interface {
    Name() string
    Description() string
    JSONSchema() map[string]any
    SystemPrompt() string
    Validate(response string, ctx *context.Context) (any, error)
    // AGO-DEVIATION: Python passes provider to validate; AGo omits it (unused by builtin impls).
}
type Schema[T any] struct{...}
func NewSchema[T any](name, description string) *Schema[T]  // JSON schema via reflection over T
func RawSchema(schema map[string]any, opts...) *RawSchemaImpl  // no validation
// JSON-schema generation (tools pkg shares it):
//   struct field tags: `json:"name"` + `agschema:"description=...,enum=a|b,required,min=1"`
//   map Go types: string→string, int→integer, float→number, bool→boolean, []T→array,
//   struct→object, map[string]T→object(additionalProperties), time.Time→string(format=date-time)
//   scalars at top level (string/int/float/bool) → wrapped {"type":"object","properties":{"data":...}} (Python parity)
```
`Validate` unmarshals JSON into T; on failure returns `*ValidationError{Err error, Response string}`
(agent uses it to re-ask with the error appended). PromptedSchema: schema + extra prompt text,
validate = parse only. DecisionSpec/Question: `Question{Text string, Options []DecisionOption}` →
enum schema.

### package `tools` (specs: tools.md — the authoritative doc)

```go
type Schema struct {  // FunctionToolSchema parity
    Name         string         `json:"name"`
    Description  string         `json:"description"`
    Parameters   map[string]any `json:"parameters"`   // JSON schema object; no "title"
    DeferLoading bool           `json:"defer_loading,omitempty"`
}
type ToolSchema interface { SchemaType() string }  // builtin/hosted schemas (type="web_search", ...)

type Tool interface {
    Name() string
    DeclaredInCode() bool
    Schemas(ctx *context.Context) ([]any, error)      // []Schema or ToolSchema impls
    Register(ctx *context.Context, mw []middleware.Middleware) ([]stream.SubID, error)  // subscribe on stream; mw = tool-scoped; returns ids for the turn scope's teardown
}
// AGO-DEVIATION: no ExitStack — Go cleanup is agent-owned: Register returns the ids of the
// subscriptions it created (on error nothing stays subscribed); the turn scope collects them
// and unsubscribes on Close. The per-stream turn lock serializes turns, so per-turn
// registration never stacks duplicates (docs/DECISIONS.md D-032).

type FunctionTool struct{...}
func NewFunctionTool(name, description string, fn any, opts ...FunctionToolOption) (*FunctionTool, error)
// fn: plain func; parameters mapped from JSON args by position/name via reflection; last-injectable params:
//   *context.Context (any param name), Inject(id string, defaults...), Variable(name, defaults...)
//   — see annotations below. Result: (T, error) | T | error only; T → TextInput(string) or DataInput / JSON.
// opts: WithToolSchema(map), WithTimeout(d), WithMiddleware(mw ...middleware.Middleware), WithSource(string)

type Toolkit struct{...}
func NewToolkit(name string, mws []middleware.Middleware, ts ...Tool) (*Toolkit, error) // dup name → ErrConflict
type ClientTool struct{...}    // DeclaredInCode=false; call → ClientToolCallEvent
type SubagentTool = FunctionTool variant wrapping agent (lives in agent pkg to avoid import cycle;
                        agent.AsTool returns tools.Tool)

type Executor struct{...}
func NewExecutor() *Executor
func (e *Executor) Register(ctx *context.Context, ts []Tool, known map[string]bool, mws []middleware.Middleware) ([]stream.SubID, error)
// subscribes: batch handler on ToolCallsEvent; per-tool handlers; fallback → ToolNotFoundEvent;
// returns all created subscription ids for the turn scope's teardown (unsubscribed on Close).
// Parallel execution (goroutine per call, errgroup-style); HumanInputError cancels siblings.
// Per-call: send ToolCallEvent → tool execution (middleware chain: tool mw innermost, agent mw outermost;
// timeout → ToolErrorEvent "Tool 'X' timed out after N seconds") → ToolResultEvent / ToolErrorEvent.
// final=true: exactly one part; TextInput→string, DataInput→JSON; emit forced ModelResponse(response_force=true).

type ResolvedTools struct { Tools []Tool; Schemas []any; Known map[string]bool }
func ResolveTools(all []Tool, ctx *context.Context) (*ResolvedTools, error)
// precedence: declared-in-code wins in declaration order (warn); runtime tools never override, dropped;
// remote-vs-remote first wins; prune losing toolkit members for this turn.

// DI annotations:
type inject struct{ ID string; HasDefault bool; Default any; VariableName string }
func Inject(id string, def ...any) inject           // struct tag alternative: `aginject:"id"`
func Variable(name string, def ...any) inject
// In NewFunctionTool reflection: param of type *context.Context → conversation ctx (tool_call_id stamped);
// param with Inject/Variable (or tags) resolved from ctx.Dependencies / ctx.Variables.

var ErrConflict = ...; ErrNotFound = ...; ErrResolution = ...
```

### package `config` (specs: config-llm.md — authoritative)

```go
type Provider string
const (
    ProviderOpenAI Provider = "openai"; ProviderAnthropic = "anthropic"; ProviderGemini = "gemini"
    ProviderBedrock = "bedrock"; ProviderDashScope = "dashscope"; ProviderMistral = "mistral"
    ProviderOllama = "ollama"; ProviderTypeSafe = "typesafe"; ProviderVertexAI = "vertexai"
    ProviderXAI = "xai"; ProviderZAI = "zai"
)

type ModelConfig interface {
    Provider() Provider
    Model() string
    Create() (LLMClient, error)
    Clone(overrides map[string]any) (ModelConfig, error)   // dataclasses.replace parity
}

type LLMClient interface {
    Call(ctx context.Context, messages []events.Event, conv *context.Context, opts CallOptions) (*events.ModelResponse, error)
}
type CallOptions struct {
    Tools          []any            // []tools.Schema / ToolSchema impls
    ResponseSchema response.Proto   // nil = plain text
}

type CallBuilder func(ctx context.Context, req *HTTPRequest) (*http.Response, error)  // test seam; default http.Client

// Config structs (one per provider, fields per specs §1; option methods instead of dataclass kwargs):
type OpenAICompletionsConfig struct { Model, APIKey, BaseURL string; Temperature, TopP *float64;
    MaxTokens, MaxCompletionTokens *int64; Stop []string; Seed *int64; User string;
    ParallelToolCalls *bool; ResponseFormat map[string]any; Streaming bool; Timeout time.Duration;
    MaxRetries int; Organization, Project string; DefaultHeaders map[string]string; ExtraBody map[string]any }
func NewOpenAICompletions(model string) *OpenAICompletionsConfig
// same pattern: NewOpenAIResponses, NewAnthropic(model) (MaxTokens=4096, MaxRetries=2, PromptCaching=true),
// NewGemini(apiKey, model), NewVertexAI(project, location, model), NewOllama(model) (host default
// http://127.0.0.1:11434), NewMistral(model), NewDashScope(model), NewZAI(model),
// NewXAI(model) [stub client], NewBedrock(model) [stub], NewTypeSafe(model) [stub]
// Missing model or key → Create() returns ConfigNotProvidedError-style error.

// Provider errors:
var ErrNotProvided, ErrUnsupportedInput, ErrUnsupportedTool  // AS/Is-match Python ConfigNotProvidedError etc.

// HTTP plumbing shared by providers: internal/sse (data: line parser with retry-after),
// internal/httpx (JSON req/resp helpers, retries with backoff honoring MaxRetries, timeouts).
```
Providers: **openai** (chat completions, streaming SSE incl. tool-call delta merge, tool mapping,
strict schema coercion, usage normalization incl. cached/reasoning tokens), **anthropic**
(messages API, SSE, tool use blocks, cache_control injection when PromptCaching, thinking replay),
**gemini** (generateContent, functionDeclarations, thinking config), **ollama** (chat, tools,
num_predict), **zai/mistral/dashscope** (OpenAI-compatible-ish REST), **xai/bedrock/typesafe** —
stub: config structs exist, `Create()` returns `errors.ErrUnsupported`("provider X not yet implemented").
Mapper pattern per provider: `mappers.go` (history→wire, schema→wire, usage normalize), `client.go`.

### package `middleware` (specs: events-network.md §2)

```go
type Middleware interface {
    OnTurn(next TurnFunc, ev *events.ModelRequest, ctx *context.Context) (*events.ModelResponse, error)
    OnLLMCall(next LLMCallFunc, evs []events.Event, ctx *context.Context) (*events.ModelResponse, error)
    OnToolExecution(next ToolExecFunc, ev *events.ToolCallEvent, ctx *context.Context) (events.Event, error)
    OnHumanInput(next HumanInputFunc, ev *events.HumanInputRequest, ctx *context.Context) (*events.HumanMessage, error)
}
type TurnFunc     func(ev *events.ModelRequest, ctx *context.Context) (*events.ModelResponse, error)
type LLMCallFunc  func(evs []events.Event, ctx *context.Context) (*events.ModelResponse, error)
type ToolExecFunc func(ev *events.ToolCallEvent, ctx *context.Context) (events.Event, error)
type HumanInputFunc func(ev *events.HumanInputRequest, ctx *context.Context) (*events.HumanMessage, error)

type Base struct{}  // embed; each hook defaults to `return next(...)`
type Factory func() Middleware
type Conditional struct{...}  // wraps Factory with events.Condition; skips inner when false
func If(c events.Condition, f Factory) Factory

// Builtins (middleware/builtin): HistoryLimiter(n), TokenLimiter(budget, charsPerToken),
// Retry(maxAttempts, isRetryable), Logging(logger), Approval(toolName string, granted map[string]bool)
```

### package `usage`

```go
type Record struct { Usage events.Usage; Kind, Model, Provider, FinishReason, Label string }
type Report struct { Total events.Usage; Records []Record; ByModel, ByProvider, ByKind map[string]events.Usage }
func FromEvents(evs []events.Event) *Report   // aggregates UsageEvent (kind model_call/subtask/compaction/aggregation)
```

### package `task` (specs: core.md §2)

```go
type State string  // created, running, completed, failed, expired, cancelled
type Spec struct { Title, Description string; Payload map[string]any; Capability string }
type Metadata struct { TaskID, OwnerID string; Spec Spec; State State; CreatedAt, StartedAt,
    CompletedAt, ExpiresAt time.Time; LastProgressAt time.Time; Progress map[string]any;
    Result any; Error string; ChannelID string }  // to/from JSON
type CheckpointStore interface { Write(taskID string, state []byte) error; Read(taskID string) ([]byte, error) }
type Task struct{...}
func Start(ctx *context.Context, owner string, spec Spec, opts ...Option) (*Task, error)
func (t *Task) Progress(update map[string]any)   // emits TaskProgress (transient)
func (t *Task) Complete(result any); Fail(err error); Cancel(reason string); Expire()
func (t *Task) Checkpoint(state []byte); ResumedState() []byte
func (t *Task) Done() <-chan struct{}
```

### package `knowledge`

```go
type Store interface {
    Read(path string) ([]byte, error); Write(path string, data []byte) error
    List(dir string) ([]string, error); Delete(path string) error; Exists(path string) bool
    Append(path string, data []byte) error; ReadRange(path string, off, n int) ([]byte, error)
    OnChange(fn func(path string))
}
// ships: MemoryStore, DiskStore(root string); Locked wrapper.
type EventLogWriter struct{...}   // append-only JSONL of events (hub WAL, agent event log)
```

### packages `policies`, `compact`, `aggregate`, `observers`, `watch` (specs: events-network.md §3, §5)

```go
// policies
type AssemblyPolicy interface { Apply(prompts []string, evs []events.Event, ctx *context.Context) ([]string, []events.Event, error) }
// impls: ConversationPolicy{}, SlidingWindowPolicy(n), TokenBudgetPolicy(budget), WorkingMemoryPolicy(store),
// EpisodicMemoryPolicy(store), AlertPolicy{} (dedup, inject non-fatal, emit HaltEvent on fatal)

// compact: Trigger interface (ShouldTrigger(evs, usage) bool); Strategy (Summarize via LLMClient); CompactMiddleware
// aggregate: same shape, aggregates child streams; AggregationMiddleware
// observers: Observer interface { Register(ctx *context.Context) error }; StreamObserver(cond, fn);
//   LoopDetector (n-gram repeat → ObserverAlert), TokenMonitor (budget → alert)
// watch: Watch interface { Wait(ctx) (events.Event, error) }; EventWatch(cond), IntervalWatch(d),
//   DelayWatch(d), AllOf/AnyOf/Sequence composites (cron postponed — DECISIONS)
```

### package `agent` (specs: core.md — authoritative; the heart)

```go
type Option func(*options)
func WithConfig(cfg config.ModelConfig); WithHitlHook(h hitl.HumanHook)
func WithTools(ts ...tools.Tool); WithMiddleware(fs ...middleware.Factory)
func WithResponseSchema(p response.Proto); WithDependencies(map[string]any); WithVariables(map[string]any)
func WithStreamBase(s stream.Stream)     // reuse state across asks
func WithTasks(cfg TaskConfig); WithKnowledge(k knowledge.Store)

type Agent struct{...}
func New(name, prompt string, opts ...Option) *Agent
func (a *Agent) Name() string
func (a *Agent) PromptHook(fn func(ctx *context.Context) ([]string, error))  // dynamic prompt fragments
func (a *Agent) Ask(ctx context.Context, msgs ...Sendable) (*AgentReply, error)
func (a *Agent) Run(ctx context.Context, msgs ...Sendable) (*AgentRun, error)
func (a *Agent) Resume(ctx context.Context, evs ...events.Event) (*AgentReply, error)
func (a *Agent) AsTool(opts ...AsToolOption) tools.Tool   // SubagentTool: description, name
func (a *Agent) Task(ctx context.Context, title string, opts ...TaskOption) (*task.Task, error)
func (a *Agent) Spec() (*spec.AgentSpec, error)  // in spec pkg; method forwards

type Sendable = any   // string | []byte | events.Input | JSON-decodable (EnsureRequest semantics)

type AgentRun struct{...}
func (r *AgentRun) Start(ctx context.Context) error
func (r *AgentRun) Result(ctx context.Context) (*AgentReply, error)  // idempotent; same error re-raised
func (r *AgentRun) Stream() stream.Stream
func (r *AgentRun) Enqueue(parts ...events.Input) error
func (r *AgentRun) Close()   // ends turn scope

type AgentReply struct{...}
func (r *AgentReply) Body() string            // "" if none
func (r *AgentReply) Response() *events.ModelResponse
func (r *AgentReply) Content() (json.RawMessage, error)        // schema-validated
func (r *AgentReply) ContentAs[T any](retries int) (T, error)  // re-ask loop on validation failure
func (r *AgentReply) Files() []events.BinaryResult
func (r *AgentReply) History() *stream.History
func (r *AgentReply) Usage(ctx context.Context) *usage.Report
func (r *AgentReply) Ask(ctx context.Context, msgs ...Sendable) (*AgentReply, error)  // continuation, same stream
func (r *AgentReply) Run(ctx context.Context, msgs ...Sendable) (*AgentRun, error)

// TaskConfig { Config config.ModelConfig; Prompt string; IncludeTools []string; ExcludeTools []string;
//              ExtraTools []tools.Tool; MaxConcurrency int }  // IncludeTools: nil = all parent tools (Python None), empty = none, else allowlist (D-039)
```

Turn loop (agent/turn.go) — parity with core.md §3: per-stream turn lock (sync.Map[streamID]*sync.Mutex);
prepare turn (resolve config → ErrConfigNotProvided; seed context.prompt from name+prompt+hooks);
middleware chain instantiation; subscribers: LLM trigger on ModelRequest|ToolResultsEvent (skip
DrainedModelRequest), HITL hook interrupt-subscriber on HumanInputRequest, ToolExecutor on
ToolCallsEvent; drain inbox → initial ModelRequest; loop: LLM call → emit UsageEvent(model_call) →
ModelResponse; while tool_calls && !response_force: send ToolCallsEvent → ToolResultsEvent (executor
drives next call); final drain of leftover inbox into one extra model call. HumanInputError escaping →
CloseUnansweredToolCalls history repair → propagate. HaltEvent → synthetic response "HALTED: <reason>".
Subtasks: run_subtask/run_subtasks tools spawning child Agents (TaskConfig), rolled-up UsageEvent(kind=subtask).

### package `spec`

```go
type AgentSpec struct { Name string; Prompt []string; ToolNames []string; ResponseSchema *SchemaSpec }
type SchemaSpec struct { Name, Description string; Schema map[string]any }
func FromAgent(a *agent.Agent) (*AgentSpec, error)
func (s *AgentSpec) ToAgent(available map[string]tools.Tool, opts ...agent.Option) (*agent.Agent, error) // missing tool → tools.ErrResolution
```

### package `network` (specs: events-network.md §4 — port the functional subset)

Subpackages: `network` (root: Envelope, Channel types, Rule, Passport, Handoff/Finish),
`network/transport` (Frame, Link protocol, LocalLink), `network/hub` (Hub core, arbiter seam,
audit JSONL, expectations), `network/client` (HubClient, AgentClient, HumanClient, Channel handle,
NetworkPlugin), `network/adapters` (Consulting, Conversation, Discussion, Workflow + views).
Envelope event types: `ag2.msg.text`, `ag2.packet`, `ag2.channel.*`, `ag2.task.*`, `ag2.context.set`,
`ag2.expectation.violated` (keep the wire vocabulary). TransitionGraph targets/conditions per spec.
WAL via knowledge.EventLogWriter.

### package `mcp`

Minimal MCP client: JSON-RPC 2.0 over stdio (`os/exec`) and streamable HTTP; `initialize`,
`tools/list` (pagination), `tools/call`; content mapping (text→TextInput, image/audio→BinaryInput
base64, resource link→UrlInput, unknown→JSON text); `is_error` → error; config
`MCPServerConfig{URL, Headers, AuthorizationToken, ServerLabel, AllowedTools, BlockedTools,
ToolNamePrefix}` and `MCPStdioConfig{Command, Args, Env, CWD}`; `MCPToolkit` → tools.Tool with
`DeclaredInCode=false`.

### package `testing`

```go
type TestConfig struct{...}      // scripted LLMClient: queue of ModelResponses; implements config.ModelConfig+LLMClient
func NewTestConfig(scripted ...*events.ModelResponse) *TestConfig
func (t *TestConfig) Calls() [][]events.Event    // what history each call saw
type TrackingConfig struct{...}  // wraps real config, records calls
```

### examples/

Offline (run without API keys): `examples/ask` (Agent + TestConfig), `examples/tools` (function
tools + scripted responses), `examples/hitl` (CLI hook), `examples/subagent` (as_tool), `examples/group`
(network WorkflowAdapter round-robin). Online (documented, need env keys): `examples/openai_chat`.
Each has its own `main.go` in package main; README table of what each demonstrates.

## Port waves (execution plan)

- **Wave 0 (manual)**: module scaffold, docs, `events`, `stream`, `context` — foundation contracts.
- **Wave 1 (workflow, parallel)**: hitl+middleware, response, tools, config-core+internal/httpx+internal/sse,
  providers openai, providers anthropic, providers gemini+ollama, providers zai/mistral/dashscope+stubs,
  usage+task, testing. Each agent: implement package + tests, run `go build ./ago/... && go test ./ago/<pkg>/`,
  iterate to green.
- **Wave 2 (manual)**: `agent` (turn loop) — most subtle, written by the lead.
- **Wave 3 (workflow, parallel)**: knowledge, policies, compact, aggregate, observers, watch, spec,
  mcp, network subset, task-subtools in agent, examples, README/PORT_STATUS.
- **Wave 4 (workflow, parallel fix loop)**: whole-repo `go vet` + `go test` green; port-parity spot checks;
  final docs + git commits.
