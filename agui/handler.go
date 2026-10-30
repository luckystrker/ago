package agui

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
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
			serveExchange(w, r, input, run)
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

// serveExchange writes one run as SSE: the events emit produces go out as
// "data: <json>" frames until the terminal one, which ends the response (Python
// parity: serve_exchange plus the EventEncoder's frame shape).
func serveExchange(w http.ResponseWriter, r *http.Request, input RunAgentInput, run RunFunc) {
	flusher, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)

	// Events flow through a channel so the run's goroutine blocks on a slow
	// client the way Python's send blocks on a slow exchange — backpressure, not
	// an unbounded buffer.
	type frame struct{ raw []byte }
	frames := make(chan frame, 16)
	var ended atomic.Bool
	emit := func(e Event) {
		raw, err := json.Marshal(e)
		if err != nil {
			slog.Error("agui: dropping an event that does not encode", "event", e.eventType(), "err", err)
			return
		}
		if isTerminal(e) {
			ended.Store(true)
		}
		frames <- frame{raw: raw}
	}

	done := make(chan struct{})
	go func() {
		defer close(frames)
		func() {
			defer func() {
				if rec := recover(); rec != nil {
					slog.Error("agui: run panicked", "panic", rec)
				}
			}()
			if err := run(r.Context(), input, emit); err != nil {
				// The outcome is on the stream; a callback error without one is
				// repaired below.
				slog.Error("agui: run failed", "err", err)
			}
		}()
		if !ended.Load() {
			// A run that started on the wire always ends on it — on a callback
			// that stopped short, and on one that panicked (Python parity:
			// serve_exchange's not-ended RUN_ERROR).
			emit(runErrorEvent(errors.New("the run was stopped before it could finish")))
		}
		close(done)
	}()

	for f := range frames {
		line := make([]byte, 0, len(f.raw)+8)
		line = append(line, "data: "...)
		line = append(line, f.raw...)
		line = append(line, '\n', '\n')
		if _, err := w.Write(line); err != nil {
			// The client went away; drain what is left rather than deadlocking
			// the run, and stop writing.
			for range frames {
			}
			break
		}
		if flusher != nil {
			flusher.Flush()
		}
	}
	<-done
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
