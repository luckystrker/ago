package tools

import (
	"encoding/json"
	"errors"
	"testing"

	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/stream"
)

// Compile-time interface conformance for every hosted carrier.
var _ Tool = (*WebSearchTool)(nil)
var _ Tool = (*WebFetchTool)(nil)
var _ Tool = (*CodeExecutionTool)(nil)
var _ Tool = (*FileSearchTool)(nil)
var _ Tool = (*ImageGenerationTool)(nil)
var _ Tool = (*ShellTool)(nil)
var _ Tool = (*MemoryTool)(nil)
var _ Tool = (*MCPServerTool)(nil)
var _ Tool = (*ToolSearchTool)(nil)

func newHostedTools() []Tool {
	mcp, _ := NewMCPServer("https://mcp.example", "docs")
	search, _ := NewToolSearch("regex", NewMemory())
	return []Tool{
		NewWebSearch(),
		NewWebFetch(),
		NewCodeExecution(),
		NewFileSearch(),
		NewImageGeneration(),
		NewShell(),
		NewMemory(),
		mcp,
		search,
	}
}

func TestHostedToolNames(t *testing.T) {
	want := []struct {
		name       string
		schemaType string
		declared   bool
	}{
		{WebSearchToolName, "web_search", true},
		{WebFetchToolName, "web_fetch", true},
		{CodeExecutionToolName, "code_execution", true},
		{FileSearchToolName, "file_search", true},
		{ImageGenerationToolName, "image_generation", true},
		{ShellToolName, "shell", true},
		{MemoryToolName, "memory", true},
		{MCPServerToolName, "mcp_server", true},
		{ToolSearchToolName, "tool_search", true},
	}
	for i, tt := range want {
		tool := newHostedTools()[i]
		if tool.Name() != tt.name {
			t.Errorf("tool %d: Name() = %q, want %q", i, tool.Name(), tt.name)
		}
		if tool.DeclaredInCode() != tt.declared {
			t.Errorf("tool %s: DeclaredInCode() = false", tt.name)
		}
		ss, err := tool.Schemas(nil)
		if err != nil {
			t.Fatalf("tool %s: Schemas: %v", tt.name, err)
		}
		if len(ss) < 1 {
			t.Fatalf("tool %s: Schemas returned no entries", tt.name)
		}
		ts, ok := ss[0].(ToolSchema)
		if !ok {
			t.Fatalf("tool %s: schema %T does not implement ToolSchema", tt.name, ss[0])
		}
		if ts.SchemaType() != tt.schemaType {
			t.Errorf("tool %s: SchemaType() = %q, want %q", tt.name, ts.SchemaType(), tt.schemaType)
		}
	}
}

func TestHostedRegisterIsNoop(t *testing.T) {
	for _, tool := range newHostedTools() {
		if tool.Name() == ToolSearchToolName {
			continue // tool_search registers its members, covered separately
		}
		ids, err := tool.Register(nil, nil)
		if err != nil {
			t.Fatalf("%s: Register: %v", tool.Name(), err)
		}
		if len(ids) != 0 {
			t.Fatalf("%s: Register returned %d subscriptions, want none", tool.Name(), len(ids))
		}
	}
}

// TestHostedSchemaGoldenJSON pins the provider-neutral schema shapes against the
// Python dataclass fields (snake_case keys, optionals omitted when unset).
func TestHostedSchemaGoldenJSON(t *testing.T) {
	sf, region, country, tz := "San Francisco", "CA", "US", "America/Los_Angeles"
	cases := []struct {
		name string
		tool Tool
		want string
	}{
		{
			name: "web_search defaults",
			tool: NewWebSearch(),
			want: `{"web_search_version":"web_search_20250305"}`,
		},
		{
			name: "web_search full",
			tool: NewWebSearch(
				WithSearchContextSize("low"),
				WithWebSearchMaxUses(3),
				WithUserLocation(UserLocation{City: &sf, Region: &region, Country: &country, Timezone: &tz}),
				WithWebSearchAllowedDomains([]string{"a.example"}),
				WithWebSearchBlockedDomains([]string{"b.example"}),
				WithWebSearchVersion("web_search_20260318"),
			),
			want: `{"search_context_size":"low","max_uses":3,` +
				`"user_location":{"city":"San Francisco","region":"CA","country":"US","timezone":"America/Los_Angeles"},` +
				`"allowed_domains":["a.example"],"blocked_domains":["b.example"],` +
				`"web_search_version":"web_search_20260318"}`,
		},
		{
			name: "web_fetch with url_sources",
			tool: NewWebFetch(
				WithWebFetchMaxUses(2),
				WithWebFetchCitations(true),
				WithWebFetchMaxContentTokens(1024),
				WithWebFetchUseCache(true),
				WithWebFetchResponseInclusion("full"),
				WithWebFetchURLSources(UrlSources{
					UserInput:         "none",
					ClientToolResults: ptr(OnlySources([]string{"get_weather"})),
					ServerToolResults: ptr(AllSources()),
				}),
			),
			want: `{"max_uses":2,"citations":true,"max_content_tokens":1024,"use_cache":true,` +
				`"response_inclusion":"full",` +
				`"url_sources":{"user_input":"none",` +
				`"client_tool_results":{"type":"only","tools":["get_weather"]},` +
				`"server_tool_results":{"type":"all"}},` +
				`"web_fetch_version":"web_fetch_20250910"}`,
		},
		{
			name: "code_execution default",
			tool: NewCodeExecution(),
			want: `{"version":"code_execution_20250825"}`,
		},
		{
			name: "file_search gemini-flavoured",
			tool: NewFileSearch(
				WithFileSearchStoreNames([]string{"fileSearchStores/k"}),
				WithMaxNumResults(5),
				WithMetadataFilter(`lang = "go"`),
			),
			want: `{"vector_store_ids":[],"store_names":["fileSearchStores/k"],` +
				`"max_num_results":5,"metadata_filter":"lang = \"go\""}`,
		},
		{
			name: "file_search openai-flavoured",
			tool: NewFileSearch(
				WithVectorStoreIDs([]string{"vs_1"}),
				WithFileSearchFilters(map[string]any{"type": "eq", "key": "lang", "value": "go"}),
				WithIncludeResults(true),
			),
			want: `{"vector_store_ids":["vs_1"],"store_names":[],` +
				`"filters":{"key":"lang","type":"eq","value":"go"},"include_results":true}`,
		},
		{
			name: "image_generation",
			tool: NewImageGeneration(
				WithImageQuality("high"),
				WithImageSize("1024x1024"),
				WithImageOutputFormat("webp"),
				WithImageOutputCompression(80),
				WithPartialImages(2),
			),
			want: `{"quality":"high","size":"1024x1024","output_format":"webp",` +
				`"output_compression":80,"partial_images":2}`,
		},
		{
			name: "shell auto container",
			tool: NewShell(WithShellEnvironment(ShellEnvironment{
				Auto: &ContainerAuto{NetworkPolicy: &NetworkPolicy{AllowedDomains: []string{"pypi.example"}}},
			})),
			want: `{"version":"bash_20250124","environment":{"auto":{"network_policy":{"allowed_domains":["pypi.example"]}}}}`,
		},
		{
			name: "shell container reference",
			tool: NewShell(WithShellEnvironment(ShellEnvironment{
				Reference: &ContainerReference{ContainerID: "c_1"},
			})),
			want: `{"version":"bash_20250124","environment":{"reference":{"container_id":"c_1"}}}`,
		},
		{
			name: "memory default",
			tool: NewMemory(),
			want: `{"version":"memory_20250818"}`,
		},
		{
			name: "mcp_server full",
			tool: mustMCPServer(t, "https://mcp.example", "docs",
				WithAuthorizationToken("tok"),
				WithMCPServerDescription("docs server"),
				WithAllowedTools([]string{"search", "fetch"}),
				WithBlockedTools([]string{"admin"}),
				WithMCPServerHeaders(map[string]string{"X-Custom": "1"}),
			),
			want: `{"server_url":"https://mcp.example","server_label":"docs","authorization_token":"tok",` +
				`"description":"docs server","allowed_tools":["search","fetch"],"blocked_tools":["admin"],` +
				`"headers":{"X-Custom":"1"}}`,
		},
		{
			name: "tool_search bm25",
			tool: mustToolSearch(t, "bm25", NewMemory()),
			want: `{"mode":"bm25"}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ss, err := tc.tool.Schemas(nil)
			if err != nil {
				t.Fatalf("Schemas: %v", err)
			}
			got, err := json.Marshal(ss[0])
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if string(got) != tc.want {
				t.Fatalf("schema JSON:\n got %s\nwant %s", got, tc.want)
			}
		})
	}
}

// TestHostedSchemaSnapshotIsolation checks Schemas returns a snapshot: mutating
// the produced schema cannot reach the tool's stored options.
func TestHostedSchemaSnapshotIsolation(t *testing.T) {
	tool := NewWebSearch(WithWebSearchAllowedDomains([]string{"a.example"}))
	ss, err := tool.Schemas(nil)
	if err != nil {
		t.Fatalf("Schemas: %v", err)
	}
	schema := ss[0].(WebSearchToolSchema)
	schema.AllowedDomains[0] = "mutated"
	fresh, err := tool.Schemas(nil)
	if err != nil {
		t.Fatalf("Schemas: %v", err)
	}
	if got := fresh[0].(WebSearchToolSchema).AllowedDomains[0]; got != "a.example" {
		t.Fatalf("tool option mutated through the schema snapshot: %q", got)
	}
}

func TestNewToolSearchValidation(t *testing.T) {
	if _, err := NewToolSearch("regex"); err == nil {
		t.Fatal("NewToolSearch with no members must fail")
	}
	if _, err := NewToolSearch("regex", nil); err == nil {
		t.Fatal("NewToolSearch with a nil member must fail")
	}
	a, errA := NewFunctionTool("a", "", func() string { return "a" })
	b, errB := NewFunctionTool("a", "", func() string { return "b" })
	if errA != nil || errB != nil {
		t.Fatalf("NewFunctionTool: %v %v", errA, errB)
	}
	_, err := NewToolSearch("regex", a, b)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate member: %v, want ErrConflict", err)
	}
}

// TestToolSearchSchemasDeferMembers checks the schema set: the tool_search
// schema first, member function schemas deferred (copies — the member's own
// schema stays eager), hosted member schemas unchanged.
func TestToolSearchSchemasDeferMembers(t *testing.T) {
	weather, err := NewFunctionTool("get_weather", "weather", func() string { return "sunny" })
	if err != nil {
		t.Fatalf("NewFunctionTool: %v", err)
	}
	search, err := NewToolSearch("regex", weather, NewWebSearch())
	if err != nil {
		t.Fatalf("NewToolSearch: %v", err)
	}
	ss, err := search.Schemas(nil)
	if err != nil {
		t.Fatalf("Schemas: %v", err)
	}
	if len(ss) != 3 {
		t.Fatalf("Schemas returned %d entries, want 3: %T", len(ss), ss)
	}
	if ts, ok := ss[0].(ToolSearchToolSchema); !ok || ts.Mode != "regex" {
		t.Fatalf("ss[0] = %#v, want ToolSearchToolSchema{regex}", ss[0])
	}
	fn, ok := ss[1].(Schema)
	if !ok || fn.Name != "get_weather" || !fn.DeferLoading {
		t.Fatalf("ss[1] = %#v, want deferred get_weather schema", ss[1])
	}
	ws, ok := ss[2].(WebSearchToolSchema)
	if !ok || ws.WebSearchVersion != DefaultWebSearchVersion {
		t.Fatalf("ss[2] = %#v, want passthrough WebSearchToolSchema", ss[2])
	}
	// The member's own schema must not have been mutated.
	own, err := weather.Schemas(nil)
	if err != nil {
		t.Fatalf("member Schemas: %v", err)
	}
	if own[0].(Schema).DeferLoading {
		t.Fatal("member's own schema was mutated to defer_loading")
	}
}

// TestToolSearchRegistersMembers checks Register creates the members'
// subscriptions on the stream (their calls are answered client-side) and
// reports them for the turn scope's teardown.
func TestToolSearchRegistersMembers(t *testing.T) {
	weather, err := NewFunctionTool("get_weather", "weather", func() string { return "sunny" })
	if err != nil {
		t.Fatalf("NewFunctionTool: %v", err)
	}
	search, err := NewToolSearch("regex", weather, NewMemory())
	if err != nil {
		t.Fatalf("NewToolSearch: %v", err)
	}
	cx := agocontext.New(stream.NewMemoryStream())
	ids, err := search.Register(cx, nil)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	// One subscription from the function-tool member; the hosted memory member
	// registers nothing.
	if len(ids) != 1 {
		t.Fatalf("Register returned %d subscriptions, want 1", len(ids))
	}
	for _, id := range ids {
		cx.Stream.Unsubscribe(id)
	}
}

func TestMCPServerConstructorValidation(t *testing.T) {
	if _, err := NewMCPServer("", "docs"); err == nil {
		t.Fatal("empty server url must fail")
	}
	if _, err := NewMCPServer("https://mcp.example", ""); err == nil {
		t.Fatal("empty server label must fail")
	}
}

func ptr[T any](v T) *T { return &v }

func mustMCPServer(t *testing.T, url, label string, opts ...MCPServerOption) *MCPServerTool {
	t.Helper()
	mcp, err := NewMCPServer(url, label, opts...)
	if err != nil {
		t.Fatalf("NewMCPServer: %v", err)
	}
	return mcp
}

func mustToolSearch(t *testing.T, mode string, ts ...Tool) *ToolSearchTool {
	t.Helper()
	s, err := NewToolSearch(mode, ts...)
	if err != nil {
		t.Fatalf("NewToolSearch: %v", err)
	}
	return s
}
