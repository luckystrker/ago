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
package wslink

import (
	"context"
	"errors"
	"fmt"
	"net/http"
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
)

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
func ServeWS(w http.ResponseWriter, r *http.Request, handle func(transport.Link)) error {
	ws, err := websocket.Accept(w, r, nil)
	if err != nil {
		return fmt.Errorf("wslink: accept: %w", err)
	}
	c := wrapAccepted(ws)
	handle(c)
	return nil
}

// wrapAccepted builds the Conn for a server-side socket (its context is
// detached from any request lifetime — the socket outlives the HTTP
// handler frame).
func wrapAccepted(ws *websocket.Conn) *Conn {
	ctx, cancel := context.WithCancel(context.Background())
	return &Conn{
		ws:     ws,
		ctx:    ctx,
		cancel: cancel,
		done:   make(chan struct{}),
	}
}

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
