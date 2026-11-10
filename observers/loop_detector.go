package observers

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
)

// previewLimit bounds the response excerpt quoted in a loop alert.
const previewLimit = 120

// defaultLoopWindow is the Python window_size default for repeated tool calls.
const defaultLoopWindow = 10

// loopRun is one origin's run of identical responses. Separating local and
// delegated histories keeps sibling delegations from combining into a false
// loop; the key is the delegation task id, or "local" for this agent's model.
type loopRun struct {
	last  string
	count int
}

// LoopDetector alerts on two shapes of repetition:
//
//   - the same tool called with the same arguments repeats times inside a
//     sliding window of windowSize calls, per origin (Python parity:
//     ag2/observers/loop_detector.py);
//   - the model repeats an identical normalized response content repeats times
//     in a row (the AGo contract's original signal).
//
// Both fire one ObserverAlert (severity warning) per origin and key; a later run
// of the same key does not fire again until Reset.
//
// It watches local events and ChildEvent observations of a descendant, so a
// subtree's loop is visible at the root without letting sibling histories
// pollute each other.
//
// AGO-DEVIATION: Python watches only tool calls; AGo keeps the model-text
// detection added by the original port (D-025) in addition to tool-call parity.
type LoopDetector struct {
	*BaseObserver

	repeats    int
	windowSize int

	mu      sync.Mutex          // guards every map below
	runs    map[string]*loopRun // origin -> current identical response run
	text    map[string]bool     // origin + content already alerted
	history map[string][]string // origin -> recent tool-call keys
	flagged map[string]bool     // origin + tool + arguments already alerted
}

// LoopDetectorOption configures NewLoopDetector.
type LoopDetectorOption func(*loopDetectorOptions)

type loopDetectorOptions struct {
	windowSize int
	name       string
	timeout    time.Duration
}

// WithLoopWindowSize bounds the per-origin tool-call history (Python parity:
// window_size). n <= 0 keeps the default of 10.
func WithLoopWindowSize(n int) LoopDetectorOption {
	return func(o *loopDetectorOptions) {
		if n > 0 {
			o.windowSize = n
		}
	}
}

// WithLoopName sets the observer's display name, used as the alert source
// (Python parity: name=). An empty name keeps "loop_detector".
//
// AGO-DEVIATION: Python's default name is "loop-detector" (hyphen); AGo keeps
// the established "loop_detector" source so existing alert consumers match.
func WithLoopName(name string) LoopDetectorOption {
	return func(o *loopDetectorOptions) {
		if name != "" {
			o.name = name
		}
	}
}

// WithLoopTimeout bounds one process call (Python parity: timeout=). Expiry is
// logged and the batch dropped. d <= 0 waits indefinitely.
func WithLoopTimeout(d time.Duration) LoopDetectorOption {
	return func(o *loopDetectorOptions) {
		if d > 0 {
			o.timeout = d
		}
	}
}

// NewLoopDetector watches ModelResponse and ToolCallEvent events. repeats below
// 2 is clamped to 2: a loop needs at least two identical observations.
func NewLoopDetector(repeats int, opts ...LoopDetectorOption) *LoopDetector {
	if repeats < 2 {
		repeats = 2
	}
	o := loopDetectorOptions{windowSize: defaultLoopWindow, name: "loop_detector"}
	for _, opt := range opts {
		opt(&o)
	}
	d := &LoopDetector{
		repeats:    repeats,
		windowSize: o.windowSize,
		runs:       map[string]*loopRun{},
		text:       map[string]bool{},
		history:    map[string][]string{},
		flagged:    map[string]bool{},
	}
	d.BaseObserver = NewBaseObserver(o.name, d.process, WithObserverCondition(watchedLoopEvent), WithBatchSize(1), WithProcessTimeout(o.timeout))
	return d
}

// Repeats returns the repetition threshold the detector fires at.
func (d *LoopDetector) Repeats() int { return d.repeats }

// WindowSize returns the sliding window bound for tool-call history.
func (d *LoopDetector) WindowSize() int { return d.windowSize }

// watchedLoopEvent matches local model responses, tool calls and forwarded
// observations. AsValue unwraps routed copies; it deliberately leaves ChildEvent
// intact so the detector can see the observation envelope and its provenance.
func watchedLoopEvent(ev events.Event) bool {
	switch events.AsValue(ev).(type) {
	case events.ModelResponse, events.ToolCallEvent, events.ChildEvent, events.TransientChildEvent:
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

// process folds one event into its origin's state and returns the alert to
// emit, if any.
func (d *LoopDetector) process(_ *agocontext.Context, evs []events.Event) (*events.ObserverAlert, error) {
	for _, ev := range evs {
		origin, path := loopOrigin(ev)
		switch e := events.AsValue(events.SourceEvent(ev)).(type) {
		case events.ToolCallEvent:
			if alert, fire := d.observeToolCall(origin, path, e); fire {
				return alert, nil
			}
		case events.ModelResponse:
			if alert, fire := d.observeResponse(origin, path, e.Content()); fire {
				return alert, nil
			}
		}
	}
	return nil, nil
}

// observeResponse folds one model response into its origin's text run.
func (d *LoopDetector) observeResponse(origin string, path []string, content string) (*events.ObserverAlert, bool) {
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
	if run.count < d.repeats || d.text[flag] {
		return nil, false
	}
	d.text[flag] = true
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

// observeToolCall folds one tool call into its origin's sliding window and
// alerts when the last repeats entries are all the same call (Python parity:
// LoopDetector.process).
func (d *LoopDetector) observeToolCall(origin string, path []string, call events.ToolCallEvent) (*events.ObserverAlert, bool) {
	key := call.Name + "\x00" + call.Arguments
	d.mu.Lock()
	defer d.mu.Unlock()
	history := append(d.history[origin], key)
	if len(history) > d.windowSize {
		history = history[len(history)-d.windowSize:]
	}
	d.history[origin] = history
	if len(history) < d.repeats {
		return nil, false
	}
	for _, k := range history[len(history)-d.repeats:] {
		if k != key {
			return nil, false
		}
	}
	flag := origin + "\x00" + key
	if d.flagged[flag] {
		return nil, false
	}
	d.flagged[flag] = true
	alert := &events.ObserverAlert{
		Source:   d.Name(),
		Severity: events.SeverityWarning,
		Message: fmt.Sprintf("Potential loop detected: tool '%s' called %d times consecutively with identical arguments. Consider a different approach.",
			call.Name, d.repeats),
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
	d.text = map[string]bool{}
	d.history = map[string][]string{}
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
