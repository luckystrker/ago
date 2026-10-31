package wslink

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

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

// TestLargeFrameSurvivesReadLimit is the regression for audit M-14:
// coder/websocket's built-in 32 KiB read limit made one legitimate large
// frame (e.g. a WAL-replay batch) fail the read and kill the link. With
// DefaultReadLimit applied on both ends, a 128 KiB payload (~171 KiB on
// the wire after base64) must round-trip through the echo server —
// exercising the server-side read on the way in and the client-side read
// on the way back.
func TestLargeFrameSurvivesReadLimit(t *testing.T) {
	s := echoServer(t)
	c, err := Dial(context.Background(), wsURL(s), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	f := transport.Frame{
		Kind:    transport.FrameNotify,
		From:    "alice",
		To:      "bob",
		Payload: bytes.Repeat([]byte("x"), 128<<10),
	}
	if err := c.Send(f); err != nil {
		t.Fatalf("send large frame: %v", err)
	}
	recvCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	got, err := c.Recv(recvCtx)
	if err != nil {
		t.Fatalf("recv large frame: %v", err)
	}
	if !bytes.Equal(got.Payload, f.Payload) {
		t.Fatalf("echo mangled the payload: got %d bytes, want %d", len(got.Payload), len(f.Payload))
	}
}

// TestMessageTooLargeDistinctFromDisconnect is the other half of M-14: an
// oversized inbound message must surface as ErrMessageTooLarge (with the
// link still reported closed), while a plain peer hangup must not
// masquerade as one.
func TestMessageTooLargeDistinctFromDisconnect(t *testing.T) {
	big := bytes.Repeat([]byte("y"), 256<<10)
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = ServeWS(w, r, func(link transport.Link) {
			raw := link.(*Conn)
			_ = raw.ws.Write(context.Background(), websocket.MessageText, big)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, _ = link.Recv(ctx) // hold the link open until the client drops it
		})
	}))
	defer s.Close()

	c, err := Dial(context.Background(), wsURL(s), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetReadLimit(64 << 10)

	recvCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err = c.Recv(recvCtx)
	if !errors.Is(err, ErrMessageTooLarge) {
		t.Fatalf("oversized message must surface ErrMessageTooLarge, got %v", err)
	}
	if !errors.Is(err, transport.ErrLinkClosed) {
		t.Fatalf("oversized message must still report the link as closed, got %v", err)
	}

	// Control case: a clean server-side hangup is a plain link closure.
	s2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = ServeWS(w, r, func(link transport.Link) { _ = link.Close() })
	}))
	defer s2.Close()

	c2, err := Dial(context.Background(), wsURL(s2), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	recvCtx2, cancel2 := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel2()
	_, err = c2.Recv(recvCtx2)
	if !errors.Is(err, transport.ErrLinkClosed) {
		t.Fatalf("peer hangup must surface transport.ErrLinkClosed, got %v", err)
	}
	if errors.Is(err, ErrMessageTooLarge) {
		t.Fatal("peer hangup must not be reported as message-too-large")
	}
}

func TestHandlerRequiresHandle(t *testing.T) {
	if _, err := Handler(ServerConfig{}); err == nil {
		t.Fatal("Handler must reject a config without Handle")
	}
}

func TestHandlerBearerToken(t *testing.T) {
	h, err := Handler(ServerConfig{
		Token: "sekrit",
		Handle: func(link transport.Link) {
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
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	s := httptest.NewServer(h)
	defer s.Close()
	url := wsURL(s)

	// A plain HTTP request without credentials is refused before upgrade.
	resp, err := http.Get(s.URL)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no token: status = %d, want 401", resp.StatusCode)
	}

	// A WebSocket dial without a token fails the handshake with 401.
	if _, err := Dial(context.Background(), url, nil); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("dial without token: err = %v, want handshake failure with 401", err)
	}

	// Wrong token: still 401.
	bad := &websocket.DialOptions{HTTPHeader: http.Header{
		"Authorization": []string{"Bearer nope"},
	}}
	if _, err := Dial(context.Background(), url, bad); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("dial with wrong token: err = %v, want handshake failure with 401", err)
	}

	// Correct token: the link works end to end.
	good := &websocket.DialOptions{HTTPHeader: http.Header{
		"Authorization": []string{"Bearer sekrit"},
	}}
	c, err := Dial(context.Background(), url, good)
	if err != nil {
		t.Fatalf("dial with correct token: %v", err)
	}
	defer c.Close()

	f := transport.Frame{Kind: transport.FramePing, From: "alice"}
	if err := c.Send(f); err != nil {
		t.Fatalf("send: %v", err)
	}
	recvCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	got, err := c.Recv(recvCtx)
	if err != nil {
		t.Fatalf("recv: %v", err)
	}
	if got.Kind != transport.FramePing || got.From != "alice" {
		t.Fatalf("echo = %+v", got)
	}
}

func TestHandlerMaxConns(t *testing.T) {
	release := make(chan struct{})
	h, err := Handler(ServerConfig{
		MaxConns: 1,
		Handle: func(link transport.Link) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, _ = link.Recv(ctx) // hold the slot until the client goes away
			<-release
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	s := httptest.NewServer(h)
	defer s.Close()
	url := wsURL(s)
	ctx := context.Background()

	// First connection takes the only slot.
	c1, err := Dial(ctx, url, nil)
	if err != nil {
		t.Fatalf("first dial: %v", err)
	}

	// Second is refused with 503 before any upgrade work.
	if _, err := Dial(ctx, url, nil); err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("second dial: err = %v, want handshake failure with 503", err)
	}

	// Closing the first frees the slot; a retry loop papers over the race
	// between the server observing the close and releasing the semaphore.
	_ = c1.Close()
	close(release)
	var c2 *Conn
	deadline := time.Now().Add(3 * time.Second)
	for {
		c2, err = Dial(ctx, url, nil)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("slot did not free after close: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	// release is already closed, so this handle returns as soon as the
	// link drops; Server.Close in cleanup will not block.
	_ = c2.Close()
}
