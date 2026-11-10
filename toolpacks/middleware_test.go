package toolpacks

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/middleware"
	agotest "github.com/luckystrker/ago/testing"
)

// countingMiddleware counts the tool calls its chain sees.
type countingMiddleware struct {
	middleware.Base
	hits atomic.Int32
	// order, when set, records this middleware's label before calling next.
	order *[]string
	label string
	mu    *sync.Mutex
}

func (m *countingMiddleware) OnToolExecution(next middleware.ToolExecFunc, ev *events.ToolCallEvent, cx *agocontext.Context) (events.Event, error) {
	m.hits.Add(1)
	if m.order != nil {
		m.mu.Lock()
		*m.order = append(*m.order, m.label)
		m.mu.Unlock()
	}
	return next(ev, cx)
}

func newCounting() *countingMiddleware { return &countingMiddleware{} }

// TestToolScopedMiddlewareApplied: every ready-made toolpack accepts the
// tool-scoped middleware option and the chain runs on the tool's own call.
func TestToolScopedMiddlewareApplied(t *testing.T) {
	_, cx := newTestContext()

	t.Run("shell", func(t *testing.T) {
		mw := newCounting()
		s, err := NewShellTool(WithShellMiddleware(mw))
		if err != nil {
			t.Fatalf("NewShellTool: %v", err)
		}
		defer s.Close()
		callTool(t, s, cx, s.Name(), `{"command":"echo mw"}`)
		if got := mw.hits.Load(); got != 1 {
			t.Fatalf("shell middleware hits = %d, want 1", got)
		}
	})

	t.Run("filesystem", func(t *testing.T) {
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, "note.txt"), []byte("content"), 0o644); err != nil {
			t.Fatal(err)
		}
		mw := newCounting()
		f, err := NewFilesystemToolkit(root, WithFilesystemMiddleware(mw))
		if err != nil {
			t.Fatalf("NewFilesystemToolkit: %v", err)
		}
		callTool(t, f, cx, "read_file", `{"path":"note.txt"}`)
		if got := mw.hits.Load(); got != 1 {
			t.Fatalf("filesystem middleware hits = %d, want 1", got)
		}
	})

	t.Run("duckduckgo", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `<div class="result"><h2><a class="result__a" href="https://x/">X</a></h2>`+
				`<a class="result__snippet">S</a></div>`)
		}))
		defer srv.Close()
		mw := newCounting()
		d, err := NewDuckDuckGoSearch(WithDDGBaseURL(srv.URL), WithDDGMiddleware(mw))
		if err != nil {
			t.Fatalf("NewDuckDuckGoSearch: %v", err)
		}
		callTool(t, d, cx, d.Name(), `{"query":"q"}`)
		if got := mw.hits.Load(); got != 1 {
			t.Fatalf("ddg middleware hits = %d, want 1", got)
		}
	})

	t.Run("tavily", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `{"query":"q","results":[]}`)
		}))
		defer srv.Close()
		mw := newCounting()
		tv, err := NewTavilySearch("key", WithTavilyBaseURL(srv.URL), WithTavilyMiddleware(mw))
		if err != nil {
			t.Fatalf("NewTavilySearch: %v", err)
		}
		callTool(t, tv, cx, tv.Name(), `{"query":"q"}`)
		if got := mw.hits.Load(); got != 1 {
			t.Fatalf("tavily middleware hits = %d, want 1", got)
		}
	})

	t.Run("perplexity", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/search":
				_, _ = io.WriteString(w, `{"results":[{"title":"T","url":"https://x"}]}`)
			case "/chat/completions":
				_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"A"}}],"search_results":[],"citations":[]}`)
			default:
				http.NotFound(w, r)
			}
		}))
		defer srv.Close()
		mw := newCounting()
		p, err := NewPerplexitySearch("key", WithPerplexityBaseURL(srv.URL), WithPerplexityMiddleware(mw))
		if err != nil {
			t.Fatalf("NewPerplexitySearch: %v", err)
		}
		callTool(t, p, cx, "perplexity_search", `{"query":"q"}`)
		callTool(t, p, cx, "perplexity_answer", `{"query":"q"}`)
		if got := mw.hits.Load(); got != 2 {
			t.Fatalf("perplexity middleware hits = %d, want 2 (both members)", got)
		}
	})

	t.Run("dynamic", func(t *testing.T) {
		mw := newCounting()
		f, err := NewDynamicAgentFactory(agotest.NewTestConfig(textResp("unused")), nil, WithDynamicMiddleware(mw))
		if err != nil {
			t.Fatalf("NewDynamicAgentFactory: %v", err)
		}
		// An unknown tool name is the recoverable error path: the middleware
		// still wraps the factory's own execution and no child is spawned.
		ev := callTool(t, f, cx, f.Name(),
			`{"spec":{"name":"w","prompt":["p"],"tool_names":["missing"]},"objective":"o"}`)
		if got := resultPartText(t, ev); !strings.Contains(got, "Error: unknown tools") {
			t.Fatalf("dynamic output = %q", got)
		}
		if got := mw.hits.Load(); got != 1 {
			t.Fatalf("dynamic middleware hits = %d, want 1", got)
		}
	})
}

// TestToolScopedMiddlewareOrder: the first middleware given to the tool option
// is the outermost layer of the tool's own chain.
func TestToolScopedMiddlewareOrder(t *testing.T) {
	var (
		mu    sync.Mutex
		order []string
	)
	outer := &countingMiddleware{order: &order, label: "outer", mu: &mu}
	inner := &countingMiddleware{order: &order, label: "inner", mu: &mu}
	s, err := NewShellTool(WithShellMiddleware(outer, inner))
	if err != nil {
		t.Fatalf("NewShellTool: %v", err)
	}
	defer s.Close()
	_, cx := newTestContext()
	callTool(t, s, cx, s.Name(), `{"command":"echo order"}`)
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(order, ",") != "outer,inner" {
		t.Fatalf("middleware order = %v, want outer then inner", order)
	}
}
