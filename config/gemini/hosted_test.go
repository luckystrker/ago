package gemini

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/luckystrker/ago/config"
	"github.com/luckystrker/ago/events"
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
