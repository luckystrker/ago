package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/luckystrker/ago/config"
	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/tools"
)

// golden renders a mapped wire entry as sorted-key JSON so a whole block can be
// compared against one string (json.Marshal orders map keys).
func golden(t *testing.T, m map[string]any) string {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal mapped tool: %v", err)
	}
	return string(b)
}

func mustSchemas(t *testing.T, tool tools.Tool) []any {
	t.Helper()
	ss, err := tool.Schemas(nil)
	if err != nil {
		t.Fatalf("Schemas: %v", err)
	}
	return ss
}

func TestConvertToolsHostedAnthropic(t *testing.T) {
	sf := "San Francisco"
	cases := []struct {
		name  string
		extra []any // tools declared before the hosted one (url_sources filters resolve against them)
		tool  any
		want  string
	}{
		{
			name: "web_search defaults",
			tool: tools.WebSearchToolSchema{},
			want: `{"name":"web_search","type":"web_search_20250305"}`,
		},
		{
			name: "web_search full",
			tool: mustSchemas(t, tools.NewWebSearch(
				tools.WithWebSearchMaxUses(3),
				tools.WithUserLocation(tools.UserLocation{City: &sf}),
				tools.WithWebSearchAllowedDomains([]string{"a.example"}),
				tools.WithWebSearchBlockedDomains([]string{"b.example"}),
				tools.WithWebSearchVersion("web_search_20260318"),
			))[0],
			// search_context_size is deliberately not sent (Python parity).
			want: `{"allowed_domains":["a.example"],"blocked_domains":["b.example"],` +
				`"max_uses":3,"name":"web_search",` +
				`"type":"web_search_20260318","user_location":{"city":"San Francisco","type":"approximate"}}`,
		},
		{
			name: "code_execution",
			tool: mustSchemas(t, tools.NewCodeExecution(tools.WithCodeExecutionVersion("code_execution_20260521")))[0],
			want: `{"name":"code_execution","type":"code_execution_20260521"}`,
		},
		{
			name:  "web_fetch full (filter tools declared as a function tool)",
			extra: []any{tools.Schema{Name: "get_weather"}},
			tool: mustSchemas(t, tools.NewWebFetch(
				tools.WithWebFetchMaxUses(2),
				tools.WithWebFetchCitations(true),
				tools.WithWebFetchUseCache(true),
				tools.WithWebFetchResponseInclusion("full"),
				tools.WithWebFetchURLSources(tools.UrlSources{
					UserInput:         "none",
					ClientToolResults: ptrTools(tools.OnlySources([]string{"get_weather"})),
					ServerToolResults: ptrTools(tools.AllSources()),
				}),
				tools.WithWebFetchVersion("web_fetch_20260318"),
			))[0],
			want: `{"citations":{"enabled":true},"max_uses":2,"name":"web_fetch","response_inclusion":"full",` +
				`"type":"web_fetch_20260318",` +
				`"url_sources":{"client_tool_results":{"tools":[{"name":"get_weather","type":"tool_reference"}],"type":"only"},` +
				`"server_tool_results":{"type":"all"},"user_input":{"type":"none"}},"use_cache":true}`,
		},
		{
			name: "web_fetch minimal (unset fields are the API default)",
			tool: mustSchemas(t, tools.NewWebFetch())[0],
			want: `{"name":"web_fetch","type":"web_fetch_20250910"}`,
		},
		{
			name: "memory",
			tool: mustSchemas(t, tools.NewMemory())[0],
			want: `{"name":"memory","type":"memory_20250818"}`,
		},
		{
			name: "mcp_server with allowed and blocked tools",
			tool: mustSchemas(t, mustMCPServerTool(t,
				tools.WithAllowedTools([]string{"search", "fetch"}),
				tools.WithBlockedTools([]string{"admin"}),
			))[0],
			want: `{"configs":{"admin":{"enabled":false},"fetch":{"enabled":true},"search":{"enabled":true}},` +
				`"default_config":{"enabled":false},"mcp_server_name":"docs","type":"mcp_toolset"}`,
		},
		{
			name: "mcp_server unrestricted",
			tool: mustSchemas(t, mustMCPServerTool(t))[0],
			want: `{"mcp_server_name":"docs","type":"mcp_toolset"}`,
		},
		{
			name: "tool_search regex",
			tool: tools.ToolSearchToolSchema{Mode: "regex"},
			want: `{"name":"tool_search_tool_regex","type":"tool_search_tool_regex_20251119"}`,
		},
		{
			name: "tool_search bm25",
			tool: tools.ToolSearchToolSchema{Mode: "bm25"},
			want: `{"name":"tool_search_tool_bm25","type":"tool_search_tool_bm25_20251119"}`,
		},
		{
			name: "pointer schema dereferences",
			tool: ptrSchema(tools.MemoryToolSchema{}),
			want: `{"name":"memory","type":"memory_20250818"}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ConvertTools(append(tc.extra, tc.tool))
			if err != nil {
				t.Fatalf("ConvertTools: %v", err)
			}
			if g := golden(t, got[len(got)-1]); g != tc.want {
				t.Fatalf("mapped tool:\n got %s\nwant %s", g, tc.want)
			}
		})
	}
}

func TestConvertToolsHostedUnsupportedAnthropic(t *testing.T) {
	for _, tc := range []struct {
		name string
		tool any
	}{
		{"file_search", tools.FileSearchToolSchema{StoreNames: []string{"s"}}},
		{"image_generation", tools.ImageGenerationToolSchema{Quality: "high"}},
		{"shell", tools.ShellToolSchema{Version: tools.ShellVersion}},
		{"shell pointer", ptrSchema(tools.ShellToolSchema{})},
		{"unknown hosted type", unknownHosted{}},
		{"nil hosted pointer", (*tools.MemoryToolSchema)(nil)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ConvertTools([]any{tc.tool})
			if !errors.Is(err, config.ErrUnsupportedTool) {
				t.Fatalf("ConvertTools error = %v, want ErrUnsupportedTool", err)
			}
		})
	}
}

func TestWebFetchVersionGateAnthropic(t *testing.T) {
	cases := []struct {
		name    string
		version string
		option  func(*tools.WebFetchToolSchema)
		wantErr bool
	}{
		{"use_cache before 20260309", "web_fetch_20250910", func(s *tools.WebFetchToolSchema) { s.UseCache = ptrTools(true) }, true},
		{"use_cache since 20260309", "web_fetch_20260309", func(s *tools.WebFetchToolSchema) { s.UseCache = ptrTools(true) }, false},
		{"response_inclusion before 20260318", "web_fetch_20260309", func(s *tools.WebFetchToolSchema) { s.ResponseInclusion = "full" }, true},
		{"response_inclusion since 20260318", "web_fetch_20260318", func(s *tools.WebFetchToolSchema) { s.ResponseInclusion = "full" }, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := tools.WebFetchToolSchema{WebFetchVersion: tc.version}
			tc.option(&s)
			_, err := ConvertTools([]any{s})
			if tc.wantErr && !errors.Is(err, config.ErrUnsupportedTool) {
				t.Fatalf("error = %v, want ErrUnsupportedTool", err)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestConvertToolsURLSourcesUnknownTool(t *testing.T) {
	fetch := mustSchemas(t, tools.NewWebFetch(
		tools.WithWebFetchURLSources(tools.UrlSources{
			ClientToolResults: ptrTools(tools.OnlySources([]string{"get_weather"})),
		}),
	))
	if _, err := ConvertTools(fetch); !errors.Is(err, config.ErrUnsupportedTool) {
		t.Fatalf("undeclared filter tool: %v, want ErrUnsupportedTool", err)
	}

	// Declared as a function tool: the filter resolves.
	weather := tools.Schema{Name: "get_weather"}
	if _, err := ConvertTools([]any{weather, fetch[0]}); err != nil {
		t.Fatalf("function-declared filter tool: %v", err)
	}

	// Declared as an mcp_toolset configs entry: the filter resolves too.
	mcp := mustSchemas(t, mustMCPServerTool(t, tools.WithAllowedTools([]string{"get_weather"})))
	if _, err := ConvertTools([]any{mcp[0], fetch[0]}); err != nil {
		t.Fatalf("mcp-declared filter tool: %v", err)
	}
}

func TestExtractMCPServers(t *testing.T) {
	schemas := []any{
		mustSchemas(t, tools.NewMemory())[0],
		mustSchemas(t, mustMCPServerTool(t, tools.WithAuthorizationToken("tok")))[0],
		ptrSchema(tools.MCPServerToolSchema{ServerURL: "https://mcp2.example", ServerLabel: "calc"}),
	}
	servers := ExtractMCPServers(schemas)
	if len(servers) != 2 {
		t.Fatalf("ExtractMCPServers returned %d servers, want 2", len(servers))
	}
	if g := golden(t, servers[0]); g != `{"authorization_token":"tok","name":"docs","type":"url","url":"https://mcp.example"}` {
		t.Fatalf("server[0] = %s", g)
	}
	if g := golden(t, servers[1]); g != `{"name":"calc","type":"url","url":"https://mcp2.example"}` {
		t.Fatalf("server[1] = %s", g)
	}
	if ExtractMCPServers([]any{tools.Schema{Name: "fn"}}) != nil {
		t.Fatal("no MCP schemas must yield no servers")
	}
}

// TestCallHostedToolsWire exercises the full call: hosted schemas land in the
// tools array as server-tool blocks, the MCP server definitions land in the
// request's mcp_servers parameter and the request carries the connector's beta
// header (Python sends mcp_servers only under anthropic-beta).
func TestCallHostedToolsWire(t *testing.T) {
	srv, reqs := captureServer(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("anthropic-beta"); got != mcpClientBeta {
			t.Errorf("anthropic-beta = %q, want %q (the request carries mcp_servers)", got, mcpClientBeta)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, textResponse)
	})
	cl := newTestClient(srv, nil)

	_, err := cl.Call(context.Background(), []events.Event{
		&events.ModelRequest{Parts: []events.Input{&events.TextInput{Content: "search"}}},
	}, nil, config.CallOptions{
		Tools: []any{
			mustSchemas(t, tools.NewWebSearch(tools.WithWebSearchMaxUses(1)))[0],
			mustSchemas(t, tools.NewMemory())[0],
			mustSchemas(t, mustMCPServerTool(t))[0],
		},
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	wireTools := (*reqs)[0]["tools"].([]any)
	if len(wireTools) != 3 {
		t.Fatalf("tools = %d entries, want 3", len(wireTools))
	}
	first := wireTools[0].(map[string]any)
	if first["type"] != "web_search_20250305" || first["name"] != "web_search" {
		t.Fatalf("web_search block = %v", first)
	}
	if _, hasSchema := first["input_schema"]; hasSchema {
		t.Fatal("a hosted tool block must not carry input_schema")
	}
	servers := (*reqs)[0]["mcp_servers"].([]any)
	if len(servers) != 1 {
		t.Fatalf("mcp_servers = %d entries, want 1", len(servers))
	}
	srv0 := servers[0].(map[string]any)
	if srv0["type"] != "url" || srv0["name"] != "docs" || srv0["url"] != "https://mcp.example" {
		t.Fatalf("mcp_servers[0] = %v", srv0)
	}
}

// TestCallNoBetaWithoutMCPServers pins the other side of the beta gate: without
// an MCP server the request goes out without anthropic-beta (Python parity: the
// header appears only when the betas set is non-empty), hosted or not.
func TestCallNoBetaWithoutMCPServers(t *testing.T) {
	srv, _ := captureServer(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("anthropic-beta"); got != "" {
			t.Errorf("anthropic-beta = %q, want none without mcp_servers", got)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, textResponse)
	})
	cl := newTestClient(srv, nil)

	_, err := cl.Call(context.Background(), []events.Event{
		&events.ModelRequest{Parts: []events.Input{&events.TextInput{Content: "hi"}}},
	}, nil, config.CallOptions{
		Tools: []any{
			tools.Schema{Name: "get_weather"},
			mustSchemas(t, tools.NewWebSearch())[0],
		},
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
}

type unknownHosted struct{}

func (unknownHosted) SchemaType() string { return "deep_research" }

func ptrTools[T any](v T) *T { return &v }

func ptrSchema[T any](s T) *T { return &s }

func mustMCPServerTool(t *testing.T, opts ...tools.MCPServerOption) *tools.MCPServerTool {
	t.Helper()
	mcp, err := tools.NewMCPServer("https://mcp.example", "docs", opts...)
	if err != nil {
		t.Fatalf("NewMCPServer: %v", err)
	}
	return mcp
}
