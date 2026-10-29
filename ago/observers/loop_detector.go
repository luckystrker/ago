package observers

import (
	"fmt"
	"strconv"
	"strings"
	"sync"

	agocontext "ago/context"
	"ago/events"
	"ago/stream"
)

// previewLimit bounds the response excerpt quoted in a loop alert.
const previewLimit = 120

// LoopDetector alerts when the model repeats itself: identical normalized
// response content arriving repeats times in a row produces one ObserverAlert
// (severity warning). The alert is deduplicated per content — a later run of the
// same text does not fire again until Reset.
//
// AGO-DEVIATION: the Python LoopDetector watches ToolCallEvent (name, arguments)
// pairs over a bounded window; the AGo contract pins ModelResponse text content.
type LoopDetector struct {
	mu      sync.Mutex
	repeats int
	last    string // normalized content of the current identical run
	count   int    // length of the current run
	flagged map[string]bool
}

// NewLoopDetector watches ModelResponse events. repeats below 2 is clamped to 2:
// a loop needs at least two identical responses.
func NewLoopDetector(repeats int) *LoopDetector {
	if repeats < 2 {
		repeats = 2
	}
	return &LoopDetector{repeats: repeats, flagged: map[string]bool{}}
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
		r, ok := events.AsValue(ev).(events.ModelResponse)
		if !ok {
			return nil
		}
		alert, fire := d.observe(r.Content())
		if !fire {
			return nil
		}
		return ctx.Send(alert)
	}, stream.SubscribeOptions{Condition: events.TypeOf[events.ModelResponse]()})
	return nil
}

// observe folds one response into the run state and returns the alert to emit,
// if any.
func (d *LoopDetector) observe(content string) (*events.ObserverAlert, bool) {
	key := normalizeText(content)
	if key == "" {
		// Empty text is not a loop signal: tool-call rounds legitimately
		// produce contentless ModelResponses back to back.
		return nil, false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if key == d.last {
		d.count++
	} else {
		d.last, d.count = key, 1
	}
	if d.count < d.repeats || d.flagged[key] {
		return nil, false
	}
	d.flagged[key] = true
	return &events.ObserverAlert{
		Source:   d.Name(),
		Severity: events.SeverityWarning,
		Message: fmt.Sprintf("Potential loop detected: identical model response repeated %d times in a row (excerpt: %s). Consider a different approach.",
			d.repeats, strconv.Quote(preview(key))),
	}, true
}

// Reset clears the run state and the dedup memory for a fresh session.
func (d *LoopDetector) Reset() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.last, d.count = "", 0
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
