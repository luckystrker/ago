package agent

import (
	stdcontext "context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/stream"
	agotest "github.com/luckystrker/ago/testing"
	"github.com/luckystrker/ago/tools"
)

// contributeTool mirrors the upstream scoped-variables test tool: it appends to
// a SHARED list under a caller-supplied mutex, then mutates COPIED values.
func contributeTool(report *[]string, reportMu *sync.Mutex) (tools.Tool, error) {
	fn := func(args struct {
		Who string `json:"who" agschema:"description=Contributor name,required"`
		// The lock arrives through dependencies, like upstream's findings_lock.
	}, cx *agocontext.Context) (string, error) {
		lockAny, _ := cx.Dependency("findings_lock")
		lock, _ := lockAny.(*sync.Mutex)
		if lock == nil {
			return "", errors.New("findings_lock missing")
		}

		lock.Lock()
		cur, _ := cx.Variable("findings")
		next := append([]string{}, cur.([]string)...)
		next = append(next, args.Who, args.Who+"-rebound")
		err := cx.SetVariable("findings", next)
		lock.Unlock()
		if err != nil {
			return "", err
		}

		depth, _ := cx.Variable("depth")
		if err := cx.SetVariable("depth", depth.(int)+1); err != nil {
			return "", err
		}
		nested, _ := cx.Variable("nested")
		nested.(map[string]any)["n"] = nested.(map[string]any)["n"].(int) + 1

		_, hasSecret := cx.Variable("secret")
		_, hasReserved := cx.Variable("ag:private")
		reportMu.Lock()
		*report = append(*report, fmt.Sprintf("%s secret=%v reserved=%v", args.Who, hasSecret, hasReserved))
		reportMu.Unlock()
		return fmt.Sprint(depth), nil
	}
	return tools.NewFunctionTool("contribute", "Contribute to shared state", fn)
}

// TestScopedParallelChildren mirrors upstream
// test_parallel_children_share_rebindings_and_isolate_copied_and_local.
func TestScopedParallelChildrenMirrorUpstream(t *testing.T) {
	var (
		report   []string
		reportMu sync.Mutex
	)
	contribute, err := contributeTool(&report, &reportMu)
	if err != nil {
		t.Fatalf("tool: %v", err)
	}
	child := func(name string) *Agent {
		return New(name, "",
			WithConfig(agotest.NewTestConfig(
				toolResp("contribute", fmt.Sprintf(`{"who":%q}`, name)),
				textResp("done"),
			)),
			WithTools(contribute),
		)
	}
	alpha, beta := child("alpha"), child("beta")

	// Both delegation calls ride one model response, so the executor runs the
	// children concurrently (upstream's two ToolCallEvents).
	rootCfg := agotest.NewTestConfig(
		&events.ModelResponse{ToolCalls: events.ToolCallsEvent{Calls: []events.ToolCallEvent{
			{ID: "call-alpha", Name: "task_alpha", Arguments: `{"objective":"contribute"}`},
			{ID: "call-beta", Name: "task_beta", Arguments: `{"objective":"contribute"}`},
		}}},
		textResp("done"),
	)
	lock := &sync.Mutex{}
	root := New("root", "", WithConfig(rootCfg),
		WithTools(
			alpha.AsTool(WithToolName("task_alpha")),
			beta.AsTool(WithToolName("task_beta")),
		),
		WithDependencies(map[string]any{"findings_lock": lock}),
		WithVariables(map[string]any{
			"findings":   agocontext.ScopedValue{Value: []string{}, Scope: agocontext.ScopeShared},
			"depth":      0,
			"nested":     map[string]any{"n": 0},
			"secret":     agocontext.ScopedValue{Value: "root only", Scope: agocontext.ScopeLocal},
			"ag:private": "control",
		}),
	)
	reply, err := root.Ask(stdcontext.Background(), "start")
	if err != nil {
		t.Fatalf("ask: %v", err)
	}

	findingsAny, ok := reply.Context().Variable("findings")
	if !ok {
		t.Fatal("root lost the shared findings binding")
	}
	findings := findingsAny.([]string)
	slices.Sort(findings)
	if want := []string{"alpha", "alpha-rebound", "beta", "beta-rebound"}; !slices.Equal(findings, want) {
		t.Fatalf("findings = %v, want %v", findings, want)
	}
	if v, _ := reply.Context().Variable("depth"); v != 0 {
		t.Fatalf("root depth = %v, want 0 (COPIED)", v)
	}
	nested, _ := reply.Context().Variable("nested")
	if got := nested.(map[string]any)["n"]; got != 0 {
		t.Fatalf("root nested.n = %v, want 0 (deep COPIED)", got)
	}
	if v, _ := reply.Context().Variable("secret"); v != "root only" {
		t.Fatalf("root secret = %v, want root only (LOCAL)", v)
	}
	if len(report) != 2 {
		t.Fatalf("report = %v", report)
	}
	reportMu.Lock()
	defer reportMu.Unlock()
	for _, line := range report {
		if !strings.HasSuffix(line, " secret=false reserved=false") {
			t.Fatalf("child saw LOCAL or reserved state: %q", line)
		}
	}
}

// TestScopedChildCannotDeclareShared pins the root-only declaration rule:
// the delegation fails with a ScopeError instead of silently sharing a new
// name below the root.
func TestScopedChildCannotDeclareShared(t *testing.T) {
	child := New("child", "",
		WithVariables(map[string]any{
			"new": agocontext.ScopedValue{Value: map[string]any{}, Scope: agocontext.ScopeShared},
		}),
	)
	parent := agocontext.New(stream.NewMemoryStream())
	res, err := runTaskOn(stdcontext.Background(), parent, child, "work", "")
	if err != nil {
		t.Fatalf("runTaskOn returned a turn-ending error: %v", err)
	}
	if res.completed {
		t.Fatal("delegation unexpectedly completed")
	}
	var se *agocontext.ScopeError
	if !errors.As(res.err, &se) {
		t.Fatalf("want *ScopeError, got %v", res.err)
	}
}

// TestScopedRootsDoNotShareDefaults: two asks on one root agent build fresh
// invocation trees, so a SHARED default does not leak across roots.
func TestScopedRootsDoNotShareDefaults(t *testing.T) {
	root := New("root", "",
		WithConfig(agotest.NewTestConfig(textResp("one"), textResp("two"))),
		WithVariables(map[string]any{
			"kv": agocontext.ScopedValue{Value: map[string]any{}, Scope: agocontext.ScopeShared},
		}),
	)
	first, err := root.Ask(stdcontext.Background(), "start")
	if err != nil {
		t.Fatalf("first ask: %v", err)
	}
	kvAny, _ := first.Context().Variable("kv")
	kvAny.(map[string]any)["first"] = 1

	second, err := root.Ask(stdcontext.Background(), "start")
	if err != nil {
		t.Fatalf("second ask: %v", err)
	}
	kv2, _ := second.Context().Variable("kv")
	if len(kv2.(map[string]any)) != 0 {
		t.Fatalf("second root sees the first root's shared state: %v", kv2)
	}
}
