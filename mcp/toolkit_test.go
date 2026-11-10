package mcp

import (
	"errors"
	"net/http/httptest"
	"testing"

	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/stream"
)

func toolNames(t *testing.T, cx *agocontext.Context, tk *Toolkit) map[string]bool {
	t.Helper()
	ts, err := tk.Tools(cx)
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}
	names := map[string]bool{}
	for _, tl := range ts {
		names[tl.Name()] = true
	}
	return names
}

func TestToolkitDiscoveryCache(t *testing.T) {
	srv := newScenarioHTTP(false)
	hs := httptest.NewServer(srv)
	defer hs.Close()
	cx := agocontext.New(stream.NewMemoryStream())

	tenant := "alpha"
	tk := NewToolkitFunc(func(*agocontext.Context) (any, error) {
		return &MCPServerConfig{URL: hs.URL, Headers: map[string]string{"X-Tenant": tenant}, Description: "tenant " + tenant}, nil
	})
	defer tk.Close()
	lists := func() int { return len(srv.requests("tools/list")) }

	t.Run("miss discovers", func(t *testing.T) {
		if names := toolNames(t, cx, tk); !names["elicit"] {
			t.Fatalf("tools = %v", names)
		}
		if lists() != 1 {
			t.Fatalf("tools/list ran %d times, want 1", lists())
		}
	})
	t.Run("hit reuses", func(t *testing.T) {
		toolNames(t, cx, tk)
		toolNames(t, cx, tk)
		if lists() != 1 {
			t.Fatalf("tools/list ran %d times after two cached calls, want still 1", lists())
		}
	})
	t.Run("a changed resolved config invalidates", func(t *testing.T) {
		tenant = "beta"
		toolNames(t, cx, tk)
		if lists() != 2 {
			t.Fatalf("tools/list ran %d times after a config change, want 2", lists())
		}
		if tk.Description() != "tenant beta" {
			t.Errorf("description = %q", tk.Description())
		}
		toolNames(t, cx, tk)
		if lists() != 2 {
			t.Fatalf("the new config was not cached: %d lists", lists())
		}
	})
	t.Run("a failed discovery keeps the previous set", func(t *testing.T) {
		before := lists()
		bad := NewToolkitFunc(func(*agocontext.Context) (any, error) { return nil, errors.New("no config today") })
		if _, err := bad.Tools(cx); err == nil {
			t.Fatal("resolver failure was swallowed")
		}
		// And on the real toolkit: a dead endpoint errors but the old tools stay.
		tenant = "alpha"
		dead := httptest.NewServer(newScenarioHTTP(false))
		deadURL := dead.URL
		dead.Close()
		tk2 := NewToolkitFunc(func(*agocontext.Context) (any, error) {
			if tenant == "alpha" {
				return &MCPServerConfig{URL: hs.URL}, nil
			}
			return &MCPServerConfig{URL: deadURL}, nil
		})
		defer tk2.Close()
		toolNames(t, cx, tk2)
		tenant = "gamma"
		if _, err := tk2.Tools(cx); err == nil {
			t.Fatal("discovery against a dead endpoint succeeded")
		}
		tenant = "alpha"
		toolNames(t, cx, tk2)
		if got := lists() - before; got != 1 {
			t.Fatalf("tools/list ran %d more times, want 1 (the failed discovery must not evict the cache)", got)
		}
	})
}

func TestToolkitReplacedConfigKeepsOneGeneration(t *testing.T) {
	hs := httptest.NewServer(newScenarioHTTP(false))
	defer hs.Close()
	cx := agocontext.New(stream.NewMemoryStream())
	gen := "a"
	tk := NewToolkitFunc(func(*agocontext.Context) (any, error) {
		return &MCPServerConfig{URL: hs.URL, ToolNamePrefix: gen + "_"}, nil
	})
	defer tk.Close()

	first, err := tk.Tools(cx)
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}
	gen = "b"
	if _, err := tk.Tools(cx); err != nil {
		t.Fatalf("Tools: %v", err)
	}
	// A turn still holding the first generation's tools survives one replacement…
	call := func() error {
		for _, tl := range first {
			if tl.Name() == "a_caps" {
				_, err := tl.(*remoteTool).cl.CallTool("caps", nil)
				return err
			}
		}
		t.Fatal("a_caps not found")
		return nil
	}
	if err := call(); err != nil {
		t.Fatalf("tool of the previous generation failed after one replacement: %v", err)
	}
	// …and is closed by the next, so superseded clients never accumulate.
	gen = "c"
	if _, err := tk.Tools(cx); err != nil {
		t.Fatalf("Tools: %v", err)
	}
	if err := call(); !errors.Is(err, ErrClosed) {
		t.Fatalf("tool of a twice-replaced generation = %v, want ErrClosed", err)
	}
}

func TestToolkitStaticConfig(t *testing.T) {
	srv := newScenarioHTTP(false)
	hs := httptest.NewServer(srv)
	defer hs.Close()
	cfg := MCPServerConfig{URL: hs.URL, AllowedTools: []string{"caps"}}
	tk, err := NewToolkit(cfg)
	if err != nil {
		t.Fatalf("NewToolkit: %v", err)
	}
	defer tk.Close()
	cx := agocontext.New(stream.NewMemoryStream())
	// Mutating the caller's slice after construction must not change the cache key.
	cfg.AllowedTools[0] = "elicit"
	for i := 0; i < 3; i++ {
		names := toolNames(t, cx, tk)
		if len(names) != 1 || !names["caps"] {
			t.Fatalf("tools = %v, want only caps", names)
		}
	}
	if n := len(srv.requests("tools/list")); n != 1 {
		t.Fatalf("tools/list ran %d times for a static config, want 1", n)
	}
	if _, err := NewToolkit(42); !errors.Is(err, ErrUnsupportedConfig) {
		t.Errorf("NewToolkit(42) = %v, want ErrUnsupportedConfig", err)
	}
}
