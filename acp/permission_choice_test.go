package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/luckystrker/ago/agent"
	"github.com/luckystrker/ago/events"
)

// permOptionsEnv carries the permission options (JSON array) the
// acp-permission-options-agent offers in its one session/request_permission.
const permOptionsEnv = "AGO_ACP_TEST_PERM_OPTIONS"

// runPermOptionsAgent raises one session/request_permission with the options
// from the environment, in the agent's order, and streams back what the
// client's wire answer selected: "selected:<optionId>" or "cancelled".
func runPermOptionsAgent(in *os.File, out *os.File) {
	w := bufio.NewWriter(out)
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 0, 64*1024), scanBuffer)
	var options []map[string]any
	_ = json.Unmarshal([]byte(os.Getenv(permOptionsEnv)), &options)
	asked := false
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var req childRequest
		if err := json.Unmarshal([]byte(line), &req); err != nil || req.ID == nil {
			continue
		}
		switch req.Method {
		case "initialize":
			childReply(w, *req.ID, map[string]any{
				"protocolVersion":   protocolVersion,
				"agentCapabilities": map[string]any{},
				"agentInfo":         map[string]any{"name": "perm-options", "version": "1.0"},
			})
		case "session/new":
			childReply(w, *req.ID, map[string]any{"sessionId": "sess-1"})
		case "session/prompt":
			if !asked {
				asked = true
				answer := childCall(w, sc, 9001, "session/request_permission", map[string]any{
					"sessionId": "sess-1",
					"toolCall": map[string]any{
						"title": "git status", "kind": "execute",
						"locations": []map[string]any{{"path": "/repo", "line": 3}},
						"rawInput":  map[string]any{"command": "git status"},
					},
					"options": options,
				})
				got := "malformed"
				if inner, ok := answer["outcome"].(map[string]any); ok {
					if inner["outcome"] == OutcomeCancelled {
						got = "cancelled"
					} else if id, ok := inner["optionId"].(string); ok {
						got = "selected:" + id
					}
				}
				childNotify(w, "session/update", updateParams{SessionID: "sess-1", Update: agentMessageChunk(got)})
			}
			childReply(w, *req.ID, map[string]any{"stopReason": "end_turn"})
		default:
			childError(w, *req.ID, codeMethodNotFound, "method not implemented")
		}
	}
}

// geminiLayout is the option order Gemini CLI offers: the remembered option
// first, so "the first allow option" is allow_always.
const geminiLayout = `[
 {"optionId":"always-1","name":"Allow for this session","kind":"allow_always"},
 {"optionId":"once-1","name":"Allow once","kind":"allow_once"},
 {"optionId":"no-1","name":"Reject","kind":"reject_once"}]`

const noOnceLayout = `[
 {"optionId":"always-1","name":"Allow for this session","kind":"allow_always"},
 {"optionId":"no-1","name":"Reject","kind":"reject_once"}]`

const onlyAlwaysLayout = `[
 {"optionId":"always-1","name":"Allow for this session","kind":"allow_always"}]`

const rejectFirstLayout = `[
 {"optionId":"no-a","name":"Reject always","kind":"reject_always"},
 {"optionId":"no-1","name":"Reject","kind":"reject_once"},
 {"optionId":"once-1","name":"Allow once","kind":"allow_once"}]`

// permissionOutcome runs the options agent against a bridge configured by
// setup and returns what the agent saw on the wire.
func permissionOutcome(t *testing.T, options string, setup func(*Bridge)) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	env := append(os.Environ(), roleEnv+"="+rolePermOpts, permOptionsEnv+"="+options)
	b, err := NewBridge(exe, nil, env)
	if err != nil {
		t.Fatalf("NewBridge: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })
	setup(b)
	id, err := b.SessionNew(t.TempDir())
	if err != nil {
		t.Fatalf("SessionNew: %v", err)
	}
	out, err := b.Prompt(id, "hi")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	return out
}

// TestBridgePolicyYesNeverSelectsAllowAlways: a true Permission policy means
// "this call once". The agent's option order decides nothing — GHSA-3r6w-7pcg-c4x2.
func TestBridgePolicyYesNeverSelectsAllowAlways(t *testing.T) {
	yes := func(b *Bridge) { b.Permission = func(string, string) bool { return true } }
	no := func(b *Bridge) { b.Permission = func(string, string) bool { return false } }
	tests := []struct {
		name, options string
		setup         func(*Bridge)
		want          string
	}{
		{"gemini layout, yes: the one-time option", geminiLayout, yes, "selected:once-1"},
		{"no one-time option, yes: refused, not widened", noOnceLayout, yes, "selected:no-1"},
		{"only allow_always, yes: cancelled, not widened", onlyAlwaysLayout, yes, "cancelled"},
		{"deny prefers reject_once whatever the order", rejectFirstLayout, no, "selected:no-1"},
		{"deny on the gemini layout", geminiLayout, no, "selected:no-1"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := permissionOutcome(t, tc.options, tc.setup); got != tc.want {
				t.Errorf("wire outcome = %q, want %q", got, tc.want)
			}
		})
	}
}

// collisionLayout: ids that spell the approval words with the opposite meaning —
// the reject option is called "y", the allow option "n".
const collisionLayout = `[
 {"optionId":"y","name":"Reject","kind":"reject_once"},
 {"optionId":"n","name":"Allow once","kind":"allow_once"}]`

// askOutcome runs the options agent through the provider with the "ask" policy:
// the hook is the human. It returns what the agent saw on the wire and the
// request the human was shown.
func askOutcome(t *testing.T, options string, answer *events.HumanMessage) (string, *events.HumanInputRequest) {
	t.Helper()
	var shown *events.HumanInputRequest
	hook := func(_ context.Context, req *events.HumanInputRequest) (*events.HumanMessage, error) {
		cp := *req
		shown = &cp
		return answer, nil
	}
	cfg, _ := providerConfig(t, rolePermOpts, "")
	cfg.Env[permOptionsEnv] = options
	a := agent.New("acp-agent", "", agent.WithConfig(cfg), agent.WithHitlHook(hook))
	reply, err := askWithin(t, a, 20*time.Second, "go")
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	return reply.Body(), shown
}

// TestProviderAskOffersAgentOptionsAsChoices: the human sees every option the
// agent offered, as choices keyed by option id, plus what the call is.
func TestProviderAskOffersAgentOptionsAsChoices(t *testing.T) {
	_, shown := askOutcome(t, geminiLayout, &events.HumanMessage{Content: "no"})
	if shown == nil {
		t.Fatal("the human was never asked")
	}
	if raw, err := json.Marshal(shown); err == nil {
		t.Logf("request shown to the human: %s", raw)
	}
	want := []events.ApprovalChoice{
		{Value: "always-1", Label: "Allow for this session"},
		{Value: "once-1", Label: "Allow once"},
		{Value: "no-1", Label: "Reject"},
	}
	if !reflect.DeepEqual(shown.Choices, want) {
		t.Errorf("choices = %+v, want %+v", shown.Choices, want)
	}
	for _, frag := range []string{"permission for: git status", "execute", "/repo:3", "git status", "allow_always"} {
		if !strings.Contains(shown.Content, frag) {
			t.Errorf("prompt %q does not show %q", shown.Content, frag)
		}
	}
}

// TestProviderAskAnswers: what each kind of human answer selects on the wire.
func TestProviderAskAnswers(t *testing.T) {
	tests := []struct {
		name, options string
		answer        *events.HumanMessage
		want          string
	}{
		{"yes is one-time on the gemini layout", geminiLayout, &events.HumanMessage{Content: "yes"}, "selected:once-1"},
		{"y is one-time", geminiLayout, &events.HumanMessage{Content: "y"}, "selected:once-1"},
		{"1 is one-time", geminiLayout, &events.HumanMessage{Content: "1"}, "selected:once-1"},
		{"yes with no one-time option refuses", noOnceLayout, &events.HumanMessage{Content: "yes"}, "selected:no-1"},
		{"always is the remembered option", geminiLayout, &events.HumanMessage{Content: "always"}, "selected:always-1"},
		{"always without a remembered option is this call only", rejectFirstLayout, &events.HumanMessage{Content: "always"}, "selected:once-1"},
		{"anything else refuses", geminiLayout, &events.HumanMessage{Content: "perhaps"}, "selected:no-1"},
		{"an option id typed as words is not a pick", geminiLayout, &events.HumanMessage{Content: "always-1"}, "selected:no-1"},
		{"a pick selects exactly that option", geminiLayout, events.NewApprovalChoiceMessage("", "always-1"), "selected:always-1"},
		{"a pick of the reject option", geminiLayout, events.NewApprovalChoiceMessage("", "no-1"), "selected:no-1"},
		{"a pick of an id that was not offered refuses", geminiLayout, events.NewApprovalChoiceMessage("", "once-9"), "selected:no-1"},
		{"a pick is not read as words: id yes", `[{"optionId":"yes","name":"Reject","kind":"reject_once"},{"optionId":"o","name":"Allow","kind":"allow_once"}]`, events.NewApprovalChoiceMessage("", "yes"), "selected:yes"},
		// The collisions: the reject option is called "y", the allow option "n".
		{"collision: picking reject-id y selects it", collisionLayout, events.NewApprovalChoiceMessage("", "y"), "selected:y"},
		{"collision: the word y does not select the option y", collisionLayout, &events.HumanMessage{Content: "y"}, "selected:n"},
		{"collision: the word n refuses, it does not select the allow option n", collisionLayout, &events.HumanMessage{Content: "n"}, "selected:y"},
		{"collision: picking allow-id n selects it", collisionLayout, events.NewApprovalChoiceMessage("", "n"), "selected:n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := askOutcome(t, tc.options, tc.answer)
			if got != tc.want {
				t.Errorf("wire outcome = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestDecisionAllowedFollowsKind: a selected reject option is a refusal
// whatever its id; the kind of the selected option decides, not the id.
func TestDecisionAllowedFollowsKind(t *testing.T) {
	sel := func(id, kind string) Decision {
		return Decision{Outcome: OutcomeSelected, OptionID: id, Kind: kind}
	}
	tests := []struct {
		name string
		d    Decision
		want bool
	}{
		{"allow_once", sel("a", kindAllowOnce), true},
		{"allow_always", sel("a", kindAllowAlways), true},
		{"reject_once with an ordinary id", sel("a", kindRejectOnce), false},
		{"reject_always", sel("a", kindRejectAlways), false},
		{"reject_once whose id is allow", sel("allow", kindRejectOnce), false},
		{"no kind known, synthesized allow", Decision{Outcome: OutcomeSelected, OptionID: "allow"}, true},
		{"no kind known, synthesized reject", Decision{Outcome: OutcomeSelected, OptionID: "reject"}, false},
		{"cancelled", Decision{Outcome: OutcomeCancelled}, false},
		{"zero", Decision{}, false},
	}
	for _, tc := range tests {
		if got := tc.d.Allowed(); got != tc.want {
			t.Errorf("%s: Allowed() = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestResolvedDecisionCarriesKind: the decision the bridge resolves knows the
// kind of the option it selected, so Allowed() reads the kind.
func TestResolvedDecisionCarriesKind(t *testing.T) {
	opts := []permissionOption{
		{OptionID: "allow", Name: "x", Kind: kindRejectOnce}, // an id that lies about its kind
		{OptionID: "o", Name: "y", Kind: kindAllowOnce},
	}
	d := resolvePermission(opts, PermissionReply{Text: "allow", Choice: true})
	if d.Kind != kindRejectOnce || d.Allowed() {
		t.Errorf("picked reject_once option: %+v Allowed=%v, want a refusal", d, d.Allowed())
	}
	d = resolvePermission(opts, PermissionReply{Text: "yes"})
	if d.OptionID != "o" || !d.Allowed() {
		t.Errorf("yes: %+v, want the allow_once option", d)
	}
}
