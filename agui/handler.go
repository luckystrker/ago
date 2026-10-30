package agui

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"

	"github.com/luckystrker/ago/agent"
)

// HandlerConfig configures Handler.
type HandlerConfig struct {
	// Capabilities answers GET. Nil → the static default for a plain served
	// agent.
	Capabilities func() AgentCapabilities

	// Run drives one POSTed run. It emits AG-UI events — its terminal
	// RUN_FINISHED or RUN_ERROR included — through emit, and returns when the
	// run is over. The returned error is logged, never written: the outcome
	// travels on the stream. A run that ends with no terminal event behind it
	// gets one RUN_ERROR from the handler (Python parity: serve_exchange's
	// not-ended path).
	Run func(ctx context.Context, input RunAgentInput, emit func(Event)) error
}

// Handler serves the AG-UI endpoint over net/http (Python parity: asgi.py
// build_asgi): GET answers with the capabilities JSON, POST parses the body as
// RunAgentInput — 400 when it does not parse, before any stream exists — and
// streams the run as SSE.
func Handler(cfg HandlerConfig) http.Handler {
	caps := cfg.Capabilities
	if caps == nil {
		caps = func() AgentCapabilities { return staticCapabilities() }
	}
	run := cfg.Run
	if run == nil {
		run = func(context.Context, RunAgentInput, func(Event)) error { return nil }
	}
	return handlerFor(caps, func(ctx context.Context, input RunAgentInput, ex *exchange) error {
		return run(ctx, input, ex.emit)
	})
}

// handlerFor builds the endpoint: GET answers with the capabilities JSON, POST
// streams one exchange.
func handlerFor(caps func() AgentCapabilities, serve exchangeFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, http.StatusOK, caps())
		case http.MethodPost:
			body, err := io.ReadAll(r.Body)
			if err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid AG-UI RunAgentInput body"})
				return
			}
			input, err := ReadRunInput(body)
			if err != nil {
				// Refused before any stream: a run that never started has no
				// event stream for a RUN_ERROR to travel on.
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid AG-UI RunAgentInput body"})
				return
			}
			serveExchangeEx(w, r, input, serve)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
}

// ServeAgent serves an agent over AG-UI: every POST runs one turn of the agent
// on its own stream, projected into AG-UI events (Python parity: AGUIStream plus
// the ASGI endpoint it builds).
//
// The agent must be usable with a fresh stream per turn — serving shares the
// agent, and a WithStreamBase stream would have every request's history seeded
// over the same transcript.
func ServeAgent(a *agent.Agent) http.Handler {
	return Handler(HandlerConfig{
		Capabilities: func() AgentCapabilities { return CapabilitiesFor(a) },
		Run: func(ctx context.Context, input RunAgentInput, emit func(Event)) error {
			return serveTurn(ctx, a, input, emit)
		},
	})
}

// staticCapabilities is the answer when no agent or override is configured.
func staticCapabilities() AgentCapabilities {
	return AgentCapabilities{
		Transport: &TransportCapabilities{Streaming: boolPtr(true)},
		Tools:     &ToolsCapabilities{Supported: boolPtr(true)},
	}
}

// exchange is one HTTP exchange carrying a run: emit writes AG-UI events onto
// the response; done closes when the exchange is over or the client went away;
// ended closes once a terminal event has been written onto the stream.
type exchange struct {
	emit  func(Event)
	done  <-chan struct{}
	ended <-chan struct{}
}

// wait blocks until the exchange's run ended on the wire — the terminal event
// is written — or the exchange is over because the client went away. A run that
// pauses waits here until its resume ends it; a held turn outlives the wait.
func (ex *exchange) wait() {
	select {
	case <-ex.ended:
	case <-ex.done:
	}
}

// exchangeFunc drives one POSTed run against an exchange (HandlerConfig.Run's
// form for code that needs the exchange's end signals).
type exchangeFunc func(ctx context.Context, input RunAgentInput, ex *exchange) error

// serveExchange writes one run as SSE: the events emit produces go out as
// "data: <json>" frames until the terminal one, which ends the response (Python
// parity: serve_exchange plus the EventEncoder's frame shape).
func serveExchange(w http.ResponseWriter, r *http.Request, input RunAgentInput, run RunFunc) {
	serveExchangeEx(w, r, input, func(ctx context.Context, input RunAgentInput, ex *exchange) error {
		return run(ctx, input, ex.emit)
	})
}

// serveExchangeEx is serveExchange for a callback that wants the exchange's end
// signals: a run that pauses — leaving a held turn running — returns when its
// interrupt event is on the wire, not when the turn is over.
func serveExchangeEx(w http.ResponseWriter, r *http.Request, input RunAgentInput, run exchangeFunc) {
	flusher, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)

	// Events flow through a channel so the run's goroutine blocks on a slow
	// client the way Python's send blocks on a slow exchange — backpressure, not
	// an unbounded buffer. frames is never closed: the turn may outlive the
	// exchange, and a send racing the response's end is dropped on done instead
	// of panicking on a closed channel.
	type frame struct{ raw []byte }
	frames := make(chan frame, 16)
	var (
		doneOnce  sync.Once
		endedOnce sync.Once
		endedFlag atomic.Bool
		runGone   = make(chan struct{})
		done      = make(chan struct{})
		ended     = make(chan struct{})
	)
	closeDone := func() { doneOnce.Do(func() { close(done) }) }
	emit := func(e Event) {
		raw, err := json.Marshal(e)
		if err != nil {
			slog.Error("agui: dropping an event that does not encode", "event", e.eventType(), "err", err)
			return
		}
		terminal := isTerminal(e)
		if terminal {
			endedFlag.Store(true)
		}
		select {
		case frames <- frame{raw: raw}:
			if terminal {
				endedOnce.Do(func() { close(ended) })
			}
		case <-done:
			// No exchange is carrying the run any more; the event is dropped
			// (Python parity: a send into a closed stream logs and drops).
		}
	}

	go func() {
		defer close(runGone)
		defer closeDone()
		func() {
			defer func() {
				if rec := recover(); rec != nil {
					slog.Error("agui: run panicked", "panic", rec)
				}
			}()
			if err := run(r.Context(), input, &exchange{emit: emit, done: done, ended: ended}); err != nil {
				// The outcome is on the stream; a callback error without one is
				// repaired below.
				slog.Error("agui: run failed", "err", err)
			}
		}()
		if !endedFlag.Load() {
			// A run that started on the wire always ends on it — on a callback
			// that stopped short, and on one that panicked (Python parity:
			// serve_exchange's not-ended RUN_ERROR).
			emit(runErrorEvent(errors.New("the run was stopped before it could finish")))
		}
	}()

	write := func(f frame) bool {
		line := make([]byte, 0, len(f.raw)+8)
		line = append(line, "data: "...)
		line = append(line, f.raw...)
		line = append(line, '\n', '\n')
		if _, err := w.Write(line); err != nil {
			// The client went away; stop writing.
			return false
		}
		if flusher != nil {
			flusher.Flush()
		}
		return true
	}

read:
	for {
		select {
		case f := <-frames:
			if !write(f) {
				break read
			}
		case <-done:
			// The run side is finished for this exchange: what it already
			// queued — the terminal included — still goes out.
			for {
				select {
				case f := <-frames:
					if !write(f) {
						break read
					}
				default:
					break read
				}
			}
		}
	}
	// A write failure releases the producer; on the normal path done is closed
	// already and this returns at once.
	closeDone()
	<-runGone
}

// RunFunc drives one POSTed run (HandlerConfig.Run).
type RunFunc func(ctx context.Context, input RunAgentInput, emit func(Event)) error

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("agui: writing a JSON response", "err", err)
	}
}
