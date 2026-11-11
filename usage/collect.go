package usage

import (
	"sync"

	"github.com/luckystrker/ago/events"
)

// Collector gathers source accounting records live from a stream subscription
// (Python parity: usage.collect_usage_events). Attach it as a normal subscriber:
//
//	c := usage.NewCollector()
//	sub := s.Subscribe(c.Collect, stream.SubscribeOptions{})
//	defer s.Unsubscribe(sub)
//
// Every source UsageEvent is kept in arrival order, including one wrapped in an
// archived observation envelope; non-usage events are ignored. Deduplication is
// not the collector's job (Python appends the records as they arrive): Report
// counts each source event id once.
//
// Safe for concurrent use: a stream may dispatch from several goroutines.
type Collector struct {
	mu      sync.Mutex
	records []events.UsageEvent
}

// NewCollector returns an empty collector.
func NewCollector() *Collector { return &Collector{} }

// Collect is the subscriber hook; its signature matches stream.Subscriber
// without importing the stream package.
func (c *Collector) Collect(ev events.Event) error {
	u, ok := EventFrom(ev)
	if !ok {
		return nil
	}
	c.mu.Lock()
	c.records = append(c.records, u)
	c.mu.Unlock()
	return nil
}

// Records returns a snapshot of the collected source records in arrival order.
func (c *Collector) Records() []events.UsageEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]events.UsageEvent, len(c.records))
	copy(out, c.records)
	return out
}

// Report aggregates the collected records, deduplicating by source event id and
// skipping records that report no tokens (the same rules as FromEvents).
func (c *Collector) Report() *Report {
	c.mu.Lock()
	evs := make([]events.Event, len(c.records))
	for i := range c.records {
		evs[i] = &c.records[i]
	}
	c.mu.Unlock()
	return FromEvents(evs)
}
