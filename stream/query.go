package stream

import (
	"github.com/luckystrker/ago/events"
)

// HistoryQuery is an immutable pending query over a History. Construction is
// lazy: Where and SinceLastUserInput never read the storage; every terminal
// (All/First/Last/Count) takes a fresh snapshot via History.GetEvents, so a
// terminal always sees the transcript as of its own call. Combinators return
// copies — the receiver is never mutated.
//
// Terminals are synchronous (no context parameter) because the Storage
// interface is synchronous (docs/DECISIONS.md D-004): there is no await to
// cancel, matching the plain Python HistoryQuery methods.
type HistoryQuery struct {
	history   *History
	cond      events.Condition
	sinceUser bool
}

// Where starts a query over the transcript matching c (nil matches everything).
func (h *History) Where(c events.Condition) *HistoryQuery {
	return &HistoryQuery{history: h, cond: c}
}

// Where narrows the query by AND-combining c with the current filter; the
// receiver is unchanged. A nil c matches everything and yields an equivalent
// query.
func (q *HistoryQuery) Where(c events.Condition) *HistoryQuery {
	if c == nil {
		return q
	}
	outer := q.cond
	return &HistoryQuery{
		history: q.history,
		cond: func(ev events.Event) bool {
			return (outer == nil || outer(ev)) && c(ev)
		},
		sinceUser: q.sinceUser,
	}
}

// SinceLastUserInput scopes the query to events published after the last
// user-origin request: a ModelRequest with origin "user" or legacy-empty, or
// a DrainedModelRequest with an explicit origin "user". A drained merge with
// an empty origin is a legacy service-side merge (events/input.go normalizes
// it to "service" on the wire) and never opens an interval. The cut runs
// before any filtering, so a filter cannot hide the boundary; without a
// boundary in the retained transcript the result is empty.
func (q *HistoryQuery) SinceLastUserInput() *HistoryQuery {
	return &HistoryQuery{history: q.history, cond: q.cond, sinceUser: true}
}

// All returns the matching events in publication order. Nested batches stay
// one occurrence: a ToolCallsEvent is never unfolded into its calls.
func (q *HistoryQuery) All() ([]events.Event, error) {
	// Fresh snapshot per terminal: the transcript may have moved on since the
	// query was built (Python parity: get_events() on every terminal).
	evs, err := q.history.GetEvents()
	if err != nil {
		return nil, err
	}
	if q.sinceUser {
		evs = afterLastUserInput(evs)
	}
	out := make([]events.Event, 0, len(evs))
	for _, ev := range evs {
		if q.cond == nil || q.cond(ev) {
			out = append(out, ev)
		}
	}
	return out, nil
}

// First returns the first matching event; ok=false when nothing matches.
func (q *HistoryQuery) First() (events.Event, bool, error) {
	evs, err := q.All()
	if err != nil {
		return nil, false, err
	}
	if len(evs) == 0 {
		return nil, false, nil
	}
	return evs[0], true, nil
}

// Last returns the last matching event; ok=false when nothing matches.
func (q *HistoryQuery) Last() (events.Event, bool, error) {
	evs, err := q.All()
	if err != nil {
		return nil, false, err
	}
	if len(evs) == 0 {
		return nil, false, nil
	}
	return evs[len(evs)-1], true, nil
}

// Count returns the number of matching events.
func (q *HistoryQuery) Count() (int, error) {
	evs, err := q.All()
	if err != nil {
		return 0, err
	}
	return len(evs), nil
}

// afterLastUserInput cuts evs after the last user-origin request; the boundary
// itself is excluded. Go embedding is not inheritance — a DrainedModelRequest
// is its own concrete form — so both request shapes are asserted through
// AsValue (Python isinstance covers the subclass there).
func afterLastUserInput(evs []events.Event) []events.Event {
	for i := len(evs) - 1; i >= 0; i-- {
		if isUserOriginRequest(evs[i]) {
			return evs[i+1:]
		}
	}
	return nil
}

// isUserOriginRequest reports whether ev is a top-level request event whose
// sender is the user. For ModelRequest the empty origin reads as user (legacy
// records, the Python field default). A DrainedModelRequest must carry an
// explicit user origin: an empty drain is a legacy service-side merge —
// events/input.go normalizes it to "service" on the wire — and per the legacy
// rule a drain never opens a user interval by default.
func isUserOriginRequest(ev events.Event) bool {
	switch req := events.AsValue(ev).(type) {
	case events.ModelRequest:
		return req.Origin == events.OriginUser || req.Origin == ""
	case events.DrainedModelRequest:
		return req.Origin == events.OriginUser
	default:
		return false
	}
}
