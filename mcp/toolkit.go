package mcp

import (
	"fmt"
	"reflect"
	"sync"

	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/tools"
)

// ConfigResolver produces the server config for one turn from the conversation
// (its variables, dependencies, ...).
//
// AGO-DEVIATION: Python resolves Variable-valued fields inside the config
// dataclass; AGo configs are plain values, so the per-turn indirection is one
// function over the whole config. It plays the same role: the resolver may return a different config on a
// later turn — a rotated token, another endpoint — and the Toolkit rediscovers.
// It must return MCPServerConfig or MCPStdioConfig (value or pointer).
type ConfigResolver func(cx *agocontext.Context) (any, error)

// Toolkit discovers a server's tools once and reuses the discovery for as long
// as the resolved config stays the same (Python parity: MCPToolkit's discovery
// cache). It is safe for concurrent use.
//
// What invalidates the cache: only a change of the resolved config — compared
// field by field, headers, token, arguments, environment, filters and prefix
// included. There is no TTL and no invalidation on the server's own
// tools/list_changed notice. A discovery that fails keeps the previous tools and
// the previous config in place, so a transient error never discards a working
// set (Python: "replace after discovery succeeds").
//
// Each discovered tool is pinned to the Client of the config that discovered it,
// so a call always reaches the server that advertised the tool. A turn still
// holding the tools of a replaced config keeps working until the *next*
// replacement; then that Client is closed and its tools report ErrClosed.
//
// AGO-DEVIATION: Python opens a short-lived session per call and so never has
// anything to close; a Go Client owns a process or connection, so superseded
// ones must be bounded rather than accumulate across config rotations.
type Toolkit struct {
	resolve ConfigResolver
	opts    []Option

	mu      sync.Mutex
	cfg     any // snapshot of the config the cached tools were discovered with
	client  *Client
	retired *Client
	tools   []tools.Tool
	desc    string
}

// NewToolkit builds a Toolkit over a fixed config: discovery happens on the first
// Tools call and is then reused for good.
func NewToolkit(config any, opts ...Option) (*Toolkit, error) {
	snap, err := snapshotConfig(config)
	if err != nil {
		return nil, err
	}
	return &Toolkit{resolve: func(*agocontext.Context) (any, error) { return snap, nil }, opts: opts}, nil
}

// NewToolkitFunc builds a Toolkit whose config is resolved on every Tools call.
func NewToolkitFunc(resolve ConfigResolver, opts ...Option) *Toolkit {
	return &Toolkit{resolve: resolve, opts: opts}
}

// Tools returns the server's tools for this turn: the cached set when the
// resolved config is unchanged, a fresh discovery otherwise.
func (t *Toolkit) Tools(cx *agocontext.Context) ([]tools.Tool, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	raw, err := t.resolve(cx)
	if err != nil {
		return nil, fmt.Errorf("mcp: resolve server config: %w", err)
	}
	snap, err := snapshotConfig(raw)
	if err != nil {
		return nil, err
	}
	if t.client != nil && reflect.DeepEqual(snap, t.cfg) {
		return append([]tools.Tool(nil), t.tools...), nil
	}
	cl, err := New(snap, t.opts...)
	if err != nil {
		return nil, err
	}
	found, err := Tools(cl)
	if err != nil {
		cl.Close()
		return nil, err
	}
	if t.retired != nil {
		t.retired.Close()
	}
	t.retired, t.client, t.cfg, t.tools = t.client, cl, snap, found
	switch c := snap.(type) {
	case *MCPServerConfig:
		t.desc = c.Description
	case *MCPStdioConfig:
		t.desc = c.Description
	}
	return append([]tools.Tool(nil), found...), nil
}

// Description is what the most recently discovered server is for ("" before any
// discovery or when its config names none).
func (t *Toolkit) Description() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.desc
}

// Close closes every Client the toolkit still holds. Idempotent.
func (t *Toolkit) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	var first error
	for _, cl := range []*Client{t.client, t.retired} {
		if cl == nil {
			continue
		}
		if err := cl.Close(); err != nil && first == nil {
			first = err
		}
	}
	t.client, t.retired, t.cfg, t.tools = nil, nil, nil, nil
	return first
}

// snapshotConfig deep-copies a config into its pointer form, so later mutation of
// the caller's slices or maps cannot change what the cache compares against.
func snapshotConfig(config any) (any, error) {
	switch c := config.(type) {
	case *MCPServerConfig:
		if c != nil {
			return copyHTTPConfig(c), nil
		}
	case MCPServerConfig:
		return copyHTTPConfig(&c), nil
	case *MCPStdioConfig:
		if c != nil {
			return copyStdioConfig(c), nil
		}
	case MCPStdioConfig:
		return copyStdioConfig(&c), nil
	}
	return nil, fmt.Errorf("%w: %T", ErrUnsupportedConfig, config)
}
