package transport

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestFrameKinds(t *testing.T) {
	kinds := map[string]string{
		"hello": FrameHello, "welcome": FrameWelcome,
		"request": FrameRequest, "response": FrameResponse,
		"notify": FrameNotify, "receipt": FrameReceipt,
		"ping": FramePing, "pong": FramePong,
	}
	for want, got := range kinds {
		if got != want {
			t.Errorf("constant %q: got %q", want, got)
		}
	}
}

func TestFrameRoundTrip(t *testing.T) {
	f := Frame{
		Kind:        FrameNotify,
		From:        "hub",
		To:          "conn-1",
		RequestID:   "req-7",
		RecipientID: "agent-1",
		ChannelID:   "ch-1",
		Payload:     []byte(`{"text":"hi"}`),
	}
	data, err := MarshalFrame(f)
	if err != nil {
		t.Fatalf("MarshalFrame: %v", err)
	}
	got, err := UnmarshalFrame(data)
	if err != nil {
		t.Fatalf("UnmarshalFrame: %v", err)
	}
	if got.Kind != f.Kind || got.To != f.To || got.From != f.From ||
		got.RequestID != f.RequestID || got.RecipientID != f.RecipientID ||
		got.ChannelID != f.ChannelID || string(got.Payload) != string(f.Payload) {
		t.Fatalf("round trip mismatch: %+v", got)
	}
}

func TestUnmarshalFrameUnknownKind(t *testing.T) {
	if _, err := UnmarshalFrame([]byte(`{"kind":"wire"}`)); !errors.Is(err, ErrUnknownKind) {
		t.Fatalf("want ErrUnknownKind, got %v", err)
	}
	if _, err := MarshalFrame(Frame{Kind: "wire"}); !errors.Is(err, ErrUnknownKind) {
		t.Fatalf("MarshalFrame want ErrUnknownKind, got %v", err)
	}
}

func TestLocalLinkPingPong(t *testing.T) {
	ll := NewLocalLink()
	defer ll.Close()

	a, err := ll.Dial("a")
	if err != nil {
		t.Fatalf("Dial a: %v", err)
	}
	b, err := ll.Dial("b")
	if err != nil {
		t.Fatalf("Dial b: %v", err)
	}
	ctx := context.Background()

	// The server side sees the dialed links.
	accepted, err := ll.Accept(ctx)
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if accepted != b && accepted != a {
		// Accept may hand back either dial depending on goroutine timing;
		// it must be one of them and usable for Recv.
		t.Fatalf("Accept returned a foreign link: %#v", accepted)
	}

	if err := a.Send(Frame{Kind: FramePing, From: "a", To: "b"}); err != nil {
		t.Fatalf("Send ping: %v", err)
	}
	f, err := b.Recv(ctx)
	if err != nil {
		t.Fatalf("Recv ping: %v", err)
	}
	if f.Kind != FramePing || f.From != "a" {
		t.Fatalf("ping mismatch: %+v", f)
	}
	if err := b.Send(Frame{Kind: FramePong, From: "b", To: "a"}); err != nil {
		t.Fatalf("Send pong: %v", err)
	}
	f, err = a.Recv(ctx)
	if err != nil {
		t.Fatalf("Recv pong: %v", err)
	}
	if f.Kind != FramePong || f.From != "b" || f.To != "a" {
		t.Fatalf("pong mismatch: %+v", f)
	}
}

func TestLocalLinkUnknownAddress(t *testing.T) {
	ll := NewLocalLink()
	defer ll.Close()
	a, _ := ll.Dial("a")
	if err := a.Send(Frame{Kind: FramePing, To: "nobody"}); !errors.Is(err, ErrUnknownAddress) {
		t.Fatalf("want ErrUnknownAddress, got %v", err)
	}
	if err := a.Send(Frame{Kind: FramePing}); !errors.Is(err, ErrUnknownAddress) {
		t.Fatalf("empty To: want ErrUnknownAddress, got %v", err)
	}
}

func TestLocalLinkCloseWakesRecv(t *testing.T) {
	ll := NewLocalLink()
	a, _ := ll.Dial("a")

	errc := make(chan error, 1)
	go func() {
		_, err := a.Recv(context.Background())
		errc <- err
	}()
	time.Sleep(10 * time.Millisecond) // let Recv block
	if err := a.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case err := <-errc:
		if !errors.Is(err, ErrLinkClosed) {
			t.Fatalf("want ErrLinkClosed, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Recv did not wake on Close")
	}

	// The closed address is freed; dialing it again hands out a fresh link.
	fresh, err := ll.Dial("a")
	if err != nil {
		t.Fatalf("re-dial freed addr: %v", err)
	}
	if err := fresh.Send(Frame{Kind: FramePing, To: "a"}); err != nil {
		t.Fatalf("send to fresh self: %v", err)
	}
	if _, err := fresh.Recv(context.Background()); err != nil {
		t.Fatalf("fresh link recv: %v", err)
	}
}

func TestLocalLinkCloseWakesAccept(t *testing.T) {
	ll := NewLocalLink()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go func() {
		time.Sleep(10 * time.Millisecond)
		ll.Close()
	}()
	if _, err := ll.Accept(ctx); err == nil {
		t.Fatal("want Accept to fail after Close, got nil")
	}
	if _, err := ll.Dial("x"); !errors.Is(err, ErrNoConnection) {
		t.Fatalf("dial after Close: want ErrNoConnection, got %v", err)
	}
}

func TestLocalLinkRecvContextCancelled(t *testing.T) {
	ll := NewLocalLink()
	a, _ := ll.Dial("a")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := a.Recv(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
}

func TestLocalLinkOverflow(t *testing.T) {
	ll := NewLocalLink(WithLinkBuffer(1))
	defer ll.Close()
	a, _ := ll.Dial("a")
	b, _ := ll.Dial("b")
	_ = a
	if err := b.Send(Frame{Kind: FramePing, To: "a"}); err != nil {
		t.Fatalf("first send: %v", err)
	}
	if err := b.Send(Frame{Kind: FramePing, To: "a"}); !errors.Is(err, ErrLinkOverflow) {
		t.Fatalf("second send: want ErrLinkOverflow, got %v", err)
	}
}
