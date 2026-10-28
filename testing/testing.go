// Package agotesting provides scripted model doubles for tests: TestConfig
// replays a queue of ModelResponses and records what history every call saw;
// TrackingConfig wraps any real config and records its calls the same way.
//
// The directory is named "testing" but the package is agotesting so it can
// never clash with the standard library "testing" package. Import it as
//
//	agotest "ago/testing"
//
// Parity with Python AG2 ag2/testing.py (TestConfig / TrackingConfig); the
// deviations the Go shape forces are marked // AGO-DEVIATION below and
// collected in docs/DECISIONS.md (D-018).
package agotesting

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"ago/config"
	agocontext "ago/context"
	"ago/events"
)

// ProviderTest is the provider identity of a scripted config (Python parity:
// tests pass provider="test" explicitly; AGo fixes it on the double).
const ProviderTest config.Provider = "test"

// ErrScriptedExhausted is returned by TestConfig.Call once the script ran out
// of responses. Match with errors.Is.
var ErrScriptedExhausted = errors.New("agotesting: scripted responses exhausted")

// TestConfig is a scripted model double implementing both config.ModelConfig
// and config.LLMClient. Every Call hands out the next scripted
// *events.ModelResponse; once the script is exhausted the error wraps
// ErrScriptedExhausted. Each invocation — including one that finds the script
// exhausted — is recorded as a deep-copied snapshot of the history it was
// handed (see Calls). Safe for concurrent use.
//
// AGO-DEVIATION: the Python Turn vocabulary (plain strings, raised exceptions,
// mid-turn published events, tool-call shortcuts) collapses to full
// ModelResponse scripts, and the raise_tool_errors history inspection is not
// ported; recording the call history, which Python keeps on TrackingClient,
// lives here per the porting brief.
type TestConfig struct {
	// script is immutable after construction and shared with clones.
	script []*events.ModelResponse
	// model is immutable after construction ("test" unless Clone overrode it).
	model string

	mu    sync.Mutex
	next  int
	calls [][]events.Event
}

var (
	_ config.ModelConfig = (*TestConfig)(nil)
	_ config.LLMClient   = (*TestConfig)(nil)
)

// NewTestConfig scripts one ModelResponse per LLM call, handed out in order.
func NewTestConfig(responses ...*events.ModelResponse) *TestConfig {
	return &TestConfig{script: responses, model: "test"}
}

// Provider returns ProviderTest.
//
// AGO-DEVIATION: Python's TestConfig.provider raises NotImplementedError when
// the provider was not given; AGo pins the identity on the double.
func (t *TestConfig) Provider() config.Provider { return ProviderTest }

// Model returns the model name ("test" unless Clone's "model" override set it).
//
// AGO-DEVIATION: Python's TestConfig.model raises NotImplementedError when the
// model was not given; the ModelConfig contract has no error return here, so a
// stable default is reported instead.
func (t *TestConfig) Model() string { return t.model }

// Create returns the client itself: TestConfig is its own LLMClient, so every
// client handed out shares one script cursor.
//
// AGO-DEVIATION: Python's TestConfig.create() returns a fresh client with an
// independent script iterator; AGo follows the porting brief ("Create returns
// the client itself"), which makes the cursor shared across Create calls.
func (t *TestConfig) Create() (config.LLMClient, error) { return t, nil }

// Clone returns a fresh TestConfig with the same script but its own cursor and
// call recording, so a clone replays the scenario from the start. The only
// supported override is "model" (string); any other key — or a non-string
// value for it — is an error (ModelConfig.Clone contract).
//
// AGO-DEVIATION: Python's TestConfig.copy() returns self; AGo follows the
// ModelConfig.Clone contract (dataclasses.replace parity), which yields a new
// value with fresh recording state.
func (t *TestConfig) Clone(overrides map[string]any) (config.ModelConfig, error) {
	model := t.model
	for k, v := range overrides {
		if k != "model" {
			return nil, fmt.Errorf("agotesting: unknown TestConfig override key %q", k)
		}
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("agotesting: override %q must be a string, got %T", k, v)
		}
		model = s
	}
	return &TestConfig{script: t.script, model: model}, nil
}

// Call records the history it was handed, then returns the next scripted
// response verbatim (not copied — Python parity; the agent is expected to treat
// a response as immutable once produced). When the script is exhausted it
// returns an error wrapping ErrScriptedExhausted and records the call all the
// same, so Calls still shows that the call happened. conv and opts are accepted
// for the LLMClient contract and left untouched: scripted responses are
// complete ModelResponses, and publishing them to the stream is the agent's job.
func (t *TestConfig) Call(ctx context.Context, messages []events.Event, conv *agocontext.Context, opts config.CallOptions) (*events.ModelResponse, error) {
	t.mu.Lock()
	t.calls = append(t.calls, snapshotEvents(messages))
	if t.next >= len(t.script) {
		n := t.next + 1
		t.mu.Unlock()
		return nil, fmt.Errorf("%w: no scripted response for call %d", ErrScriptedExhausted, n)
	}
	resp := t.script[t.next]
	t.next++
	t.mu.Unlock()
	return resp, nil
}

// Calls returns the history snapshot of every call so far, oldest first. The
// snapshots are deep copies taken at call time and the returned slices are
// copies again, so neither the live transcript nor later callers can mutate
// the recording.
func (t *TestConfig) Calls() [][]events.Event {
	t.mu.Lock()
	defer t.mu.Unlock()
	return copyCalls(t.calls)
}

// TrackingConfig wraps any config.ModelConfig and records the history every
// LLM call sees. Create delegates to the wrapped config and returns a client
// that records before delegating; Clone clones the wrapped config and wraps
// the clone. Safe for concurrent use.
type TrackingConfig struct {
	inner config.ModelConfig
	rec   *recorder
}

var _ config.ModelConfig = (*TrackingConfig)(nil)

// NewTrackingConfig starts recording on top of inner. It reports an error when
// inner is nil rather than panicking later at call time.
func NewTrackingConfig(inner config.ModelConfig) (*TrackingConfig, error) {
	if inner == nil {
		return nil, errors.New("agotesting: TrackingConfig needs a config to wrap")
	}
	return &TrackingConfig{inner: inner, rec: &recorder{}}, nil
}

// Provider forwards to the wrapped config.
func (t *TrackingConfig) Provider() config.Provider { return t.inner.Provider() }

// Model forwards to the wrapped config.
func (t *TrackingConfig) Model() string { return t.inner.Model() }

// Create builds the wrapped config's client and returns a recording wrapper
// around it; inner errors pass through unwrapped.
func (t *TrackingConfig) Create() (config.LLMClient, error) {
	cl, err := t.inner.Create()
	if err != nil {
		return nil, err
	}
	return trackingClient{inner: cl, rec: t.rec}, nil
}

// Clone clones the wrapped config with the given overrides and returns a
// TrackingConfig around the clone. The recorder is shared, so recordings
// accumulate across every wrapper cloned from this one.
//
// AGO-DEVIATION: Python's TrackingConfig.copy() returns self (so recordings
// accumulate on one object); AGo must apply overrides per the ModelConfig.Clone
// contract, and sharing the recorder keeps the accumulation behavior.
func (t *TrackingConfig) Clone(overrides map[string]any) (config.ModelConfig, error) {
	inner, err := t.inner.Clone(overrides)
	if err != nil {
		return nil, err
	}
	return &TrackingConfig{inner: inner, rec: t.rec}, nil
}

// Calls returns the history snapshot of every call made through this config
// and its clones' clients, oldest first (same copy semantics as TestConfig.Calls).
//
// AGO-DEVIATION: Python's TrackingClient records a ModelCall of prompt, tools,
// dependencies and variables; the porting brief asks for call history
// snapshots, matching TestConfig.Calls.
func (t *TrackingConfig) Calls() [][]events.Event { return t.rec.calls() }

// trackingClient records each call's history, then delegates to the real client.
type trackingClient struct {
	inner config.LLMClient
	rec   *recorder
}

var _ config.LLMClient = trackingClient{}

func (c trackingClient) Call(ctx context.Context, messages []events.Event, conv *agocontext.Context, opts config.CallOptions) (*events.ModelResponse, error) {
	c.rec.record(messages)
	return c.inner.Call(ctx, messages, conv, opts)
}

// recorder accumulates history snapshots across every client a config handed out.
type recorder struct {
	mu        sync.Mutex
	snapshots [][]events.Event
}

func (r *recorder) record(messages []events.Event) {
	r.mu.Lock()
	r.snapshots = append(r.snapshots, snapshotEvents(messages))
	r.mu.Unlock()
}

func (r *recorder) calls() [][]events.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return copyCalls(r.snapshots)
}

// copyCalls returns a fresh deep copy of the recorded snapshots: a caller can
// neither reach the recording nor another caller's previously returned events.
func copyCalls(calls [][]events.Event) [][]events.Event {
	out := make([][]events.Event, len(calls))
	for i, c := range calls {
		evs := make([]events.Event, len(c))
		for j, ev := range c {
			evs[j] = snapshotEvent(ev)
		}
		out[i] = evs
	}
	return out
}

// snapshotEvents deep-copies a history so later mutation of the live
// transcript cannot change what a recorded call shows.
func snapshotEvents(evs []events.Event) []events.Event {
	out := make([]events.Event, len(evs))
	for i, ev := range evs {
		out[i] = snapshotEvent(ev)
	}
	return out
}

// snapshotEvent copies one event through its JSON round trip (lossless for
// every registered event type). Events that do not survive it — marshal
// failures, and types outside the marshal registry, which UnmarshalEvent would
// degrade to UnknownEvent — are kept as-is.
func snapshotEvent(ev events.Event) events.Event {
	if ev == nil {
		return nil
	}
	b, err := events.MarshalEvent(ev)
	if err != nil {
		return ev
	}
	cp, err := events.UnmarshalEvent(b)
	if err != nil {
		return ev
	}
	if _, unknown := cp.(events.UnknownEvent); unknown {
		return ev
	}
	return cp
}
