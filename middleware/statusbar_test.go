package middleware_test

import (
	stdcontext "context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/luckystrker/ago/agent"
	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/middleware"
	"github.com/luckystrker/ago/stream"
	agotest "github.com/luckystrker/ago/testing"
	"github.com/luckystrker/ago/tools"
)

// This file lives in the external test package because the statusbar tests run
// whole agent turns, and agent imports middleware.

func textResponse(s string) *events.ModelResponse {
	return &events.ModelResponse{Message: &events.ModelMessage{Content: s}}
}

func toolCallResponse(name, argsJSON string) *events.ModelResponse {
	call := events.NewToolCallEvent(name)
	call.Arguments = argsJSON
	return &events.ModelResponse{ToolCalls: events.ToolCallsEvent{Calls: []events.ToolCallEvent{call}}}
}

// statusRequests extracts the detached service views a scripted client saw.
func statusRequests(t *testing.T, evs []events.Event) []*events.ModelRequest {
	t.Helper()
	var views []*events.ModelRequest
	for _, ev := range evs {
		req, ok := ev.(*events.ModelRequest)
		if !ok {
			continue
		}
		if req.Origin == events.OriginService {
			views = append(views, req)
		}
	}
	return views
}

func viewText(t *testing.T, req *events.ModelRequest) string {
	t.Helper()
	if len(req.Parts) != 1 {
		t.Fatalf("view has %d parts, want 1", len(req.Parts))
	}
	text, ok := events.AsValue(req.Parts[0]).(events.TextInput)
	if !ok {
		t.Fatalf("view part = %T, want *TextInput", req.Parts[0])
	}
	return text.Content
}

// TestStatusbarDynamicStatusFollowsToolRound runs a tool round through a whole
// agent: the status is recomputed on every model call — the second call sees the
// post-round value — and the parts never enter the transcript.
func TestStatusbarDynamicStatusFollowsToolRound(t *testing.T) {
	tool, err := tools.NewFunctionTool("note", "Note something", func(args struct {
		Text string `json:"text" agschema:"description=The text,required"`
	}) (string, error) {
		return "noted " + args.Text, nil
	})
	if err != nil {
		t.Fatalf("tool: %v", err)
	}
	cfg := agotest.NewTestConfig(toolCallResponse("note", `{"text":"x"}`), textResponse("done"))
	sb := middleware.Statusbar(func(ctx *agocontext.Context) ([]events.Input, error) {
		hist, err := ctx.History()
		if err != nil {
			return nil, err
		}
		rounds := 0
		for _, ev := range hist {
			if _, ok := events.AsValue(ev).(events.ToolResultsEvent); ok {
				rounds++
			}
		}
		return []events.Input{&events.TextInput{Content: strings.Repeat("r", rounds+1)}}, nil
	})
	a := agent.New("assistant", "", agent.WithConfig(cfg), agent.WithTools(tool), agent.WithMiddleware(sb))
	reply, err := a.Ask(stdcontext.Background(), "go")
	if err != nil {
		t.Fatalf("ask: %v", err)
	}

	calls := cfg.Calls()
	if len(calls) != 2 {
		t.Fatalf("recorded %d calls, want 2 (tool round + final)", len(calls))
	}
	views := statusRequests(t, calls[0])
	if len(views) != 1 {
		t.Fatalf("call 0 carries %d service views, want 1", len(views))
	}
	if got := viewText(t, views[0]); got != "r" {
		t.Fatalf("call 0 status = %q, want %q (no tool results yet)", got, "r")
	}
	views = statusRequests(t, calls[1])
	if len(views) != 1 {
		t.Fatalf("call 1 carries %d service views, want 1", len(views))
	}
	if got := viewText(t, views[0]); got != "rr" {
		t.Fatalf("call 1 status = %q, want %q (one tool round happened)", got, "rr")
	}

	// The status never became a turn of the conversation: no service request and
	// no status text among the durable events.
	hist, err := reply.History().GetEvents()
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if views := statusRequests(t, hist); len(views) != 0 {
		t.Fatalf("transcript holds %d service requests, want 0", len(views))
	}
	for _, ev := range hist {
		if req, ok := events.AsValue(ev).(events.ModelRequest); ok {
			for _, p := range req.Parts {
				if text, ok := events.AsValue(p).(events.TextInput); ok && (text.Content == "r" || text.Content == "rr") {
					t.Fatalf("status text %q entered the transcript as a request part", text.Content)
				}
			}
		}
	}
}

// TestStatusbarDIRecomputedPerCall pins the DI contract: the callback reads the
// context on every call, so a changed dependency changes the very next view.
func TestStatusbarDIRecomputedPerCall(t *testing.T) {
	cx := agocontext.New(stream.NewMemoryStream())
	cx.SetDependency("greeting", "one")

	var saw []string
	next := func(evs []events.Event, _ *agocontext.Context) (*events.ModelResponse, error) {
		views := statusRequests(t, evs)
		if len(views) != 1 {
			t.Fatalf("call carries %d views, want 1", len(views))
		}
		saw = append(saw, viewText(t, views[0]))
		return textResponse("ok"), nil
	}
	mw := middleware.Statusbar(func(ctx *agocontext.Context) ([]events.Input, error) {
		v, _ := ctx.Dependency("greeting")
		s, _ := v.(string)
		return []events.Input{&events.TextInput{Content: s}}, nil
	})()
	for _, want := range []string{"one", "two"} {
		if _, err := mw.OnLLMCall(next, nil, cx); err != nil {
			t.Fatalf("OnLLMCall: %v", err)
		}
		if got := saw[len(saw)-1]; got != want {
			t.Fatalf("status = %q, want %q", got, want)
		}
		cx.SetDependency("greeting", "two")
	}
	if len(saw) != 2 || saw[0] != "one" || saw[1] != "two" {
		t.Fatalf("statuses per call = %v, want [one two]", saw)
	}
}

// TestStatusbarCallbackErrorStopsCall pins the failure contract: a callback
// error stops the pipeline before the model call, so no response is produced
// and nothing — not even the persist observation — is recorded.
func TestStatusbarCallbackErrorStopsCall(t *testing.T) {
	boom := errors.New("status exploded")
	cx := agocontext.New(stream.NewMemoryStream())
	next := func([]events.Event, *agocontext.Context) (*events.ModelResponse, error) {
		t.Fatal("the model call must not happen after a callback error")
		return nil, nil
	}
	mw := middleware.Statusbar(func(*agocontext.Context) ([]events.Input, error) {
		return nil, boom
	}, middleware.WithPersist(true))()
	_, err := mw.OnLLMCall(next, nil, cx)
	if !errors.Is(err, boom) {
		t.Fatalf("error = %v, want it to wrap the callback error", err)
	}
	hist, err2 := cx.History()
	if err2 != nil {
		t.Fatalf("history: %v", err2)
	}
	if len(hist) != 0 {
		t.Fatalf("transcript holds %d events after a callback error, want 0", len(hist))
	}
}

// TestStatusbarPersistAccumulatesAcrossAsks runs two asks over one conversation:
// the second ask skips the callback, yet its model call still sees the last
// committed status, projected from the persisted observation.
func TestStatusbarPersistAccumulatesAcrossAsks(t *testing.T) {
	cfg := agotest.NewTestConfig(textResponse("a"), textResponse("b"))
	ticks := 0
	sb := middleware.Statusbar(func(*agocontext.Context) ([]events.Input, error) {
		ticks++
		if ticks > 1 {
			return nil, nil // skip: no fresh status on the second ask
		}
		return []events.Input{&events.TextInput{Content: "tick=1"}}, nil
	}, middleware.WithPersist(true))
	a := agent.New("assistant", "", agent.WithConfig(cfg), agent.WithMiddleware(sb))

	r1, err := a.Ask(stdcontext.Background(), "q1")
	if err != nil {
		t.Fatalf("ask 1: %v", err)
	}
	r2, err := r1.Ask(stdcontext.Background(), "q2")
	if err != nil {
		t.Fatalf("ask 2: %v", err)
	}

	calls := cfg.Calls()
	if len(calls) != 2 {
		t.Fatalf("recorded %d calls, want 2", len(calls))
	}
	views := statusRequests(t, calls[0])
	if len(views) != 1 || viewText(t, views[0]) != "tick=1" {
		t.Fatalf("ask 1 views = %v, want one view tick=1", views)
	}
	// The second ask skipped the callback but still rides the committed status.
	views = statusRequests(t, calls[1])
	if len(views) != 1 || viewText(t, views[0]) != "tick=1" {
		t.Fatalf("ask 2 views = %v, want the retained tick=1", views)
	}

	// Exactly one durable observation backs the retained status.
	hist, err := r2.History().GetEvents()
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	n := 0
	for _, ev := range hist {
		if _, ok := events.AsValue(ev).(events.StatusObservation); ok {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("transcript holds %d observations, want 1", n)
	}
}

// TestStatusbarSkipKeepsPreviousObservation pins the retention rule on the unit
// level: a skipping callback must not wipe earlier observations — the agent's
// projected input still carries their service view.
func TestStatusbarSkipKeepsPreviousObservation(t *testing.T) {
	ms := stream.NewMemoryStream()
	obs := &events.StatusObservation{Parts: []events.Input{&events.TextInput{Content: "held"}}, Source: "earlier"}
	if err := ms.Send(obs); err != nil {
		t.Fatalf("seed observation: %v", err)
	}
	cx := agocontext.New(ms)
	seed, err := cx.History()
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	evs := events.ModelInputs(seed)

	next := func(got []events.Event, _ *agocontext.Context) (*events.ModelResponse, error) {
		views := statusRequests(t, got)
		if len(views) != 1 || viewText(t, views[0]) != "held" {
			t.Fatalf("views after skip = %v, want the retained held", views)
		}
		return textResponse("ok"), nil
	}
	mw := middleware.Statusbar(func(*agocontext.Context) ([]events.Input, error) {
		return nil, nil // skip
	}, middleware.WithPersist(true))()
	if _, err := mw.OnLLMCall(next, evs, cx); err != nil {
		t.Fatalf("OnLLMCall: %v", err)
	}
}

// TestStatusbarObservationRoundTripsStorage marshals a published observation
// through the wire (what a real Storage persists) and asserts parts, source and
// created_at survive.
func TestStatusbarObservationRoundTripsStorage(t *testing.T) {
	at := time.Now()
	obs := &events.StatusObservation{
		Base:   events.Base{CreatedAt: at},
		Parts:  []events.Input{&events.TextInput{Content: "s"}, &events.DataInput{Data: map[string]any{"k": "v"}}},
		Source: "github.com/luckystrker/ago/middleware.TestStatusbarObservationRoundTripsStorage.func1",
	}
	b, err := events.MarshalEvent(obs)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	ev, err := events.UnmarshalEvent(b)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got, ok := ev.(*events.StatusObservation)
	if !ok {
		t.Fatalf("round trip = %T, want *events.StatusObservation", ev)
	}
	if got.Source != obs.Source {
		t.Fatalf("source = %q, want %q", got.Source, obs.Source)
	}
	if !got.At().Equal(at) {
		t.Fatalf("created_at = %v, want %v", got.At(), at)
	}
	if len(got.Parts) != len(obs.Parts) {
		t.Fatalf("parts = %d, want %d", len(got.Parts), len(obs.Parts))
	}
	for i := range got.Parts {
		want, err := events.MarshalEvent(obs.Parts[i])
		if err != nil {
			t.Fatalf("marshal part %d: %v", i, err)
		}
		have, err := events.MarshalEvent(got.Parts[i])
		if err != nil {
			t.Fatalf("marshal copied part %d: %v", i, err)
		}
		if string(want) != string(have) {
			t.Fatalf("part %d wire form changed: %s vs %s", i, want, have)
		}
	}
}

// TestStatusbarSnapshotIsolatedFromSourceMutation pins detachment: mutating the
// callback's containers after the call must not rewrite either the published
// observation or the view the model saw.
func TestStatusbarSnapshotIsolatedFromSourceMutation(t *testing.T) {
	ms := stream.NewMemoryStream()
	cx := agocontext.New(ms)
	text := &events.TextInput{Content: "before"}
	parts := []events.Input{text}

	var sawView string
	next := func(evs []events.Event, _ *agocontext.Context) (*events.ModelResponse, error) {
		views := statusRequests(t, evs)
		if len(views) != 1 {
			t.Fatalf("call carries %d views, want 1", len(views))
		}
		sawView = viewText(t, views[0])
		return textResponse("ok"), nil
	}
	mw := middleware.Statusbar(func(*agocontext.Context) ([]events.Input, error) {
		return parts, nil
	}, middleware.WithPersist(true))()
	if _, err := mw.OnLLMCall(next, nil, cx); err != nil {
		t.Fatalf("OnLLMCall: %v", err)
	}

	// Mutate every source container after the call.
	text.Content = "after"
	parts[0] = &events.TextInput{Content: "replaced"}

	if sawView != "before" {
		t.Fatalf("view the model saw = %q, want the call-time value before", sawView)
	}
	hist, err := cx.History()
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	var published string
	for _, ev := range hist {
		if o, ok := events.AsValue(ev).(events.StatusObservation); ok {
			text, ok := events.AsValue(o.Parts[0]).(events.TextInput)
			if !ok {
				t.Fatalf("published part = %T, want TextInput", o.Parts[0])
			}
			published = text.Content
		}
	}
	if published != "before" {
		t.Fatalf("published observation = %q, want the call-time value before", published)
	}
}

// statusRejectingStorage fails the transcript write when a batch carries a
// StatusObservation. History writes through SetHistory (GetHistory+SetHistory
// pairs); SaveEvent mirrors the policy for storages built on per-event writes.
type statusRejectingStorage struct {
	inner stream.Storage
	err   error
}

func (s *statusRejectingStorage) reject(evs []events.Event) error {
	for _, ev := range evs {
		if _, ok := events.AsValue(ev).(events.StatusObservation); ok {
			return s.err
		}
	}
	return nil
}

func (s *statusRejectingStorage) SaveEvent(id string, ev events.Event) error {
	if err := s.reject([]events.Event{ev}); err != nil {
		return err
	}
	return s.inner.SaveEvent(id, ev)
}

func (s *statusRejectingStorage) GetHistory(id string) ([]events.Event, error) {
	return s.inner.GetHistory(id)
}

func (s *statusRejectingStorage) SetHistory(id string, evs []events.Event) error {
	if err := s.reject(evs); err != nil {
		return err
	}
	return s.inner.SetHistory(id, evs)
}

func (s *statusRejectingStorage) DropHistory(id string) error { return s.inner.DropHistory(id) }

// TestStatusbarStorageFailureStopsModelCall pins the publish-before-act order:
// a storage refusal of the observation aborts the pipeline before the model
// call, so the turn produces nothing rather than an unrecorded response.
func TestStatusbarStorageFailureStopsModelCall(t *testing.T) {
	refused := errors.New("storage refuses status observations")
	ms := stream.NewMemoryStream(stream.WithStorage(&statusRejectingStorage{inner: stream.NewMemoryStorage(), err: refused}))
	cx := agocontext.New(ms)
	next := func([]events.Event, *agocontext.Context) (*events.ModelResponse, error) {
		t.Fatal("the model call must not happen when persistence fails")
		return nil, nil
	}
	mw := middleware.Statusbar(func(*agocontext.Context) ([]events.Input, error) {
		return []events.Input{&events.TextInput{Content: "s"}}, nil
	}, middleware.WithPersist(true))()
	_, err := mw.OnLLMCall(next, nil, cx)
	if !errors.Is(err, refused) {
		t.Fatalf("error = %v, want it to wrap the storage error", err)
	}
	hist, err2 := cx.History()
	if err2 != nil {
		t.Fatalf("history: %v", err2)
	}
	if len(hist) != 0 {
		t.Fatalf("transcript holds %d events, want 0", len(hist))
	}
}

// TestStatusbarProviderFailureKeepsObservation pins the commit semantics: the
// observation is durable before the pipeline continues, so a provider failure
// leaves the already-committed status in the transcript.
func TestStatusbarProviderFailureKeepsObservation(t *testing.T) {
	boom := errors.New("provider unavailable")
	ms := stream.NewMemoryStream()
	cx := agocontext.New(ms)
	next := func([]events.Event, *agocontext.Context) (*events.ModelResponse, error) {
		return nil, boom
	}
	mw := middleware.Statusbar(func(*agocontext.Context) ([]events.Input, error) {
		return []events.Input{&events.TextInput{Content: "s"}}, nil
	}, middleware.WithPersist(true))()
	if _, err := mw.OnLLMCall(next, nil, cx); !errors.Is(err, boom) {
		t.Fatalf("error = %v, want the provider failure", err)
	}
	hist, err := cx.History()
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	var obs *events.StatusObservation
	for _, ev := range hist {
		if o, ok := events.AsValue(ev).(events.StatusObservation); ok {
			obs = &o
		}
	}
	if obs == nil {
		t.Fatal("the committed observation is missing from the transcript after a provider failure")
	}
	if got := events.AsValue(obs.Parts[0]).(events.TextInput).Content; got != "s" {
		t.Fatalf("kept observation = %q, want s", got)
	}
}

// TestStatusbarOutsideRetryOneSnapshotPerLogicalCall pins the registration
// contract: Statusbar outside Retry computes and publishes once per logical
// call, whatever the attempt count.
func TestStatusbarOutsideRetryOneSnapshotPerLogicalCall(t *testing.T) {
	boom := errors.New("flaky provider")
	ms := stream.NewMemoryStream()
	cx := agocontext.New(ms)
	attempts := 0
	next := func([]events.Event, *agocontext.Context) (*events.ModelResponse, error) {
		attempts++
		if attempts < 3 {
			return nil, boom
		}
		return textResponse("ok"), nil
	}
	computed := 0
	chain := middleware.Chain(
		middleware.Statusbar(func(*agocontext.Context) ([]events.Input, error) {
			computed++
			return []events.Input{&events.TextInput{Content: "s"}}, nil
		}, middleware.WithPersist(true)),
		middleware.Retry(3),
	)
	if _, err := chain().OnLLMCall(next, nil, cx); err != nil {
		t.Fatalf("chained call: %v", err)
	}
	if computed != 1 {
		t.Fatalf("callback ran %d times over 3 attempts, want 1", computed)
	}
	hist, err := cx.History()
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	n := 0
	for _, ev := range hist {
		if _, ok := events.AsValue(ev).(events.StatusObservation); ok {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("published %d observations over 3 attempts, want 1", n)
	}
}

// TestStatusbarNilCallbackPassesThrough pins the degenerate configuration.
func TestStatusbarNilCallbackPassesThrough(t *testing.T) {
	cx := agocontext.New(stream.NewMemoryStream())
	ran := false
	next := func(evs []events.Event, _ *agocontext.Context) (*events.ModelResponse, error) {
		ran = true
		if len(evs) != 1 {
			t.Fatalf("events = %d, want the untouched input", len(evs))
		}
		return textResponse("ok"), nil
	}
	mw := middleware.Statusbar(nil)()
	in := []events.Event{&events.ModelRequest{Origin: events.OriginUser}}
	if _, err := mw.OnLLMCall(next, in, cx); err != nil {
		t.Fatalf("OnLLMCall: %v", err)
	}
	if !ran {
		t.Fatal("nil callback must pass through to next")
	}
}
