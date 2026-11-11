package network

import (
	"errors"
	"reflect"
	"testing"
)

func TestSetAndDeleteContextOnWorkflowChannel(t *testing.T) {
	h := newTestHub(t)
	hc := NewHubClient(h)
	alice, err := hc.Register("alice")
	if err != nil {
		t.Fatal(err)
	}
	bob, err := hc.Register("bob")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = alice.Close(); _ = bob.Close() }()
	graph, _ := NewRoundRobinGraph([]string{alice.AgentID(), bob.AgentID()}, 0)
	ch, err := alice.CreateChannel(ChannelManifest{Type: TypeWorkflow, Knobs: map[string]any{"graph": graph.ToDict()}}, bob.AgentID())
	if err != nil {
		t.Fatal(err)
	}

	vars := func() map[string]any {
		got, _ := h.Channel(ch.ID)
		return got.AdapterState.(*workflowState).ContextVars
	}
	if err := alice.SetContext(ch.ID, "category", "billing"); err != nil {
		t.Fatalf("set: %v", err)
	}
	// Loose semantics: the non-expected speaker may set too, out of turn.
	if err := bob.SetContext(ch.ID, "priority", float64(2)); err != nil {
		t.Fatalf("set by the other participant: %v", err)
	}
	if want := map[string]any{"category": "billing", "priority": float64(2)}; !reflect.DeepEqual(vars(), want) {
		t.Fatalf("vars after sets: %v", vars())
	}
	if err := alice.DeleteContext(ch.ID, "category"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := alice.DeleteContext(ch.ID, "never-set"); err != nil {
		t.Fatalf("deleting an unset key is a no-op: %v", err)
	}
	if want := map[string]any{"priority": float64(2)}; !reflect.DeepEqual(vars(), want) {
		t.Fatalf("vars after delete: %v", vars())
	}
	// The helpers rode the wire as ordinary context envelopes.
	wal, _ := h.ReadWAL(ch.ID)
	n := 0
	for _, e := range wal {
		if e.EventType == EnvContextSet {
			n++
		}
	}
	if n != 4 {
		t.Fatalf("context envelopes in WAL: %d", n)
	}
}

func TestContextHelpersRejectNonWorkflowChannels(t *testing.T) {
	h := newTestHub(t)
	hc := NewHubClient(h)
	alice, _ := hc.Register("alice")
	bob, _ := hc.Register("bob")
	defer func() { _ = alice.Close(); _ = bob.Close() }()
	ch, err := alice.CreateChannel(ChannelManifest{Type: TypeConversation}, bob.AgentID())
	if err != nil {
		t.Fatal(err)
	}
	before, _ := h.ReadWAL(ch.ID)

	if err := alice.SetContext(ch.ID, "k", "v"); !errors.Is(err, ErrProtocol) {
		t.Fatalf("SetContext on a conversation channel: %v", err)
	}
	if err := alice.DeleteContext(ch.ID, "k"); !errors.Is(err, ErrProtocol) {
		t.Fatalf("DeleteContext on a conversation channel: %v", err)
	}
	if after, _ := h.ReadWAL(ch.ID); len(after) != len(before) {
		t.Fatal("the guard must reject before anything is posted")
	}
	if err := alice.SetContext("no-such-channel", "k", "v"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown channel: %v", err)
	}
}
