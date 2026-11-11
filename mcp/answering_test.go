package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/luckystrker/ago/config"
	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/hitl"
	"github.com/luckystrker/ago/stream"
	agotesting "github.com/luckystrker/ago/testing"
)

// humanLog is a HITL hook answering every question with a fixed answer.
type humanLog struct {
	mu     sync.Mutex
	asked  []string
	answer string
	delay  time.Duration
}

func (h *humanLog) hook(ctx context.Context, req *events.HumanInputRequest) (*events.HumanMessage, error) {
	h.mu.Lock()
	h.asked = append(h.asked, req.Content)
	h.mu.Unlock()
	select {
	case <-time.After(h.delay):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return &events.HumanMessage{Content: h.answer}, nil
}

func (h *humanLog) questions() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.asked...)
}

// conversation builds a conversation context whose human is the given hook (nil:
// nobody to ask) and whose agent runs the given model config (nil: none).
func conversation(h *humanLog, model config.ModelConfig) *agocontext.Context {
	cx := agocontext.New(stream.NewMemoryStream())
	if h != nil {
		cx.SetHitlHook(h.hook)
	}
	if model != nil {
		cx.SetDependency(modelConfigDependency, model)
	}
	return cx
}

var askPolicy = AnswerPolicy{Elicitation: ElicitationAsk}

// --- elicitation -------------------------------------------------------------------

func TestElicitationRoutedToHumanInput(t *testing.T) {
	for _, kind := range bothKinds {
		t.Run(string(kind), func(t *testing.T) {
			cl, srv := newScenarioClient(t, kind, false, "", 0, WithAnswering(askPolicy))
			human := &humanLog{answer: "blue"}
			out, err := cl.CallToolWith(context.Background(), conversation(human, nil), "elicit", nil)
			if err != nil {
				t.Fatalf("CallToolWith: %v", err)
			}
			if q := human.questions(); len(q) != 1 || q[0] != "Pick a color" {
				t.Fatalf("human was asked %q, want the server's question once", q)
			}
			// The server's view of the answer: accept, with the answer under the
			// form's single property.
			containsAll(t, firstText(t, out), `"action":"accept"`, `"answer":"blue"`)
			if srv != nil {
				// The reply went back on the same session, as a reply (no method).
				var replies int
				for _, r := range srv.log {
					if r.Reply {
						replies++
						if r.Session != "sess-1" {
							t.Errorf("reply carried session %q, want sess-1", r.Session)
						}
					}
				}
				if replies != 1 {
					t.Errorf("server saw %d client replies, want 1", replies)
				}
			}
		})
	}
}

func TestElicitationSlowHumanDoesNotBurnTheExchangeDeadline(t *testing.T) {
	// The human takes longer than the exchange timeout: the clock must stop while
	// the client answers, or every thoughtful human fails the call.
	for _, kind := range bothKinds {
		t.Run(string(kind), func(t *testing.T) {
			cl, _ := newScenarioClient(t, kind, false, "", 250*time.Millisecond, WithAnswering(askPolicy))
			human := &humanLog{answer: "slowly", delay: 600 * time.Millisecond}
			out, err := cl.CallToolWith(context.Background(), conversation(human, nil), "elicit", nil)
			if err != nil {
				t.Fatalf("CallToolWith with a slow human: %v", err)
			}
			containsAll(t, firstText(t, out), `"answer":"slowly"`)
		})
	}
}

func TestElicitationDeclinedWhenNotAnswerable(t *testing.T) {
	cases := []struct {
		name   string
		policy AnswerPolicy
		shape  string
	}{
		{"policy off", AnswerPolicy{}, ""},
		{"explicit decline policy", AnswerPolicy{Elicitation: ElicitationDecline}, ""},
		{"several fields", askPolicy, "multi"},
		{"url mode", askPolicy, "url"},
	}
	for _, kind := range bothKinds {
		for _, tc := range cases {
			t.Run(string(kind)+"/"+tc.name, func(t *testing.T) {
				cl, _ := newScenarioClient(t, kind, false, "", 0, WithAnswering(tc.policy))
				human := &humanLog{answer: "never used"}
				out, err := cl.CallToolWith(context.Background(), conversation(human, nil), "elicit", map[string]any{"shape": tc.shape})
				if err != nil {
					t.Fatalf("CallToolWith: %v", err)
				}
				containsAll(t, firstText(t, out), `"action":"decline"`)
				if q := human.questions(); len(q) != 0 {
					t.Errorf("a declined question reached the human: %q", q)
				}
			})
		}
	}
}

func TestElicitationWithoutHumanChannelFailsTheCall(t *testing.T) {
	// An absent channel is not a refusal: declining on the operator's behalf
	// would invent a decision they never made.
	for _, kind := range bothKinds {
		t.Run(string(kind)+"/no hook", func(t *testing.T) {
			cl, _ := newScenarioClient(t, kind, false, "", 0, WithAnswering(askPolicy))
			_, err := cl.CallToolWith(context.Background(), conversation(nil, nil), "elicit", nil)
			if !errors.Is(err, ErrAnswering) || !errors.Is(err, hitl.ErrNotProvided) {
				t.Fatalf("err = %v, want ErrAnswering wrapping hitl.ErrNotProvided", err)
			}
		})
		t.Run(string(kind)+"/no conversation", func(t *testing.T) {
			cl, _ := newScenarioClient(t, kind, false, "", 0, WithAnswering(askPolicy))
			if _, err := cl.CallTool("elicit", nil); !errors.Is(err, ErrAnswering) {
				t.Fatalf("err = %v, want ErrAnswering", err)
			}
		})
	}
}

// --- sampling ----------------------------------------------------------------------

func TestSamplingThroughCallerHandler(t *testing.T) {
	for _, kind := range bothKinds {
		t.Run(string(kind), func(t *testing.T) {
			var got SamplingRequest
			policy := AnswerPolicy{SamplingHandler: func(_ context.Context, _ *agocontext.Context, req SamplingRequest) (SamplingResult, error) {
				got = req
				return SamplingResult{Text: "4", Model: "tiny", StopReason: "endTurn"}, nil
			}}
			cl, _ := newScenarioClient(t, kind, false, "", 0, WithAnswering(policy))
			out, err := cl.CallToolWith(context.Background(), conversation(nil, nil), "sample", nil)
			if err != nil {
				t.Fatalf("CallToolWith: %v", err)
			}
			containsAll(t, firstText(t, out), `"text":"4"`, `"model":"tiny"`, `"role":"assistant"`, `"stopReason":"endTurn"`)
			if got.SystemPrompt != "be brief" || got.MaxTokens != 16 || len(got.Messages) != 1 || got.Messages[0].Role != "user" {
				t.Fatalf("handler saw %+v", got)
			}
			if txt := textContent(t, got.Messages[0].Parts[0]); txt != "what is 2+2?" {
				t.Errorf("handler saw message %q", txt)
			}
		})
	}
}

func TestSamplingOnTheAgentsOwnModel(t *testing.T) {
	for _, kind := range bothKinds {
		t.Run(string(kind), func(t *testing.T) {
			model := agotesting.NewTestConfig(&events.ModelResponse{
				Message: &events.ModelMessage{Content: "four"}, Model: "scripted-1", FinishReason: "stop",
			})
			cl, _ := newScenarioClient(t, kind, false, "", 0, WithAnswering(AnswerPolicy{Sampling: true}))
			out, err := cl.CallToolWith(context.Background(), conversation(nil, model), "sample", nil)
			if err != nil {
				t.Fatalf("CallToolWith: %v", err)
			}
			containsAll(t, firstText(t, out), `"text":"four"`, `"model":"scripted-1"`, `"stopReason":"stop"`)
			calls := model.Calls()
			if len(calls) != 1 || len(calls[0]) != 1 {
				t.Fatalf("model calls = %v, want one call over the server's one message", calls)
			}
		})
	}
}

func TestSamplingRefusals(t *testing.T) {
	// Sampling and roots have no decline arm on the wire: a request for a
	// capability that was never advertised, or one that cannot be served, fails
	// the tool call (Python parity).
	for _, kind := range bothKinds {
		t.Run(string(kind)+"/not enabled", func(t *testing.T) {
			cl, _ := newScenarioClient(t, kind, false, "", 0)
			if _, err := cl.CallToolWith(context.Background(), conversation(nil, nil), "sample", nil); !errors.Is(err, ErrAnswering) {
				t.Fatalf("err = %v, want ErrAnswering", err)
			}
		})
		t.Run(string(kind)+"/no model configured", func(t *testing.T) {
			cl, _ := newScenarioClient(t, kind, false, "", 0, WithAnswering(AnswerPolicy{Sampling: true}))
			_, err := cl.CallToolWith(context.Background(), conversation(nil, nil), "sample", nil)
			if !errors.Is(err, ErrAnswering) || !strings.Contains(err.Error(), "no model configured") {
				t.Fatalf("err = %v, want ErrAnswering about a missing model", err)
			}
		})
		t.Run(string(kind)+"/handler error", func(t *testing.T) {
			boom := errors.New("budget exhausted")
			policy := AnswerPolicy{SamplingHandler: func(context.Context, *agocontext.Context, SamplingRequest) (SamplingResult, error) {
				return SamplingResult{}, boom
			}}
			cl, _ := newScenarioClient(t, kind, false, "", 0, WithAnswering(policy))
			if _, err := cl.CallToolWith(context.Background(), conversation(nil, nil), "sample", nil); !errors.Is(err, ErrAnswering) || !errors.Is(err, boom) {
				t.Fatalf("err = %v, want ErrAnswering wrapping the handler's error", err)
			}
		})
	}
}

// --- roots and ping ------------------------------------------------------------------

func TestRootsListed(t *testing.T) {
	dir := t.TempDir()
	for _, kind := range bothKinds {
		t.Run(string(kind), func(t *testing.T) {
			cl, _ := newScenarioClient(t, kind, false, "", 0, WithAnswering(AnswerPolicy{Roots: []string{dir}}))
			out, err := cl.CallToolWith(context.Background(), conversation(nil, nil), "roots", nil)
			if err != nil {
				t.Fatalf("CallToolWith: %v", err)
			}
			var res struct {
				Roots []struct{ URI, Name string }
			}
			if err := json.Unmarshal([]byte(strings.TrimPrefix(firstText(t, out), "roots:")), &res); err != nil {
				t.Fatalf("roots reply: %v", err)
			}
			if len(res.Roots) != 1 || res.Roots[0].URI != "file://"+filepath.ToSlash(dir) || res.Roots[0].Name != filepath.Base(dir) {
				t.Fatalf("roots = %+v, want the named directory as a file:// URI", res.Roots)
			}
		})
	}
	t.Run("not enabled", func(t *testing.T) {
		cl, _ := newScenarioClient(t, overHTTP, false, "", 0)
		if _, err := cl.CallToolWith(context.Background(), conversation(nil, nil), "roots", nil); !errors.Is(err, ErrAnswering) {
			t.Fatalf("err = %v, want ErrAnswering", err)
		}
	})
}

func TestPingAnswered(t *testing.T) {
	for _, kind := range bothKinds {
		t.Run(string(kind), func(t *testing.T) {
			// No policy at all: ping needs no capability.
			cl, _ := newScenarioClient(t, kind, false, "", 0)
			out, err := cl.CallTool("ping", nil)
			if err != nil {
				t.Fatalf("CallTool: %v", err)
			}
			if got := firstText(t, out); got != "pong:{}" {
				t.Fatalf("server saw %q, want an empty result object", got)
			}
		})
	}
}

func TestIdlePingAnsweredByTheStdioReader(t *testing.T) {
	// A ping that arrives while no call is waiting is answered too — otherwise a
	// server that probes liveness between calls would drop the session.
	cl, _ := newScenarioClient(t, overStdio, false, "", 0)
	if _, err := cl.CallTool("idle_ping", nil); err != nil {
		t.Fatalf("idle_ping: %v", err)
	}
	time.Sleep(600 * time.Millisecond) // the stub pings 200ms after replying
	deadline := time.Now().Add(5 * time.Second)
	for {
		out, err := cl.CallTool("idle_report", nil)
		if err != nil {
			t.Fatalf("idle_report: %v", err)
		}
		if firstText(t, out) == "true" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the server's idle ping was never answered")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// --- capabilities ---------------------------------------------------------------------

func TestCapabilitiesAdvertisedOnlyWhenEnabled(t *testing.T) {
	handler := func(context.Context, *agocontext.Context, SamplingRequest) (SamplingResult, error) {
		return SamplingResult{}, nil
	}
	cases := []struct {
		name   string
		policy AnswerPolicy
		want   []string // capability names advertised, exactly
	}{
		{"none", AnswerPolicy{}, nil},
		{"decline is not advertised", AnswerPolicy{Elicitation: ElicitationDecline}, nil},
		{"elicitation", askPolicy, []string{"elicitation"}},
		{"sampling flag", AnswerPolicy{Sampling: true}, []string{"sampling"}},
		{"sampling handler", AnswerPolicy{SamplingHandler: handler}, []string{"sampling"}},
		{"roots", AnswerPolicy{Roots: []string{"."}}, []string{"roots"}},
		{"everything", AnswerPolicy{Elicitation: ElicitationAsk, Sampling: true, Roots: []string{"."}}, []string{"elicitation", "roots", "sampling"}},
	}
	for _, kind := range bothKinds {
		for _, modern := range []bool{false, true} {
			mode := ProtocolMode("")
			if modern {
				mode = ProtocolAuto
			}
			for _, tc := range cases {
				name := string(kind) + "/legacy/" + tc.name
				if modern {
					name = string(kind) + "/modern/" + tc.name
				}
				t.Run(name, func(t *testing.T) {
					cl, _ := newScenarioClient(t, kind, modern, mode, 0, WithAnswering(tc.policy))
					out, err := cl.CallTool("caps", nil)
					if err != nil {
						t.Fatalf("CallTool: %v", err)
					}
					var caps map[string]json.RawMessage
					if err := json.Unmarshal([]byte(firstText(t, out)), &caps); err != nil {
						t.Fatalf("capabilities %q: %v", firstText(t, out), err)
					}
					if len(caps) != len(tc.want) {
						t.Fatalf("advertised %v, want exactly %v", caps, tc.want)
					}
					for _, w := range tc.want {
						if _, ok := caps[w]; !ok {
							t.Errorf("capability %q missing from %v", w, caps)
						}
					}
				})
			}
		}
	}
}

func TestAnswerPolicyValidation(t *testing.T) {
	if _, err := New(&MCPServerConfig{URL: "http://127.0.0.1:1/mcp"}, WithAnswering(AnswerPolicy{MaxRounds: -1})); !errors.Is(err, ErrUnsupportedConfig) {
		t.Errorf("negative MaxRounds = %v, want ErrUnsupportedConfig", err)
	}
	if _, err := New(&MCPServerConfig{URL: "http://127.0.0.1:1/mcp"}, WithAnswering(AnswerPolicy{Elicitation: "sometimes"})); !errors.Is(err, ErrUnsupportedConfig) {
		t.Errorf("unknown elicitation policy = %v, want ErrUnsupportedConfig", err)
	}
	if _, err := New(&MCPServerConfig{URL: "http://127.0.0.1:1/mcp", ProtocolMode: "psychic"}); !errors.Is(err, ErrUnsupportedConfig) {
		t.Errorf("unknown protocol mode = %v, want ErrUnsupportedConfig", err)
	}
	if _, err := New(&MCPStdioConfig{Command: "x", ProtocolMode: "psychic"}); !errors.Is(err, ErrUnsupportedConfig) {
		t.Errorf("unknown stdio protocol mode = %v, want ErrUnsupportedConfig", err)
	}
}

// --- input_required and protocol_mode=auto --------------------------------------------

func TestInputRequiredRoundTrip(t *testing.T) {
	for _, kind := range bothKinds {
		t.Run(string(kind), func(t *testing.T) {
			cl, srv := newScenarioClient(t, kind, true, ProtocolAuto, 0, WithAnswering(askPolicy))
			human := &humanLog{answer: "green"}
			out, err := cl.CallToolWith(context.Background(), conversation(human, nil), "needs_input", map[string]any{"rounds": 2})
			if err != nil {
				t.Fatalf("CallToolWith: %v", err)
			}
			// Two rounds of questions, the state token echoed back verbatim, and
			// the server saw the answers keyed by its own request key.
			containsAll(t, firstText(t, out), "completed after 2 rounds", "state=state-2", `"q1"`, `"answer":"green"`)
			if q := human.questions(); len(q) != 2 {
				t.Errorf("human asked %q, want two questions (one per round)", q)
			}
			if srv != nil {
				methods := srv.methods()
				for _, m := range methods {
					if m == "initialize" {
						t.Fatalf("modern session ran the initialize handshake: %v", methods)
					}
				}
				if len(methods) == 0 || methods[0] != "server/discover" {
					t.Fatalf("first request = %v, want server/discover", methods)
				}
				if calls := srv.requests("tools/call"); len(calls) != 3 {
					t.Errorf("server saw %d tools/call requests, want 3 (initial + 2 retries)", len(calls))
				} else {
					if v := calls[2].ProtoVersion; v != "2026-07-28" {
						t.Errorf("MCP-Protocol-Version = %q, want the discovered revision", v)
					}
					var p map[string]json.RawMessage
					_ = json.Unmarshal(calls[2].Params, &p)
					if string(p["requestState"]) != `"state-2"` || p["inputResponses"] == nil || p["arguments"] == nil {
						t.Errorf("retry params = %s, want the original arguments plus requestState and inputResponses", calls[2].Params)
					}
				}
			}
		})
	}
}

func TestInputRequiredMaxRoundsExceeded(t *testing.T) {
	for _, kind := range bothKinds {
		t.Run(string(kind), func(t *testing.T) {
			policy := AnswerPolicy{Elicitation: ElicitationAsk, MaxRounds: 2}
			cl, srv := newScenarioClient(t, kind, true, ProtocolAuto, 0, WithAnswering(policy))
			human := &humanLog{answer: "again"}
			_, err := cl.CallToolWith(context.Background(), conversation(human, nil), "needs_input", map[string]any{"rounds": 99})
			if !errors.Is(err, ErrMaxRounds) {
				t.Fatalf("err = %v, want ErrMaxRounds", err)
			}
			if q := human.questions(); len(q) != 2 {
				t.Errorf("human asked %d questions, want exactly MaxRounds=2", len(q))
			}
			if srv != nil {
				if calls := srv.requests("tools/call"); len(calls) != 3 {
					t.Errorf("server saw %d tools/call requests, want 3 (initial + MaxRounds retries)", len(calls))
				}
			}
			// The bound abandons the call, not the session.
			if _, err := cl.CallTool("caps", nil); err != nil {
				t.Errorf("follow-up call after the bound: %v", err)
			}
		})
	}
}

func TestInputRequiredUnansweredRequestFailsTheCall(t *testing.T) {
	// The server asks for a question to be put to the human, but the operator
	// declined elicitation: the embedded request is answered with a decline, the
	// server then decides; here it asks forever, so the round bound ends it.
	cl, _ := newScenarioClient(t, overHTTP, true, ProtocolAuto, 0, WithAnswering(AnswerPolicy{MaxRounds: 1}))
	_, err := cl.CallToolWith(context.Background(), conversation(nil, nil), "needs_input", map[string]any{"rounds": 9})
	if !errors.Is(err, ErrMaxRounds) {
		t.Fatalf("err = %v, want ErrMaxRounds after one declined round", err)
	}
}

func TestProtocolAutoFallsBackToHandshake(t *testing.T) {
	for _, kind := range bothKinds {
		t.Run(string(kind), func(t *testing.T) {
			cl, srv := newScenarioClient(t, kind, false, ProtocolAuto, 0)
			info, err := cl.Initialize()
			if err != nil {
				t.Fatalf("Initialize: %v", err)
			}
			if info.ProtocolVersion != "2024-11-05" {
				t.Errorf("protocol version = %q, want the handshake's", info.ProtocolVersion)
			}
			if srv != nil {
				methods := srv.methods()
				if len(methods) < 2 || methods[0] != "server/discover" || methods[1] != "initialize" {
					t.Fatalf("requests = %v, want server/discover then the initialize fallback", methods)
				}
				if v := srv.requests("initialize")[0].ProtoVersion; v != "" {
					t.Errorf("handshake carried MCP-Protocol-Version %q", v)
				}
			}
			if _, err := cl.CallTool("echo", nil); err != nil {
				t.Errorf("call after fallback: %v", err)
			}
		})
	}
}

func TestProtocolLegacyNeverProbes(t *testing.T) {
	cl, srv := newScenarioClient(t, overHTTP, true, ProtocolLegacy, 0)
	if _, err := cl.Initialize(); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	for _, m := range srv.methods() {
		if m == "server/discover" {
			t.Fatalf("legacy mode probed server/discover: %v", srv.methods())
		}
	}
}

func TestModernDiscoveryInfo(t *testing.T) {
	cl, _ := newScenarioClient(t, overHTTP, true, ProtocolAuto, 0)
	info, err := cl.Initialize()
	if err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if info.Name != "scenario-modern" || info.ProtocolVersion != "2026-07-28" {
		t.Errorf("info = %+v, want the discovered server and the newest modern revision", info)
	}
}

// --- embedded resources -----------------------------------------------------------------

func TestEmbeddedResourceBlocksReachTheToolResult(t *testing.T) {
	for _, kind := range bothKinds {
		t.Run(string(kind), func(t *testing.T) {
			cl, _ := newScenarioClient(t, kind, false, "", 0)
			out, err := cl.CallTool("resource", nil)
			if err != nil {
				t.Fatalf("CallTool: %v", err)
			}
			if len(out.Parts) != 4 {
				t.Fatalf("%d parts, want 4", len(out.Parts))
			}
			if got := textContent(t, out.Parts[0]); got != "embedded text" {
				t.Errorf("text resource = %q, want the text itself, not block JSON", got)
			}
			img, ok := events.AsValue(out.Parts[1]).(events.BinaryInput)
			if !ok || string(img.Data) != "png-bytes" || img.MediaType != "image/png" || img.Kind != events.BinaryImage {
				t.Errorf("blob resource = %#v, want an image BinaryInput with the decoded bytes", out.Parts[1])
			}
			bin, ok := events.AsValue(out.Parts[2]).(events.BinaryInput)
			if !ok || string(bin.Data) != "bin" || bin.MediaType != "application/octet-stream" || bin.Kind != events.BinaryBinary {
				t.Errorf("blob without media type = %#v, want application/octet-stream binary", out.Parts[2])
			}
			// A resource with neither text nor blob is preserved, never dropped.
			if got := textContent(t, out.Parts[3]); !strings.Contains(got, "file:///d") {
				t.Errorf("payload-less resource = %q, want the block's JSON", got)
			}
		})
	}
}

func TestEmbeddedResourceBadBase64(t *testing.T) {
	_, err := mapContent([]map[string]any{{"type": "resource", "resource": map[string]any{"blob": "***"}}}, nil)
	if !errors.Is(err, ErrProtocol) {
		t.Fatalf("err = %v, want ErrProtocol for a malformed blob", err)
	}
}

// --- end to end through the agent's tool path ---------------------------------------------

func TestRemoteToolAnswersThroughTheTurnConversation(t *testing.T) {
	// The wiring the agent actually uses: Tools(cl) wrappers executing on the
	// conversation of the call, so the elicitation reaches that conversation's
	// human without the caller touching CallToolWith.
	cl, _ := newScenarioClient(t, overHTTP, false, "", 0, WithAnswering(askPolicy))
	ts, err := Tools(cl)
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}
	var tool *remoteTool
	for _, tl := range ts {
		if tl.Name() == "elicit" {
			tool = tl.(*remoteTool)
		}
	}
	if tool == nil {
		t.Fatal("elicit tool not discovered")
	}
	human := &humanLog{answer: "violet"}
	call := events.NewToolCallEvent("elicit")
	ev, err := tool.exec(&call, conversation(human, nil))
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	res, ok := events.AsValue(ev).(events.ToolResultEvent)
	if !ok {
		t.Fatalf("exec returned %T, want a tool result", events.AsValue(ev))
	}
	containsAll(t, textContent(t, res.Result.Parts[0]), `"answer":"violet"`)
}

// --- the modern wire, field by field against the SDK ---------------------------------

func TestCapabilityShapesMatchTheSDK(t *testing.T) {
	// mcp.client.session._build_capabilities: sampling {}, elicitation form+url,
	// roots listChanged=true — and in the modern era the same object rides in
	// every request's _meta.
	policy := AnswerPolicy{Elicitation: ElicitationAsk, Sampling: true, Roots: []string{"."}}
	want := map[string]any{
		"sampling":    map[string]any{},
		"elicitation": map[string]any{"form": map[string]any{}, "url": map[string]any{}},
		"roots":       map[string]any{"listChanged": true},
	}
	for _, kind := range bothKinds {
		for _, modern := range []bool{false, true} {
			mode := ProtocolMode("")
			if modern {
				mode = ProtocolAuto
			}
			cl, _ := newScenarioClient(t, kind, modern, mode, 0, WithAnswering(policy))
			out, err := cl.CallTool("caps", nil)
			if err != nil {
				t.Fatalf("%s modern=%v: %v", kind, modern, err)
			}
			var got map[string]any
			if err := json.Unmarshal([]byte(firstText(t, out)), &got); err != nil || !reflect.DeepEqual(got, want) {
				t.Errorf("%s modern=%v: capabilities = %s, want %v", kind, modern, firstText(t, out), want)
			}
		}
	}
}

func TestModernRequestsAreStampedLikeTheSDK(t *testing.T) {
	cl, srv := newScenarioClient(t, overHTTP, true, ProtocolAuto, 0, WithAnswering(askPolicy))
	if _, err := cl.CallTool("caps", nil); err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if len(srv.log) < 2 {
		t.Fatalf("log = %+v", srv.log)
	}
	for _, r := range srv.log {
		if r.Reply {
			continue
		}
		if r.Method == "initialize" || r.Method == "DELETE" {
			t.Fatalf("modern session sent %s", r.Method)
		}
		if r.Headers["mcp-protocol-version"] != "2026-07-28" || r.Headers["mcp-method"] != r.Method {
			t.Errorf("%s: headers = %v, want MCP-Protocol-Version and Mcp-Method", r.Method, r.Headers)
		}
		var p struct {
			Name string                     `json:"name"`
			Meta map[string]json.RawMessage `json:"_meta"`
		}
		_ = json.Unmarshal(r.Params, &p)
		for _, key := range []string{metaVersionKey, metaInfoKey, metaCapsKey} {
			if _, ok := p.Meta[key]; !ok {
				t.Errorf("%s: _meta lacks %s: %s", r.Method, key, r.Params)
			}
		}
		if string(p.Meta[metaVersionKey]) != `"2026-07-28"` {
			t.Errorf("%s: _meta version = %s", r.Method, p.Meta[metaVersionKey])
		}
		if string(p.Meta[metaInfoKey]) != `{"name":"`+clientName+`","version":"`+clientVersion+`"}` {
			t.Errorf("%s: clientInfo = %s", r.Method, p.Meta[metaInfoKey])
		}
		if r.Method == "tools/call" && r.Headers["mcp-name"] != p.Name {
			t.Errorf("tools/call: Mcp-Name = %q, want %q", r.Headers["mcp-name"], p.Name)
		}
		if r.Method != "tools/call" && r.Headers["mcp-name"] != "" {
			t.Errorf("%s carries Mcp-Name %q", r.Method, r.Headers["mcp-name"])
		}
	}
	if srv.log[0].Method != "server/discover" {
		t.Errorf("first request = %s, want server/discover", srv.log[0].Method)
	}
}

func TestEncodeHeaderValue(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"echo", "echo"},
		{"with space", "with space"},
		{" edge", "=?base64?IGVkZ2U=?="},
		{"edge ", "=?base64?ZWRnZSA=?="},
		{"café", "=?base64?Y2Fmw6k=?="},
		{"tab\there", "=?base64?dGFiCWhlcmU=?="},
		{"=?base64?Zm9v?=", "=?base64?PT9iYXNlNjQ/Wm05dj89?="},
		{"", ""},
	} {
		if got := encodeHeaderValue(tc.in); got != tc.want {
			t.Errorf("encodeHeaderValue(%q) = %q, want %q", tc.in, got, tc.want)
		}
		if got := decodeHeaderValue(encodeHeaderValue(tc.in)); got != tc.in {
			t.Errorf("round trip of %q = %q", tc.in, got)
		}
	}
}

func TestPickModernVersion(t *testing.T) {
	for _, tc := range []struct {
		supported []string
		want      string
	}{
		{[]string{"2026-07-28"}, "2026-07-28"},
		{[]string{"2025-11-25", "2026-07-28"}, "2026-07-28"},
		{[]string{"2025-11-25"}, ""},
		{[]string{"2024-11-05", "2025-06-18"}, ""},
		// A revision newer than the client knows is not assumed compatible.
		{[]string{"2099-01-01"}, ""},
		{[]string{"2026-07-29"}, ""},
		{[]string{"2099-01-01", "2026-07-28"}, "2026-07-28"},
		{nil, ""},
	} {
		if got := pickModernVersion(tc.supported); got != tc.want {
			t.Errorf("pickModernVersion(%v) = %q, want %q", tc.supported, got, tc.want)
		}
	}
}

func TestDiscoverWithoutModernVersionFallsBackToHandshake(t *testing.T) {
	// A server that answers discover but advertises only handshake revisions is
	// an explicit legacy advertisement (the SDK falls back, it does not fail).
	srv := newScenarioHTTP(true)
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var msg wireMessage
		_ = json.Unmarshal(body, &msg)
		if msg.Method == "server/discover" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": map[string]any{
				"resultType": "complete", "supportedVersions": []string{"2025-11-25"}, "capabilities": map[string]any{},
			}})
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		srv.ServeHTTP(w, r)
	}))
	defer hs.Close()
	cl, err := New(&MCPServerConfig{URL: hs.URL, ProtocolMode: ProtocolAuto})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer cl.Close()
	info, err := cl.Initialize()
	if err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if info.ProtocolVersion != "2024-11-05" {
		t.Errorf("protocol version = %q, want the handshake's", info.ProtocolVersion)
	}
}

func TestUnsupportedVersionOnDiscover(t *testing.T) {
	t.Run("modern-only server sharing nothing is an error", func(t *testing.T) {
		cl, srv := newScenarioClient(t, overHTTP, true, ProtocolAuto, 0)
		srv.brain.unsupported = []string{"2027-01-01"}
		_, err := cl.Initialize()
		var rpcErr *rpcError
		if !errors.As(err, &rpcErr) || rpcErr.Code != codeUnsupportedProtocolVersion {
			t.Fatalf("err = %v, want the -32022 error", err)
		}
		if len(srv.requests("initialize")) != 0 {
			t.Error("fell back to the handshake against a server that named no handshake revision")
		}
	})
	t.Run("a server that also speaks the handshake falls back", func(t *testing.T) {
		cl, srv := newScenarioClient(t, overHTTP, true, ProtocolAuto, 0)
		srv.brain.unsupported = []string{"2025-11-25", "2027-01-01"}
		if _, err := cl.Initialize(); err != nil {
			t.Fatalf("Initialize: %v", err)
		}
		if len(srv.requests("initialize")) != 1 {
			t.Error("no handshake after -32022 naming a handshake revision")
		}
	})
}

func TestStrictModernServerRefusesUnstampedRequests(t *testing.T) {
	// Control for the fake itself: the compatibility tests above are only worth
	// something because this server refuses what the SDK's server refuses.
	srv := newScenarioHTTP(true)
	hs := httptest.NewServer(srv)
	defer hs.Close()
	post := func(headers map[string]string, params map[string]any) (int, int) {
		body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": params})
		req, _ := http.NewRequest(http.MethodPost, hs.URL, bytes.NewReader(body))
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var msg wireMessage
		_ = json.NewDecoder(resp.Body).Decode(&msg)
		code := 0
		if msg.Error != nil {
			code = msg.Error.Code
		}
		return resp.StatusCode, code
	}
	meta := func(version string) map[string]any {
		return map[string]any{metaVersionKey: version, metaCapsKey: map[string]any{}}
	}
	good := map[string]string{"MCP-Protocol-Version": "2026-07-28", "Mcp-Method": "tools/call", "Mcp-Name": "echo"}
	for _, tc := range []struct {
		name    string
		headers map[string]string
		params  map[string]any
		want    int
	}{
		{"no envelope", good, map[string]any{"name": "echo"}, codeInvalidParams},
		{"no headers", nil, map[string]any{"name": "echo", "_meta": meta("2026-07-28")}, codeHeaderMismatch},
		{"wrong method header", map[string]string{"MCP-Protocol-Version": "2026-07-28", "Mcp-Method": "tools/list", "Mcp-Name": "echo"}, map[string]any{"name": "echo", "_meta": meta("2026-07-28")}, codeHeaderMismatch},
		{"wrong name header", map[string]string{"MCP-Protocol-Version": "2026-07-28", "Mcp-Method": "tools/call", "Mcp-Name": "other"}, map[string]any{"name": "echo", "_meta": meta("2026-07-28")}, codeHeaderMismatch},
		{"unknown version", map[string]string{"MCP-Protocol-Version": "2099-01-01", "Mcp-Method": "tools/call", "Mcp-Name": "echo"}, map[string]any{"name": "echo", "_meta": meta("2099-01-01")}, codeUnsupportedProtocolVersion},
	} {
		if status, code := post(tc.headers, tc.params); status != http.StatusBadRequest || code != tc.want {
			t.Errorf("%s: status %d code %d, want 400 / %d", tc.name, status, code, tc.want)
		}
	}
	if status, code := post(good, map[string]any{"name": "echo", "_meta": meta("2026-07-28")}); status != http.StatusOK || code != 0 {
		t.Errorf("a correctly stamped request: status %d code %d", status, code)
	}
}

func TestStateOnlyRoundBacksOffAndOmitsInputResponses(t *testing.T) {
	cl, srv := newScenarioClient(t, overHTTP, true, ProtocolAuto, 0, WithAnswering(askPolicy))
	start := time.Now()
	out, err := cl.CallToolWith(context.Background(), conversation(nil, nil), "state_only", nil)
	if err != nil {
		t.Fatalf("CallToolWith: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 50*time.Millisecond {
		t.Errorf("retried after %v, want the 50ms state-only backoff first", elapsed)
	}
	containsAll(t, firstText(t, out), "state_only done", "state=s1")
	calls := srv.requests("tools/call")
	if len(calls) != 2 {
		t.Fatalf("%d tools/call requests, want 2", len(calls))
	}
	var p map[string]json.RawMessage
	_ = json.Unmarshal(calls[1].Params, &p)
	if _, present := p["inputResponses"]; present {
		t.Errorf("state-only retry carried inputResponses: %s", calls[1].Params)
	}
	if string(p["requestState"]) != `"s1"` {
		t.Errorf("requestState = %s", p["requestState"])
	}
}

func TestSamplingMediaBlocks(t *testing.T) {
	for _, kind := range bothKinds {
		t.Run(string(kind), func(t *testing.T) {
			var got SamplingRequest
			policy := AnswerPolicy{SamplingHandler: func(_ context.Context, _ *agocontext.Context, req SamplingRequest) (SamplingResult, error) {
				got = req
				return SamplingResult{Text: "ok", Model: "m"}, nil
			}}
			cl, _ := newScenarioClient(t, kind, false, "", 0, WithAnswering(policy))
			if _, err := cl.CallToolWith(context.Background(), conversation(nil, nil), "sample_media", nil); err != nil {
				t.Fatalf("CallToolWith: %v", err)
			}
			if len(got.Messages) != 2 || got.Messages[0].Role != "user" || got.Messages[1].Role != "assistant" {
				t.Fatalf("messages = %+v", got.Messages)
			}
			parts := got.Messages[0].Parts
			if len(parts) != 3 {
				t.Fatalf("%d parts, want text+image+audio", len(parts))
			}
			img, ok := events.AsValue(parts[1]).(events.BinaryInput)
			if !ok || img.Kind != events.BinaryImage || img.MediaType != "image/png" || string(img.Data) != "png-bytes" {
				t.Errorf("image part = %#v", parts[1])
			}
			aud, ok := events.AsValue(parts[2]).(events.BinaryInput)
			if !ok || aud.Kind != events.BinaryAudio || aud.MediaType != "audio/wav" || string(aud.Data) != "wav-bytes" {
				t.Errorf("audio part = %#v", parts[2])
			}
		})
		t.Run(string(kind)+"/not base64", func(t *testing.T) {
			called := false
			policy := AnswerPolicy{SamplingHandler: func(context.Context, *agocontext.Context, SamplingRequest) (SamplingResult, error) {
				called = true
				return SamplingResult{}, nil
			}}
			cl, _ := newScenarioClient(t, kind, false, "", 0, WithAnswering(policy))
			_, err := cl.CallToolWith(context.Background(), conversation(nil, nil), "sample_media", map[string]any{"bad": true})
			if !errors.Is(err, ErrAnswering) || !strings.Contains(err.Error(), "not base64") {
				t.Fatalf("err = %v, want ErrAnswering about a non-base64 block", err)
			}
			if called {
				t.Error("the handler ran on a malformed request")
			}
		})
	}
}

func TestModernToolNameTravelsEncodedInTheHeader(t *testing.T) {
	// A name outside printable ASCII must reach the server as the base64 sentinel,
	// not as raw bytes in a header field.
	cl, srv := newScenarioClient(t, overHTTP, true, ProtocolAuto, 0)
	_, err := cl.CallTool("café", nil)
	var rpcErr *rpcError
	if !errors.As(err, &rpcErr) || rpcErr.Code != codeInvalidParams || !strings.Contains(rpcErr.Message, "unknown tool café") {
		t.Fatalf("err = %v, want the server's unknown-tool error (the headers were accepted)", err)
	}
	calls := srv.requests("tools/call")
	if len(calls) != 1 || calls[0].Headers["mcp-name"] != "=?base64?Y2Fmw6k=?=" {
		t.Fatalf("Mcp-Name = %v, want the base64 sentinel", calls)
	}
}

func TestReplyErrorKeepsTheJSONRPCCode(t *testing.T) {
	msg := wireMessage{Error: &rpcError{Code: -32022, Message: "nope"}}
	_, err := msg.outcome()
	var rpcErr *rpcError
	if !errors.Is(err, ErrProtocol) || !errors.As(err, &rpcErr) || rpcErr.Code != -32022 {
		t.Fatalf("err = %v, want ErrProtocol carrying the *rpcError", err)
	}
}
