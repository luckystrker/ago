package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/luckystrker/ago/agent"
	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
	agotest "github.com/luckystrker/ago/testing"
	"github.com/luckystrker/ago/tools"
)

// TestOneServerPerRequestVariables: one MCP server serves two requests whose
// host-supplied variables differ; each agent turn's tool resolves its own value.
// The tenant comes from the HTTP request (the host resolves it in Ask, the
// role Python's context provider plays), so nothing is shared between turns.
func TestOneServerPerRequestVariables(t *testing.T) {
	var (
		mu   sync.Mutex
		seen []string
	)
	who, err := tools.NewFunctionTool("who_am_i", "Records the tenant",
		func(a struct {
			Tenant tools.Variable `agvariable:"tenant"`
		}) (string, error) {
			mu.Lock()
			seen = append(seen, fmt.Sprint(a.Tenant.Default))
			mu.Unlock()
			return "ok", nil
		})
	if err != nil {
		t.Fatal(err)
	}
	modelCall := func() *events.ModelResponse {
		return &events.ModelResponse{ToolCalls: events.ToolCallsEvent{Calls: []events.ToolCallEvent{
			{ID: "c1", Name: "who_am_i", Arguments: "{}"},
		}}}
	}
	done := func() *events.ModelResponse {
		return &events.ModelResponse{Message: &events.ModelMessage{Content: "done"}}
	}
	a := agent.New("served", "", agent.WithConfig(agotest.NewTestConfig(modelCall(), done(), modelCall(), done())),
		agent.WithTools(who))
	s, err := NewServer(ServerConfig{
		Ask: func(ctx context.Context, text string) (string, error) {
			// The host resolves the request's tenant; each call gets its own
			// Variables map through the per-run seam.
			tenant := ""
			if r := RequestFromContext(ctx); r != nil {
				tenant = r.Header.Get("X-Tenant")
			}
			run, err := a.RunWith(ctx, agent.RunOptions{Variables: map[string]any{"tenant": tenant}}, text)
			if err != nil {
				return "", err
			}
			defer run.Close()
			if err := run.Start(ctx); err != nil {
				return "", err
			}
			reply, err := run.Result(ctx)
			if err != nil {
				return "", err
			}
			return reply.Body(), nil
		},
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	rpcOver(t, ts.URL, nil, initMsg)

	for _, tenant := range []string{"alpha", "beta"} {
		header := http.Header{"X-Tenant": []string{tenant}}
		out := rpcOver(t, ts.URL, header,
			`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"ask","arguments":{"message":"who"}}}`)
		if out["error"] != nil {
			t.Fatalf("%s: tools/call ask: %v", tenant, out["error"])
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 || seen[0] != "alpha" || seen[1] != "beta" {
		t.Fatalf("tenants seen = %v, want [alpha beta]", seen)
	}
}

// TestServerForAgentCarriesRequestContext: NewServerForAgent passes each
// request's Variables, Dependencies and RequestDependency into the agent's ask
// through agent.RunOptions, so a served agent's tool resolves the same slots a
// deterministic MCP tool does (Python parity: mcp/executor.py:344-347).
func TestServerForAgentCarriesRequestContext(t *testing.T) {
	type seenContext struct {
		tenant  string
		tool    string
		header  string
		missing bool
	}
	var (
		mu   sync.Mutex
		seen []seenContext
	)
	who, err := tools.NewFunctionTool("who_am_i", "Records the request context",
		func(a struct {
			Tenant tools.Variable `agvariable:"tenant"`
			Req    tools.Inject   `aginject:"mcp_request"`
		}) (string, error) {
			rec := seenContext{missing: true}
			if a.Tenant.Default != nil {
				rec.tenant = fmt.Sprint(a.Tenant.Default)
				rec.missing = false
			}
			if rc, ok := a.Req.Default.(*RequestContext); ok {
				rec.tool = rc.Tool
				if rc.HTTP != nil {
					rec.header = rc.HTTP.Header.Get("X-Tenant")
				}
			}
			mu.Lock()
			seen = append(seen, rec)
			mu.Unlock()
			return "recorded", nil
		})
	if err != nil {
		t.Fatal(err)
	}

	modelCall := func() *events.ModelResponse {
		return &events.ModelResponse{ToolCalls: events.ToolCallsEvent{Calls: []events.ToolCallEvent{
			{ID: "c1", Name: "who_am_i", Arguments: "{}"},
		}}}
	}
	done := func() *events.ModelResponse {
		return &events.ModelResponse{Message: &events.ModelMessage{Content: "done"}}
	}

	for _, tenant := range []string{"alpha", "beta"} {
		tenant := tenant
		t.Run(tenant, func(t *testing.T) {
			cfg := agotest.NewTestConfig(modelCall(), done())
			a := agent.New("served-"+tenant, "", agent.WithConfig(cfg), agent.WithTools(who))
			s, err := NewServerForAgent(a, WithVariables(map[string]any{"tenant": tenant}))
			if err != nil {
				t.Fatalf("NewServerForAgent: %v", err)
			}
			ts := httptest.NewServer(s.Handler())
			defer ts.Close()
			header := http.Header{"X-Tenant": []string{tenant}}
			rpcOver(t, ts.URL, header, initMsg)
			out := rpcOver(t, ts.URL, header,
				`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"ask","arguments":{"message":"who"}}}`)
			if out["error"] != nil {
				t.Fatalf("tools/call ask: %v", out["error"])
			}
			mu.Lock()
			defer mu.Unlock()
			if len(seen) != 1 {
				t.Fatalf("tool invocations = %v, want one", seen)
			}
			got := seen[0]
			if got.missing || got.tenant != tenant {
				t.Fatalf("tool saw tenant %q (missing=%v), want %q", got.tenant, got.missing, tenant)
			}
			if got.tool != "ask" || got.header != tenant {
				t.Fatalf("RequestDependency = tool %q header %q, want ask/%q", got.tool, got.header, tenant)
			}
			seen = nil
		})
	}
}

// TestContextProviderPerRequestVariables: one server, two requests with
// different X-Tenant headers and no state change in between. The context
// provider reads the header; each agent turn's tool sees its own value.
func TestContextProviderPerRequestVariables(t *testing.T) {
	var (
		mu   sync.Mutex
		seen []string
	)
	who, err := tools.NewFunctionTool("who_am_i", "Records the tenant",
		func(a struct {
			Tenant tools.Variable `agvariable:"tenant"`
			Suffix tools.Inject   `aginject:"suffix"`
		}) (string, error) {
			mu.Lock()
			seen = append(seen, fmt.Sprint(a.Tenant.Default)+fmt.Sprint(a.Suffix.Default))
			mu.Unlock()
			return "ok", nil
		})
	if err != nil {
		t.Fatal(err)
	}
	modelCall := func() *events.ModelResponse {
		return &events.ModelResponse{ToolCalls: events.ToolCallsEvent{Calls: []events.ToolCallEvent{
			{ID: "c1", Name: "who_am_i", Arguments: "{}"},
		}}}
	}
	done := func() *events.ModelResponse {
		return &events.ModelResponse{Message: &events.ModelMessage{Content: "done"}}
	}
	a := agent.New("served", "",
		agent.WithConfig(agotest.NewTestConfig(modelCall(), done(), modelCall(), done())),
		agent.WithTools(who))
	s, err := NewServerForAgent(a, WithContextProvider(
		func(_ context.Context, token string, req *RequestContext) (RequestScope, error) {
			if token != "" {
				return RequestScope{}, fmt.Errorf("unexpected token %q", token)
			}
			return RequestScope{
				Variables:    map[string]any{"tenant": req.HTTP.Header.Get("X-Tenant")},
				Dependencies: map[string]any{"suffix": "!"},
			}, nil
		}))
	if err != nil {
		t.Fatalf("NewServerForAgent: %v", err)
	}
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	rpcOver(t, ts.URL, nil, initMsg)

	for _, tenant := range []string{"alpha", "beta"} {
		out := rpcOver(t, ts.URL, http.Header{"X-Tenant": []string{tenant}},
			`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"ask","arguments":{"message":"who"}}}`)
		if out["error"] != nil {
			t.Fatalf("%s: tools/call ask: %v", tenant, out["error"])
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 || seen[0] != "alpha!" || seen[1] != "beta!" {
		t.Fatalf("tenants seen = %v, want [alpha! beta!]", seen)
	}
}

// TestContextProviderErrorRefusesCall: a provider failure is a JSON-RPC error
// and the agent is never started (its tool does not run).
func TestContextProviderErrorRefusesCall(t *testing.T) {
	var toolRan atomic.Bool
	tool, err := tools.NewFunctionTool("who_am_i", "Unused",
		func(struct{}) (string, error) {
			toolRan.Store(true)
			return "ok", nil
		})
	if err != nil {
		t.Fatal(err)
	}
	cfg := agotest.NewTestConfig(&events.ModelResponse{Message: &events.ModelMessage{Content: "unreached"}})
	a := agent.New("served", "", agent.WithConfig(cfg), agent.WithTools(tool))
	s, err := NewServerForAgent(a, WithContextProvider(
		func(context.Context, string, *RequestContext) (RequestScope, error) {
			return RequestScope{}, errors.New("tenant not resolvable")
		}))
	if err != nil {
		t.Fatalf("NewServerForAgent: %v", err)
	}
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	rpcOver(t, ts.URL, nil, initMsg)

	out := rpcOver(t, ts.URL, http.Header{"X-Tenant": []string{"alpha"}},
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"ask","arguments":{"message":"who"}}}`)
	rpcErr, ok := out["error"].(map[string]any)
	if !ok {
		t.Fatalf("tools/call result = %v, want a JSON-RPC error", out)
	}
	if code := rpcErr["code"]; code != float64(codeInternalError) {
		t.Fatalf("error code = %v, want %d", code, codeInternalError)
	}
	if msg, _ := rpcErr["message"].(string); !strings.Contains(msg, "tenant not resolvable") {
		t.Fatalf("error message = %q", msg)
	}
	if toolRan.Load() {
		t.Fatal("the agent started despite the provider error")
	}
}

// TestServerSnapshotsVariables: WithVariables snapshots the caller's map at
// construction; mutating it afterwards does not reach a request (the provider,
// not a live map, is the per-request seam).
func TestServerSnapshotsVariables(t *testing.T) {
	var (
		mu   sync.Mutex
		seen string
	)
	who, err := tools.NewFunctionTool("who_am_i", "Records the tenant",
		func(a struct {
			Tenant tools.Variable `agvariable:"tenant"`
		}) (string, error) {
			mu.Lock()
			seen = fmt.Sprint(a.Tenant.Default)
			mu.Unlock()
			return "ok", nil
		})
	if err != nil {
		t.Fatal(err)
	}
	cfg := agotest.NewTestConfig(
		&events.ModelResponse{ToolCalls: events.ToolCallsEvent{Calls: []events.ToolCallEvent{
			{ID: "c1", Name: "who_am_i", Arguments: "{}"},
		}}},
		&events.ModelResponse{Message: &events.ModelMessage{Content: "done"}},
	)
	a := agent.New("served", "", agent.WithConfig(cfg), agent.WithTools(who))
	vars := map[string]any{"tenant": "alpha"}
	s, err := NewServerForAgent(a, WithVariables(vars))
	if err != nil {
		t.Fatalf("NewServerForAgent: %v", err)
	}
	vars["tenant"] = "beta" // after construction
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	rpcOver(t, ts.URL, nil, initMsg)
	out := rpcOver(t, ts.URL, nil,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"ask","arguments":{"message":"who"}}}`)
	if out["error"] != nil {
		t.Fatalf("tools/call ask: %v", out["error"])
	}
	mu.Lock()
	defer mu.Unlock()
	if seen != "alpha" {
		t.Fatalf("tenant seen = %q, want the construction-time snapshot alpha", seen)
	}
}

// TestContextProviderReachesDeterministicToolsAndMeta: the provider runs on
// every request — tools/list and tools/call of a deterministic WithTools tool
// included — and its variables override the server's.
func TestContextProviderReachesDeterministicToolsAndMeta(t *testing.T) {
	tenantTool, err := tools.NewFunctionTool("tenant_tool", "Returns the tenant",
		func(a struct {
			Tenant tools.Variable `agvariable:"tenant"`
		}) (string, error) {
			return fmt.Sprint(a.Tenant.Default), nil
		})
	if err != nil {
		t.Fatal(err)
	}
	vars := map[string]any{"tenant": "server"}
	s, err := NewServer(ServerConfig{
		Ask:       func(context.Context, string) (string, error) { return "ok", nil },
		Tools:     []tools.Tool{tenantTool},
		Variables: vars,
		ToolMeta: map[string]ToolMeta{"tenant_tool": {
			Resolve: func(cx *agocontext.Context) ToolMeta {
				v, _ := cx.Variable("tenant")
				return ToolMeta{Title: fmt.Sprint(v)}
			},
		}},
		ContextProvider: func(_ context.Context, _ string, req *RequestContext) (RequestScope, error) {
			return RequestScope{Variables: map[string]any{
				"tenant": req.HTTP.Header.Get("X-Tenant"),
			}}, nil
		},
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	rpcOver(t, ts.URL, nil, initMsg)

	list := rpcOver(t, ts.URL, http.Header{"X-Tenant": []string{"alpha"}},
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	if title := toolByName(t, list, "tenant_tool")["title"]; title != "alpha" {
		t.Fatalf("ToolMeta.Resolve title = %v, want the provider's alpha", title)
	}
	call := rpcOver(t, ts.URL, http.Header{"X-Tenant": []string{"beta"}},
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"tenant_tool","arguments":{}}}`)
	text := call["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"]
	if text != "beta" {
		t.Fatalf("deterministic tool saw %v, want the provider's beta (provider must win over server)", text)
	}
}

// TestNewServerSnapshotsVariables: WithVariables is snapshotted in NewServer;
// mutating the caller's map afterwards changes neither a deterministic tool nor
// ToolMeta.Resolve.
func TestNewServerSnapshotsVariables(t *testing.T) {
	tenantTool, err := tools.NewFunctionTool("tenant_tool", "Returns the tenant",
		func(a struct {
			Tenant tools.Variable `agvariable:"tenant"`
		}) (string, error) {
			return fmt.Sprint(a.Tenant.Default), nil
		})
	if err != nil {
		t.Fatal(err)
	}
	vars := map[string]any{"tenant": "alpha"}
	s, err := NewServer(ServerConfig{
		Ask:       func(context.Context, string) (string, error) { return "ok", nil },
		Tools:     []tools.Tool{tenantTool},
		Variables: vars,
		ToolMeta: map[string]ToolMeta{"tenant_tool": {
			Resolve: func(cx *agocontext.Context) ToolMeta {
				v, _ := cx.Variable("tenant")
				return ToolMeta{Title: fmt.Sprint(v)}
			},
		}},
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	vars["tenant"] = "beta" // after construction
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	rpcOver(t, ts.URL, nil, initMsg)

	list := rpcOver(t, ts.URL, nil, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	if title := toolByName(t, list, "tenant_tool")["title"]; title != "alpha" {
		t.Fatalf("title = %v, want the construction-time snapshot alpha", title)
	}
	call := rpcOver(t, ts.URL, nil,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"tenant_tool","arguments":{}}}`)
	text := call["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"]
	if text != "alpha" {
		t.Fatalf("tool saw %v, want the construction-time snapshot alpha", text)
	}
}

// TestContextProviderScopeToolsAndPrompt: the provider's Tools are registered
// for the agent's turn and its Prompt replaces the agent's system prompt.
func TestContextProviderScopeToolsAndPrompt(t *testing.T) {
	var (
		probeRan   atomic.Bool
		seenPrompt string
	)
	probe, err := tools.NewFunctionTool("provider_probe", "Records the prompt",
		func(_ struct{}, cx *agocontext.Context) (string, error) {
			probeRan.Store(true)
			seenPrompt = strings.Join(cx.PromptLines(), "|")
			return "probe", nil
		})
	if err != nil {
		t.Fatal(err)
	}
	cfg := agotest.NewTestConfig(
		&events.ModelResponse{ToolCalls: events.ToolCallsEvent{Calls: []events.ToolCallEvent{
			{ID: "c1", Name: "provider_probe", Arguments: "{}"},
		}}},
		&events.ModelResponse{Message: &events.ModelMessage{Content: "done"}},
	)
	a := agent.New("served", "agent prompt", agent.WithConfig(cfg))
	s, err := NewServerForAgent(a, WithContextProvider(
		func(context.Context, string, *RequestContext) (RequestScope, error) {
			return RequestScope{
				Tools:  []tools.Tool{probe},
				Prompt: []string{"provider prompt"},
			}, nil
		}))
	if err != nil {
		t.Fatalf("NewServerForAgent: %v", err)
	}
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	rpcOver(t, ts.URL, nil, initMsg)
	out := rpcOver(t, ts.URL, nil,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"ask","arguments":{"message":"go"}}}`)
	if out["error"] != nil {
		t.Fatalf("tools/call ask: %v", out["error"])
	}
	if !probeRan.Load() {
		t.Fatal("the provider's tool was not registered: scope.Tools dropped")
	}
	if seenPrompt != "provider prompt" {
		t.Fatalf("tool saw prompt %q, want the provider prompt (agent prompt must be replaced)", seenPrompt)
	}
}

// TestContextProviderErrorOnToolsList: a provider error on tools/list is a
// JSON-RPC -32603 and no tool list is produced.
func TestContextProviderErrorOnToolsList(t *testing.T) {
	s, err := NewServer(ServerConfig{
		Ask: func(context.Context, string) (string, error) { return "ok", nil },
		ContextProvider: func(context.Context, string, *RequestContext) (RequestScope, error) {
			return RequestScope{}, errors.New("tenant lookup failed")
		},
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	rpcOver(t, ts.URL, nil, initMsg)

	out := rpcOver(t, ts.URL, nil, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	if out["result"] != nil {
		t.Fatalf("tools/list result = %v, want no tool list on a provider error", out["result"])
	}
	rpcErr, ok := out["error"].(map[string]any)
	if !ok {
		t.Fatalf("tools/list response = %v, want a JSON-RPC error", out)
	}
	if code := rpcErr["code"]; code != float64(codeInternalError) {
		t.Fatalf("error code = %v, want %d", code, codeInternalError)
	}
	if msg, _ := rpcErr["message"].(string); !strings.Contains(msg, "tenant lookup failed") {
		t.Fatalf("error message = %q", msg)
	}
}
