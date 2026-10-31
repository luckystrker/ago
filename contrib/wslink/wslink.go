// Package wslink carries the transport.Link surface (the Frame vocabulary
// of github.com/luckystrker/ago/network/transport) over a WebSocket, porting
// ag2/network/transport/ws.py: one JSON text message per frame.
//
// This is an optional module (its own go.mod, one dependency:
// github.com/coder/websocket). The core module stays dependency-free.
//
// Heartbeat is delegated to coder/websocket (protocol-level ping/pong with
// its default handlers), matching ws.py's reliance on the websockets
// library control frames. Reconnect is NOT automatic — Python parity:
// WsLinkClient.open does one connect, callers needing backoff wrap Dial;
// the reference protocol carries since_envelope_id on the application's
// Hello frame, not in this transport (AGO-DEVIATION note on Dial).
//
// Deployment guards: every link caps inbound message size at
// DefaultReadLimit, and Handler mounts ServeWS behind an optional bearer
// token and a connection cap. ServeWS on its own is intended for loopback
// only, like the Python reference (whose websockets library applies its
// own 1 MiB max_size on the server side).
package wslink

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/coder/websocket"

	"github.com/luckystrker/ago/network/transport"
)

// Link errors. Match with errors.Is on consumption.
var (
	// ErrClosed is returned by Send and Recv on a closed link.
	ErrClosed = errors.New("wslink: link closed")
	// ErrBadFrame is returned for an inbound message that is not a valid
	// frame (unknown kind or malformed JSON); the read loop skips it.
	ErrBadFrame = errors.New("wslink: bad frame")
	// ErrMessageTooLarge is returned by Recv when an inbound message
	// exceeds the link's read limit (DefaultReadLimit unless overridden
	// via Conn.SetReadLimit). The message cannot be skipped — the library
	// has already torn the socket down with close code 1009 — so Recv
	// closes the link, but the returned error also matches
	// transport.ErrLinkClosed and this sentinel, letting callers tell an
	// oversized payload apart from a network drop.
	ErrMessageTooLarge = errors.New("wslink: inbound message exceeds read limit")
)

// DefaultReadLimit is the per-message inbound size cap every link applies
// on both ends: 4 MiB, large enough for WAL-replay batches that dwarf
// coder/websocket's built-in 32 KiB default (which one legitimate large
// frame would trip, killing the link) while still bounding hostile frames.
// Frames payload travel base64-encoded inside the JSON message, so budget
// ~1.37x the payload size. Override per link with Conn.SetReadLimit.
const DefaultReadLimit = 4 << 20 // 4 MiB

// Conn is one WebSocket-backed transport.Link. Both dial-side and
// server-side connections are the same type. Safe for concurrent use:
// coder/websocket allows one concurrent reader and one concurrent writer,
// which matches Link's Send/Recv contract.
type Conn struct {
	ws *websocket.Conn

	// ctx drives Write; it cancels on Close so a blocked Send fails instead
	// of hanging on a dead socket.
	ctx    context.Context
	cancel context.CancelFunc

	closeOnce sync.Once
	done      chan struct{}
}

// compile-time checks: Conn implements the Link surface, and close errors
// surface as transport.ErrLinkClosed via CloseError wrapping.
var (
	_ transport.Link = (*Conn)(nil)
)

// Dial connects to a ws:// or wss:// URL and returns the client-side link.
// One connect per call — no automatic reconnect. AGO-DEVIATION vs the wire
// protocol sketch in the spec: replay-after-reconnect (since_envelope_id)
// is an application-level Hello frame field the hub answers from its WAL;
// this transport carries frames opaquely and keeps none.
func Dial(ctx context.Context, url string, opts *websocket.DialOptions) (*Conn, error) {
	ws, _, err := websocket.Dial(ctx, url, opts)
	if err != nil {
		return nil, fmt.Errorf("wslink: dial %s: %w", url, err)
	}
	ws.SetReadLimit(DefaultReadLimit)
	linkCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	c := &Conn{
		ws:     ws,
		ctx:    linkCtx,
		cancel: cancel,
		done:   make(chan struct{}),
	}
	return c, nil
}

// ServeWS upgrades an HTTP request to a WebSocket and hands the server-side
// link to handle. It blocks until the connection closes (handler return or
// peer close); use it as an http.HandlerFunc (go serveWS(r, w)). The HTTP
// response is written by the upgrade — after ServeWS returns the handler
// must not write again.
//
// ServeWS performs no authentication and no connection accounting; mount
// Handler instead whenever the endpoint is reachable by more than
// loopback clients.
func ServeWS(w http.ResponseWriter, r *http.Request, handle func(transport.Link)) error {
	ws, err := websocket.Accept(w, r, nil)
	if err != nil {
		return fmt.Errorf("wslink: accept: %w", err)
	}
	c := wrapAccepted(ws)
	handle(c)
	return nil
}

// ServerConfig is the guarded front door for ServeWS: the per-connection
// callback plus optional bearer authentication and a connection cap. Zero
// Token and zero MaxConns reproduce bare ServeWS semantics — acceptable on
// loopback, not on anything exposed (audit H-5).
type ServerConfig struct {
	// Handle receives every accepted server-side link and runs for the
	// connection's lifetime. Required.
	Handle func(transport.Link)

	// Token, when non-empty, makes Handler refuse with HTTP 401 any
	// upgrade request whose Authorization header is not "Bearer <Token>"
	// (compared in constant time). Browsers cannot set that header on a
	// WebSocket open, so this authenticates non-browser clients;
	// browser-facing deployments rely on the library's Origin check and
	// a reverse proxy instead.
	Token string

	// MaxConns caps the number of concurrently served connections;
	// requests beyond the cap are refused with HTTP 503 before any
	// upgrade work happens, so connection floods cannot exhaust
	// goroutines or file descriptors. 0 disables the cap.
	MaxConns int
}

// Handler returns an http.Handler that checks ServerConfig.Token, enforces
// ServerConfig.MaxConns and then upgrades via ServeWS. Wire it into a mux
// like any handler; it blocks per connection for the link's lifetime.
func Handler(cfg ServerConfig) (http.Handler, error) {
	if cfg.Handle == nil {
		return nil, errors.New("wslink: ServerConfig.Handle is required")
	}
	authorized := func(*http.Request) bool { return true }
	if cfg.Token != "" {
		authorized = bearerAuth(cfg.Token)
	}
	var slots chan struct{}
	if cfg.MaxConns > 0 {
		slots = make(chan struct{}, cfg.MaxConns)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !authorized(r) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="wslink"`)
			http.Error(w, "wslink: unauthorized", http.StatusUnauthorized)
			return
		}
		if slots != nil {
			select {
			case slots <- struct{}{}:
				defer func() { <-slots }()
			default:
				w.Header().Set("Retry-After", "1")
				http.Error(w, "wslink: connection limit reached", http.StatusServiceUnavailable)
				return
			}
		}
		_ = ServeWS(w, r, cfg.Handle)
	}), nil
}

// bearerAuth builds a request predicate for a fixed bearer token. It
// hashes both sides before a constant-time compare, so neither the token's
// content nor its length leaks through timing (audit M-20 pattern).
func bearerAuth(token string) func(*http.Request) bool {
	want := sha256.Sum256([]byte(token))
	const scheme = "Bearer "
	return func(r *http.Request) bool {
		h := r.Header.Get("Authorization")
		if len(h) <= len(scheme) || !strings.EqualFold(h[:len(scheme)], scheme) {
			return false
		}
		got := sha256.Sum256([]byte(h[len(scheme):]))
		return subtle.ConstantTimeCompare(want[:], got[:]) == 1
	}
}

// wrapAccepted builds the Conn for a server-side socket (its context is
// detached from any request lifetime — the socket outlives the HTTP
// handler frame).
func wrapAccepted(ws *websocket.Conn) *Conn {
	ws.SetReadLimit(DefaultReadLimit)
	ctx, cancel := context.WithCancel(context.Background())
	return &Conn{
		ws:     ws,
		ctx:    ctx,
		cancel: cancel,
		done:   make(chan struct{}),
	}
}

// SetReadLimit overrides the inbound message size cap (DefaultReadLimit by
// default). Safe for concurrent use; it applies to subsequent reads. An
// inbound message over the cap surfaces from Recv as an error matching
// ErrMessageTooLarge and closes the link.
func (c *Conn) SetReadLimit(n int64) { c.ws.SetReadLimit(n) }

// Send writes one frame as a JSON text message. It fails with ErrClosed
// after Close; write failures surface wrapped (peer gone, broken pipe).
func (c *Conn) Send(f transport.Frame) error {
	select {
	case <-c.done:
		return ErrClosed
	default:
	}
	data, err := transport.MarshalFrame(f)
	if err != nil {
		return err
	}
	if err := c.ws.Write(c.ctx, websocket.MessageText, data); err != nil {
		return fmt.Errorf("wslink: write: %w", err)
	}
	return nil
}

// Recv blocks for the next inbound frame until one arrives, ctx is done, or
// the link closes. Malformed inbound messages are skipped (Python parity:
// the ws.py frame loop logs a decode failure and continues); a caller can
// tell a skipped-message gap only from the server logs. Close and peer
// disconnects surface as an error wrapping transport.ErrLinkClosed.
func (c *Conn) Recv(ctx context.Context) (transport.Frame, error) {
	for {
		if err := ctx.Err(); err != nil {
			return transport.Frame{}, err
		}
		select {
		case <-c.done:
			return transport.Frame{}, fmt.Errorf("wslink: %w", transport.ErrLinkClosed)
		default:
		}
		// The read itself honors ctx: canceling ctx interrupts a blocked
		// read (coder/websocket closes the connection on canceled reads).
		_, data, err := c.ws.Read(ctx)
		if err != nil {
			// A canceled read is a control-plane interruption, not a peer
			// hangup: distinguish for the caller, then drop the socket (the
			// library requires closing canceled connections).
			if ctx.Err() != nil {
				return transport.Frame{}, ctx.Err()
			}
			// A message over the read limit is a policy trip, not a
			// transport failure: report it with its own sentinel so a
			// caller can raise the limit (Conn.SetReadLimit) or blame the
			// payload instead of misreading a network drop. The socket is
			// already dead — the library closes it with code 1009 — so the
			// link still comes down.
			if errors.Is(err, websocket.ErrMessageTooBig) {
				c.Close()
				return transport.Frame{}, fmt.Errorf("wslink: %w: %w: %w", transport.ErrLinkClosed, ErrMessageTooLarge, err)
			}
			c.Close()
			return transport.Frame{}, fmt.Errorf("wslink: %w: %v", transport.ErrLinkClosed, err)
		}
		f, err := transport.UnmarshalFrame(data)
		if err != nil {
			// Skip malformed messages; keep reading (Python parity).
			continue
		}
		return f, nil
	}
}

// Close tears the WebSocket down (normal closure code) and wakes every
// blocked Send and Recv. Idempotent.
func (c *Conn) Close() error {
	c.closeOnce.Do(func() {
		close(c.done)
		c.cancel()
		_ = c.ws.Close(websocket.StatusNormalClosure, "")
	})
	return nil
}

// Done exposes the link's closed channel (server loops select over it to
// reclaim the endpoint).
func (c *Conn) Done() <-chan struct{} { return c.done }
