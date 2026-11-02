package agotesting

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/luckystrker/ago/config"
	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
)

func ctx() context.Context { return context.Background() }

// eventJSON renders events deterministically (encoding/json sorts map keys) so
// recorded snapshots can be compared with the history a call was handed.
func eventJSON(t *testing.T, evs []events.Event) []string {
	t.Helper()
	out := make([]string, len(evs))
	for i, ev := range evs {
		b, err := events.MarshalEvent(ev)
		if err != nil {
			t.Fatalf("marshal %T: %v", ev, err)
		}
		out[i] = string(b)
	}
	return out
}

func sameEvents(t *testing.T, got, want []events.Event) bool {
	t.Helper()
	g, w := eventJSON(t, got), eventJSON(t, want)
	if len(g) != len(w) {
		return false
	}
	for i := range g {
		if g[i] != w[i] {
			return false
		}
	}
	return true
}

func hist(parts ...any) []events.Event {
	req := events.EnsureRequest(parts...)
	return []events.Event{req}
}

func TestProviderTestValue(t *testing.T) {
	if string(ProviderTest) != "test" {
		t.Fatalf("ProviderTest = %q, want %q", string(ProviderTest), "test")
	}
}

func TestTestConfigScriptedResponses(t *testing.T) {
	r1 := &events.ModelResponse{Message: &events.ModelMessage{Content: "one"}}
	r2 := &events.ModelResponse{Message: &events.ModelMessage{Content: "two"}}
	cfg := NewTestConfig(r1, r2)

	var mc config.ModelConfig = cfg
	if mc.Provider() != ProviderTest {
		t.Fatalf("Provider() = %q, want %q", mc.Provider(), ProviderTest)
	}
	if mc.Model() != "test" {
		t.Fatalf("Model() = %q, want default %q", mc.Model(), "test")
	}
	cl, err := mc.Create()
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if cl != config.LLMClient(cfg) {
		t.Fatal("Create must return the config itself as the client")
	}

	got1, err := cl.Call(ctx(), hist("hi"), nil, config.CallOptions{})
	if err != nil {
		t.Fatalf("call 1: %v", err)
	}
	if got1 != r1 {
		t.Fatalf("call 1 response = %+v, want the first scripted response", got1)
	}
	got2, err := cl.Call(ctx(), hist("again"), nil, config.CallOptions{})
	if err != nil {
		t.Fatalf("call 2: %v", err)
	}
	if got2 != r2 {
		t.Fatalf("call 2 response = %+v, want the second scripted response", got2)
	}
}

func TestTestConfigExhausted(t *testing.T) {
	t.Run("after script ends", func(t *testing.T) {
		cfg := NewTestConfig(&events.ModelResponse{Message: &events.ModelMessage{Content: "only"}})
		cl, err := cfg.Create()
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if _, err := cl.Call(ctx(), hist("1"), nil, config.CallOptions{}); err != nil {
			t.Fatalf("call 1: %v", err)
		}
		_, err = cl.Call(ctx(), hist("2"), nil, config.CallOptions{})
		if !errors.Is(err, ErrScriptedExhausted) {
			t.Fatalf("call 2 error = %v, want ErrScriptedExhausted", err)
		}
		// The cursor does not advance past the end: the next call fails again.
		if _, err := cl.Call(ctx(), hist("3"), nil, config.CallOptions{}); !errors.Is(err, ErrScriptedExhausted) {
			t.Fatalf("call 3 error = %v, want ErrScriptedExhausted", err)
		}
	})
	t.Run("empty script", func(t *testing.T) {
		cfg := NewTestConfig()
		_, err := cfg.Call(ctx(), nil, nil, config.CallOptions{})
		if !errors.Is(err, ErrScriptedExhausted) {
			t.Fatalf("error = %v, want ErrScriptedExhausted", err)
		}
	})
}

func TestTestConfigRecordsCalls(t *testing.T) {
	cfg := NewTestConfig(
		&events.ModelResponse{Message: &events.ModelMessage{Content: "a"}},
		&events.ModelResponse{Message: &events.ModelMessage{Content: "b"}},
	)
	h1 := hist("first")
	h2 := hist("second")
	if _, err := cfg.Call(ctx(), h1, nil, config.CallOptions{}); err != nil {
		t.Fatalf("call 1: %v", err)
	}
	if _, err := cfg.Call(ctx(), h2, nil, config.CallOptions{}); err != nil {
		t.Fatalf("call 2: %v", err)
	}

	calls := cfg.Calls()
	if len(calls) != 2 {
		t.Fatalf("Calls() has %d entries, want 2", len(calls))
	}
	if !sameEvents(t, calls[0], h1) || !sameEvents(t, calls[1], h2) {
		t.Fatalf("recorded histories do not match the handed ones:\n%v\n%v", calls, eventJSON(t, h1))
	}

	// A call that finds the script exhausted is recorded too.
	if _, err := cfg.Call(ctx(), hist("third"), nil, config.CallOptions{}); !errors.Is(err, ErrScriptedExhausted) {
		t.Fatalf("call 3 error = %v, want ErrScriptedExhausted", err)
	}
	if got := len(cfg.Calls()); got != 3 {
		t.Fatalf("Calls() has %d entries after the failed call, want 3", got)
	}
}

func TestTestConfigSnapshotsAreIsolated(t *testing.T) {
	cfg := NewTestConfig(&events.ModelResponse{})
	part := &events.TextInput{Content: "hello"}
	h := []events.Event{&events.ModelRequest{Parts: []events.Input{part}}}
	if _, err := cfg.Call(ctx(), h, nil, config.CallOptions{}); err != nil {
		t.Fatalf("call: %v", err)
	}

	// Mutating the handed history after the call must not change the snapshot.
	part.Content = "mutated"
	h[0].(*events.ModelRequest).Metadata = map[string]any{"touched": true}

	calls := cfg.Calls()
	if len(calls) != 1 {
		t.Fatalf("Calls() has %d entries, want 1", len(calls))
	}
	req, ok := calls[0][0].(*events.ModelRequest)
	if !ok {
		t.Fatalf("recorded event type = %T, want *events.ModelRequest", calls[0][0])
	}
	if req.Metadata != nil {
		t.Fatalf("snapshot saw metadata mutation: %v", req.Metadata)
	}
	if len(req.Parts) != 1 {
		t.Fatalf("snapshot has %d parts, want 1", len(req.Parts))
	}
	text, ok := req.Parts[0].(*events.TextInput)
	if !ok {
		t.Fatalf("recorded part type = %T, want *events.TextInput", req.Parts[0])
	}
	if text.Content != "hello" {
		t.Fatalf("recorded part content = %q, want the value at call time %q", text.Content, "hello")
	}

	// Mutating the returned slices must not corrupt the recording either.
	calls[0][0].(*events.ModelRequest).Metadata = map[string]any{"later": true}
	calls[0] = append(calls[0], &events.ModelRequest{})
	again := cfg.Calls()
	if len(again) != 1 || len(again[0]) != 1 {
		t.Fatalf("recording changed after caller mutation: %d calls, %d events", len(again), len(again[0]))
	}
	if again[0][0].(*events.ModelRequest).Metadata != nil {
		t.Fatal("recording saw mutation of a returned snapshot")
	}
}

func TestTestConfigClone(t *testing.T) {
	r1 := &events.ModelResponse{Message: &events.ModelMessage{Content: "one"}}
	r2 := &events.ModelResponse{Message: &events.ModelMessage{Content: "two"}}
	cfg := NewTestConfig(r1, r2)

	clonedAny, err := cfg.Clone(map[string]any{"model": "scripted-1"})
	if err != nil {
		t.Fatalf("Clone: %v", err)
	}
	cloned, ok := clonedAny.(*TestConfig)
	if !ok {
		t.Fatalf("Clone returned %T, want *TestConfig", clonedAny)
	}
	if clonedAny.Provider() != ProviderTest {
		t.Fatalf("clone Provider() = %q, want %q", clonedAny.Provider(), ProviderTest)
	}
	if clonedAny.Model() != "scripted-1" {
		t.Fatalf("clone Model() = %q, want %q", clonedAny.Model(), "scripted-1")
	}

	// The clone replays the same scenario from the start...
	cl, err := clonedAny.Create()
	if err != nil {
		t.Fatalf("clone Create: %v", err)
	}
	got1, err := cl.Call(ctx(), hist("x"), nil, config.CallOptions{})
	if err != nil || got1 != r1 {
		t.Fatalf("clone call 1 = (%+v, %v), want the first scripted response", got1, err)
	}
	got2, err := cl.Call(ctx(), hist("y"), nil, config.CallOptions{})
	if err != nil || got2 != r2 {
		t.Fatalf("clone call 2 = (%+v, %v), want the second scripted response", got2, err)
	}

	// ...with its own recording state; the original is untouched.
	if got := len(cfg.Calls()); got != 0 {
		t.Fatalf("original recorded %d calls through the clone, want 0", got)
	}
	if got := len(cloned.Calls()); got != 2 {
		t.Fatalf("clone recorded %d calls, want 2", got)
	}
}

func TestTestConfigCloneBadOverrides(t *testing.T) {
	cfg := NewTestConfig()
	if _, err := cfg.Clone(map[string]any{"temperature": 0.5}); err == nil {
		t.Fatal("unknown override key accepted, want error")
	}
	if _, err := cfg.Clone(map[string]any{"model": 3}); err == nil {
		t.Fatal("non-string model override accepted, want error")
	}
}

func TestTrackingConfigOverTestConfig(t *testing.T) {
	r1 := &events.ModelResponse{Message: &events.ModelMessage{Content: "one"}}
	r2 := &events.ModelResponse{Message: &events.ModelMessage{Content: "two"}}
	inner := NewTestConfig(r1, r2)
	tc, err := NewTrackingConfig(inner)
	if err != nil {
		t.Fatalf("NewTrackingConfig: %v", err)
	}

	var mc config.ModelConfig = tc
	if mc.Provider() != ProviderTest {
		t.Fatalf("Provider() = %q, want the wrapped %q", mc.Provider(), ProviderTest)
	}
	if mc.Model() != "test" {
		t.Fatalf("Model() = %q, want the wrapped %q", mc.Model(), "test")
	}

	cl, err := mc.Create()
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	h1 := hist("first")
	h2 := hist("second")
	got1, err := cl.Call(ctx(), h1, nil, config.CallOptions{})
	if err != nil || got1 != r1 {
		t.Fatalf("call 1 = (%+v, %v), want the first scripted response", got1, err)
	}
	got2, err := cl.Call(ctx(), h2, nil, config.CallOptions{})
	if err != nil || got2 != r2 {
		t.Fatalf("call 2 = (%+v, %v), want the second scripted response", got2, err)
	}

	if calls := tc.Calls(); len(calls) != 2 || !sameEvents(t, calls[0], h1) || !sameEvents(t, calls[1], h2) {
		t.Fatalf("tracking recording = %v, want the two handed histories", calls)
	}
	// The wrapped TestConfig recorded the same calls on its own.
	if calls := inner.Calls(); len(calls) != 2 {
		t.Fatalf("inner recording has %d entries, want 2", len(calls))
	}

	// Inner errors pass through: exhaustion keeps its sentinel.
	if _, err := cl.Call(ctx(), hist("third"), nil, config.CallOptions{}); !errors.Is(err, ErrScriptedExhausted) {
		t.Fatalf("call 3 error = %v, want ErrScriptedExhausted", err)
	}
}

func TestTrackingConfigClone(t *testing.T) {
	r1 := &events.ModelResponse{Message: &events.ModelMessage{Content: "one"}}
	inner := NewTestConfig(r1)
	tc, err := NewTrackingConfig(inner)
	if err != nil {
		t.Fatalf("NewTrackingConfig: %v", err)
	}

	clonedAny, err := tc.Clone(map[string]any{"model": "scripted-1"})
	if err != nil {
		t.Fatalf("Clone: %v", err)
	}
	if clonedAny.Model() != "scripted-1" {
		t.Fatalf("clone Model() = %q, want the override applied to the inner config", clonedAny.Model())
	}
	cloned := clonedAny.(*TrackingConfig)

	c0, err := tc.Create()
	if err != nil {
		t.Fatalf("original Create: %v", err)
	}
	c1, err := cloned.Create()
	if err != nil {
		t.Fatalf("clone Create: %v", err)
	}
	h1 := hist("via original")
	h2 := hist("via clone")
	if _, err := c0.Call(ctx(), h1, nil, config.CallOptions{}); err != nil {
		t.Fatalf("original call: %v", err)
	}
	// The clone's inner config has a fresh cursor, so the scenario replays.
	if _, err := c1.Call(ctx(), h2, nil, config.CallOptions{}); err != nil {
		t.Fatalf("clone call: %v", err)
	}

	// The recorder is shared across clones: both calls land in one recording.
	calls := tc.Calls()
	if len(calls) != 2 || !sameEvents(t, calls[0], h1) || !sameEvents(t, calls[1], h2) {
		t.Fatalf("shared recording = %v, want both calls", calls)
	}
	if calls := cloned.Calls(); len(calls) != 2 {
		t.Fatalf("clone recording has %d entries, want the same shared 2", len(calls))
	}
}

// failingClone wraps a TestConfig but refuses to clone, to prove Clone errors
// propagate through TrackingConfig.
type failingClone struct{ *TestConfig }

func (failingClone) Clone(map[string]any) (config.ModelConfig, error) {
	return nil, errors.New("clone refused")
}

func TestTrackingConfigDelegatesErrors(t *testing.T) {
	if _, err := NewTrackingConfig(nil); err == nil {
		t.Fatal("nil inner accepted, want error")
	}

	tc, err := NewTrackingConfig(failingClone{NewTestConfig(&events.ModelResponse{})})
	if err != nil {
		t.Fatalf("NewTrackingConfig: %v", err)
	}
	if _, err := tc.Clone(nil); err == nil || err.Error() != "clone refused" {
		t.Fatalf("Clone error = %v, want the inner clone error", err)
	}
}

// probeEvent is not in the marshal registry: snapshots must keep the original
// event instead of degrading it to UnknownEvent.
type probeEvent struct {
	events.Base
	Note string
}

func (probeEvent) EventType() string    { return "ProbeEvent" }
func (probeEvent) Transient() bool      { return false }
func (probeEvent) Conversational() bool { return true }

func TestSnapshotKeepsUnregisteredEvents(t *testing.T) {
	cfg := NewTestConfig(&events.ModelResponse{})
	h := []events.Event{probeEvent{Note: "keep"}}
	if _, err := cfg.Call(ctx(), h, nil, config.CallOptions{}); err != nil {
		t.Fatalf("call: %v", err)
	}
	calls := cfg.Calls()
	if _, ok := calls[0][0].(probeEvent); !ok {
		t.Fatalf("recorded event type = %T, want the original probeEvent", calls[0][0])
	}
	if got := calls[0][0].(probeEvent).Note; got != "keep" {
		t.Fatalf("recorded note = %q, want %q", got, "keep")
	}
}

func TestTestConfigConcurrentCalls(t *testing.T) {
	script := make([]*events.ModelResponse, 20)
	for i := range script {
		script[i] = &events.ModelResponse{Message: &events.ModelMessage{Content: fmt.Sprintf("r%d", i)}}
	}
	cfg := NewTestConfig(script...)

	var wg sync.WaitGroup
	errs := make([]error, len(script))
	for i := range script {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = cfg.Call(ctx(), hist(fmt.Sprintf("call %d", i)), nil, config.CallOptions{})
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if got := len(cfg.Calls()); got != len(script) {
		t.Fatalf("recorded %d calls, want %d", got, len(script))
	}
	if _, err := cfg.Call(ctx(), nil, nil, config.CallOptions{}); !errors.Is(err, ErrScriptedExhausted) {
		t.Fatalf("post-concurrency error = %v, want ErrScriptedExhausted", err)
	}
}

// stubConfig hands out one fixed client: the minimal ModelConfig a tracking
// scenario needs (Python parity: tests build a tiny provider stub directly).
type stubConfig struct {
	cl       config.LLMClient
	provider config.Provider
	model    string
}

func (s *stubConfig) Provider() config.Provider         { return s.provider }
func (s *stubConfig) Model() string                     { return s.model }
func (s *stubConfig) Create() (config.LLMClient, error) { return s.cl, nil }

func (s *stubConfig) Clone(overrides map[string]any) (config.ModelConfig, error) {
	if len(overrides) > 0 {
		return nil, fmt.Errorf("stubConfig: no overrides supported, got %v", overrides)
	}
	return &stubConfig{cl: s.cl, provider: s.provider, model: s.model}, nil
}

// scriptedStub fails the first `failures` calls with err, then returns resp
// (or keeps failing when resp is nil) — the retry-then-success and
// final-failure shapes of the upstream snapshot tests without pulling in the
// agent or middleware packages.
type scriptedStub struct {
	mu       sync.Mutex
	failures int
	err      error
	resp     *events.ModelResponse
	calls    int
}

func (s *scriptedStub) Call(_ context.Context, _ []events.Event, _ *agocontext.Context, _ config.CallOptions) (*events.ModelResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if s.calls <= s.failures {
		return nil, s.err
	}
	return s.resp, nil
}

// TestTrackingConfigRecordsFailedCalls pins the upstream fix the port already
// implements: TrackingClient records BEFORE delegating, so every attempt —
// including failed ones — is in Calls(), even when the caller never inspects
// the recording until after the final call.
func TestTrackingConfigRecordsFailedCalls(t *testing.T) {
	boom := errors.New("provider unavailable")
	scenarios := []struct {
		name     string
		failures int
		resp     *events.ModelResponse
		wantErr  bool
	}{
		{"failed attempt then success", 1, &events.ModelResponse{Message: &events.ModelMessage{Content: "ok"}}, false},
		{"every attempt fails", 2, nil, true},
	}
	for _, tt := range scenarios {
		t.Run(tt.name, func(t *testing.T) {
			inner := &stubConfig{
				cl:       &scriptedStub{failures: tt.failures, err: boom, resp: tt.resp},
				provider: ProviderTest,
				model:    "stub",
			}
			tc, err := NewTrackingConfig(inner)
			if err != nil {
				t.Fatalf("NewTrackingConfig: %v", err)
			}
			cl, err := tc.Create()
			if err != nil {
				t.Fatalf("Create: %v", err)
			}

			h1 := hist("first")
			if _, err := cl.Call(ctx(), h1, nil, config.CallOptions{}); !errors.Is(err, boom) {
				t.Fatalf("call 1 error = %v, want the stub failure", err)
			}
			// The failed attempt is already recorded before the second call:
			// recording happens at delegation time, not on success.
			calls := tc.Calls()
			if len(calls) != 1 || !sameEvents(t, calls[0], h1) {
				t.Fatalf("after the failed call, Calls() = %d entries, want the handed history", len(calls))
			}

			h2 := hist("second")
			_, err = cl.Call(ctx(), h2, nil, config.CallOptions{})
			if tt.wantErr && !errors.Is(err, boom) {
				t.Fatalf("call 2 error = %v, want the stub failure", err)
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("call 2: %v", err)
			}

			calls = tc.Calls()
			if len(calls) != 2 {
				t.Fatalf("Calls() has %d entries, want both attempts", len(calls))
			}
			if !sameEvents(t, calls[0], h1) || !sameEvents(t, calls[1], h2) {
				t.Fatalf("recorded histories do not match the handed ones")
			}
		})
	}
}

// TestTrackingConfigSnapshotsAreIsolated mirrors TestTestConfigSnapshotsAreIsolated
// on the TrackingConfig path: the recording is a deep copy taken at call time,
// so later mutation of the handed history — and of anything a caller does with
// the returned snapshot — cannot rewrite it.
func TestTrackingConfigSnapshotsAreIsolated(t *testing.T) {
	inner := &stubConfig{
		cl:       &scriptedStub{resp: &events.ModelResponse{}},
		provider: ProviderTest,
		model:    "stub",
	}
	tc, err := NewTrackingConfig(inner)
	if err != nil {
		t.Fatalf("NewTrackingConfig: %v", err)
	}
	cl, err := tc.Create()
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	text := &events.TextInput{Content: "hello"}
	data := &events.DataInput{Data: map[string]any{"nested": "value"}}
	req := &events.ModelRequest{
		Parts:    []events.Input{text, data},
		Metadata: map[string]any{"k": "v"},
		Origin:   events.OriginUser,
	}
	h := []events.Event{req}
	if _, err := cl.Call(ctx(), h, nil, config.CallOptions{}); err != nil {
		t.Fatalf("call: %v", err)
	}

	// Mutate everything reachable after the call.
	text.Content = "mutated"
	data.Data.(map[string]any)["nested"] = "rewritten"
	req.Metadata["k"] = "changed"
	req.Metadata = map[string]any{"added": true}
	h[0] = &events.ModelRequest{Origin: events.OriginService}

	calls := tc.Calls()
	if len(calls) != 1 {
		t.Fatalf("Calls() has %d entries, want 1", len(calls))
	}
	got, ok := calls[0][0].(*events.ModelRequest)
	if !ok {
		t.Fatalf("recorded event = %T, want *events.ModelRequest", calls[0][0])
	}
	if got.Origin != events.OriginUser {
		t.Fatalf("recorded origin = %q, want the call-time %q", got.Origin, events.OriginUser)
	}
	if got.Metadata["k"] != "v" || got.Metadata["added"] != nil {
		t.Fatalf("recorded metadata = %v, want the call-time value", got.Metadata)
	}
	if len(got.Parts) != 2 {
		t.Fatalf("recorded parts = %d, want 2", len(got.Parts))
	}
	if text0, ok := got.Parts[0].(*events.TextInput); !ok || text0.Content != "hello" {
		t.Fatalf("recorded text part mutated: %+v", got.Parts[0])
	}
	if nested := got.Parts[1].(*events.DataInput).Data.(map[string]any)["nested"]; nested != "value" {
		t.Fatalf("recorded nested value mutated: %v", nested)
	}

	// Mutating a returned snapshot must not corrupt the recording either.
	got.Parts[0].(*events.TextInput).Content = "caller-mutated"
	got.Metadata["k"] = "caller-changed"
	again := tc.Calls()
	got2 := again[0][0].(*events.ModelRequest)
	if got2.Parts[0].(*events.TextInput).Content != "hello" || got2.Metadata["k"] != "v" {
		t.Fatalf("recording changed after caller mutation: %+v", got2)
	}
}
