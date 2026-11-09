package agent

import (
	"fmt"
	"log/slog"

	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/observers"
)

// startObservers registers the turn's observers on the conversation context and
// brackets each registration with ObserverStarted, so an observer subscribed to
// its own start sees it (Python parity: _observer_lifecycle's register+send
// loop). The names are returned in registration order for the completion pair
// Close publishes.
//
// Registration failures end the turn scope: an observer that cannot attach
// leaves the turn incomplete, matching upstream's observer.register raising
// inside the turn scope.
func (r *AgentRun) startObservers(list []observers.Observer) ([]string, error) {
	names := make([]string, 0, len(list))
	for _, o := range list {
		if o == nil {
			continue
		}
		if err := o.Register(r.cx); err != nil {
			return nil, fmt.Errorf("agent: register observer: %w", err)
		}
		name := o.Name()
		if err := r.cx.Send(&events.ObserverStarted{Name: name}); err != nil {
			return nil, fmt.Errorf("agent: observer started event: %w", err)
		}
		names = append(names, name)
	}
	return names, nil
}

// finishObservers publishes ObserverCompleted for every observer the turn
// registered, before the turn scope tears its subscriptions down, so an observer
// watching its own completion still receives it (Python parity:
// _observer_lifecycle's finally). Publication failures are only logged: a
// finished observer must not change the turn's outcome.
func (r *AgentRun) finishObservers() {
	for _, name := range r.observerNames {
		// A close after a failed scope entry has no turn goroutine to stop; the
		// send is safe either way because the stream outlives the run.
		if err := r.cx.Send(&events.ObserverCompleted{Name: name}); err != nil {
			slog.Warn("agent: observer completed event failed", "observer", name, "err", err)
		}
	}
	r.observerNames = nil
}
