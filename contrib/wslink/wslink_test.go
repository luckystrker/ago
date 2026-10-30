package wslink

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/luckystrker/ago/network/transport"
)

// wsURL converts an httptest server URL to its ws:// form.
func wsURL(s *httptest.Server) string {
	return "ws" + strings.TrimPrefix(s.URL, "http")
}

// echoServer serves one echo endpoint: every frame received is sent back
// verbatim (LocalLink semantics: Send routes by To, and the echo makes the
// client's own frames return to it).
func echoServer(t *testing.T) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		err := ServeWS(w, r, func(link transport.Link) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			for {
				f, err := link.Recv(ctx)
				if err != nil {
					return
				}
				if err := link.Send(f); err != nil {
					return
				}
			}
		})
		if err != nil {
			t.Logf("serve: %v", err)
		}
	}))
	t.Cleanup(s.Close)
	return s
}

func TestEchoThroughLinkSemantics(t *testing.T) {
	s := echoServer(t)
	ctx := context.Background()

	c, err := Dial(ctx, wsURL(s), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	// Send routes by Frame.To on a Link; the echo bounces it back so Recv
	// observes exactly what was sent.
	frames := []transport.Frame{
		{Kind: transport.FrameNotify, From: "alice", To: "bob", Payload: []byte(`{"text":"hi"}`)},
		{Kind: transport.FrameRequest, From: "alice", To: "bob", RequestID: "r-1", Payload: []byte(`{"op":"ping"}`)},
	}
	for i, f := range frames {
		if err := c.Send(f); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
	}
	recvCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	for i, want := range frames {
		got, err := c.Recv(recvCtx)
		if err != nil {
			t.Fatalf("recv %d: %v", i, err)
		}
		if got.Kind != want.Kind || got.From != want.From || got.To != want.To ||
			got.RequestID != want.RequestID || string(got.Payload) != string(want.Payload) {
			t.Fatalf("echo %d = %+v, want %+v", i, got, want)
		}
	}
}

func TestServerSendsToClient(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = ServeWS(w, r, func(link transport.Link) {
			_ = link.Send(transport.Frame{Kind: transport.FrameWelcome, To: "alice", Message: "hello"})
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, _ = link.Recv(ctx) // hold the link open until the client goes
		})
	}))
	defer s.Close()

	ctx := context.Background()
	c, err := Dial(ctx, wsURL(s), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	recvCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	f, err := c.Recv(recvCtx)
	if err != nil {
		t.Fatal(err)
	}
	if f.Kind != transport.FrameWelcome || f.Message != "hello" {
		t.Fatalf("got %+v", f)
	}
}

func TestMalformedInboundSkipped(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = ServeWS(w, r, func(link transport.Link) {
			// Two garbage messages, then a valid one: the read loop must
			// skip the garbage (Python ws.py parity) and deliver the frame.
			raw := link.(*Conn)
			_ = raw.ws.Write(context.Background(), 1, []byte("not json"))
			_ = raw.ws.Write(context.Background(), 1, []byte(`{"kind":"no_such_kind"}`))
			_ = link.Send(transport.Frame{Kind: transport.FrameNotify, Message: "ok"})
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, _ = link.Recv(ctx)
		})
	}))
	defer s.Close()

	ctx := context.Background()
	c, err := Dial(ctx, wsURL(s), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	recvCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	f, err := c.Recv(recvCtx)
	if err != nil {
		t.Fatal(err)
	}
	if f.Message != "ok" {
		t.Fatalf("malformed frames were not skipped, got %+v", f)
	}
}

func TestRecvContextCancellation(t *testing.T) {
	s := echoServer(t)
	ctx := context.Background()
	c, err := Dial(ctx, wsURL(s), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	recvCtx, cancel := context.WithTimeout(ctx, 150*time.Millisecond)
	defer cancel()
	if _, err := c.Recv(recvCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want context.DeadlineExceeded, got %v", err)
	}
}

func TestCloseWakesRecv(t *testing.T) {
	s := echoServer(t)
	ctx := context.Background()
	c, err := Dial(ctx, wsURL(s), nil)
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := c.Recv(context.Background())
		done <- err
	}()
	time.Sleep(50 * time.Millisecond)
	_ = c.Close()

	select {
	case err := <-done:
		if !errors.Is(err, transport.ErrLinkClosed) {
			t.Fatalf("want transport.ErrLinkClosed, got %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not wake Recv")
	}
	// Send after close fails with ErrClosed.
	if err := c.Send(transport.Frame{Kind: transport.FramePing}); !errors.Is(err, ErrClosed) {
		t.Fatalf("send after close = %v", err)
	}
	// Close is idempotent.
	if err := c.Close(); err != nil {
		t.Fatalf("second close = %v", err)
	}
}

func TestSendMarshalError(t *testing.T) {
	s := echoServer(t)
	ctx := context.Background()
	c, err := Dial(ctx, wsURL(s), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	if err := c.Send(transport.Frame{Kind: "bogus"}); !errors.Is(err, transport.ErrUnknownKind) {
		t.Fatalf("unknown kind must surface transport.ErrUnknownKind, got %v", err)
	}
}

func TestDialRefusesPlainHTTP(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer s.Close()

	if _, err := Dial(context.Background(), s.URL, nil); err == nil {
		t.Fatal("dialing a non-WebSocket endpoint must fail")
	}
}
