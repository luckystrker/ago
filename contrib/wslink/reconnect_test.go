package wslink

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/luckystrker/ago/network"
	"github.com/luckystrker/ago/network/transport"
)

// recordingLink wraps the hub's end of a WebSocket and remembers the frames the
// hub received.
type recordingLink struct {
	transport.Link
	mu     *sync.Mutex
	frames *[]transport.Frame // shared by every link: all frames the hub received
	own    *[]transport.Frame // this link's frames
}

func (r recordingLink) Recv(ctx context.Context) (transport.Frame, error) {
	f, err := r.Link.Recv(ctx)
	if err == nil {
		r.mu.Lock()
		*r.frames = append(*r.frames, f)
		*r.own = append(*r.own, f)
		r.mu.Unlock()
	}
	return f, err
}

type textRec struct {
	mu    sync.Mutex
	texts []string
}

func (t *textRec) option() network.AgentOption {
	return network.WithEnvironment(func(env network.Envelope) (string, error) {
		if env.EventType == network.EnvText {
			var p struct {
				Text string `json:"text"`
			}
			_ = json.Unmarshal(env.Payload, &p)
			t.mu.Lock()
			t.texts = append(t.texts, p.Text)
			t.mu.Unlock()
		}
		return "", nil
	})
}

func (t *textRec) get() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]string(nil), t.texts...)
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// A WebSocket client whose connection drops re-dials by itself, re-attaches its
// agent with a hello that carries its per-channel since mark, and receives what
// arrived meanwhile — once, in order — without the application doing anything.
func TestReconnectPicksUpWhereItLeftOff(t *testing.T) {
	hub, err := network.OpenHub(network.WithAckTimeout(3*time.Second),
		network.WithTTLSweepInterval(0), network.WithExpectationSweepInterval(0))
	if err != nil {
		t.Fatal(err)
	}
	defer hub.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var mu sync.Mutex
	type served struct {
		link transport.Link
		own  *[]transport.Frame
	}
	var links []served
	var received []transport.Frame
	handler, err := Handler(ServerConfig{Handle: func(link transport.Link) {
		own := &[]transport.Frame{}
		mu.Lock()
		links = append(links, served{link, own})
		mu.Unlock()
		_ = hub.ServeLink(ctx, recordingLink{Link: link, mu: &mu, frames: &received, own: own})
	}})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(handler)
	defer srv.Close()
	url := wsURL(srv)

	events := make(chan network.ConnectionEvent, 32)
	pol := network.ReconnectPolicy{Initial: 5 * time.Millisecond, Max: 40 * time.Millisecond, Factor: 2, MaxAttempts: 100}
	hcA, err := network.ConnectHub(ctx, RemoteConfig(url, nil, pol, nil))
	if err != nil {
		t.Fatal(err)
	}
	defer hcA.Close()
	hcB, err := network.ConnectHub(ctx, RemoteConfig(url, nil, pol, func(ev network.ConnectionEvent) { events <- ev }))
	if err != nil {
		t.Fatal(err)
	}
	defer hcB.Close()

	log := &textRec{}
	bob, err := hcB.Register("bob", log.option())
	if err != nil {
		t.Fatal(err)
	}
	alice, err := hcA.Register("alice", network.WithEnvironment(func(network.Envelope) (string, error) { return "", nil }))
	if err != nil {
		t.Fatal(err)
	}
	ch, err := alice.CreateChannel(network.ChannelManifest{Type: network.TypeConversation}, bob.AgentID())
	if err != nil {
		t.Fatal(err)
	}
	if err := alice.Say(ch.ID, "m1"); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "m1", func() bool { return len(log.get()) == 1 })
	waitUntil(t, "m1 acked", func() bool { return hub.InboxCursor(bob.AgentID(), ch.ID) != "" })

	// The server drops bob's WebSocket (the link whose register request names him).
	mu.Lock()
	var bobLink transport.Link
	for _, sv := range links {
		for _, f := range *sv.own {
			if f.Kind == transport.FrameRequest && f.Op == network.OpRegister && strings.Contains(string(f.Payload), `"bob"`) {
				bobLink = sv.link
			}
		}
	}
	mu.Unlock()
	if bobLink == nil {
		t.Fatal("bob's link not found")
	}
	_ = bobLink.Close()
	for lost := false; !lost; {
		select {
		case ev := <-events:
			lost = ev.Kind == network.ConnLost
		case <-time.After(5 * time.Second):
			t.Fatal("bob's client never noticed the drop")
		}
	}
	for _, m := range []string{"m2", "m3"} {
		if err := alice.Say(ch.ID, m); err != nil {
			t.Fatal(err)
		}
	}
	for back := false; !back; {
		select {
		case ev := <-events:
			back = ev.Kind == network.ConnReconnected
		case <-time.After(10 * time.Second):
			t.Fatal("bob's client never reconnected")
		}
	}
	waitUntil(t, "m2 and m3", func() bool { return len(log.get()) >= 3 })
	if err := alice.Say(ch.ID, "m4"); err != nil { // live again
		t.Fatal(err)
	}
	waitUntil(t, "m4", func() bool { return len(log.get()) >= 4 })
	time.Sleep(50 * time.Millisecond)
	if got := log.get(); len(got) != 4 || got[0] != "m1" || got[1] != "m2" || got[2] != "m3" || got[3] != "m4" {
		t.Fatalf("bob received %v, want m1..m4 once each in order", got)
	}

	// The re-attach was a hello that carries the per-channel since mark.
	mu.Lock()
	defer mu.Unlock()
	var hellos []network.HelloBody
	for _, f := range received {
		if f.Kind == transport.FrameHello {
			var b network.HelloBody
			if err := json.Unmarshal(f.Payload, &b); err == nil && b.Name == "bob" {
				hellos = append(hellos, b)
			}
		}
	}
	if len(hellos) != 1 {
		t.Fatalf("expected one re-attach hello for bob, got %d", len(hellos))
	}
	if mark := hellos[0].Since[ch.ID]; mark == "" {
		t.Fatalf("the hello carries no since mark for the channel: %+v", hellos[0])
	}
}

// The policy's attempt limit applies: when the server is gone for good the
// client gives up and says so, and requests fail typed.
func TestReconnectGivesUpWhenTheServerIsGone(t *testing.T) {
	hub, err := network.OpenHub(network.WithTTLSweepInterval(0), network.WithExpectationSweepInterval(0))
	if err != nil {
		t.Fatal(err)
	}
	defer hub.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	handler, _ := HubHandler(ctx, hub, ServerConfig{})
	srv := httptest.NewServer(handler)
	events := make(chan network.ConnectionEvent, 32)
	pol := network.ReconnectPolicy{Initial: time.Millisecond, Max: 2 * time.Millisecond, Factor: 2, MaxAttempts: 3}
	hc, err := network.ConnectHub(ctx, RemoteConfig(wsURL(srv), nil, pol, func(ev network.ConnectionEvent) { events <- ev }))
	if err != nil {
		t.Fatal(err)
	}
	defer hc.Close()
	if _, err := hc.Register("alice"); err != nil {
		t.Fatal(err)
	}
	cancel()    // the hub stops serving: every WebSocket is closed
	srv.Close() // and the listener is gone for good
	var kinds []string
	timeout := time.After(10 * time.Second)
	for len(kinds) == 0 || kinds[len(kinds)-1] != network.ConnGaveUp {
		select {
		case ev := <-events:
			kinds = append(kinds, ev.Kind)
			if ev.Kind == network.ConnGaveUp && (ev.Attempt != 3 || ev.Err == nil) {
				t.Fatalf("gave up after %d attempts with %v", ev.Attempt, ev.Err)
			}
		case <-timeout:
			t.Fatalf("events: %v", kinds)
		}
	}
	if kinds[0] != network.ConnLost {
		t.Fatalf("event order: %v", kinds)
	}
}
