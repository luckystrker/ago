# AGENTS.md — conventions for agents working on AGo

AGo is a Go port of AG2 (Python). Read `docs/ARCHITECTURE.md` (binding contracts) and the
relevant spec in `docs/specs/` before writing code. The Python reference is in `ag2/`
(git-ignored); consult it for behavior details, but write Go, not transliterated Python.

## Hard rules

1. **Module `github.com/luckystrker/ago`**, Go 1.24. `go.mod` sits at the repository root and
   every framework package is a directory at the root (`events/`, `agent/`, ...; import paths
   are `github.com/luckystrker/ago/agent` etc.). Examples are part of the same module, in
   `examples/`; there is no `go.work`. All gates run from the repo root.
2. **Zero external dependencies.** stdlib only. Need a dep? Stop and record a DECISIONS entry.
3. **No import cycles.** Check the dependency graph in ARCHITECTURE.md before importing:
   `agent` never imports `network`; `config` never imports `agent`.
4. **Gates before you finish a task:** from the repo root: `go build ./...`, `go vet ./...`,
   `go test ./...` (or at minimum `go test ./<your-package>/...`) — all green, no skipped
   tests. For examples: `go build ./examples/...` and `go vet ./examples/...`.
5. Every deviation from Python parity: code comment `// AGO-DEVIATION: <reason>` + entry in
   `docs/DECISIONS.md`.
6. Comments explain constraints, not history. No "ported from ag2/..." chatter.

## Style

- gofmt-clean; idiomatic Go naming (`GetHistory` → `History`, `NewX` constructors, interfaces
  end with behavior not "er" unless idiomatic — `Storage`, `Stream`).
- Errors: package-level sentinels (`var ErrNotFound = errors.New(...)`), wrap with `%w`,
  `errors.Is/As` on consumption. No panics in library code.
- Concurrency: every exported type that has mutable state must be safe for concurrent use;
  document which. Use `context.Context` as first param (`ctx`) for cancellable operations.
- JSON tags match Python field names (snake_case) for wire compatibility of events.
- Tests: stdlib `testing`, table-driven where natural; test files next to the code
  (`events_test.go`); no network access in tests (fake clients, httptest).

## Mapping cheatsheet (Python → Go)

| Python | AGo |
|---|---|
| `async def f(ctx)` | `func F(ctx context.Context, ...)` |
| `BaseEvent` + `Field()` | struct embedding `events.Base` + `EventType() string` |
| condition DSL | `events.Condition` predicates (`AllOf/AnyOf/Not/TypeOf/Equal`) |
| `Optional[X]` / `X \| None` | `*X` or sentinel per field |
| dataclass kwarg config | option funcs / exported struct fields |
| `ExitStack` | turn scope in agent; cleanup via defer |
| `HumanHook` (sync/async callable) | `hitl.HumanHook func(ctx, *HumanInputRequest) (*HumanMessage, error)` |
| `to_dict()/from_dict()` | `events.MarshalEvent/UnmarshalEvent` ("@type" discriminator) |

## Where things live (quick map)

- Event vocabulary: `events/` (one file per Python module: input.go, model.go, tool.go, task.go, lifecycle.go, alert.go, base.go, condition.go, marshal.go, sizing.go)
- Conversation plumbing: `stream/`, `context/`, `hitl/`
- LLM: `config/` (core), `config/<provider>/`, `internal/httpx`, `internal/sse`, `internal/uuid`
- Tools: `tools/`
- Turn loop: `agent/`
- CLI helper: `cmd/ago/` (`ago version` / `doctor` / `demo`)
- Reference specs: `docs/specs/{core,config-llm,tools,events-network}.md`
- Status tracker: `docs/PORT_STATUS.md` — update when your package reaches green.
