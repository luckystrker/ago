package agui

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/luckystrker/ago/config/ollama"
	"github.com/luckystrker/ago/events"
)

// captureWarnings routes slog's default logger into a buffer for the test.
func captureWarnings(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// TestUnsupportedAttachmentIsDroppedNotFatal: the served agent talks to Ollama,
// whose mapper takes inline images but no audio. A turn carrying both succeeds
// with the audio dropped and a warning logged, instead of failing the turn
// inside the model client (Python parity: ag_ui/stream.py _accepted_parts).
func TestUnsupportedAttachmentIsDroppedNotFatal(t *testing.T) {
	var (
		mu      sync.Mutex
		gotBody []byte
	)
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		gotBody = b
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"model":"m","message":{"role":"assistant","content":"ok"},"done":true,"done_reason":"stop"}`)
	}))
	t.Cleanup(fake.Close)

	warnings := captureWarnings(t)
	audio := base64.StdEncoding.EncodeToString([]byte("RIFFfakewav"))
	image := base64.StdEncoding.EncodeToString([]byte("\x89PNGfake"))
	body := fmt.Sprintf(`{"threadId":"t1","runId":"r1","messages":[{"id":"m1","role":"user","content":[
	  {"type":"text","text":"what is this?"},
	  {"type":"audio","source":{"type":"data","value":%q,"mimeType":"audio/wav"}},
	  {"type":"image","source":{"type":"data","value":%q,"mimeType":"image/png"}}
	]}]}`, audio, image)

	evs, _ := postRun(t, ServeAgent(servedAgent(ollama.NewOllama("m", ollama.WithHost(fake.URL)))), body)

	var finished bool
	for _, ev := range evs {
		switch ev.(type) {
		case RunErrorEvent:
			t.Fatalf("the turn must not fail on an unsupported attachment: %+v", ev)
		case RunFinishedEvent:
			finished = true
		}
	}
	if !finished {
		t.Fatalf("no RUN_FINISHED among %d events", len(evs))
	}

	mu.Lock()
	defer mu.Unlock()
	var req struct {
		Messages []struct {
			Content string   `json:"content"`
			Images  []string `json:"images"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(gotBody, &req); err != nil {
		t.Fatalf("model request not captured: %v (%q)", err, gotBody)
	}
	var sawText, sawImage bool
	for _, m := range req.Messages {
		sawText = sawText || strings.Contains(m.Content, "what is this?")
		sawImage = sawImage || len(m.Images) == 1
	}
	if !sawText || !sawImage {
		t.Fatalf("the accepted parts must reach the model: %s", gotBody)
	}
	if bytes.Contains(gotBody, []byte(audio)) {
		t.Fatal("the unsupported audio part reached the model")
	}

	log := warnings.String()
	if !strings.Contains(log, "does not accept") || !strings.Contains(log, "kind=audio") ||
		!strings.Contains(log, "media_type=audio/wav") || !strings.Contains(log, "provider=ollama") || !strings.Contains(log, "position=user") {
		t.Fatalf("warning not logged as expected: %q", log)
	}
	if strings.Contains(log, audio) {
		t.Fatal("the warning must not carry the payload")
	}
}

// TestMapRunInputMessagesForFiltersByPosition: the filter sees the position
// (user vs tool result) and the provider's table.
func TestMapRunInputMessagesForFiltersByPosition(t *testing.T) {
	warnings := captureWarnings(t)
	image := base64.StdEncoding.EncodeToString([]byte("\x89PNGfake"))
	part := fmt.Sprintf(`[{"type":"image","source":{"type":"data","value":%q,"mimeType":"image/png"}}]`, image)
	input := RunAgentInput{Messages: []Message{
		{ID: "m1", Role: "user", Content: json.RawMessage(part)},
		{ID: "m2", Role: "assistant", ToolCalls: []ToolCall{{ID: "c1", Type: "function", Function: FunctionCall{Name: "f", Arguments: "{}"}}}},
		{ID: "m3", Role: "tool", ToolCallID: "c1", Content: json.RawMessage(part)},
		{ID: "m4", Role: "user", Content: json.RawMessage(part)},
	}}

	cfg := ollama.NewOllama("m")
	got := mapRunInputMessagesFor(input, cfg)
	if len(got.current) != 1 {
		t.Fatalf("the user-position image is accepted by Ollama: current = %d parts", len(got.current))
	}
	var toolParts []events.Input
	userImages := 0
	for _, ev := range got.history {
		switch e := ev.(type) {
		case *events.ToolResultsEvent:
			toolParts = e.Results[0].Result.Parts
		case *events.ModelRequest:
			userImages += len(e.Parts)
		}
	}
	if userImages != 1 {
		t.Fatalf("the earlier user-position image must be kept: %d parts in history requests", userImages)
	}
	if len(toolParts) != 1 || toolParts[0].(*events.TextInput) == nil {
		t.Fatalf("a tool-position image Ollama cannot take must be dropped, leaving the empty-result placeholder: %+v", toolParts)
	}
	if !strings.Contains(warnings.String(), "position=tool") {
		t.Fatalf("warning missing: %q", warnings.String())
	}

	// No config → no filter (the plain wrapper keeps its old behavior).
	if plain := mapRunInputMessages(input); len(plain.current) != 1 {
		t.Fatalf("nil config must accept everything: %+v", plain.current)
	}
}
