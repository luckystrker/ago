package agui

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
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

	// Guard is the endpoint's access control and limits (see NewGuard). Nil
	// still applies the default limits — a 16 MiB body, eight concurrent POST
	// exchanges — with no token and no origin check.
	Guard *Guard
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
	guard := cfg.Guard
	if guard == nil {
		guard = NewGuard()
	}
	return handlerFor(caps, func(ctx context.Context, input RunAgentInput, ex *exchange) error {
		return run(ctx, input, ex.emit)
	}, guard)
}

// The limits a Guard applies when nothing is configured: a body of 16 MiB and
// eight POST exchanges at once. Large enough that an ordinary run input never
// notices them, small enough that one client cannot spend the server's memory
// or its LLM budget on its own.
const (
	defaultMaxBodyBytes      int64 = 16 << 20
	defaultMaxConcurrentRuns       = 8
)

// Guard is the access control and the resource limits of an AG-UI endpoint: a
// bearer token, an origin allow-list for state-changing requests, a cap on one
// request body, and a cap on POST exchanges served at once. Build it with
// NewGuard; the limits always apply, the access checks only when configured.
//
// A Guard may be shared by several handlers; its run slots are shared too.
// Safe for concurrent use.
type Guard struct {
	token   string
	origins map[string]bool // empty → no origin check
	maxBody int64
	sem     chan struct{}
}

// GuardOption configures a Guard at construction.
type GuardOption func(*Guard)

// WithAuthToken requires every request to carry token as an HTTP bearer token
// ("Authorization: Bearer <token>"), GET included: the capabilities answer
// names the served agent, which is not public wallpaper. The value is compared
// in constant time; ConstantTimeCompare returns early on a length mismatch,
// which reveals the token's length — not a secret worth hiding.
func WithAuthToken(token string) GuardOption {
	return func(g *Guard) {
		if token != "" {
			g.token = token
		}
	}
}

// WithAllowedOrigins refuses a state-changing request — anything but GET —
// whose Origin header names an origin not in the list. A browser sends Origin
// on every cross-site POST, a mode "no-cors" fetch included, so a page
// elsewhere cannot drive this server's runs or read their cost; a request
// without Origin is not a browser and passes — from such clients the token is
// the barrier. Origins match exactly, "scheme://host:port".
func WithAllowedOrigins(origins ...string) GuardOption {
	return func(g *Guard) {
		set := make(map[string]bool, len(origins))
		for _, o := range origins {
			set[o] = true
		}
		g.origins = set
	}
}

// WithMaxBodyBytes caps one request body at max bytes: past it the request
// fails 413 instead of being buffered whole. A non-positive max keeps the
// default.
func WithMaxBodyBytes(max int64) GuardOption {
	return func(g *Guard) {
		if max > 0 {
			g.maxBody = max
		}
	}
}

// WithMaxConcurrentRuns caps how many POST exchanges are served at once; the
// next one gets 429 instead of a goroutine and an LLM call of its own. A
// non-positive n keeps the default.
//
// The slot covers one HTTP exchange, not a turn's whole life: a run that pauses
// on an interrupt ends its exchange and frees its slot while the turn waits.
// Counting the wait would let one client pin every slot with questions nobody
// answers.
func WithMaxConcurrentRuns(n int) GuardOption {
	return func(g *Guard) {
		if n > 0 {
			g.sem = make(chan struct{}, n)
		}
	}
}

// NewGuard builds a Guard with the default limits: a 16 MiB body and eight
// concurrent POST exchanges, no token, no origin check.
func NewGuard(opts ...GuardOption) *Guard {
	g := &Guard{
		maxBody: defaultMaxBodyBytes,
		sem:     make(chan struct{}, defaultMaxConcurrentRuns),
	}
	for _, o := range opts {
		o(g)
	}
	return g
}

// authorize applies the access options — the token to every method, the origin
// allow-list to the state-changing ones — answering the request itself and
// reporting false when it refused it.
func (g *Guard) authorize(w http.ResponseWriter, r *http.Request) bool {
	if g.token != "" {
		got, bearer := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !bearer || subtle.ConstantTimeCompare([]byte(got), []byte(g.token)) != 1 {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "a bearer token is required"})
			return false
		}
	}
	if len(g.origins) > 0 && r.Method != http.MethodGet {
		if origin := r.Header.Get("Origin"); origin != "" && !g.origins[origin] {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "origin not allowed"})
			return false
		}
	}
	return true
}

// beginRun takes a run slot for one POST exchange, answering the request with
// 429 when none is left.
func (g *Guard) beginRun(w http.ResponseWriter) bool {
	select {
	case g.sem <- struct{}{}:
		return true
	default:
		w.Header().Set("Retry-After", "1")
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "too many concurrent runs"})
		return false
	}
}

// endRun returns a slot taken by beginRun.
func (g *Guard) endRun() { <-g.sem }

// handlerFor builds the endpoint: GET answers with the capabilities JSON, POST
// streams one exchange.
func handlerFor(caps func() AgentCapabilities, serve exchangeFunc, guard *Guard) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !guard.authorize(w, r) {
			return
		}
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, http.StatusOK, caps())
		case http.MethodPost:
			if !guard.beginRun(w) {
				return
			}
			defer guard.endRun()
			// The cap stops the read at the limit; the excess is never
			// buffered.
			r.Body = http.MaxBytesReader(w, r.Body, guard.maxBody)
			body, err := io.ReadAll(r.Body)
			if err != nil {
				// A body past the cap is refused for its size, not for its
				// shape.
				status, msg := http.StatusBadRequest, "invalid AG-UI RunAgentInput body"
				var tooLarge *http.MaxBytesError
				if errors.As(err, &tooLarge) {
					status, msg = http.StatusRequestEntityTooLarge, "request body exceeds the allowed size"
				}
				writeJSON(w, status, map[string]string{"error": msg})
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
	// dropEvent forgets an event that cannot go on the wire. A dropped
	// terminal still ends the exchange: nothing behind it can repair a stream
	// whose end was the thing dropped, and the waiters would otherwise wait
	// for an end that never comes.
	dropEvent := func(e Event, why string, err error) {
		attrs := []any{"event", e.eventType(), "reason", why}
		if err != nil {
			attrs = append(attrs, "err", err)
		}
		slog.Error("agui: dropping an event", attrs...)
		if isTerminal(e) {
			endedFlag.Store(true)
			endedOnce.Do(func() { close(ended) })
		}
	}
	emit := func(e Event) {
		raw, err := json.Marshal(e)
		if err != nil {
			dropEvent(e, "does not encode", err)
			return
		}
		// The frame a break would split is the last line of defense, not the
		// first: json.Marshal compacts MarshalJSON output, so today a literal
		// CR or LF cannot reach here — an invalid one is refused above, and an
		// insignificant one is compacted away. The check holds for any future
		// writer of frames that does not go through the compactor (security
		// audit L-16).
		if breaksFrame(raw) {
			dropEvent(e, "would split its SSE frame", nil)
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

// breaksFrame reports whether the encoded form of an event carries a literal CR
// or LF. An SSE data line ends at either byte, so such a payload would split
// its own frame — and whatever followed the break could pose as a field or a
// frame of its own, a "data: ..." line an injector shapes (security audit
// L-16). json.Marshal compacts the output of MarshalJSON, so a literal break
// does not reach the wire today; this is the invariant that keeps it so
// whichever writer comes next.
func breaksFrame(raw []byte) bool {
	return bytes.IndexByte(raw, '\n') >= 0 || bytes.IndexByte(raw, '\r') >= 0
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("agui: writing a JSON response", "err", err)
	}
}
