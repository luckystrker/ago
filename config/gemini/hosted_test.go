package gemini

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/luckystrker/ago/config"
	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/stream"
	"github.com/luckystrker/ago/tools"
)

// golden renders a mapped tools entry as sorted-key JSON so a whole block can
// be compared against one string (json.Marshal orders map keys).
func golden(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
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

// TestBuildToolsHostedGemini pins the hosted mappings (Python parity:
// build_tools in gemini/mappers.py, camelCase REST aliases of google-genai).
func TestBuildToolsHostedGemini(t *testing.T) {
	cases := []struct {
		name string
		tool any
		want string
	}{
		{"web_search defaults", mustSchemas(t, tools.NewWebSearch())[0], `{"googleSearch":{}}`},
		{
			"web_search blocked domains",
			mustSchemas(t, tools.NewWebSearch(tools.WithWebSearchBlockedDomains([]string{"b.example"})))[0],
			`{"googleSearch":{"excludeDomains":["b.example"]}}`,
		},
		// Every web_fetch field is ignored: Gemini only knows the on/off tool.
		{"web_fetch ignores fields", mustSchemas(t, tools.NewWebFetch(tools.WithWebFetchMaxUses(2)))[0], `{"urlContext":{}}`},
		{"code_execution", mustSchemas(t, tools.NewCodeExecution())[0], `{"codeExecution":{}}`},
		{
			"file_search full",
			mustSchemas(t, tools.NewFileSearch(
				tools.WithFileSearchStoreNames([]string{"fileSearchStores/k"}),
				tools.WithMaxNumResults(5),
				tools.WithMetadataFilter(`lang = "go"`),
			))[0],
			`{"fileSearch":{"fileSearchStoreNames":["fileSearchStores/k"],"metadataFilter":"lang = \"go\"","topK":5}}`,
		},
		{
			"file_search store names only",
			mustSchemas(t, tools.NewFileSearch(tools.WithFileSearchStoreNames([]string{"s"})))[0],
			`{"fileSearch":{"fileSearchStoreNames":["s"]}}`,
		},
		{"pointer schema dereferences", ptrSchema(tools.CodeExecutionToolSchema{}), `{"codeExecution":{}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := buildTools([]any{tc.tool})
			if err != nil {
				t.Fatalf("buildTools: %v", err)
			}
			if len(got) != 1 {
				t.Fatalf("buildTools returned %d tools, want 1", len(got))
			}
			if g := golden(t, got[0]); g != tc.want {
				t.Fatalf("mapped tool:\n got %s\nwant %s", g, tc.want)
			}
		})
	}
}

// TestBuildToolsHostedMixedOrder checks a mixed request: the function
// declarations share one block first, then each hosted tool appends its own
// block in schema order (Python parity).
func TestBuildToolsHostedMixedOrder(t *testing.T) {
	got, err := buildTools([]any{
		tools.Schema{Name: "get_weather"},
		mustSchemas(t, tools.NewWebSearch())[0],
		tools.Schema{Name: "get_time"},
		mustSchemas(t, tools.NewCodeExecution())[0],
	})
	if err != nil {
		t.Fatalf("buildTools: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("buildTools returned %d tools, want 3: %s", len(got), golden(t, got))
	}
	decls := got[0]["functionDeclarations"].([]map[string]any)
	if len(decls) != 2 || decls[0]["name"] != "get_weather" || decls[1]["name"] != "get_time" {
		t.Fatalf("functionDeclarations = %v", decls)
	}
	if _, ok := got[1]["googleSearch"]; !ok {
		t.Fatalf("tools[1] = %v, want the googleSearch block", got[1])
	}
	if _, ok := got[2]["codeExecution"]; !ok {
		t.Fatalf("tools[2] = %v, want the codeExecution block", got[2])
	}
}

// TestBuildToolsHostedUnsupportedGemini checks the hosted types the ported
// Gemini client cannot carry, and that an unrecognized implementation fails
// even when it claims a supported type string (Python maps by isinstance).
func TestBuildToolsHostedUnsupportedGemini(t *testing.T) {
	for _, tc := range []struct {
		name string
		tool any
	}{
		{"file_search without store names", mustSchemas(t, tools.NewFileSearch(tools.WithVectorStoreIDs([]string{"vs_1"})))[0]},
		{"file_search empty", tools.FileSearchToolSchema{}},
		{"image_generation", tools.ImageGenerationToolSchema{}},
		{"shell", tools.ShellToolSchema{}},
		{"memory", tools.MemoryToolSchema{}},
		{"mcp_server", tools.MCPServerToolSchema{ServerURL: "https://mcp.example", ServerLabel: "docs"}},
		{"tool_search", tools.ToolSearchToolSchema{Mode: "regex"}},
		{"unknown hosted type", fakeHosted{}},
		{"nil hosted pointer", (*tools.WebSearchToolSchema)(nil)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := buildTools([]any{tc.tool})
			if !errors.Is(err, config.ErrUnsupportedTool) {
				t.Fatalf("buildTools error = %v, want ErrUnsupportedTool", err)
			}
		})
	}
}

// TestGeminiHostedToolsInRequest exercises the full call: the mixed tool set
// lands in the request's tools array as [functionDeclarations, googleSearch].
func TestGeminiHostedToolsInRequest(t *testing.T) {
	srv, cap := captureServer(t, 200, `{"candidates":[]}`)
	cl := startClient(t, newTestConfig(srv.URL))
	_, err := cl.Call(context.Background(), []events.Event{
		&events.ModelRequest{Parts: []events.Input{&events.TextInput{Content: "search"}}},
	}, nil, config.CallOptions{
		Tools: []any{
			tools.Schema{Name: "get_weather"},
			mustSchemas(t, tools.NewWebSearch(tools.WithWebSearchBlockedDomains([]string{"b.example"})))[0],
		},
	})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	toolsWire := cap.JSON(t)["tools"].([]any)
	if len(toolsWire) != 2 {
		t.Fatalf("tools = %d entries, want 2", len(toolsWire))
	}
	if _, ok := toolsWire[0].(map[string]any)["functionDeclarations"]; !ok {
		t.Fatalf("tools[0] = %v, want functionDeclarations", toolsWire[0])
	}
	gs := toolsWire[1].(map[string]any)["googleSearch"].(map[string]any)
	if got := golden(t, gs); got != `{"excludeDomains":["b.example"]}` {
		t.Fatalf("googleSearch = %s", got)
	}
}

func ptrSchema[T any](s T) *T { return &s }

// TestGoogleMapsRequestBody: Maps grounding reaches the wire as a googleMaps
// tool entry plus a toolConfig.retrievalConfig geo bias (Python parity:
// build_tools / build_tool_config); a request without Maps carries no toolConfig.
func TestGoogleMapsRequestBody(t *testing.T) {
	call := func(schemas ...any) map[string]any {
		t.Helper()
		srv, cap := captureServer(t, 200, `{"candidates":[]}`)
		cl := startClient(t, newTestConfig(srv.URL))
		req := &events.ModelRequest{Parts: []events.Input{&events.TextInput{Content: "cafes nearby"}}}
		if _, err := cl.Call(context.Background(), []events.Event{req}, nil, config.CallOptions{Tools: schemas}); err != nil {
			t.Fatalf("call: %v", err)
		}
		return cap.JSON(t)
	}

	full := call(mustSchemas(t, tools.NewGoogleMaps(
		tools.WithGoogleMapsLocation(59.9, 10.7), tools.WithGoogleMapsLanguageCode("nb"), tools.WithGoogleMapsWidget(true)))[0])
	if got, want := golden(t, full["tools"]), `[{"googleMaps":{"enableWidget":true}}]`; got != want {
		t.Fatalf("tools = %s, want %s", got, want)
	}
	if got, want := golden(t, full["toolConfig"]),
		`{"retrievalConfig":{"languageCode":"nb","latLng":{"latitude":59.9,"longitude":10.7}}}`; got != want {
		t.Fatalf("toolConfig = %s, want %s", got, want)
	}

	// Maps without a location: the tool entry only, no toolConfig; the widget
	// flag is absent when false. A language code alone needs a location to travel.
	bare := call(mustSchemas(t, tools.NewGoogleMaps(tools.WithGoogleMapsLanguageCode("nb")))[0])
	if got := golden(t, bare["tools"]); got != `[{"googleMaps":{}}]` {
		t.Fatalf("bare tools = %s", got)
	}
	if _, ok := bare["toolConfig"]; ok {
		t.Fatalf("toolConfig must stay off the wire without lat/lng: %v", bare["toolConfig"])
	}

	// No Maps tool: the request body is unchanged.
	plain := call(mustSchemas(t, tools.NewWebSearch())[0])
	if _, ok := plain["toolConfig"]; ok {
		t.Fatalf("toolConfig appeared without Maps: %v", plain["toolConfig"])
	}
}

// TestBuildToolConfigPartialLocation: a location with one coordinate (reachable
// through a Variable bound to only latitude or longitude) is not sent and does
// not panic; the pointer schema form and a nil pointer behave like the value.
func TestBuildToolConfigPartialLocation(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	cx := agocontext.New(stream.NewMemoryStream())
	if err := cx.SetVariable("lat", 10.5); err != nil {
		t.Fatal(err)
	}
	byVar := mustSchemasCx(t, tools.NewGoogleMaps(
		tools.WithGoogleMapsVariable("latitude", tools.NewVariable("lat")),
		tools.WithGoogleMapsLanguageCode("nb")), cx)[0]
	if sch := byVar.(tools.GoogleMapsToolSchema); sch.Latitude == nil || sch.Longitude != nil {
		t.Fatalf("fixture: want latitude only, got %+v", sch)
	}

	for name, schemas := range map[string][]any{
		"latitude only (variable)": {byVar},
		"longitude only":           {tools.GoogleMapsToolSchema{Longitude: f(2)}},
		"latitude only pointer":    {&tools.GoogleMapsToolSchema{Latitude: f(1)}},
		"nil pointer":              {(*tools.GoogleMapsToolSchema)(nil)},
		"no schemas":               nil,
	} {
		if got := buildToolConfig(schemas); got != nil {
			t.Errorf("%s: toolConfig = %v, want none", name, got)
		}
	}
	// The tool entry itself is still produced for a partial location.
	if got, err := buildTools([]any{byVar}); err != nil || golden(t, got) != `[{"googleMaps":{}}]` {
		t.Fatalf("buildTools(partial) = %v, %v", got, err)
	}

	// Pointer form with a full location behaves like the value form; the last
	// Maps schema wins, and a nil pointer between them is skipped.
	got := buildToolConfig([]any{
		tools.GoogleMapsToolSchema{Latitude: f(1), Longitude: f(2), LanguageCode: "en"},
		(*tools.GoogleMapsToolSchema)(nil),
		&tools.GoogleMapsToolSchema{Latitude: f(3), Longitude: f(4)},
	})
	if want := `{"retrievalConfig":{"latLng":{"latitude":3,"longitude":4}}}`; golden(t, got) != want {
		t.Fatalf("toolConfig = %s, want %s (last wins, no language code)", golden(t, got), want)
	}
	// A nil pointer in tools[] is a fail-fast error, like the other hosted types.
	if _, err := buildTools([]any{(*tools.GoogleMapsToolSchema)(nil)}); !errors.Is(err, config.ErrUnsupportedTool) {
		t.Fatalf("nil Maps pointer in buildTools: %v", err)
	}
	// The pointer form maps like the value form, widget flag included.
	if got, err := buildTools([]any{&tools.GoogleMapsToolSchema{EnableWidget: true}}); err != nil || golden(t, got) != `[{"googleMaps":{"enableWidget":true}}]` {
		t.Fatalf("pointer Maps = %v, %v", got, err)
	}
}

func mustSchemasCx(t *testing.T, tool tools.Tool, cx *agocontext.Context) []any {
	t.Helper()
	ss, err := tool.Schemas(cx)
	if err != nil {
		t.Fatalf("Schemas: %v", err)
	}
	return ss
}
