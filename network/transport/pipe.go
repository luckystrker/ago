package transport

import (
	"context"
	"sync"
)

// NewPipe returns the two ends of one in-memory duplex connection: a frame
// sent on one end is received on the other, in order. Unlike the LocalLink
// switchboard nothing is routed by address — Frame.From and Frame.To are not
// interpreted — so each end IS one connection, and the receiver cannot be
// fooled about which connection a frame came from by what the sender wrote in
// the frame. A hub serves one pipe end per client (Hub.ServeLink) and derives
// the client's identity from that connection alone.
//
// Closing either end ends both: the peer's pending Recv returns
// ErrLinkClosed once what was already delivered is drained, and Send on a
// closed pipe fails with ErrLinkClosed. buffer is the per-direction capacity
// (<= 0: 256); a Send into a full buffer fails with ErrLinkOverflow.
func NewPipe(buffer int) (Link, Link) {
	if buffer <= 0 {
		buffer = 256
	}
	done := make(chan struct{})
	var once sync.Once
	a := &pipeEnd{in: make(chan Frame, buffer), done: done, once: &once}
	b := &pipeEnd{in: make(chan Frame, buffer), done: done, once: &once}
	a.peer, b.peer = b, a
	return a, b
}

type pipeEnd struct {
	in   chan Frame
	peer *pipeEnd
	done chan struct{}
	once *sync.Once
}

// Send implements Link.
func (p *pipeEnd) Send(f Frame) error {
	select {
	case <-p.done:
		return ErrLinkClosed
	default:
	}
	select {
	case p.peer.in <- f:
		return nil
	default:
		return ErrLinkOverflow
	}
}

// Recv implements Link.
func (p *pipeEnd) Recv(ctx context.Context) (Frame, error) {
	select {
	case f := <-p.in:
		return f, nil
	case <-ctx.Done():
		return Frame{}, ctx.Err()
	case <-p.done:
		select {
		case f := <-p.in:
			return f, nil
		default:
			return Frame{}, ErrLinkClosed
		}
	}
}

// Close implements Link; it ends both ends.
func (p *pipeEnd) Close() error {
	p.once.Do(func() { close(p.done) })
	return nil
}

// MemListener hands out the server ends of in-memory pipes: Dial opens a new
// connection and returns the client end, Accept returns the matching server
// end. Safe for concurrent use.
type MemListener struct {
	buffer int
	conns  chan Link
	done   chan struct{}
	once   sync.Once
}

// NewMemListener creates a listener whose pipes buffer buffer frames per
// direction (<= 0: 256).
func NewMemListener(buffer int) *MemListener {
	return &MemListener{buffer: buffer, conns: make(chan Link, 128), done: make(chan struct{})}
}

// Dial opens a connection and returns the client end. It fails with
// ErrNoConnection once the listener is closed and with ErrLinkOverflow when
// 128 connections wait unaccepted.
func (l *MemListener) Dial() (Link, error) {
	select {
	case <-l.done:
		return nil, ErrNoConnection
	default:
	}
	client, server := NewPipe(l.buffer)
	select {
	case l.conns <- server:
		return client, nil
	default:
		_ = client.Close()
		return nil, ErrLinkOverflow
	}
}

// Accept returns the server end of the next dialed connection, in dial order.
func (l *MemListener) Accept(ctx context.Context) (Link, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-l.done:
		return nil, ErrNoConnection
	}
}

// Close stops the listener: Accept and Dial fail with ErrNoConnection.
// Connections already accepted stay open.
func (l *MemListener) Close() error {
	l.once.Do(func() { close(l.done) })
	return nil
}
