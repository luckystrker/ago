package transport

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// Link is one duplex connection between a client and a hub. Send pushes a
// frame towards the peer named by Frame.To; Recv blocks for the next frame
// addressed to this link; Close tears the link down (pending Recv calls
// return an error wrapping ErrLinkClosed).
//
// Send never blocks indefinitely: the inbox is a bounded buffer and an
// overflow fails the send. Implementations must be safe for concurrent use.
type Link interface {
	Send(f Frame) error
	Recv(ctx context.Context) (Frame, error)
	Close() error
}

var (
	// ErrLinkClosed is returned by Recv after the link (or its peer end)
	// was closed, and by Send on a closed link.
	ErrLinkClosed = errors.New("transport: link closed")
	// ErrUnknownAddress is returned by Send when Frame.To names no known
	// link on the switch.
	ErrUnknownAddress = errors.New("transport: unknown address")
	// ErrAddressInUse is returned by Dial for an address already registered.
	ErrAddressInUse = errors.New("transport: address in use")
	// ErrLinkOverflow is returned by Send when the recipient's inbox is
	// full. AGO-DEVIATION: the Python LocalLink queues are unbounded; AGo
	// bounds the buffer so a stuck peer fails sends instead of leaking
	// memory.
	ErrLinkOverflow = errors.New("transport: link inbox overflow")
	// ErrNoConnection is returned by Accept when the LocalLink is closed
	// while waiting for the next dial.
	ErrNoConnection = errors.New("transport: link switch closed")
)

// LocalLink is an in-memory switchboard: every dialed link registers under an
// address, and a frame is delivered by looking up Frame.To. It exists so
// tests and in-process deployments run real frame traffic through the same
// Link surface a wire transport would implement — no socket, no wire.
//
// Dial creates a link addressed as addr and queues it for Accept, so a
// hub-style server loop can pick up connections as they arrive. Delivery
// itself never involves Accept: Send routes purely by Frame.To.
//
// All methods are safe for concurrent use.
type LocalLink struct {
	mu      sync.Mutex
	closed  bool
	conns   map[string]*localConn
	dialed  chan *localConn
	bufSize int
}

// LocalLinkOption configures a LocalLink.
type LocalLinkOption func(*LocalLink)

// WithLinkBuffer sets the per-link inbox capacity (default 256).
func WithLinkBuffer(n int) LocalLinkOption {
	return func(l *LocalLink) { l.bufSize = n }
}

// NewLocalLink creates an empty switchboard.
func NewLocalLink(opts ...LocalLinkOption) *LocalLink {
	l := &LocalLink{
		conns:   make(map[string]*localConn),
		dialed:  make(chan *localConn),
		bufSize: 256,
	}
	for _, opt := range opts {
		opt(l)
	}
	return l
}

// Dial registers a new link under addr and returns it. The link is also
// queued for Accept so a server loop observes the connection.
func (l *LocalLink) Dial(addr string) (Link, error) {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return nil, ErrNoConnection
	}
	if _, ok := l.conns[addr]; ok {
		l.mu.Unlock()
		return nil, fmt.Errorf("%w: %q", ErrAddressInUse, addr)
	}
	c := &localConn{
		addr:  addr,
		sw:    l,
		inbox: make(chan Frame, l.bufSize),
		done:  make(chan struct{}),
	}
	l.conns[addr] = c
	l.mu.Unlock()

	// The Accept queue is unbuffered on purpose: a Dial with no Accept
	// pending must not block (the link is fully usable regardless), so the
	// handoff is best-effort in the background and abandoned on Close.
	go func() {
		select {
		case l.dialed <- c:
		case <-c.done:
		}
	}()
	return c, nil
}

// Accept returns the next dialed link, in dial order. It fails once the
// switch is closed.
func (l *LocalLink) Accept(ctx context.Context) (Link, error) {
	select {
	case c := <-l.dialed:
		return c, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Close tears down every registered link and the switch itself. Pending and
// future Dials fail with ErrNoConnection.
func (l *LocalLink) Close() error {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return nil
	}
	l.closed = true
	conns := make([]*localConn, 0, len(l.conns))
	for _, c := range l.conns {
		conns = append(conns, c)
	}
	l.conns = make(map[string]*localConn)
	l.mu.Unlock()
	for _, c := range conns {
		c.shutdown()
	}
	return nil
}

// deliver routes one frame to the link registered under addr.
func (l *LocalLink) deliver(addr string, f Frame) error {
	l.mu.Lock()
	c, ok := l.conns[addr]
	l.mu.Unlock()
	if !ok {
		return fmt.Errorf("%w: %q", ErrUnknownAddress, addr)
	}
	return c.push(f)
}

// localConn is one registered endpoint of a LocalLink.
type localConn struct {
	addr  string
	sw    *LocalLink
	inbox chan Frame
	done  chan struct{}

	closeOnce sync.Once
}

// push never blocks and must be safe against a concurrent Close. Closing
// the inbox instead of (or with) done would turn this select into a panic
// ("send on closed channel"), so the inbox channel is never closed: done is
// the only shutdown signal, and whichever case this select picks, the send
// targets an open channel. A frame that loses the race to done is dropped
// along with the link — Recv drains the inbox once and then reports closed.
func (c *localConn) push(f Frame) error {
	select {
	case c.inbox <- f:
		return nil
	case <-c.done:
		return ErrLinkClosed
	default:
		return ErrLinkOverflow
	}
}

// Send delivers the frame to the link named by f.To.
func (c *localConn) Send(f Frame) error {
	select {
	case <-c.done:
		return ErrLinkClosed
	default:
	}
	if f.To == "" {
		return fmt.Errorf("%w: empty destination", ErrUnknownAddress)
	}
	return c.sw.deliver(f.To, f)
}

// Recv waits for the next frame delivered to this link, until ctx is done or
// the link closes.
func (c *localConn) Recv(ctx context.Context) (Frame, error) {
	select {
	case f := <-c.inbox:
		return f, nil
	case <-ctx.Done():
		return Frame{}, ctx.Err()
	case <-c.done:
		// Drain whatever landed before shutdown so no delivered frame is
		// lost by a racing Close.
		select {
		case f := <-c.inbox:
			return f, nil
		default:
			return Frame{}, ErrLinkClosed
		}
	}
}

// Close unregisters the link and wakes every Recv. Idempotent.
//
// Only done is closed here. The inbox must stay open: push runs without any
// lock and a send racing close(inbox) would panic; with done as the sole
// shutdown signal, Recv reports closure after draining and the buffered
// inbox is garbage-collected together with the conn.
func (c *localConn) Close() error {
	c.closeOnce.Do(func() {
		c.sw.mu.Lock()
		if c.sw.conns[c.addr] == c {
			delete(c.sw.conns, c.addr)
		}
		c.sw.mu.Unlock()
		close(c.done)
	})
	return nil
}

// shutdown is the switch-side teardown (switch Close closes all conns).
func (c *localConn) shutdown() {
	c.Close()
}
