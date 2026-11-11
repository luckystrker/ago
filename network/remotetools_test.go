package network

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/luckystrker/ago/tools"
)

// remoteToolsRig: alice (the caller) and bob (a peer with a resume) on a hub
// reached over the link, each on its own connection.
func remoteToolsRig(t *testing.T) (*wireRig, *HubClient, *AgentClient, *AgentClient, []tools.Tool) {
	t.Helper()
	rig := newWireRig(t)
	hcA, hcB := rig.connect(), rig.connect()
	bob, err := hcB.Register("bob",
		WithAgentResume(Resume{Capabilities: []string{"poetry"}}),
		WithAgentFunc(func(msg string) (string, error) { return "bob says: 42", nil }))
	if err != nil {
		t.Fatal(err)
	}
	alice, err := hcA.Register("alice", WithAgentResume(Resume{Capabilities: []string{"math"}}))
	if err != nil {
		t.Fatal(err)
	}
	set, err := ClientTools(hcA, alice)
	if err != nil {
		t.Fatalf("ClientTools over a remote hub: %v", err)
	}
	return rig, hcA, alice, bob, set
}

func TestRemoteClientToolsPeersChannelsTasksContext(t *testing.T) {
	rig, hcA, alice, bob, set := remoteToolsRig(t)

	// peers: discovery over the wire.
	found := toolResultList(t, callTool(t, set, "peers", `{"action":"find","capability":"poetry"}`))
	if len(found) != 1 || found[0]["name"] != "bob" {
		t.Fatalf("peers find: %v", found)
	}
	all := toolResultList(t, callTool(t, set, "peers", `{"action":"find"}`))
	if len(all) != 1 { // the caller is not listed
		t.Fatalf("peers find all: %v", all)
	}
	desc := toolResultJSON(t, callTool(t, set, "peers", `{"action":"describe","name":"bob"}`))
	if pass, _ := desc["passport"].(map[string]any); pass["agent_id"] != bob.AgentID() {
		t.Fatalf("peers describe: %v", desc)
	}

	// channels: open → list → info → close, all through the link.
	open := toolResultJSON(t, callTool(t, set, "channels", `{"action":"open","type":"conversation","target":"bob","message":"hello peer"}`))
	chID, _ := open["channel_id"].(string)
	if chID == "" {
		t.Fatalf("channels open: %v", open)
	}
	listed := toolResultList(t, callTool(t, set, "channels", `{"action":"list"}`))
	if len(listed) != 1 || listed[0]["channel_id"] != chID {
		t.Fatalf("channels list: %v", listed)
	}
	info := toolResultJSON(t, callTool(t, set, "channels", `{"action":"info","channel_id":"`+chID+`"}`))
	if info["channel_id"] != chID {
		t.Fatalf("channels info: %v", info)
	}

	// context: search and quote read the channel log through read_wal.
	waitFor(t, func() bool {
		log, _ := rig.hub.ReadWAL(chID)
		return countText(log, "hello peer") == 1
	})
	hits := toolResultList(t, callTool(t, set, "context", `{"action":"search","query":"hello","channel_id":"`+chID+`"}`))
	if len(hits) == 0 {
		t.Fatalf("context search found nothing")
	}

	// tasks: a task alice observes is listed for her, hidden from others.
	if err := hcA.Call(context.Background(), OpObserveTask, map[string]any{"metadata": map[string]any{"task_id": "tk-1", "owner_id": alice.AgentID()}}, nil); err != nil {
		t.Fatal(err)
	}
	tasks := toolResultList(t, callTool(t, set, "tasks", `{"action":"list","scope":"own"}`))
	if len(tasks) != 1 || tasks[0]["task_id"] != "tk-1" {
		t.Fatalf("tasks list: %v", tasks)
	}
	status := toolResultJSON(t, callTool(t, set, "tasks", `{"action":"status","task_id":"tk-1"}`))
	if status["task_id"] != "tk-1" {
		t.Fatalf("tasks status: %v", status)
	}

	closed := callTool(t, set, "channels", `{"action":"close","channel_id":"`+chID+`"}`)
	if s, _ := closed.(string); strings.HasPrefix(s, "Error") {
		t.Fatalf("channels close: %v", closed)
	}
	waitForChannel(t, rig.hub, chID, ChannelClosed)
}

func TestRemoteDelegateToolConsult(t *testing.T) {
	rig, _, _, _, set := remoteToolsRig(t)
	got := callTool(t, set, "delegate", `{"target":"bob","prompt":"what is the answer?","timeout":10}`)
	if s, _ := got.(string); s != "bob says: 42" {
		t.Fatalf("remote delegate = %v", got)
	}
	for _, ch := range rig.hub.ListChannels() {
		if ch.Manifest.Type == TypeConsulting && !ch.State.Terminal() {
			t.Fatalf("consulting channel %s never closed", ch.ID)
		}
	}
	if got := callTool(t, set, "delegate", `{"target":"nobody","prompt":"hi","timeout":1}`); !strings.Contains(toolText(got), "bob") {
		t.Fatalf("unknown target must list the peers: %v", got)
	}
}

func toolText(v any) string {
	s, _ := v.(string)
	return s
}

// ChannelTools over a remote hub folds the adapter state from the channel log
// client-side and offers the same verbs as in-process.
func TestRemoteChannelToolsPerAdapter(t *testing.T) {
	rig := newWireRig(t)
	hcA, hcB, hcC := rig.connect(), rig.connect(), rig.connect()
	alice, _ := hcA.Register("alice")
	bob, _ := hcB.Register("bob")
	carol, _ := hcC.Register("carol")

	// Consulting: say follows the floor of the 1Q1R cycle.
	cons, err := alice.CreateChannel(ChannelManifest{Type: TypeConsulting}, bob.AgentID())
	if err != nil {
		t.Fatal(err)
	}
	if got := channelToolNames(t, alice, cons.ID); !reflect.DeepEqual(got, []string{"say"}) {
		t.Fatalf("consulting initiator: %v", got)
	}
	if got := channelToolNames(t, bob, cons.ID); len(got) != 0 {
		t.Fatalf("consulting respondent before the question: %v", got)
	}
	alice.SuppressHandler(cons.ID)
	bob.SuppressHandler(cons.ID)
	if err := alice.Say(cons.ID, "q"); err != nil {
		t.Fatal(err)
	}
	if got := channelToolNames(t, alice, cons.ID); len(got) != 0 {
		t.Fatalf("consulting initiator after asking: %v", got)
	}
	if got := channelToolNames(t, bob, cons.ID); !reflect.DeepEqual(got, []string{"say"}) {
		t.Fatalf("consulting respondent after the question: %v", got)
	}
	conv, _ := alice.CreateChannel(ChannelManifest{Type: TypeConversation}, bob.AgentID())
	if a, b := channelToolNames(t, alice, conv.ID), channelToolNames(t, bob, conv.ID); !reflect.DeepEqual(a, []string{"say"}) || !reflect.DeepEqual(b, []string{"say"}) {
		t.Fatalf("conversation: %v %v", a, b)
	}
	disc, _ := alice.CreateChannel(ChannelManifest{Type: TypeDiscussion}, bob.AgentID(), carol.AgentID())
	if got := channelToolNames(t, alice, disc.ID); len(got) != 0 {
		t.Fatalf("discussion: %v", got)
	}
	// The say tool posts through the link.
	bound, err := alice.ChannelTools(conv.ID)
	if err != nil || len(bound) != 1 {
		t.Fatalf("channel tools: %v %v", bound, err)
	}
	if got := callTool(t, bound, "say", `{"content":"via the tool"}`); strings.HasPrefix(toolText(got), "Error") {
		t.Fatalf("say: %v", got)
	}
	log, _ := rig.hub.ReadWAL(conv.ID)
	if countText(log, "via the tool") != 1 {
		t.Fatalf("the say tool did not post: %d", countText(log, "via the tool"))
	}
	// Closed and unknown channels.
	if err := hcA.Call(context.Background(), OpCloseChannel, closeChannelParams{ChannelID: conv.ID}, nil); err != nil {
		t.Fatal(err)
	}
	if got := channelToolNames(t, alice, conv.ID); len(got) != 0 {
		t.Fatalf("closed channel: %v", got)
	}
	if _, err := alice.ChannelTools("ghost"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown channel: %v", err)
	}
}

func TestRemoteNetworkPlugin(t *testing.T) {
	rig := newWireRig(t)
	hc := rig.connect()
	alice, _ := hc.Register("alice")
	p, err := NewNetworkPlugin(hc, alice)
	if err != nil {
		t.Fatalf("NewNetworkPlugin over a remote hub: %v", err)
	}
	var names []string
	for _, tl := range p.Tools() {
		names = append(names, tl.Name())
	}
	if !reflect.DeepEqual(names, []string{"peers", "channels", "tasks", "context", "delegate"}) {
		t.Fatalf("tools: %v", names)
	}
	if got := p.PromptFragments(); len(got) != 1 || !strings.Contains(got[0], alice.AgentID()) {
		t.Fatalf("prompt fragments: %v", got)
	}
}

// AddResumeExample is one op on the hub: concurrent calls never overwrite each
// other (a client-side read-modify-write would lose examples).
func TestRemoteAddResumeExampleIsAtomic(t *testing.T) {
	rig := newWireRig(t)
	hc := rig.connect()
	alice, _ := hc.Register("alice")
	const n = 24
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := alice.AddExample(ResumeExample{Title: "ex"}); err != nil {
				t.Errorf("add example: %v", err)
			}
		}(i)
	}
	wg.Wait()
	r, _ := rig.hub.ResumeOf(alice.AgentID())
	if len(r.Examples) != n {
		t.Fatalf("%d of %d examples survived concurrent adds", len(r.Examples), n)
	}
	if err := hc.api.AddResumeExample("ghost", ResumeExample{}); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("an agent the connection does not hold: %v", err)
	}
}

var _ = time.Second
