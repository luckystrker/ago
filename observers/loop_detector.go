package observers

import (
	"fmt"
	"strconv"
	"strings"
	"sync"

	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/stream"
)

// previewLimit bounds the response excerpt quoted in a loop alert.
const previewLimit = 120

// loopRun is one origin's run of identical responses. Separating local and
// delegated histories keeps sibling delegations from combining into a false
// loop; the key is the delegation task id, or "local" for this agent's model.
type loopRun struct {
	last  string
	count int
}

// LoopDetector alerts when the model repeats itself: identical normalized
// response content arriving repeats times in a row produces one ObserverAlert
// (severity warning). The alert is deduplicated per origin and content — a later
// run of the same text does not fire again until Reset.
//
// It watches both local ModelResponse events and ChildEvent observations of a
// descendant's response, so a subtree's loop is visible at the root without
// letting sibling histories pollute each other.
//
// AGO-DEVIATION: the Python LoopDetector watches ToolCallEvent (name, arguments)
// pairs over a bounded window; the AGo contract pins ModelResponse text content.
type LoopDetector struct {
	mu      sync.Mutex
	repeats int
	runs    map[string]*loopRun // origin -> current identical run
	flagged map[string]bool     // origin + content already alerted
}

// NewLoopDetector watches ModelResponse events. repeats below 2 is clamped to 2:
// a loop needs at least two identical responses.
func NewLoopDetector(repeats int) *LoopDetector {
	if repeats < 2 {
		repeats = 2
	}
	return &LoopDetector{repeats: repeats, runs: map[string]*loopRun{}, flagged: map[string]bool{}}
}

// Name returns "loop_detector" — the alert source, fixed by the package contract.
func (d *LoopDetector) Name() string { return "loop_detector" }

// Repeats returns the repetition threshold the detector fires at.
func (d *LoopDetector) Repeats() int { return d.repeats }

// Register subscribes the detector on the conversation stream. Alerts are sent
// on the same stream; a send failure is reported to the dispatcher like any
// subscriber error.
func (d *LoopDetector) Register(ctx *agocontext.Context) error {
	if err := validateTarget(ctx); err != nil {
		return err
	}
	ctx.Stream.Subscribe(func(ev events.Event) error {
		if !watchedLoopEvent(ev) {
			return nil
		}
		origin, path := loopOrigin(ev)
		r, ok := events.AsValue(events.SourceEvent(ev)).(events.ModelResponse)
		if !ok {
			return nil
		}
		alert, fire := d.observe(origin, path, r.Content())
		if !fire {
			return nil
		}
		return ctx.Send(alert)
	}, stream.SubscribeOptions{Condition: watchedLoopEvent})
	return nil
}

// watchedLoopEvent matches local model responses and forwarded observations.
// AsValue unwraps routed copies; it deliberately leaves ChildEvent intact so
// the detector can see the observation envelope and its provenance.
func watchedLoopEvent(ev events.Event) bool {
	switch events.AsValue(ev).(type) {
	case events.ModelResponse, events.ChildEvent, events.TransientChildEvent:
		return true
	}
	return false
}

// loopOrigin names the authoring delegation and its path. A local event has no
// envelope; a ChildEvent carries the leaf's identity through every hop.
func loopOrigin(ev events.Event) (string, []string) {
	switch e := events.AsValue(ev).(type) {
	case events.ChildEvent:
		return e.TaskID, e.Path
	case events.TransientChildEvent:
		return e.TaskID, e.Path
	}
	return "local", nil
}

// observe folds one response into its origin's run state and returns the alert
// to emit, if any.
func (d *LoopDetector) observe(origin string, path []string, content string) (*events.ObserverAlert, bool) {
	key := normalizeText(content)
	if key == "" {
		// Empty text is not a loop signal: tool-call rounds legitimately
		// produce contentless ModelResponses back to back.
		return nil, false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	run := d.runs[origin]
	if run == nil {
		run = &loopRun{}
		d.runs[origin] = run
	}
	if key == run.last {
		run.count++
	} else {
		run.last, run.count = key, 1
	}
	flag := origin + "\x00" + key
	if run.count < d.repeats || d.flagged[flag] {
		return nil, false
	}
	d.flagged[flag] = true
	alert := &events.ObserverAlert{
		Source:   d.Name(),
		Severity: events.SeverityWarning,
		Message: fmt.Sprintf("Potential loop detected: identical model response repeated %d times in a row (excerpt: %s). Consider a different approach.",
			d.repeats, strconv.Quote(preview(key))),
	}
	if origin != "local" {
		alert.Data = map[string]any{"task_id": origin, "path": append([]string(nil), path...)}
	}
	return alert, true
}

// Reset clears the run state and the dedup memory for a fresh session.
func (d *LoopDetector) Reset() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.runs = map[string]*loopRun{}
	d.flagged = map[string]bool{}
}

// normalizeText collapses whitespace runs so responses differing only in
// spacing or line breaks count as identical.
func normalizeText(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// preview truncates s for inclusion in an alert message, without splitting a rune.
func preview(s string) string {
	r := []rune(s)
	if len(r) > previewLimit {
		return string(r[:previewLimit]) + "…"
	}
	return s
}
