package mcpserver

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/luckystrker/ago/agent"
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
