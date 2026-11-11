package tools

import (
	"encoding/json"
	"errors"
	"strings"
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
var _ Tool = (*RetrievalTool)(nil)

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
		NewRetrieval("kb-1"),
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
		{RetrievalToolName, "retrieval", true},
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
			// Secrets are masked in the serialized form (MarshalJSON); the
			// provider wire request still carries the real values.
			want: `{"server_url":"https://mcp.example","server_label":"docs","authorization_token":"***",` +
				`"description":"docs server","allowed_tools":["search","fetch"],"blocked_tools":["admin"],` +
				`"headers":{"X-Custom":"***"}}`,
		},
		{
			name: "tool_search bm25",
			tool: mustToolSearch(t, "bm25", NewMemory()),
			want: `{"mode":"bm25"}`,
		},
		{
			name: "retrieval id only",
			tool: NewRetrieval("kb-1"),
			want: `{"knowledge_id":"kb-1"}`,
		},
		{
			name: "retrieval with template",
			tool: NewRetrieval("kb-1", WithRetrievalPromptTemplate("Use {{knowledge}} to answer {{question}}")),
			want: `{"knowledge_id":"kb-1","prompt_template":"Use {{knowledge}} to answer {{question}}"}`,
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

// TestMCPServerSchemaMarshalMasksSecrets pins the L-14 contract: the token and
// the header values never appear in the schema's serialized form, while the
// struct the provider mappers read keeps the real values.
func TestMCPServerSchemaMarshalMasksSecrets(t *testing.T) {
	tool := mustMCPServer(t, "https://mcp.example", "docs",
		WithAuthorizationToken("tok-supersecret"),
		WithMCPServerHeaders(map[string]string{"Authorization": "Bearer tok-supersecret", "X-Custom": "custom-value"}),
	)
	ss, err := tool.Schemas(nil)
	if err != nil {
		t.Fatalf("Schemas: %v", err)
	}
	for name, schema := range map[string]any{"value": ss[0], "pointer": &ss[0]} {
		b, err := json.Marshal(schema)
		if err != nil {
			t.Fatalf("%s: marshal: %v", name, err)
		}
		got := string(b)
		for _, secret := range []string{"tok-supersecret", "Bearer", "custom-value"} {
			if strings.Contains(got, secret) {
				t.Fatalf("%s form leaks %q: %s", name, secret, got)
			}
		}
		if !strings.Contains(got, `"authorization_token":"***"`) {
			t.Fatalf("%s form: token not masked (set tokens must stay visible as masked): %s", name, got)
		}
		if !strings.Contains(got, `"Authorization":"***"`) || !strings.Contains(got, `"X-Custom":"***"`) {
			t.Fatalf("%s form: header values not masked: %s", name, got)
		}
	}

	// The wire path reads the struct fields; marshaling must not mutate them.
	sch := ss[0].(MCPServerToolSchema)
	if sch.AuthorizationToken != "tok-supersecret" {
		t.Fatalf("marshal mutated the token: %q", sch.AuthorizationToken)
	}
	if sch.Headers["X-Custom"] != "custom-value" {
		t.Fatalf("marshal mutated the headers: %v", sch.Headers)
	}
}

// TestMCPServerSchemaMarshalOmitsAbsentSecrets: no token, no headers — nothing
// secret-looking is invented in the serialized form.
func TestMCPServerSchemaMarshalOmitsAbsentSecrets(t *testing.T) {
	b, err := json.Marshal(MCPServerToolSchema{ServerURL: "https://mcp.example", ServerLabel: "docs"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := string(b)
	if strings.Contains(got, "authorization_token") || strings.Contains(got, "headers") {
		t.Fatalf("absent secrets must stay omitted: %s", got)
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

func varContext(t *testing.T, vars map[string]any) *agocontext.Context {
	t.Helper()
	cx := agocontext.New(stream.NewMemoryStream())
	for k, v := range vars {
		if err := cx.SetVariable(k, v); err != nil {
			t.Fatalf("SetVariable(%s): %v", k, err)
		}
	}
	return cx
}

func firstSchemaJSON(t *testing.T, tool Tool, cx *agocontext.Context) string {
	t.Helper()
	ss, err := tool.Schemas(cx)
	if err != nil {
		t.Fatalf("Schemas: %v", err)
	}
	b, err := json.Marshal(ss[0])
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestHostedVariableKwargsResolvePerTurn: a bound parameter follows the
// conversation variable on every Schemas call (Python: resolve_variable).
func TestHostedVariableKwargsResolvePerTurn(t *testing.T) {
	tool := NewWebSearch(
		WithWebSearchMaxUses(1), // overridden by the binding
		WithWebSearchVariable("max_uses", NewVariable("")),
		WithWebSearchVariable("allowed_domains", NewVariable("doms", []string{"default.example"})),
		WithWebSearchVariable("user_location", NewVariable("loc")),
	)
	cx := varContext(t, map[string]any{
		"max_uses": 7,
		"loc":      map[string]any{"city": "Oslo", "country": "NO"},
	})
	want := `{"max_uses":7,"user_location":{"city":"Oslo","country":"NO"},` +
		`"allowed_domains":["default.example"],"web_search_version":"web_search_20250305"}`
	if got := firstSchemaJSON(t, tool, cx); got != want {
		t.Fatalf("turn 1 schema =\n%s\nwant\n%s", got, want)
	}
	// The next turn sees the new value: nothing is cached at construction.
	if err := cx.SetVariable("max_uses", 9); err != nil {
		t.Fatal(err)
	}
	ss, _ := tool.Schemas(cx)
	if got := *ss[0].(WebSearchToolSchema).MaxUses; got != 9 {
		t.Fatalf("turn 2 max_uses = %d, want 9", got)
	}
}

func TestHostedVariableKwargsErrors(t *testing.T) {
	cx := varContext(t, map[string]any{"mu": "not-a-number"})
	if _, err := NewWebSearch(WithWebSearchVariable("max_uses", NewVariable("absent"))).Schemas(cx); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing variable without default: err = %v, want ErrNotFound", err)
	}
	if _, err := NewWebSearch(WithWebSearchVariable("max_uses", NewVariable("mu"))).Schemas(cx); err == nil ||
		!strings.Contains(err.Error(), "wrong type") {
		t.Fatalf("mistyped variable: err = %v, want a wrong-type error", err)
	}
	if _, err := NewWebSearch(WithWebSearchVariable("nope", NewVariable("x", 1))).Schemas(cx); err == nil ||
		!strings.Contains(err.Error(), "unknown parameter") {
		t.Fatalf("unknown parameter: err = %v", err)
	}
	// A nil context resolves through defaults only.
	ss, err := NewWebFetch(WithWebFetchVariable("max_uses", NewVariable("m", 3))).Schemas(nil)
	if err != nil || *ss[0].(WebFetchToolSchema).MaxUses != 3 {
		t.Fatalf("nil-context default: %v, %v", ss, err)
	}
}

// TestHostedVariableCarriersAllBind covers the remaining carriers, including
// MCP server url/label supplied by variable only.
func TestHostedVariableCarriersAllBind(t *testing.T) {
	cx := varContext(t, map[string]any{
		"url": "https://mcp.example", "label": "docs", "kb": "kb-9", "size": "512x512",
		"stores": []string{"fileSearchStores/a"}, "env": map[string]any{"reference": map[string]any{"container_id": "c1"}},
	})
	mcp, err := NewMCPServer("", "", WithMCPServerVariable("server_url", NewVariable("url")),
		WithMCPServerVariable("server_label", NewVariable("label")))
	if err != nil {
		t.Fatalf("NewMCPServer with variables: %v", err)
	}
	if _, err := NewMCPServer("", "x"); err == nil {
		t.Fatal("an empty url without a bound variable must still fail")
	}
	for _, tc := range []struct {
		tool Tool
		want string
	}{
		{mcp, `{"server_url":"https://mcp.example","server_label":"docs"}`},
		{NewRetrieval("", WithRetrievalVariable("knowledge_id", NewVariable("kb"))), `{"knowledge_id":"kb-9"}`},
		{NewImageGeneration(WithImageGenerationVariable("size", NewVariable("size"))), `{"size":"512x512"}`},
		{NewFileSearch(WithFileSearchVariable("store_names", NewVariable("stores"))),
			`{"vector_store_ids":[],"store_names":["fileSearchStores/a"]}`},
		{NewShell(WithShellVariable("environment", NewVariable("env"))),
			`{"version":"bash_20250124","environment":{"reference":{"container_id":"c1"}}}`},
	} {
		if got := firstSchemaJSON(t, tc.tool, cx); got != tc.want {
			t.Errorf("%s = %s, want %s", tc.tool.Name(), got, tc.want)
		}
	}
}

func TestGoogleMapsCarrier(t *testing.T) {
	var _ Tool = (*GoogleMapsTool)(nil)
	plain := NewGoogleMaps(WithGoogleMapsLocation(59.9, 10.7), WithGoogleMapsLanguageCode("nb"), WithGoogleMapsWidget(true))
	if got, want := firstSchemaJSON(t, plain, nil), `{"latitude":59.9,"longitude":10.7,"language_code":"nb","enable_widget":true}`; got != want {
		t.Fatalf("schema = %s, want %s", got, want)
	}
	if got := firstSchemaJSON(t, NewGoogleMaps(), nil); got != `{"enable_widget":false}` {
		t.Fatalf("bare schema = %s", got)
	}
	if plain.Name() != "google_maps" || !plain.DeclaredInCode() {
		t.Fatalf("name/declared = %q/%v", plain.Name(), plain.DeclaredInCode())
	}
	ts, _ := plain.Schemas(nil)
	if ts[0].(ToolSchema).SchemaType() != GoogleMapsToolName {
		t.Fatal("schema type")
	}
	if ids, err := plain.Register(nil, nil); err != nil || len(ids) != 0 {
		t.Fatalf("Register = %v, %v; want a no-op", ids, err)
	}
	// Per-turn latitude/longitude from the conversation (user location).
	byVar := NewGoogleMaps(
		WithGoogleMapsVariable("latitude", NewVariable("user_lat")),
		WithGoogleMapsVariable("longitude", NewVariable("user_lng")),
	)
	cx := varContext(t, map[string]any{"user_lat": 1.5, "user_lng": 2.5})
	if got, want := firstSchemaJSON(t, byVar, cx), `{"latitude":1.5,"longitude":2.5,"enable_widget":false}`; got != want {
		t.Fatalf("variable schema = %s, want %s", got, want)
	}
}
