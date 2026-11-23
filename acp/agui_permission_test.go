package acp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/luckystrker/ago/agent"
	"github.com/luckystrker/ago/agui"
	"github.com/luckystrker/ago/internal/sse"
)

// aguiPost drives one POST of an AG-UI exchange and decodes its frames.
func aguiPost(t *testing.T, url string, body any) []agui.Event {
	t.Helper()
	raw, _ := json.Marshal(body)
	resp, err := http.Post(url, "application/json", strings.NewReader(string(raw)))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	var evs []agui.Event
	sc := sse.NewScanner(resp.Body)
	for sc.Next() {
		_, data := sc.Event()
		ev, err := agui.UnmarshalEvent(data)
		if err != nil {
			t.Fatalf("frame %q: %v", data, err)
		}
		evs = append(evs, ev)
	}
	return evs
}

func aguiInterrupt(t *testing.T, evs []agui.Event) agui.Interrupt {
	t.Helper()
	fin, ok := evs[len(evs)-1].(agui.RunFinishedEvent)
	if !ok {
		t.Fatalf("last event = %T, want RUN_FINISHED", evs[len(evs)-1])
	}
	out, ok := fin.Outcome.(agui.InterruptOutcome)
	if !ok || len(out.Interrupts) != 1 {
		t.Fatalf("outcome = %#v, want one interrupt", fin.Outcome)
	}
	return out.Interrupts[0]
}

// aguiPermissionRun serves an ACP-backed agent over AG-UI, whose fake agent
// raises one session/request_permission with `options`, and resumes the
// interrupt with payload. It returns what the agent saw on the wire (or the
// error code of a refused resume) and the interrupt the client was shown.
func aguiPermissionRun(t *testing.T, options string, payload any) (string, agui.Interrupt) {
	t.Helper()
	srv, err := agui.NewServer()
	if err != nil {
		t.Fatalf("server: %v", err)
	}
	cfg, _ := providerConfig(t, rolePermOpts, "")
	cfg.Env[permOptionsEnv] = options
	// A refused resume leaves the turn held with the agent parked on its
	// question; closing the server releases it, so cfg.Close can stop the agent.
	t.Cleanup(srv.Close)
	h := srv.Handler(agent.New("served", "", append(srv.Options(), agent.WithConfig(cfg))...))
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)

	first := aguiPost(t, ts.URL, map[string]any{
		"threadId": "t1", "runId": "r1",
		"messages": []any{map[string]any{"id": "m1", "role": "user", "content": "go"}},
	})
	in := aguiInterrupt(t, first)
	second := aguiPost(t, ts.URL, map[string]any{
		"threadId": "t1", "runId": "r2", "messages": []any{},
		"resume": []map[string]any{{"interruptId": in.ID, "status": "resolved", "payload": payload}},
	})
	var text strings.Builder
	for _, ev := range second {
		switch e := ev.(type) {
		case agui.TextMessageContentEvent:
			text.WriteString(e.Delta)
		case agui.RunErrorEvent:
			return "error:" + e.Code, in
		}
	}
	return text.String(), in
}

// TestAGUIResumeSelectsTheRightPermissionOption: an AG-UI client answers an ACP
// agent's permission request; each kind of answer reaches the agent as exactly
// the option it means (GHSA-3r6w-7pcg-c4x2 and the "y"/"n" id collisions).
func TestAGUIResumeSelectsTheRightPermissionOption(t *testing.T) {
	pick := func(v string) any { return map[string]any{"choice": v} }
	tests := []struct {
		name, options string
		payload       any
		want          string
	}{
		{"approve button on the gemini layout is one-time", geminiLayout, true, "selected:once-1"},
		{"the word yes on the gemini layout is one-time", geminiLayout, "yes", "selected:once-1"},
		{"refuse button refuses", geminiLayout, false, "selected:no-1"},
		{"a pick of the remembered option is honoured", geminiLayout, pick("always-1"), "selected:always-1"},
		{"approve button, allow option is called n: one-time allow", collisionLayout, true, "selected:n"},
		{"refuse button, allow option is called n: the reject option, not n", collisionLayout, false, "selected:y"},
		{"picking the reject option whose id is y selects it", collisionLayout, pick("y"), "selected:y"},
		{"the string y is the word, which allows once: the option n", collisionLayout, "y", "selected:n"},
		{"a pick that was not offered is refused", geminiLayout, pick("чужое"), "error:" + agui.CodeInterruptPayloadRefused},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, in := aguiPermissionRun(t, tc.options, tc.payload)
			if got != tc.want {
				t.Errorf("agent saw %q, want %q", got, tc.want)
			}
			if in.Reason != agui.ReasonToolCall || !strings.Contains(in.Message, "git status") {
				t.Errorf("interrupt = %+v", in)
			}
			if _, ok := in.ResponseSchema["anyOf"]; !ok {
				t.Errorf("response schema %v does not offer the agent's options", in.ResponseSchema)
			}
		})
	}
}
