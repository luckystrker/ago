# AGo — User Guide

AGo is a Go framework for building LLM agents: one `Agent` type, an event stream per
conversation, tools registered as stream subscribers, middleware wrapped around every
turn, model call, tool execution and human-input hop, and a hub that hosts multi-agent
channels. It is a port of AG2 (Python) with zero external dependencies — stdlib only,
module `github.com/luckystrker/ago`, Go 1.24; `go get github.com/luckystrker/ago` and import
`"github.com/luckystrker/ago/..."`. Every snippet below was compile-checked against the tree
as of 2026-10-30.

Sections: [Concepts](#concepts) · [Agents](#agents) · [Tools](#tools) ·
[Tool packs](#tool-packs-toolpacks) · [Plugins](#plugins) ·
[Model providers](#model-providers) ·
[Files API](#files-api) · [Middleware](#middleware) ·
[Human-in-the-loop](#human-in-the-loop) ·
[Observers, watches, alerts and halts](#observers-watches-alerts-and-halts) ·
[Knowledge, policies, compaction, aggregation](#knowledge-policies-compaction-aggregation) ·
[MCP](#mcp-model-context-protocol) ·
[Protocol endpoints](#protocol-endpoints-a2a-acp-ag-ui) · [Multi-agent network](#multi-agent-network) ·
[Testing](#testing) · [Evaluation](#evaluation) ·
[Terminal chat and voice](#terminal-chat-and-voice-tui-live) ·
[Examples index](#examples-index) · [Troubleshooting](#troubleshooting)

## Concepts

**Agent.** An `agent.Agent` is a conversable LLM loop. A bare agent renders your prompt
fragments as the system message, sends the conversation history to the configured model
and returns the answer. Everything else — tools, middleware, HITL, structured output,
subtasks — is opt-in via `agent.With*` options (`agent/agent.go`).

**Stream.** Every conversation lives on a `stream.Stream` (`github.com/luckystrker/ago/stream`): an event bus
with a durable transcript (`History`) and an inbox (`Enqueue`/`Drain` of pending
`ModelRequest`s). Subscribers receive events in registration order:
```go
s := stream.NewMemoryStream() // options: WithStorage, WithStreamID, WithPersistAll, WithLogger

id := s.Subscribe(func(ev events.Event) error {
	fmt.Println("saw", ev.EventType())
	return nil
}, stream.SubscribeOptions{Condition: events.TypeOf[events.ModelResponse]()})
defer s.Unsubscribe(id)
```

- `Send(ev)` dispatches an event and, unless it is transient (streaming chunks,
  bookkeeping), appends it to the transcript.
- An **interrupt** subscriber (`SubscribeOptions.Interrupt`) runs first; returning
  `stream.ErrHandled` swallows the event (not delivered, not persisted); any other
  error stops delivery for everyone and surfaces from `Send`.
- `s.Where(cond)` returns a filtered view sharing id/history; `s.Get(ctx, cond)` waits
  for the first matching incoming event (already-persisted history is not scanned).
  `agent.WithStreamBase(s)`
  reuses one stream across asks — one conversation, several turns.

**Events.** Events are the only vocabulary. Each is a struct embedding `events.Base`;
the type name travels on the wire as `@type` and round-trips through
`events.MarshalEvent` / `events.UnmarshalEvent`. The main families:

| Family | Events |
|---|---|
| inputs | `TextInput`, `DataInput`, `BinaryInput`, `UrlInput`, `FileIdInput`, `ModelRequest` |
| model | `ModelMessageChunk`, `ModelReasoning` (transient), `ModelResponse` (durable) |
| tools | `ToolCallsEvent`, `ToolResultsEvent`, `ToolResultEvent`, `ToolErrorEvent`, `ToolNotFoundEvent`, `ClientToolCallEvent`, `BuiltinToolCallEvent`, `BuiltinToolResultEvent` |
| human | `HumanInputRequest`, `ToolApprovalRequest`, `HumanMessage` |
| usage | `UsageEvent` (kinds `model_call`, `subtask`, `compaction`, `aggregation`) |
| tasks | `TaskStarted/Progress/Completed/Failed/Expired/Cancelled` |
| lifecycle | `CompactionSummary`, `ObserverAlert`, `HaltEvent`, `CompactionStarted/...` |

Match events with the `Condition` DSL (`events/condition.go`) and normalize
pointer/value forms with `events.AsValue(ev)` in type switches:

```go
cond := events.AnyOf(
	events.TypeOf[events.ModelResponse](), // matches value or pointer form
	events.Not(events.TypeOf[events.UsageEvent]()),
)
switch e := events.AsValue(ev).(type) {
case events.ModelResponse:
	fmt.Println(e.Content())
case events.ToolCallsEvent:
	for _, c := range e.Calls {
		fmt.Println("tool call", c.Name, c.Arguments)
	}
}
```

Send user content with `events.EnsureRequest` (`string` → `TextInput`, anything
JSON-decodable → `DataInput`). Binary content has factories, each taking exactly one of
URL, path or data:

```go
img, err := events.ImageInput("https://example.com/cat.png", "", nil, "") // → UrlInput
ask := events.EnsureRequest("What is in this picture?", img)
```

**Context.** `agocontext.Context` (`github.com/luckystrker/ago/context`) is the per-conversation handle tools,
middleware and observers receive: the stream, the system-prompt fragments, dependency
(DI) and variable maps, and the HITL hook.

```go
cx := agocontext.New(s)          // or NewWithDeps(s, deps, vars)
cx.AddPrompt("Extra fragment.")  // system-prompt lines
cx.SetDependency("db", pool)     // DI: tools.Inject reads this
cx.SetVariable("tone", "terse")  // variables: tools.Variable reads this
```

It is safe for concurrent use; `Clone()` shares the stream but copies the maps (the
agent clones the context between turns, so per-turn mutations do not leak backwards).

## Agents

### Creating and asking

```go
a := agent.New("assistant", "You are helpful.",
	agent.WithConfig(cfg)) // required before Ask (see Troubleshooting)

reply, err := a.Ask(context.Background(), "What is the weather in Lisbon?")
// ...
fmt.Println(reply.Body()) // final text ("" when the model answered only with tool calls)
```

All options (`agent/agent.go`): `WithConfig`, `WithHitlHook`, `WithTools`,
`WithMiddleware`, `WithDependencies`, `WithVariables`, `WithResponseSchema`,
`WithStreamBase`, `WithPromptHook` (dynamic fragments per turn), `WithPromptFragments`,
`WithTasks`. A `Sendable` message is `any`: string, `events.Input`, `[]byte`, or a
JSON-decodable value (it becomes `DataInput`).

### Continuations

`AgentReply.Ask` continues the same stream — the whole transcript rides forward, so
constraints stated once stay in force:

```go
reply, err = reply.Ask(ctx, "Also make it vegetarian.")

evs, _ := reply.History().GetEvents() // durable transcript
fmt.Println(stream.RenderTranscript(evs))
rep := reply.Usage() // aggregates UsageEvents: Total, Records, ByModel/Provider/Kind
fmt.Println("tokens:", rep.Total.TotalTokens, "by kind:", rep.ByKind)
```

### Observable runs

`Ask` is blocking. `Run` hands you the `AgentRun` handle for progress and mid-turn
input (`agent/run.go`):

```go
run, err := a.Run(ctx, "Long question")

// Watch the conversation live:
run.Stream().Subscribe(func(ev events.Event) error {
	if chunk, ok := events.AsValue(ev).(events.ModelMessageChunk); ok {
		fmt.Print(chunk.Content) // streaming deltas, when the provider streams
	}
	return nil
}, stream.SubscribeOptions{})

// Feed extra input while the turn is running; it merges into the model's view:
_ = run.Enqueue(events.EnsureInput("One more constraint: keep it short."))

reply, err = run.Result(ctx) // Start() is implicit and idempotent; waits for the turn
defer run.Close()            // ends the turn scope; cancels an in-flight turn
```

### Resume

`Resume` replays a trajectory: all events but the last seed the fresh stream's history,
the last is the re-entry trigger. A `ModelRequest` trigger re-enters through the inbox;
any other event (for example a `ToolResultsEvent`) is sent and the turn continues from
the model's next response:

```go
seed := make([]events.Event, 0, len(savedJSONL))
for _, b := range savedJSONL { // events previously stored with events.MarshalEvent
	ev, err := events.UnmarshalEvent(b)
	if err != nil {
		return nil, err
	}
	seed = append(seed, ev)
}
reply, err := a.Resume(ctx, seed...)
```

### Structured output

Attach a response schema (`github.com/luckystrker/ago/response`); the model is asked for JSON of that shape,
and validation holds the answer to it:

```go
type Recipe struct {
	Title       string   `json:"title" agschema:"description=Short human title."`
	Servings    int      `json:"servings"`
	Ingredients []string `json:"ingredients"`
}

schema := response.NewSchema[Recipe]("Recipe", "A complete recipe.")
chef := agent.New("chef", "Return a Recipe object.",
	agent.WithConfig(cfg), agent.WithResponseSchema(schema))
reply, err := chef.Ask(ctx, "Carbonara for 6, please.")

raw, err := reply.Content()                           // schema-validated raw JSON
recipe, err := agent.ContentAs[Recipe](ctx, reply, 1) // retries=1: on validation failure
// the model is re-asked once with the error text appended, on the same stream.
```

The schema is generated from the Go type by reflection; `json` tags fix field names,
`agschema` tags add `description=…`, `enum=a|b|c`, `required`, `min=…`. Validation
failures come back as `*response.ValidationError` (wraps `response.ErrInvalidResponse`);
bare scalars travel in a `{"data": …}` envelope. `response.RawSchema(map[string]any{...})`
takes a hand-written schema without validation; `response.QuestionSchema(response.Question{...})`
builds a yes/no / choice / score question.

### Tasks and subtasks

`Agent.Task` opens a tracked unit of work on the agent's private stream; the caller
drives the lifecycle (`agent/task.go`, `github.com/luckystrker/ago/task`):

```go
t, err := a.Task(ctx, "Quarterly report", agent.TaskWithTTL(10*time.Minute))
_ = t.Progress(map[string]any{"step": "outline"})
_ = t.Checkpoint(stateBytes) // pairs with task.WithCheckpointStore(store)
_ = t.Complete(report)
// also: t.Fail(err), t.Cancel(reason), t.Expire(), t.State(), t.Done(), t.ResumedState()
```

`WithTasks` instead gives the *model* delegation tools: `run_subtask` spawns a bare
child agent for one objective, `run_subtasks` runs several — concurrently by default
(`parallel: false` runs them sequentially). Children never receive the subtask tools,
so delegation cannot recurse:

```go
a := agent.New("lead", "Delegate research to subtasks.", agent.WithConfig(cfg),
	agent.WithTasks(agent.TaskConfig{
		Config: cfg, // nil → the parent's config
		Prompt: "You are a careful researcher.",
		// IncludeTools: nil (the default) → child inherits all parent tools;
		// []string{} → none; a list of names → allowlist.
		// ExcludeTools is applied after the allowlist.
		ExcludeTools:   []string{"deploy"},
		MaxConcurrency: 4,
	}))
```

Every delegation is reported on the caller's stream: `TaskStarted` before the child
turn, `TaskCompleted`/`TaskFailed` after, and the child's spend as one rolled-up
`UsageEvent{Kind: "subtask"}` — `reply.Usage().ByKind["subtask"]` reports it. The
child starts from a copy of the caller's dependencies and variables (the approval
bypass key `ag:approval_required:always` is dropped, so tool approvals are not
inherited) and its variable mutations stay scoped to the child run. A failed sub-task
comes back as `Error: …` text in the tool result, so the model can react;
`run_subtasks` joins the answers as `## <task>` sections.

If the child has no HITL hook of its own, its questions are bridged to the parent's
hook: a copy of the request stamped with the task id is sent to the parent stream (the
one delegation event that *is* transcribed there), and only the answer lands in the
child's transcript. With no hook on either side, the calling turn ends with a
`*hitl.NotProvidedError`.

### Agents as tools

`AsTool` exposes an agent as a callable tool for another agent. It delegates through
the same machinery as `WithTasks`: the tool takes an `objective` (plus optional
`context`), the caller's stream gets `TaskStarted`/`TaskCompleted`/`TaskFailed` and
the rolled-up subtask usage, and the child runs on its own stream persisted into the
caller's storage. The child's final text is the tool result; a failed sub-task is the
text `Sub-task '<name>' failed: …`. The default tool name is `task_<name>`
(`WithToolName` overrides it; Python parity — it was `ask_<name>` in earlier AGo
builds):

```go
specialist := agent.New("specialist", "You are a database specialist.",
	agent.WithConfig(childCfg))
lead := agent.New("lead", "You coordinate and delegate.", agent.WithConfig(cfg),
	agent.WithTools(specialist.AsTool(
		agent.WithToolName("ask_specialist"),
		agent.WithToolDescription("Delegate a database question."))))
```

## Tools

### Function tools

`tools.NewFunctionTool(name, description, fn, opts...)` wraps a Go function. The
function's arguments are the fields of **one struct parameter**, bound from the model's
JSON arguments by field name; the JSON schema is generated from the struct. Accepted
signatures (`tools/function_tool.go`):

```go
func(args Args) (T, error)  // the common form
func(args Args) T           // T != error
func(args Args) error
func() (T, error)           // no model arguments
func(args Args, cx *agocontext.Context) (T, error) // *agocontext.Context anywhere
```

```go
weather, err := tools.NewFunctionTool("get_weather",
	"Current weather for a city.",
	func(args struct {
		City string `json:"city" agschema:"description=City name"`
		Days int    `json:"days,omitempty"` // required despite omitempty: no agschema tag + non-pointer — json tags play no part
	}) (string, error) {
		return "sunny, +24 C", nil
	},
	tools.WithTimeout(5*time.Second), // expiry → ToolErrorEvent "Tool 'X' timed out after N seconds"
	tools.WithSource("weather:v1"), // identity stamped onto handled calls (Approval grants on it)
)
```

Return-value mapping: `events.ToolResult` passes through, `[]events.Input` becomes the
result parts, a string becomes a `TextInput`, anything else a `DataInput`. A returned
non-HITL error becomes a `ToolErrorEvent` the model sees and can recover from. To end
the turn immediately, return a **final** result — the executor stops the tool loop and
the single part becomes the forced answer:

```go
func(args struct{ Report string `json:"report"` }) (events.ToolResult, error) {
	return events.NewToolResult(true, args.Report), nil // final=true: exactly one part
}
```

A `*agocontext.Context` parameter receives the conversation context stamped with the
tool-call id — use it to enqueue follow-up input (`cx.Enqueue`), send events or ask the
human (`cx.Input`).

### Dependency injection

Fields of the argument struct that are not model arguments are DI slots
(`tools/di.go`), in two forms:

```go
type args struct {
	Query string `json:"query"` // model-supplied

	// 1. Typed slots — the framework delivers an Inject/Variable value whose
	//    Default field carries the resolved payload:
	API  tools.Inject   `aginject:"search-api"`
	Rate tools.Variable `agvariable:"rate_limit"`

	// 2. Plain fields with a tag — the resolved value is written directly:
	Token string `aginject:"api_token" agdefault:"dev-token"`
}
```

- `Inject` reads `Context.Dependencies` (set via `agent.WithDependencies` or `cx.SetDependency`); `Variable` reads `Context.Variables`. The lookup key is the tag value, falling back to the field's JSON name.
- A missing value with an `agdefault` (scalars only) uses — and materializes into the per-call context — the default. A missing value without one fails the call with an error wrapping `tools.ErrNotFound` ("… is not provided").
- DI fields are excluded from the schema the model sees; a DI value wins over a model-supplied one for the same field.

### Toolkits

A `Toolkit` groups tools under one name; duplicate member names are a
`tools.ErrConflict`. Its middleware is prepended to each `FunctionTool` member's own
chain:

```go
tk, err := tools.NewToolkit("weather",
	[]middleware.Middleware{ /* toolkit-scoped, optional */ },
	weather, radar)
agent.WithTools(tk) // the toolkit contributes all member schemas
```

### Hosted (builtin) tools

Some capabilities run **at the provider**, server-side. Their `github.com/luckystrker/ago/tools` constructors
(`tools/hosted.go`) build schema carriers: the schema joins the model request (translated
by the provider's mapper) and the provider executes — `Register` subscribes nothing, so
there is no local function to run and no local result event:

```go
country := "US"
a := agent.New("researcher", "...", agent.WithConfig(cfg),
	agent.WithTools(
		tools.NewWebSearch(tools.WithWebSearchMaxUses(3),
			tools.WithUserLocation(tools.UserLocation{Country: &country})),
		tools.NewCodeExecution(),
	))

// tool_search defers member tools: their schemas travel with defer_loading and
// the model discovers them through the provider's search tool on demand.
finder, err := tools.NewToolSearch("regex", deepResearch, crunchNumbers)
```

Constructors mirror the Python builtin kwargs — also `NewWebFetch`, `NewFileSearch`,
`NewImageGeneration`, `NewShell`, `NewMemory` and `NewMCPServer(url, label, opts...)`
(a remote MCP server the *provider* connects to; to connect to one yourself, use the
[MCP client](#mcp-model-context-protocol)). Which types a provider actually carries is
what the ported Python clients carry; anything else fails the call fast with
`config.ErrUnsupportedTool`:

| Provider | Carries | Rejects (`config.ErrUnsupportedTool`) |
|---|---|---|
| `config/anthropic` | web_search, web_fetch, code_execution, memory, mcp_server (incl. the request's `mcp_servers` parameter), tool_search | file_search, image_generation, shell |
| `config/gemini` | web_search (`googleSearch`), web_fetch (`urlContext`), code_execution, file_search (requires `WithFileSearchStoreNames`) | memory, mcp_server, tool_search, shell, image_generation |
| `config/openai` **Responses** (`openai.NewOpenAIResponses`, below) | web_search, file_search, code_interpreter, image_generation, shell (container auto or reference), mcp, tool_search (`defer_loading`) — hosted output comes back as `BuiltinToolCallEvent`/`BuiltinToolResultEvent` | — |
| chat-completions (`openai.NewOpenAICompletions`, zai, mistral, dashscope) and `config/ollama` | — | every hosted type (hosted mappings live on the Responses API and the other providers) |

The OpenAI Responses client is what carries OpenAI hosted tools — with the Chat
Completions config the same `tools.NewWebSearch(...)` fails fast. See
[The OpenAI Responses API](#the-openai-responses-api-openairesponsesconfig) below.

### Executor semantics

The agent registers a `tools.Executor` for every turn. What it guarantees
(`tools/executor.go`):

- **Parallel batch.** All calls of one `ToolCallsEvent` run on their own goroutines; the batch settles into one `ToolResultsEvent` that triggers the next model call.
- **final=true.** Exactly one `ToolResult` part; the executor emits a forced `ModelResponse` (`response_force=true`) and the turn ends without another tool loop.
- **Timeout.** `WithTimeout` bounds one invocation; expiry answers `ToolErrorEvent("Tool 'X' timed out after N seconds")`. The running function itself cannot be interrupted; its late outcome is discarded.
- **Not found.** A call whose name no registered tool owns is answered with `ToolNotFoundEvent` (error text `` Tool `X` not found ``). Hosted/builtin schemas (non-`function` `SchemaType`) are excluded — the provider settles those calls.
- **HITL first.** A human-input failure inside a tool (or approval middleware) is never recorded as a tool result: it abandons the batch and ends the turn with a `*hitl` error.
- **Client tools.** `tools.NewClientTool(map[string]any{...})` declares a tool the remote client executes itself — the argument is the flat wire schema (`"name"` required, plus `"description"`, `"parameters"`, `"defer_loading"`); the call becomes a `ClientToolCallEvent` replayed to the model as a forced `ModelResponse` for the client to answer.

## Tool packs (`toolpacks`)

`github.com/luckystrker/ago/toolpacks` carries the ready-made tools Python ships beside
the framework (`ag2/tools/{shell,code,search,skills,dynamic}`, `toolkits/filesystem.py`) —
each constructor returns a `tools.Tool` (or a toolkit of them) you pass to `agent.WithTools`
like any hand-written tool (D-062/D-063):

```go
shell, err := toolpacks.NewShellTool(
	toolpacks.WithShellWorkdir("C:/work"),   // commands run here (unset: the caller's dir)
	toolpacks.WithShellTimeout(30*time.Second),
	toolpacks.WithShellEnv(map[string]string{"GOFLAGS": "-trimpath"}),
	toolpacks.WithShellBlockPatterns(`rm\s+-rf\s+/`)) // advisory refusal patterns

runner, err := toolpacks.NewLocalCodeRunner() // run_code: python/bash/javascript locally

fs, err := toolpacks.NewFilesystemToolkit("C:/work/data") // read/write/list/mkdir/glob, root-guarded
a := agent.New("ops", "...", agent.WithConfig(cfg),
	agent.WithTools(shell, runner))
a2 := agent.New("filer", "...", agent.WithConfig(cfg), agent.WithTools(fs.Tools()...))
```

Web search against the real engines (HTML/REST, no SDK): `NewDuckDuckGoSearch(opts...)`,
`NewTavilySearch(apiKey, opts...)`, `NewPerplexitySearch(apiKey, opts...)` — the Tavily
and Perplexity keys can instead ride the conversation: build with
`WithTavilyAPIKeyVariable()` / `WithPerplexityAPIKeyVariable()` and set the declared
variable per conversation. `NewSkillsToolkit(dir).Tools()`
serves an [agentskills.io](https://agentskills.io) layout (`list`/`load`/
`read_skill_resource`). `NewDynamicAgentFactory(cfg, pool, opts...)` builds
spawn-on-demand sub-agents from a spec: the model names a role, the factory composes
`spec.ToAgent` + `AsTool` and the child's lifecycle events and usage roll up on the
caller's stream.

Deferred from the Python packs: Docker/Daytona sandboxes (the code runner is the local
backend), the TypeScript runner, `update_file`/`delete_file`, the skills script runner.

## Plugins

A plugin bundles an agent's cross-cutting contributions — tools, middleware and
system-prompt material — behind one named value (`github.com/luckystrker/ago/plugin`). Build a
`plugin.Bundle`, then hand it to `agent.WithPlugin`; the bundle expands into the very
options the agent already accepts, once, at construction (`agent/agent.go` `WithPlugin`):

```go
bundle := plugin.NewBundle("research-rules").
	WithTool(searchTool).
	WithMiddleware(mwFactory).                                    // any middleware.Factory (see Middleware below)
	WithPrompt("Cite sources for every claim.").
	WithPromptHook(func(cx *agocontext.Context) ([]string, error) {   // dynamic fragment, per fresh turn
		return []string{"Today is " + time.Now().Format("2006-01-02") + "."}, nil
	})

a := agent.New("researcher", "...", agent.WithConfig(cfg), agent.WithPlugin(bundle.Plugin()))
```

Ordering is exactly the plain options': plugin tools append after the agent's own
tools, plugin middleware appends after the agent's own factories (which places it
**innermost**, closest to the model), fragments extend the collectors, and hooks run
like `agent.WithPromptHook` hooks — seeded into the system prompt per fresh turn, not
re-run on continuation clones. A zero or nil plugin contributes nothing, so bundles are
safe to apply conditionally.

Plugins apply at construction only (D-044): there is no attach/detach
mid-conversation (Python's turn-scoped `additional_plugins` is not ported). The legs
Python plugins also collect — HITL hooks, dependencies, variables, observers and
assembly policies — are not carried, because the agent surface has no slot for them;
compose those from `WithHitlHook`, `WithDependencies`, `WithVariables`, `observers`
and middleware directly.

## Model providers

A provider config implements `config.ModelConfig` (`Provider()`, `Model()`,
`Create() (config.LLMClient, error)`, `Clone(overrides)`). Each provider lives in its
own package (`github.com/luckystrker/ago/config/openai`, `github.com/luckystrker/ago/config/anthropic`, …); API keys resolve the
config field first, then the standard environment variable. Missing model or key →
`Create()` returns an error wrapping `config.ErrNotProvided`.

| Constructor (package `github.com/luckystrker/ago/config/…`) | Fields / options | Env fallback |
|---|---|---|
| `openai.NewOpenAICompletions(model)` | `APIKey`, `BaseURL`, `Temperature/TopP *float64`, `MaxTokens/MaxCompletionTokens *int64`, `Stop`, `Seed`, `User`, `ParallelToolCalls *bool`, `ResponseFormat`, `Streaming`, `Timeout`, `MaxRetries` (default 4), `Organization`, `Project`, `DefaultHeaders`, `ExtraBody` | `OPENAI_API_KEY` |
| `openai.NewOpenAIResponses(model)` | the Completions fields above, Responses-shaped: `MaxOutputTokens`, `MaxToolCalls`, `Store *bool` (nil = default true), `TopLogprobs`, `Metadata`, `PromptCacheKey`, `PromptCacheOptions`, `Reasoning{Effort, Summary}`, `ServiceTier`, `Truncation`; the only config whose hosted tools reach the wire (below) | `OPENAI_API_KEY` |
| `openai.NewOpenAIDecisions()` | `APIKey`, `BaseURL`, `Organization`, `Project`, `Timeout`, `MaxRetries` (4), `SafetyIdentifier`, `BooleanThreshold` (0.5), `Descriptions`, `QuestionInstructions`, `ExtraBody`; model defaults to `openai.DefaultDecisionsModel` | `OPENAI_API_KEY` |
| `anthropic.NewAnthropic(model)` | `APIKey`, `BaseURL`, `MaxTokens` (default 4096), `StopSequences`, `Temperature/TopP`, `Streaming`, `Timeout`, `MaxRetries` (2), `PromptCaching` (true) | `ANTHROPIC_API_KEY` |
| `gemini.NewGemini(apiKey, model, opts...)` | `WithBaseURL`, `WithTemperature`, `WithTopP`, `WithTopK`, `WithMaxOutputTokens`, `WithStopSequences`, `WithStreaming` | `GEMINI_API_KEY` |
| `ollama.NewOllama(model, opts...)` | `WithHost` (default `http://127.0.0.1:11434`), `WithAPIKey`, `WithTemperature`, `WithTopP`, `WithMaxTokens`, `WithStop`, `WithSeed`; `WithStreaming(true)` refuses to create (non-streaming port) | `OLLAMA_API_KEY` (optional) |
| `zai.NewZAI(model)` | `APIKey`, `BaseURL`, `MaxRetries` (3), `Temperature/TopP/MaxTokens`, `Streaming` | `ZAI_API_KEY` |
| `mistral.NewMistral(model)` | `APIKey`, `BaseURL`, `Temperature/TopP/MaxTokens`, `SafePrompt`, `Streaming` | `MISTRAL_API_KEY` |
| `dashscope.NewDashScope(model)` | `APIKey`, `BaseURL`, `Temperature/TopP/MaxTokens`, `Streaming` | `DASHSCOPE_API_KEY` |
| `xai.NewXAI`, `bedrock.NewBedrock`, `typesafe.NewTypeSafe(model)` | config structs exist; `Create()` reports "has no client yet" (wraps `config.ErrNotImplemented`) | — |

```go
cfg := openai.NewOpenAICompletions("gpt-4o-mini")
temp := 0.2
cfg.Temperature = &temp // sampling fields are *float64: nil = unset, not sent
maxTok := int64(1024)
cfg.MaxTokens = &maxTok
cfg.Streaming = true // SSE streaming: ModelMessageChunk / ModelReasoning events appear on the stream

hot, err := cfg.Clone(map[string]any{"Temperature": &temp}) // Go field name or python_case alias
```

**Structured output** is provider-transparent: with `WithResponseSchema` the OpenAI engine sends a strict `json_schema` response format; Anthropic/Gemini/Ollama embed the schema in the system prompt and validate on return. **Offline testing**: swap the config for `agotest.NewTestConfig(...)` (see [Testing](#testing)) — nothing else changes.

### The OpenAI Responses API (`OpenAIResponsesConfig`)

`openai.NewOpenAIResponses(model)` builds a `config.ModelConfig` for the Responses API —
`POST {base}/v1/responses` over raw HTTP/SSE (D-060), same `Create()`/`Clone` contract as
every other config, plus `CreateFilesClient()`:

```go
rcfg := openai.NewOpenAIResponses("gpt-5.2")
rcfg.Streaming = true // SSE: chunks/reasoning events appear on the stream
effort := "medium"
rcfg.Reasoning = &openai.Reasoning{Effort: effort}
store := false
rcfg.Store = &store // nil would keep the API default (true); nil-clears work in Clone too

a := agent.New("researcher", "...", agent.WithConfig(rcfg),
	agent.WithTools(
		tools.NewWebSearch(tools.WithWebSearchMaxUses(5)), // hosted: runs at OpenAI
		tools.NewFileSearch(tools.WithVectorStoreIDs([]string{"vs_123"})),
		myLocalTool)) // ordinary function tools ride along as function items
```

Hosted tools configured in [Hosted (builtin) tools](#hosted-builtin-tools) reach the
wire here (web_search/file_search/code_interpreter/image_generation/shell/mcp/
tool_search); hosted output items map back to `BuiltinToolCallEvent`/
`BuiltinToolResultEvent` on the stream, so observers and usage accounting see them like
local tool activity. `events.FileIdInput` inputs map to `input_file.file_id` — the one
input path that needs `CreateFilesClient()` on the same config.

### The OpenAI Decisions API (`OpenAIDecisionsConfig`)

`openai.NewOpenAIDecisions()` builds the Decisions client: give the agent a
decision-shaped response schema and the turn's answer is what the API committed to — a
bool probability against `BooleanThreshold`, an enum choice, a 0..1 score — rendered as
JSON the schema validates. The schema arrives through `WithResponseSchema` (or
`CallOptions.ResponseSchema` headless); the question text needs instructions — the
agent prompt, `QuestionInstructions`, or the schema's description. A schema that is not
one of the four decision shapes (bool, string enum, int enum from 0, 0..1 number) is
refused before the request; an API refusal comes back as
`*openai.DecisionRefusedError` (match with `errors.As`):

```go
dcfg := openai.NewOpenAIDecisions()
judge := agent.New("gatekeeper", "Decide whether the tests may run in prod.",
	agent.WithConfig(dcfg),
	agent.WithResponseSchema(response.NewSchema[bool]("ship_it", "Ship the release today?")))
rep, err := judge.Ask(ctx, events.TextInput{Content: "All 214 tests green, coverage 91%."})
var verdict bool
_ = json.Unmarshal([]byte(rep.Body()), &verdict)
```

## Files API

`github.com/luckystrker/ago/files` is the provider-agnostic Files API (Python parity: `FilesAPI`).
`files.Client` is the contract every provider implements — `Upload`/`Read`/`List`/
`Delete` — and `files.NewAPI` wraps one provider's client with the argument checks
Python's wrapper performs. A provider's client is built from its model config, so the
API key resolves the same way `Create()` resolves it (config field, then the standard
environment variable):

```go
fc, err := openai.NewOpenAICompletions("gpt-4o-mini").CreateFilesClient() // OPENAI_API_KEY fallback
api := files.NewAPI(fc)

up, err := api.Upload(ctx, pdfBytes, "report.pdf", "assistants") // empty purpose → "assistants" (OpenAI)
content, err := api.Read(ctx, up.ID)      // files.FileContent: Data, Filename, Provider, Raw
stored, err := api.List(ctx)              // every file; the client walks the provider's pagination
err = api.Delete(ctx, up.ID)
```

`anthropic.NewAnthropic(model).CreateFilesClient()` returns the Anthropic variant
(`/v1/files` under the `files-api-2025-04-14` beta header; upload sends no purpose
field, list pages by `next_page`, and an unset timeout becomes 600s). Errors:
`api.Upload` rejects nil data (`files.ErrNoData`) and an empty filename
(`files.ErrFilenameRequired`); a non-2xx provider answer wraps the per-provider
`ErrFilesAPI` (`openai.ErrFilesAPI` / `anthropic.ErrFilesAPI`) — match with
`errors.Is`. Uploaded values are metadata; `Raw` carries the provider's decoded
response verbatim. Handing a file reference into a *message* is not supported —
`events.FileIdInput` still fails fast with `config.ErrUnsupportedInput`.

## Middleware

Middleware is a four-hook onion around the agent turn (`github.com/luckystrker/ago/middleware`):

```go
type Middleware interface {
	OnTurn(next TurnFunc, ev *events.ModelRequest, ctx *agocontext.Context) (*events.ModelResponse, error)
	OnLLMCall(next LLMCallFunc, evs []events.Event, ctx *agocontext.Context) (*events.ModelResponse, error)
	OnToolExecution(next ToolExecFunc, ev *events.ToolCallEvent, ctx *agocontext.Context) (events.Event, error)
	OnHumanInput(next HumanInputFunc, ev *events.HumanInputRequest, ctx *agocontext.Context) (*events.HumanMessage, error)
}
```

Embed `middleware.Base` (pass-through for every hook) and override what you need. A
`middleware.Factory func() Middleware` builds one instance per turn — factories are
shared, instances may carry per-turn state:

```go
type audit struct{ middleware.Base }

func (audit) OnToolExecution(next middleware.ToolExecFunc, ev *events.ToolCallEvent,
	cx *agocontext.Context) (events.Event, error) {
	log.Println("tool call:", ev.Name)
	return next(ev, cx)
}

a := agent.New("bot", "...", agent.WithConfig(cfg),
	agent.WithMiddleware(func() middleware.Middleware { return audit{} }))
```

Ordering: the **first** factory is the outermost layer. Tool execution runs agent
middleware outermost and the tool's own middleware (from `tools.WithMiddleware`)
innermost. Compose with `middleware.Chain(fs ...Factory)`; apply conditionally with
`middleware.If(cond events.Condition, f Factory)` — for `OnLLMCall` the condition sees
the event that triggered the call.

Builtins:

| Factory | Effect |
|---|---|
| `middleware.HistoryLimiter(n)` | last n non-transient events per model call |
| `middleware.TokenLimiter(budget)` | newest events that fit the estimated-token budget (newest always kept) |
| `middleware.Retry(attempts)` | retries a failing model call, linear backoff; never retries cancellation |
| `middleware.Logging()` | slog log lines around every hook (honors `slog.SetDefault`) |
| `middleware.Approval()` | every tool call asks the human Y/N/Always via HITL (see below) |
| `contrib/otelx.Middleware(tp)` | OpenTelemetry spans around turn / LLM call / tool execution (own module — see below) |

**Telemetry (`contrib/otelx`).** The OTEL middleware lives in its own module so the core
stays dependency-free (D-064): `go get github.com/luckystrker/ago/contrib/otelx` from a
module that already depends on AGo, then

```go
factory, shutdown, err := otelx.InstallDefault(ctx,
	otelx.WithCollector("localhost:4318")) // OTLP/HTTP; or otelx.WithStdout()
defer shutdown(ctx)

a := agent.New("bot", "...", agent.WithConfig(cfg), agent.WithMiddleware(factory))
```

Spans: `ago.turn` → `ago.llm_call` (model, provider, usage tokens incl. cache/thinking,
finish reasons) and `ago.tool_execution` (tool name, call id); errors and
`ToolErrorEvent` answers set the span status. Have your own `TracerProvider`? Hand it to
`otelx.Middleware(tp)` instead of `InstallDefault`.

## Human-in-the-loop

Install a hook; every `HumanInputRequest` (a question, or a tool approval from
`middleware.Approval`) is routed to it, and only the answer lands on the transcript:

```go
hook := func(ctx context.Context, req *events.HumanInputRequest) (*events.HumanMessage, error) {
	fmt.Println("question:", req.Content)
	answer := readLine() // e.g. "y" for approvals
	return &events.HumanMessage{ParentID: req.ID, Content: answer}, nil
}

a := agent.New("operator", "You manage releases.", agent.WithConfig(cfg),
	agent.WithTools(deploy),
	agent.WithMiddleware(middleware.Approval()),
	agent.WithHitlHook(hook))
```

Semantics (`github.com/luckystrker/ago/hitl`, `context/context.go`):

- The question is delivered to the hook **without being transcribed** (an interrupt subscriber swallows it). An external participant may answer instead of the hook: send a `HumanMessage` with `ParentID` = the request's id and `Stream.Get` picks it up.
- `req.Timeout` (seconds) covers asking and waiting; expiry is `*hitl.TimeoutError`.
- Failures are normalized: no hook → `*hitl.NotProvidedError` (wraps `hitl.ErrNotProvided`); hook failure → `*hitl.FailedError`; deadline → `*hitl.TimeoutError`. A tool calling `cx.Input(ctx, "...", d)` without a hook fails the same way — the error travels up, unanswered tool calls in the history are repaired with the sentinel result (`events.SentinelToolResult`: "This tool call was not completed: the turn ended because a human-input request could not be answered."), and the next model call stays valid.
- `middleware.Approval()` asks on every call. Answers `y`/`yes`/`approve` pass; `always` grants the tool's source for the rest of the turn; anything else returns a `ToolErrorEvent("denied by human")` the model can see and react to.
- Unlike Python, a running Go hook cannot be cancelled — at the deadline it is abandoned and a late answer is discarded.

## Observers, watches, alerts and halts

There is no `observers=[...]` agent option: an observer registers itself on the
conversation stream, so **own the stream, register the watchdog, point the agent at
the same stream**:

```go
s := stream.NewMemoryStream()
cx := agocontext.New(s)
monitor := observers.NewTokenMonitor(100_000) // int64 token budget over UsageEvents
detector := observers.NewLoopDetector(3)      // 3 identical responses → alert
for _, o := range []observers.Observer{monitor, detector} {
	// each Observer is Name() + Register(*agocontext.Context) error
	_ = o.Register(cx)
}
writer := agent.New("writer", "...", agent.WithConfig(cfg), agent.WithStreamBase(s))
```

- `observers.Observer` is `Name() string` + `Register(*agocontext.Context) error`;
  `observers.NewStreamObserver(name, cond, func(ev events.Event))` adapts any callback.
- Both builtins emit `events.ObserverAlert{Source, Severity, Message, Data}`;
  `TokenMonitor.Total()` reports the tracked spend.

A hand-written observer subscribes on the stream inside `Register` — and can escalate
to the **fatal halt** path (`examples/safety_halt`):

```go
type PathGuardian struct{}

func (g *PathGuardian) Name() string { return "path_guardian" }

func (g *PathGuardian) Register(cx *agocontext.Context) error {
	_ = cx.Stream.Subscribe(func(ev events.Event) error {
		if call, ok := events.AsValue(ev).(events.ToolCallEvent); ok &&
			call.Name == "write_file" && forbidden(call.Arguments) {
			_ = cx.Send(&events.ObserverAlert{Source: "path_guardian",
				Severity: events.SeverityFatal, Message: "writes outside /tmp are forbidden"})
		}
		return nil
	}, stream.SubscribeOptions{Condition: events.TypeOf[events.ToolCallEvent]()})
	return nil
}
```

`policies.AlertPolicy` (below) turns non-fatal alerts into prompt fragments
(deduplicated) and fatal ones into a `events.HaltEvent`; the agent's halt check
converts that into a synthetic response `"HALTED: <reason>"` instead of a model call.
For pull-style waiting use `github.com/luckystrker/ago/watch`: `watch.EventWatch(s, cond)`,
`watch.IntervalWatch(d)`, `watch.DelayWatch(d)`, composites `AllOf` / `AnyOf` /
`Sequence`, and `WatchFunc` for plain functions — each `Wait(ctx)` blocks until fired
or cancelled.

## Knowledge, policies, compaction, aggregation

**Knowledge.** `knowledge.Store` is a tiny virtual filesystem
(Read/Write/List/Delete/Exists/Append/ReadRange/OnChange). Backends:
`knowledge.NewMemoryStore()`, `knowledge.NewDiskStore(root)`, wrapper
`knowledge.NewLockedStore(inner)`. `knowledge.NewEventLogWriter(store)` appends a JSONL
log of any stream's events (`AppendEvent(streamID, ev)`, `Replay(streamID)`,
`Streams()`) — the durability primitive behind network WALs and resume.

**Knowledge on an agent.** `agent.WithKnowledgeConfig(knowledge.NewConfig(store))`
attaches a store: it registers it in the conversation dependencies under
`knowledge.DepKey`, injects the `knowledge` tool the model can call (`read`,
`write`, `list`, `delete` on store paths), rewrites `logs/<streamID>.jsonl` at the
end of every turn (`WriteEventLog`; a failure emits a transient `EventLogFailed`
and the reply still lands), and bootstraps the layout once per store. The
bootstrap (`knowledge.DefaultBootstrap`) writes a root `SKILL.md` describing the
`/logs/`, `/artifacts/` and `/memory/` directories; `knowledge.BootstrapOnce`
guards it with the `/.initialized` sentinel (`knowledge.SentinelPath`), so a
shared store is initialized by whichever agent reaches it first and never
re-initialized. Build the config directly
(`knowledge.Config{Store: s, ExposeTool: false, ...}`) to opt out of a piece —
`NewConfig` applies the Python defaults (tool + event log on; the zero `Config`
disables both, and a nil `Store` makes the whole option a no-op).

**Optional stores (contrib).** The SQLite and Redis backends live in their own modules
so the core keeps zero dependencies (D-048/D-059/D-069). From a module that already
depends on AGo: `go get github.com/luckystrker/ago/contrib/knowledgesqlite` (or
`knowledgeredis`, `streamredis`), then

```go
kstore, err := knowledgesqlite.NewStore("agent.db") // pure-Go SQLite, no cgo
// or: kstore := knowledgeredis.NewStore(rdb, knowledgeredis.WithPrefix("myapp"))
// or, for conversation transcripts: sstore := streamredis.NewStorage(rdb)
s := stream.NewMemoryStream(stream.WithStorage(sstore)) // streamredis speaks stream.Storage
mem := agent.New("memo", "...", agent.WithConfig(cfg), agent.WithStreamBase(s),
	agent.WithMiddleware(/* policies over kstore, e.g. WorkingMemoryPolicy */))
```

`knowledgesqlite.NewStore(dbPath)` and `knowledgeredis.NewStore(rdb, opts...)` are
drop-in `knowledge.Store` implementations (atomic `Append` — cross-goroutine, and for
SQLite cross-process too); `streamredis.NewStorage(rdb, opts...)` is a `stream.Storage`
(one Redis list per stream). OnChange notifies only mutations through the same store
instance — cross-process change detection is a poll loop (`knowledge.NewPollingWatcher`
below, or your own).

**Watching a store for changes.** `knowledge.NewPollingWatcher` polls a store through the
`knowledge.VersionSource` capability — `VersionsUnder(prefix) (map[string]string, error)`
returning `{path: version}`; `MemoryStore` implements it (a store-wide monotonic counter):

```go
w := knowledge.NewPollingWatcher(kstore, "/memory/", func(path string) {
    fmt.Println("changed:", path)
}, 500*time.Millisecond) // 0 → default 500ms; shorter clamps to a 50ms floor
if err := w.Start(ctx); err != nil {
    return err // takes the initial snapshot: pre-Start writes never fire
}
defer w.Close()
```

New, rewritten and removed paths all fire once per tick; a failed read skips the tick, a
panicking callback is dropped, and `Close` (or cancelling `Start`'s context) stops it.

**Policies.** A `policies.AssemblyPolicy` transforms `(prompts, events)` before each
model call; policies compose left to right — filters and injections first, reductions
last. Builtins: `NewConversationPolicy()` (keep only conversational events),
`NewSlidingWindowPolicy(n)`, `NewTokenBudgetPolicy(budget)`,
`NewWorkingMemoryPolicy(store)` (injects `/memory/working.md` — the
`policies.WorkingMemoryPath` constant), `NewEpisodicMemoryPolicy(store, ...)`,
`NewAlertPolicy()`. The package ships no middleware of its own; run the chain in
`OnLLMCall` (the assembler pattern, complete version in `examples/policies_compact`):

```go
type assembler struct {
	middleware.Base
	policies []policies.AssemblyPolicy
}

func (a assembler) OnLLMCall(next middleware.LLMCallFunc, evs []events.Event,
	cx *agocontext.Context) (*events.ModelResponse, error) {
	prompts := cx.PromptLines()
	for _, p := range a.policies {
		var err error
		prompts, evs, err = p.Apply(prompts, evs, cx) // each Apply sees the previous output
		if err != nil {
			return nil, err
		}
	}
	return next(evs, cx)
}
```

**Compaction.** `compact` **rewrites history**: when its trigger fires, the old span is
replaced by one `CompactionSummary` plus the recent tail (`compact.KeepRecent = 2`
conversational events). It is a normal middleware factory:

```go
summarizer := anthropic.NewAnthropic("claude-sonnet-4-5") // needs ANTHROPIC_API_KEY
cl, err := summarizer.Create()

longChat := agent.New("chat", "...", agent.WithConfig(cfg),
	agent.WithMiddleware(
		compact.New(
			compact.EventCountTrigger(40),   // or compact.TokenTrigger(budget)
			compact.LLMStrategy{Client: cl}, // one summarization call when it fires
			compact.WithAgent("chat"))))
```

Lifecycle events `CompactionStarted/Completed/Failed` are transient — subscribe on the
stream to observe them.

**Aggregation.** `aggregate` folds existing summaries **and** superseded events into
one summary: triggers `SummaryCountTrigger(n)` / `UsageTrigger(total)`, strategy
`&aggregate.LLMStrategy{Client: cl}`, wired the same way —
`aggregate.New(trigger, strategy)` as a middleware factory. Its spend appears as
`UsageEvent{Kind: "aggregation"}`.

## MCP (Model Context Protocol)

`github.com/luckystrker/ago/mcp` is a minimal MCP client: JSON-RPC 2.0 over **streamable HTTP**
(`mcp.MCPServerConfig`) or a **stdio subprocess** (`mcp.MCPStdioConfig`):

```go
cl, err := mcp.New(&mcp.MCPServerConfig{
	URL:            "https://mcp.example.com/mcp",
	Headers:        map[string]string{"X-Team": "core"},
	ServerLabel:    "main",
	ToolNamePrefix: "main-", // local names only; the server never sees the prefix
})
// or: mcp.New(&mcp.MCPStdioConfig{Command: "npx", Args: []string{"-y", "some-mcp-server"}})
// Env inherits the parent process unless set; AllowedTools/BlockedTools filter discovery.
defer cl.Close()
info, err := cl.Initialize() // handshake; the session opens lazily otherwise
fmt.Printf("connected: %s v%s, protocol %s\n", info.Name, info.Version, info.ProtocolVersion)
remote, err := mcp.Tools(cl) // every server tool wrapped as an ordinary tools.Tool

a := agent.New("client", "...", agent.WithConfig(cfg), agent.WithTools(remote...))
```

Remote tools are `DeclaredInCode() == false`, so a code-declared tool keeps name
precedence. MCP content maps back to inputs: text → `TextInput`, image/audio →
`BinaryInput`, resource links → `UrlInput`; `isError` results become tool errors.

**MCP UI.** `github.com/luckystrker/ago/mcpui` builds the data side of MCP Apps — no connection of its own: a
UI resource is a block a tool result hands to an MCP-UI-capable host (which renders
HTML, an iframe URL or a Remote-DOM script from it), and a UI action is the JSON an
in-frame button posts back to the host:

```go
res, err := mcpui.RawHTML("ui://shop/item", "<b>Item A</b>",
	mcpui.WithUIMetadata(map[string]any{"target": "panel"})) // uri must start with ui://

result := events.NewToolResult(false, "Pick an item.")
result = mcpui.WithUIResource(result, res) // UI blocks ride in the tool result's parts

// The postMessage payload an in-frame button emits (html-attribute-safe):
action := mcpui.ToolCall("buy_item", map[string]any{"sku": "A-1"})
html := `<button onclick="` + mcpui.PostMessage(action) + `">Buy</button>`
```

Builders: `RawHTML` (inline HTML), `ExternalURL` (rendered in an iframe),
`RemoteDOM` (script; framework via `mcpui.WithFramework` — `mcpui.FrameworkReact`
(default) or `mcpui.FrameworkWebComponents`), each with
`mcpui.WithEncoding(mcpui.EncodingBlob)` for base64 transport. Actions:
`mcpui.ToolCall`, `Prompt`, `Link`, `Intent`, `Notify`. Resource URIs must start
`ui://`; `mcpui.Marshal(res)` renders the byte-exact wire JSON (plain `json.Marshal`
is shape-equivalent but HTML-escapes).

### Serving an agent over MCP (`mcpserver`)

`github.com/luckystrker/ago/mcpserver` is the inverse of the client: it exposes an
agent as an MCP server — the conversation is one `ask` tool the remote MCP client
calls (D-058). The server is stateless; every `tools/call` is an independent exchange.

```go
srv, err := mcpserver.NewServerForAgent(a) // one blocking agent turn per ask on a fresh stream
// or a plain bridge: mcpserver.NewServer(mcpserver.ServerConfig{
// 	Ask: func(ctx context.Context, userText string) (string, error) { ... },
// 	Tools: []tools.Tool{calc}, // extra deterministic tools served next to ask
// })

err = srv.Stdio()                    // serve on stdin/stdout until EOF...
http.Handle("/mcp", srv.Handler())   // ...or streamable HTTP (POST + SSE stream)
```

A client connecting with `mcp.New(&mcp.MCPStdioConfig{Command: "your-agent"})` (as in
`examples/mcp_stdio`) now talks to your agent: `tools/list` reports `ask` plus the
extra tools, `tools/call` runs one exchange, text/image/file results map back through
the ordinary MCP content blocks. Deferred: sessions, paused runs, OAuth, MCP Apps.

## Protocol endpoints: A2A, ACP, AG-UI, A2UI

Four packages expose an agent to the outside world over standard protocols, stdlib only.
Each is a functional subset of its Python module — the deferred pieces are listed in
[docs/PORT_STATUS.md](PORT_STATUS.md) and decided in [docs/DECISIONS.md](DECISIONS.md)
(D-049/D-050 + D-067 for A2A, D-052 + D-066 for ACP, D-053 + D-065 for AG-UI, D-076 for
A2UI).

### A2A (`github.com/luckystrker/ago/a2a`) — agent-to-agent over HTTP/JSON-RPC

`a2a.NewServer` turns a handler function into an A2A server: agent-card discovery plus
the JSON-RPC methods `message/send`, `message/stream` (SSE), `tasks/get` and
`tasks/cancel`. The handler is one synchronous `Message → Message` call; the server
records every exchange as a `Task` in its store, which is what makes `tasks/get` and
`tasks/cancel` meaningful:

```go
srv := a2a.NewServer(a2a.ServerConfig{
	Card: a2a.AgentCard{
		Name:               "ago-assistant",
		Version:            "0.1.0",
		Capabilities:       a2a.AgentCapabilities{Streaming: true}, // gates message/stream
		DefaultInputModes:  []string{"text/plain"},
		DefaultOutputModes: []string{"text/plain"},
	},
	Handle: func(ctx context.Context, msg *a2a.Message) (*a2a.Message, error) {
		reply, err := assistant.Ask(ctx, a2aText(msg)) // concat the *a2a.TextPart parts
		if err != nil {
			return nil, err // → JSON-RPC internal error + a failed Task in the store
		}
		return a2a.NewMessage(a2a.RoleAgent, a2a.NewTextPart(reply.Body())), nil
	},
	Store: a2a.NewMemoryTaskStore(), // the default; implement a2a.TaskStore to persist
})
http.ListenAndServe(":8000", srv.Handler()) // also serves /.well-known/agent-card.json
```

The served card always carries `protocolVersion` and `supportedInterfaces` — the
discovery fields 1.x clients resolve a transport through. When the config leaves
them unset, the server fills in one JSONRPC interface over the card URL at the
current protocol version (`a2a.ProtocolVersionCurrent`, `"1.0"`); interfaces the
config declares are served verbatim.

The client is transport-level — Python's conversational `A2AClient` (an `LLMClient`
shipping full history and driving streams to a terminal task) is deferred (D-049):

```go
cl := a2a.NewClient("http://localhost:8000")
card, _ := cl.AgentCard(ctx)                                        // discovery
reply, _ := cl.SendMessage(ctx, a2a.NewMessage(a2a.RoleUser, a2a.NewTextPart("Hi!")))
task, _ := cl.TasksGet(ctx, reply.TaskID)                           // correlate by task id
// also: cl.TasksCancel(ctx, taskID); WithHTTPClient/WithHeaders/WithCardPath options
```

Message parts are discriminated on the wire by `kind`: `a2a.NewTextPart(text)`,
`a2a.NewDataPart(any)`, `a2a.NewFilePart(a2a.FileBlock{URI: …})` (or `Bytes` for inline
base64). Remote failures come back as sentinel errors — match with `errors.Is` against
`a2a.ErrTaskNotFound` (-32001), `a2a.ErrTaskNotCancelable` (-32002), `a2a.ErrWire`,
`a2a.ErrTransport`, `a2a.ErrRPC`.

**Push notifications.** Configure a store and the server grows the whole push surface —
the card flips `capabilities.pushNotifications`, the four JSON-RPC methods
`tasks/pushNotificationConfig/set|get|list|delete` answer, and every terminal task fires
the registered webhooks asynchronously (D-067):

```go
srv := a2a.NewServer(a2a.ServerConfig{
	// Card, Handle, Store as above…
	PushStore: a2a.NewMemoryPushConfigStore(), // implement a2a.PushConfigStore to persist
})
mux := http.NewServeMux()
mux.Handle("/a2a", srv.Handler())
mux.Handle("/webhook", a2a.NewPushReceiver("s3cret", func(ctx context.Context, t *a2a.Task) error {
	fmt.Println("task", t.ID, "finished on the far side")
	return nil
}))
```

The client registers one config per task (`cl.SetPushConfig(ctx, taskID, a2a.PushConfig{URL: …, Token: …})`,
plus `Get/List/DeletePushConfig`); `DefaultPushSender` POSTs the task as
`application/a2a+json` with the token on `X-A2A-Token`, which is the header
`PushReceiver` checks constant-time.

**Client-side tools and HITL over the wire** — the `urn:ag2:client-tools:v1` extension.
A client that executes tools itself advertises their schemas on the message; the agent
answers `input-required` with tool-call parts; the client runs them locally and resumes
the same task with the results (D-067):

```go
schemas := []a2a.ToolSchema{ /* from tools.NewClientTool(...).Schemas() */ }
final, err := cl.RunClientTools(ctx,
	a2a.AttachClientTools(a2a.NewMessage(a2a.RoleUser, a2a.NewTextPart("Zip the reports")),
		schemas),
	schemas,
	func(ctx context.Context, calls []a2a.ToolCall) ([]a2a.ToolResultEntry, error) {
		// execute locally, one ToolResultEntry per call (Content + Error string)
		return results, nil
	})
```

`RunWithHumanInput` is the same loop for a parked HITL question: its callback answers
the agent's question and the text continuation rides the same task id. The server side
opts in with `ServerConfig.HandleClientTools` + `HandleToolResults`; context-variable
sync rides `Message.metadata` under `ag2.context_update` when `ServerConfig.SyncVariables`
is set (`a2a.ExtractContextUpdate`/`WithContextUpdate`/`StripReservedVariables` are
usable directly). Non-RPC callers can use the REST twin on the same handler:
`POST /message:send`, `POST /tasks`, `GET /tasks/{id}?historyLength=N`,
`POST /tasks/{id}:cancel`. The gRPC transport does not exist and never will (D-071) —
JSON-RPC and REST carry the full served surface.

### ACP (`github.com/luckystrker/ago/acp`) — Agent Client Protocol over stdio

Both protocol roles live in one package, over newline-delimited JSON-RPC (the same
transport template as the MCP stdio client). **Serve** an agent so any ACP client can
drive it — blocks on stdin/stdout until the client disconnects:

```go
a := agent.New("ago-assistant", "...", agent.WithConfig(cfg))
err := acp.ServeAgent(a, acp.ServeConfig{AgentName: "ago-assistant", AgentVersion: "0.1.0"})
```

A custom handler streams incremental text through `emit` (each `emit.Message` call pushes
one `agent_message_chunk` update) and returns the final body — it is delivered once more
only when it differs from what already streamed. The handler owns the turn's `ctx`: it is
cancelled when the client sends `session/cancel` mid-turn, and a cancelled turn answers
`{"stopReason":"cancelled"}`, never a protocol error (D-066). The `Emitter` carries the
rest of the update vocabulary — `Thought`, `Plan`, `ToolCall`/`ToolCallUpdate`, `Usage`:

```go
var sess *acp.Session
err := acp.Serve(acp.ServeConfig{
	OnSession: func(s *acp.Session) { sess = s }, // capture the session handle
	Handle: func(ctx context.Context, sessionID, userText string, emit *acp.Emitter) (string, error) {
		emit.Message("working on it…")
		emit.ToolCall("t1", "write_file", "write", "pending", `{"path":"a.txt"}`)
		dec, err := sess.RequestPermission(ctx, "write_file", `{"path":"a.txt"}`)
		if err != nil {
			return "", err
		}
		if !dec.Allowed() {
			return "", errors.New("write rejected")
		}
		ans, ok, _ := sess.Elicit(ctx, "Proceed?") // ok=false → declined
		_ = ans
		emit.ToolCallUpdate("t1", "write_file", "completed", "")
		return "done", nil
	},
})
```

`ServeAgent` (the served-agent variant) projects a real conversation: reasoning →
`agent_thought_chunk` (only with `StreamThoughts: true`), the per-call tool events →
`tool_call`/`tool_call_update`, and each turn's `UsageEvent` → `usage_update{used, size}`
when `ContextWindow` is set. **`session/load`** reattaches to a session this connection
issued and replays its history before answering (`ServeAgent` keeps per-session
conversations; a plain `Serve` needs `ServeConfig.History` for it). **Auth**: set
`ServeConfig.Auth` (e.g. `acp.NewStaticTokenAuth("token")`) — every session method then
requires a successful `authenticate` first, and `initialize` advertises the methods.
**Mediated fs/terminal** (client role): give the `Bridge` an `FS: &acp.FSConfig{Root: "C:/work"}`
field and it answers the agent's `fs/read_text_file`/`fs/write_text_file` and
`terminal/*` requests confined under that root (symlinks resolved, escapes refused;
terminal output capped with its head kept and a truncated flag). `Bridge.FS == nil`
(the default) disables it: every mediated request is answered `-32601` and
`initialize` advertises no fs capability (D-066). `Bridge.OnUpdate` sees every
`session/update` the agent streams, which is how a `Load` replay reaches your UI.

The `Permission`/`Elicit` policies on `ServeConfig` decide what ever reaches the client:
a nil policy (or a false answer) denies **locally** — the reject decision is synthesized,
no wire traffic, nothing fabricated; a true answer relays the client's choice.

**Bridge** is the other role — drive an external ACP agent (a CLI coding agent in ACP
mode) as a subprocess:

```go
br, err := acp.NewBridge("some-acp-agent", []string{"--acp"}, nil) // nil env → os.Environ()
defer br.Close() // idempotent; closes stdin, kills after a grace period
br.Permission = func(toolCall, input string) bool { return toolCall == "read_file" }
br.Elicit = func(message string) (string, bool) { return "yes", true }
sid, err := br.SessionNew("C:/work")
answer, err := br.Prompt(sid, "Which tests fail?") // the agent_message_chunk text of the turn
```

Operations serialize under the bridge's lock — at most one exchange in flight. Errors are
sentinels: `acp.ErrNotProvided` (empty command), `acp.ErrProtocol` (wire failure, peer
error, peer exited), `acp.ErrClosed` (operations after `Close`).

### AG-UI (`github.com/luckystrker/ago/agui`) — serve over the AG-UI event stream

`agui.ServeAgent` serves an agent over HTTP speaking AG-UI 1.0: GET answers the
capabilities JSON, POST runs one turn and streams it as SSE `data:` events:

```go
mux := http.NewServeMux()
mux.Handle("/agent", agui.ServeAgent(a)) // the agent must be usable with a fresh stream per turn
http.ListenAndServe(":8080", mux)
```

Each run is projected from the conversation stream's events: chunks →
`TEXT_MESSAGE_START/CONTENT/END`, reasoning → `REASONING_*` spans, tool calls →
`TOOL_CALL_START/ARGS/END` + `TOOL_CALL_RESULT`, task lifecycle → `SUBAGENT_*`, token
usage onto `RUN_FINISHED`. The pieces are usable directly — `agui.NewStream(emit)` builds
the projection (`Handle` one ago event at a time, `TakeUsage`/`SuccessOutcome` read the
run's ledger), `agui.ReadRunInput(body)` parses a request, `agui.CapabilitiesFor(a)`
reads the capability snapshot.

Custom handlers drive the run themselves through `agui.Handler` and may emit protocol
events directly — including the extension points (`STATE_SNAPSHOT`, `CUSTOM`, `Raw` for
anything the package does not model):

```go
h := agui.Handler(agui.HandlerConfig{
	Capabilities: func() agui.AgentCapabilities { return agui.CapabilitiesFor(a) },
	Run: func(ctx context.Context, in agui.RunAgentInput, emit func(agui.Event)) error {
		emit(agui.NewRunStartedEvent(in.ThreadID, in.RunID))
		emit(agui.NewCustomEvent("progress", map[string]any{"step": 1}))
		emit(agui.NewRunFinishedEvent(in.ThreadID, in.RunID))
		return nil // the outcome travels on the stream; errors are logged, not written
	},
})
```

An agent that asks a human a question does not fail the run anymore — the held-turn
machinery of Python's `interrupts.py` is ported (D-065). `agui.NewServer` owns the turns:
a `HumanInputRequest` (a question or a tool approval) parks the run — the exchange ends
with RUN_FINISHED carrying an `interrupt` outcome (`InterruptOutcome{Interrupts []Interrupt}`),
and the answer rides the next POST of the same thread as a resume entry
(`ResumeEntry{InterruptID, Status: "resolved"|"cancelled", Payload}`):

```go
srv, err := agui.NewServer(
	agui.WithRetention(agui.Retention{TTL: 15 * time.Minute, MaxHeld: 64}), // defaults 900s/128
	// agui.WithResumeProof(),   // refuse resumes without the issued proof token
	// agui.WithStateSnapshots(), // STATE_SNAPSHOT of the conversation variables per run
)
mux := http.NewServeMux()
mux.Handle("/agent", srv.ServeAgent("assistant", "You help humans.", agent.WithConfig(cfg)))
defer srv.Close() // cancels every held or running turn
```

Resolved interrupts rebind to the new exchange (RUN_STARTED → the held events → the
turn continues → RUN_FINISHED success); cancelled ones abandon the run and close its
open entities. Bad resumes (unknown interrupt id, missing record, wrong proof, malformed
payload) are a single coded RUN_ERROR **before** RUN_STARTED, and the turn stays
answerable. Without the server, plain `agui.ServeAgent(a)`/`Handler` keep the one-exchange
behavior: a question surfaces as a `RUN_ERROR` like any turn failure.

### A2UI (`github.com/luckystrker/ago/a2ui`) — model-generated UI over HTTP

A2UI (protocol **v1.0 only**, D-076) is the protocol where the model renders declarative
UI — surfaces of components wrapped in `<a2ui-json>` tags — and the client renderer
answers with clicks and function calls. The package pieces:

- **Wire types** — `a2ui.Message` (the server→client union: `CreateSurface`,
  `UpdateComponents`, `UpdateDataModel`, `DeleteSurface`, `CallRendererFunction`,
  `AgentFunctionResponse`; exactly one payload per envelope), the flat `a2ui.Component`
  with `NewText`/`NewImage`, and `a2ui.ToJSONL(messages)` as the canonical wire form.
- **Parser + schema** — `a2ui.NewSchemaManager()` vendors the v1.0 basic catalog;
  `a2ui.NewParser(mgr).Parse(response)` splits a model reply into prose and A2UI
  operations, `Validate` checks them against the catalog (a hand-rolled JSON-Schema
  subset) and `FormatValidationError` renders the errors for model self-correction;
  `PromptSection(actions, opts)` builds the how-to-render-A2UI prompt section.
- **Actions** — named server-side buttons: the model describes them, the server runs the
  handler on click with no model round-trip.
- **Server** — one `http.Handler` for the whole exchange (SSE frames by default, NDJSON
  via `a2ui.WithEncoding(a2ui.EncodingJSONL)`).

```go
mgr, err := a2ui.NewSchemaManager()
parser := a2ui.NewParser(mgr) // Parse(reply) → prose + <a2ui-json> operations

actions := a2ui.NewActionRegistry()
actions.Register(a2ui.Action{
    Name:           "confirm_order",
    Description:    "Confirm the current order.", // described to the model in the prompt
    ExampleContext: map[string]any{"order_id": "A-114"},
    Fn: func(ctx context.Context, params map[string]any) (any, error) {
        return a2ui.NewText("status", "Order A-114 confirmed"), nil // a click is answered server-side
    },
})

srv := a2ui.NewServer(
    a2ui.WithActions(actions),
    a2ui.WithPromptHandler(func(ctx context.Context, prompt string) (a2ui.Turn, error) {
        reply, err := assistant.Ask(ctx, prompt) // your agent runs the turn
        if err != nil {
            return a2ui.Turn{}, err
        }
        res := parser.Parse(reply.Body()) // res.Text is the prose; res.Operations the UI payloads
        return a2ui.Turn{Text: res.Text}, nil
    }),
)
mux.Handle("/a2ui", srv) // POST only; SSE frames by default
```

The server is stateless — the client sends its full state on every POST. Clicks on
registered actions and every renderer `callAgentFunction` are answered on the spot
(`UNKNOWN_FUNCTION` / `INVALID_FUNCTION_CALL` / `FUNCTION_FAILED`, correlated by
`functionCallId`); unregistered clicks, client error reports, function responses and
trailing user text are rewritten into one prompt for the `PromptHandler`, whose
`Turn{Text, Messages}` streams back as `text`/`data` frames.

## Multi-agent network

`github.com/luckystrker/ago/network` hosts many participants on one in-process hub. A channel is a typed
conversation (`consulting`, `conversation`, `discussion`, `workflow` adapters ship
builtin routing); a `TransitionGraph` in the manifest steers a workflow channel.
Participants are callbacks or bridged agents — the caller bridges agent turns; the
network never imports `agent`:

```go
hub := network.NewHub()
hub.RegisterAdapter(network.WorkflowAdapter{}, "")
client := network.NewHubClient(hub)

// A real agent behind a channel participant:
reviewer := agent.New("spec-reviewer", "You review specs.", agent.WithConfig(cfg))
reviewerC, err := client.Register("reviewer",
	network.WithAgentFunc(func(msg string) (string, error) {
		rep, err := reviewer.Ask(ctx, msg)
		if err != nil {
			return "", err
		}
		return rep.Body(), nil
	}))

// A scripted callback participant:
writer, err := client.Register("writer", network.WithEnvironment(
	func(env network.Envelope) (string, error) { return "REQ-1: ...", nil }))

graph := network.TransitionGraph{
	Initial:     writer.AgentID(),
	Transitions: []network.Transition{{When: network.Always{}, To: &network.RoundRobinTarget{}}},
	MaxTurns:    4,
}
ch, err := writer.CreateChannel(network.ChannelManifest{
	Type: network.TypeWorkflow, Knobs: map[string]any{"graph": graph.ToDict()},
}, reviewerC.AgentID())

answer, err := writer.Ask(ch.ID, "Kick off the review")
fmt.Println("writer got:", answer)
```

The round-robin continues until the graph terminates (a `TerminateTarget` transition,
or `MaxTurns`), then the hub closes the channel. Everything accepted lands in the
channel's WAL: `hub.ReadWAL(ch.ID)` returns the envelopes; `hub.Channel(ch.ID)` the
handle (`.State`, `.CloseReason`). Other pieces: the `Envelope` wire vocabulary
(`ag2.msg.text`, `ag2.packet`, …), passports, routing rules (`network.WithAgentRule`),
handoff/finish markers, and `HumanClient` for operator seats (`client.RegisterHuman`,
inbox via `Recv`). The full runnable story is `examples/group`.

**Views, arbiter, expectations, task mirror, client tools** (D-056):

- *Views* project a channel's WAL into what a participant's model should see — the
  caller folds them in a participant handler: `network.FullTranscript{}`,
  `network.NewWindowedSummary(20)`, `network.NamedTranscript{}`,
  `network.NewNamedWindowedSummary(20)`. Pass `ViewOptions{RenderEnvelope: …, NameFor: hub.NameFor}`
  to keep the adapter's envelope rendering and passport names:

  ```go
  view, err := network.NewWindowedSummary(20)
  evs := view.Project(hub.ReadWAL(ch.ID), writerC.AgentID(),
      network.ViewOptions{NameFor: hub.NameFor})
  // evs: []events.Event — ModelMessage/ModelRequest lines, a CompactionSummary head
  ```

- *Arbiter*: `network.NewHub(network.WithHubArbiter(myArbiter))` swaps the default
  `RuleBasedArbiter` (which enforces the per-agent `Rule`) for your own — embed
  `network.BaseArbiter` and override the gates you care about (`AuthorizeSend`,
  `AuthorizeDispatch`, `AuthorizeChannelOpen`, `ResolveUnknownAudience` for federated
  rerouting). A `Deny(reason)` on send fails the send; on dispatch it silently skips
  that one recipient.

- *Expectations*: declare them in `ChannelManifest.Expectations`
  (`acks_within`/`reply_within`/`max_silence` evaluators, `notify_channel`/`auto_close`
  handlers) and run `hub.SweepExpectations()` on a ticker — or
  `network.NewExpectationSweeper(hub, interval)` — plus `network.WithAuditSink`
  (`network.NewFileAuditLog("audit.jsonl")`) for the violation trail. Nobody auto-starts
  the loop; hub owners opt in.

- *Task mirror*: `network.NewTaskMirror(hub, ownerID, channelID)` subscribes an agent's
  stream and forwards the `Task*` events into the hub task store
  (`hub.ObserveTask`/`UpdateTask`/`Tasks`).

- *Client tools*: `tools, err := network.ClientTools(hc, writerC)` builds
  `peers`/`channels`/`tasks`/`context`/`delegate` as ordinary `[]tools.Tool` — hand them
  to `agent.WithTools` and a bridged agent can find peers, open channels, report task
  progress and delegate work over a consulting channel by itself.

## Testing

`agotest "github.com/luckystrker/ago/testing"` (package `agotesting`) scripts the model — no API keys, no
network, everything offline:

```go
cfg := agotest.NewTestConfig(
	&events.ModelResponse{ // call 1: request a tool
		ToolCalls: events.ToolCallsEvent{Calls: []events.ToolCallEvent{
			{ID: "call-1", Name: "get_weather", Arguments: `{"city":"Lisbon"}`}}},
		Usage: events.Usage{PromptTokens: events.I64(30), CompletionTokens: events.I64(8), TotalTokens: events.I64(38)},
	},
	&events.ModelResponse{Message: &events.ModelMessage{Content: "It is sunny, +24 C."}}, // call 2
)
```

- Responses are handed out in order, one per model call; running out returns an error
  wrapping `agotest.ErrScriptedExhausted`.
- `cfg.Calls()` returns what history every call actually saw (deep copies) — assert on
  the wire view, not the raw stream.
- `agotest.NewTrackingConfig(realCfg)` records calls through a real provider the same
  way. Provider packages are tested with `httptest` fake servers; your own tests can
  follow the same pattern.

## Evaluation

`github.com/luckystrker/ago/eval` grades agent runs, produce-then-grade (Python parity: `ag2.eval`; the trace is
built from the event log — no OpenTelemetry anywhere, D-051). `eval.Run` asks the agent
per case, rebuilds a `Trace` from the reply's stream history, and scores it:

```go
suite := eval.Suite{
	{Name: "capital", Input: "Capital of France?", Expected: "Paris",
		Scorers: []eval.Scorer{
			eval.Correctness{}, // zero Mode = casefold equality (the Python default)
			eval.TokenBudget{Max: 300},
		}},
	{Name: "uses-tool", Input: "Weather in Lisbon?",
		Scorers: []eval.Scorer{
			eval.ToolsCalled{Names: []string{"get_weather"}}, // Mode: CallAll (default) or CallAny
			eval.Threshold{Inner: eval.Correctness{Mode: eval.MatchContains}, Min: 1.0},
		}},
}
res, err := eval.Run(ctx, a, suite) // 4 cases in flight by default (WithConcurrency(1) = strict order);
                                    // a failing ask never aborts the run
for _, cr := range res.Cases {
	fmt.Println(cr.Name, cr.Passed, cr.Output, cr.Err)
}
fmt.Println("pass rate:", res.PassRate)
_ = res.SaveJSON("results/baseline.json")
```

Scorer conventions: boolean scorers carry 1.0/0.0 values (so `Threshold` and run math
treat them uniformly); a scorer error never aborts a run — `ScoreWith` records it as a
failing score (Python: `Feedback(score=None)`).

LLM-graded scorers take any `config.LLMClient`; the judge's reply must be a JSON verdict
`{"score": …, "reason": …}`, clamped to [0,1], passing at ≥ 0.5 (use `Threshold` for a
different gate). A non-JSON verdict is a recorded fail, not a re-ask:

```go
cl, err := anthropic.NewAnthropic("claude-sonnet-4-5").Create() // ANTHROPIC_API_KEY
judge := eval.LLMJudge{Client: cl, Rubric: "Factually correct and concise."}
pair := eval.Pairwise{Client: cl, Rubric: "Which answer serves the user better?"}
outcome, err := pair.Compare(ctx, traceA, traceB) // Winner "a"/"b"/"tie"; a verdict that
// flips between the two response orders resolves to a tie (positional-bias guard)
```

Traces stand alone too — build one from any history and read it:

```go
evs, _ := reply.History().GetEvents()
tr := eval.BuildTrace(evs)
fmt.Println(tr.FinalResponse, tr.Tokens().TotalTokens, len(tr.ToolCalls))
```

`eval.Compare(a, b)` tallies two `RunResult`s paired by case name — `WinRate` is B's rate
and ties count 0.5. `eval.LoadJSON` round-trips the stored events through the `@type`
registry, so token math and event-based scorers keep working on a loaded run.

**Datasets, registries, diffs, stats, concurrency** (D-061):

```go
// Suite from a JSONL dataset: one case per line — input required, expected/scorers
// optional; scorers are registry names or {"name": ..., "params": {...}}.
suite, err := eval.SuiteFromJSONL("tasks.jsonl", reg)

// The scorer registry builds scorers from names + params (the @scorer stand-in);
// SetClient feeds the LLM-backed built-ins (llm_judge, failure attribution).
reg := eval.NewScorerRegistry()
reg.SetClient(cl)
sc, err := reg.Build("threshold", map[string]any{"min": 0.8,
	"inner": map[string]any{"name": "correctness", "params": map[string]any{"mode": "contains"}}},
	eval.CaseScope{Expected: "Paris"})
userScorer := func(params map[string]any, scope eval.CaseScope) (eval.Scorer, error) { /* ... */ }
_ = reg.Register("my_scorer", userScorer)

// Failure attribution: mechanical detection over the trace, plus a one-call
// LLM attributor for the semantic modes.
attr := eval.FailureAttribution{Client: cl}
verdict, err := attr.Score(ctx, trace)
mechanical := eval.DetectMechanical(trace) // ask error / terminal tool error / no answer

// Parallel cases (default 4 in flight); 1 = strict suite order.
res, err = eval.Run(ctx, a, suite, eval.WithConcurrency(8))
```

Compare a run against its baseline and gate on regressions — cases pair by name, and
`Summary` renders the printable form:

```go
diff := eval.Diff(current, baseline)            // DiffStrict errors on mismatched suites
if len(diff.Regressions) > 0 { /* fail the gate */ }
fmt.Println(diff.Summary())                     // or iterate diff.Cases / Improvements
lo, hi := baseline.PassRateCI()                 // Wilson CI, z = 1.96
agr := eval.CohenKappa(pairs)                   // raw agreement + chance-corrected kappa
```

## Terminal chat and voice (`tui`, `live`)

**`tui`** — the text UI of `ag2/textual.py` without a TUI framework (D-056): a plain
line REPL. The first input runs `Agent.Ask`; every later line continues the same
conversation through `AgentReply.Ask`, so history accumulates across the session:

```go
err := tui.REPL(a, os.Stdin, os.Stdout, tui.WithStreaming()) // /help, /exit; WithStreaming echoes chunks
```

**`live`** — the HTTP cascade of `ag2/live/cascade.py` (D-057/D-072): one spoken turn is
transcribe → answer → synthesize. The stages are interfaces you fill — the OpenAI ones
ship ready — and the LLM leg is a plain function, so an agent reply body works as-is:

```go
casc := live.Cascade{
	STT: &live.OpenAITranscriber{APIKey: key},            // whisper-1, /audio/transcriptions
	LLM: func(ctx context.Context, text string) (string, error) {
		rep, err := assistant.Ask(ctx, text)
		if err != nil {
			return "", err
		}
		return rep.Body(), nil
	},
	TTS: &live.OpenAISynthesizer{APIKey: key, Format: "mp3"}, // tts-1, /audio/speech
}
replyText, audioOut, mime, err := casc.Handle(ctx, clip, "audio/wav", "alloy")
```

A whitespace-only transcript ends the turn silently. Provider errors wrap `live.ErrAPI`;
an incomplete cascade fails with `live.ErrIncomplete`. The realtime-websocket stack
(turn detection, barge-in, streaming TTS) and OS sound devices are out of scope (D-072) —
bring your own audio source through the `Transcriber`/`Synthesizer` interfaces.

## Examples index

All examples run offline on scripted configs: `go run ./examples/<name>`.

| Example | Demonstrates |
|---|---|
| `ask` | Smallest agent: two turns on one stream, transcript, usage report |
| `tools` | Function tools with and without arguments; scripted tool-call rounds |
| `multi_turn` | Five turns chained via `reply.Ask`; earlier constraints ride the transcript |
| `structured_output` | Typed `response.Schema[T]` + `agent.ContentAs` retry on a broken first answer |
| `hitl` | Tool approval through `middleware.Approval` and a stub HITL hook |
| `subagent` | Delegation: a specialist exposed to a lead via `AsTool` |
| `observers` | `TokenMonitor`, `LoopDetector` and a hand-written alert console |
| `safety_halt` | Fatal-alert → `HaltEvent` → "HALTED" response; `AlertPolicy` assembly |
| `knowledge_memory` | `DiskStore` + `WorkingMemoryPolicy`: memory that survives new agent instances |
| `policies_compact` | Conversation/Window/Budget assembly on top of `compact` middleware |
| `mcp_stdio` | MCP over stdio: a subprocess server's `calc_add` as an ordinary agent tool |
| `mcp_http` | MCP over streamable HTTP against a local server on 127.0.0.1 |
| `group` | Network hub: two participants take turns in a workflow channel under a `TransitionGraph` |

## Troubleshooting

**`agent: no model config provided (pass WithConfig)`** (`agent.ErrConfigNotProvided`) — `Ask`/`Run` was called on an agent without `agent.WithConfig`; the error surfaces when the turn scope opens, before any model call. A config whose model or key is missing fails later, in `cfg.Create()`, with an error wrapping `config.ErrNotProvided` — check the field **and** the environment variable.

**The model calls a tool nothing answers** — find `ToolNotFoundEvent` in the transcript (error text `` Tool `X` not found ``, also carried in the batch's `ToolResultsEvent`). Causes: the tool was not passed via `WithTools`; the name differs from the schema's `Name()`; or the turn's subscriptions were torn down early (only possible if you call `run.Close()` before `Result`). Names must be unique — a toolkit refuses duplicates with `tools.ErrConflict`, and code-declared tools win over same-named runtime (MCP/client) tools.

**`hitl: no human input hook provided`** (`*hitl.NotProvidedError`, wraps `hitl.ErrNotProvided`) — a question or approval was raised but no `WithHitlHook` is installed: `middleware.Approval()` on any tool, or a tool calling `cx.Input`/`cx.Ask`. Either install a hook, answer from outside the agent (send a `HumanMessage` with `ParentID` = the request id), or drop the middleware. The turn ends with this error; the transcript is repaired so the next ask is valid.

**`agotesting: scripted responses exhausted`** (`agotest.ErrScriptedExhausted`) — the model made more calls than the script queued. Each tool round consumes one response; queue one `ModelResponse` per round (see `examples/tools`).

**`Tool 'X' timed out after N seconds`** — the tool's `WithTimeout` expired. The function kept running in the background; its later result is discarded. Raise the timeout or do the slow work behind your own cancellation.

**`denied by human`** — an `Approval` answer was not `y`/`yes`/`approve`/`always`; the refusal is a normal `ToolErrorEvent` the model can respond to. "always" grants only the same tool source for the current turn.

**Prompt fragments duplicated on continuation?** They cannot be: the system prompt is seeded only when the context carries none, and continuations clone the previous turn's context. If you need per-turn fragments, use `agent.WithPromptHook` (runs per fresh turn, not on clones).
