package mcpserver

import (
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
