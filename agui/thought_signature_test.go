package agui

import (
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/luckystrker/ago/config"
	"github.com/luckystrker/ago/config/gemini"
	"github.com/luckystrker/ago/events"
)

// A Gemini call's thought signature goes out as REASONING_ENCRYPTED_VALUE right
// after the call starts, and a call without one emits no such event.
func TestStreamEmitsThoughtSignature(t *testing.T) {
	sig := []byte("opaque-gemini-signature")
	var out []Event
	s := NewStream(func(e Event) { out = append(out, e) })
	s.Handle(&events.ToolCallEvent{
		ID: "call-1", Name: "get_weather", Arguments: `{"city":"Paris"}`,
		VendorMetadata: map[string]any{ThoughtSignatureKey: sig},
	})

	types := make([]EventType, len(out))
	for i, e := range out {
		types[i] = eventTypeOf(e)
	}
	want := []EventType{EventToolCallStart, EventReasoningEncryptedValue, EventToolCallArgs, EventToolCallEnd}
	if len(types) != len(want) {
		t.Fatalf("events = %v, want %v", types, want)
	}
	for i := range want {
		if types[i] != want[i] {
			t.Fatalf("events = %v, want %v", types, want)
		}
	}
	sigEvent, ok := out[1].(ReasoningEncryptedValueEvent)
	if !ok {
		t.Fatalf("event = %T", out[1])
	}
	if sigEvent.Subtype != EncryptedSubtypeToolCall || sigEvent.EntityID != "call-1" {
		t.Fatalf("signature event = %+v", sigEvent)
	}
	if sigEvent.EncryptedValue != base64.StdEncoding.EncodeToString(sig) {
		t.Fatalf("value = %q", sigEvent.EncryptedValue)
	}

	// A plain call emits nothing extra.
	out = nil
	s.Handle(&events.ToolCallEvent{ID: "call-2", Name: "get_weather", Arguments: "{}"})
	for _, e := range out {
		if eventTypeOf(e) == EventReasoningEncryptedValue {
			t.Fatal("a call without a signature must not emit one")
		}
	}
}

// The event decodes from the wire and round-trips.
func TestReasoningEncryptedValueWireShape(t *testing.T) {
	raw := `{"type":"REASONING_ENCRYPTED_VALUE","subtype":"tool-call","entityId":"call-9","encryptedValue":"QUJD","timestamp":7}`
	ev, err := UnmarshalEvent([]byte(raw))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	sig, ok := ev.(ReasoningEncryptedValueEvent)
	if !ok {
		t.Fatalf("decoded %T", ev)
	}
	if sig.Subtype != "tool-call" || sig.EntityID != "call-9" || sig.EncryptedValue != "QUJD" {
		t.Fatalf("decoded %+v", sig)
	}
	encoded, err := json.Marshal(sig)
	if err != nil {
		t.Fatal(err)
	}
	var back map[string]any
	if err := json.Unmarshal(encoded, &back); err != nil {
		t.Fatal(err)
	}
	if back["type"] != "REASONING_ENCRYPTED_VALUE" || back["entityId"] != "call-9" {
		t.Fatalf("encoded %s", encoded)
	}
}

// The round trip: a signature the client hands back on the replayed call lands
// on the call's vendor metadata for a Gemini-family run, and nowhere else.
func TestRestoreToolCallRoundTrip(t *testing.T) {
	sig := []byte("signature-bytes")
	call := events.ToolCallEvent{ID: "c1", Name: "search", Arguments: "{}",
		VendorMetadata: map[string]any{ThoughtSignatureKey: sig}}
	encrypted, ok := EncryptedSignatureOf(call)
	if !ok {
		t.Fatal("a call carrying a signature must yield one")
	}

	for _, provider := range []config.Provider{config.ProviderGemini, config.ProviderVertexAI} {
		restored := RestoreToolCall(provider, "c1", "search", "{}", encrypted)
		got, ok := restored.VendorMetadata[ThoughtSignatureKey].([]byte)
		if !ok || string(got) != string(sig) {
			t.Fatalf("%s: restored signature = %v", provider, restored.VendorMetadata)
		}
		if restored.ID != "c1" || restored.Name != "search" || restored.Arguments != "{}" {
			t.Fatalf("%s: call identity lost: %+v", provider, restored)
		}
	}

	// Another provider keeps no signature (it never issued one), and an empty
	// or mangled value restores nothing.
	for _, provider := range []config.Provider{config.ProviderOpenAI, config.ProviderAnthropic, ""} {
		if restored := RestoreToolCall(provider, "c1", "search", "{}", encrypted); restored.VendorMetadata != nil {
			t.Fatalf("%q: unexpected metadata %+v", provider, restored.VendorMetadata)
		}
	}
	if restored := RestoreToolCall(config.ProviderGemini, "c1", "search", "{}", ""); restored.VendorMetadata != nil {
		t.Fatal("an empty value must restore nothing")
	}
	if restored := RestoreToolCall(config.ProviderGemini, "c1", "search", "{}", "not base64!"); restored.VendorMetadata != nil {
		t.Fatal("a mangled value must restore nothing")
	}
}

func TestEncryptedSignatureOfIgnoresForeignMetadata(t *testing.T) {
	if _, ok := EncryptedSignatureOf(events.ToolCallEvent{ID: "c", VendorMetadata: map[string]any{"other": "x"}}); ok {
		t.Fatal("unrelated metadata is not a signature")
	}
	if _, ok := EncryptedSignatureOf(events.ToolCallEvent{ID: "c"}); ok {
		t.Fatal("no metadata is not a signature")
	}
	// The wire form (a base64 string) passes through unchanged.
	got, ok := EncryptedSignatureOf(events.ToolCallEvent{ID: "c",
		VendorMetadata: map[string]any{ThoughtSignatureKey: "QUJD"}})
	if !ok || got != "QUJD" {
		t.Fatalf("string signature = %q, %v", got, ok)
	}
}

// --- provider-tagged file handles --------------------------------------------

// A handle tagged for the served provider resolves; one tagged for another
// provider is skipped; an untagged one passes. The check lives in mapPart,
// which is where the handle's tag meets the run's provider.
func TestFileHandlesResolveForTheServedProvider(t *testing.T) {
	handle := func(provider string) ContentPart {
		part := ContentPart{Type: PartDocument, Source: &PartSource{Type: SourceFile, Value: "handle-" + provider}}
		part.Source.Provider = provider
		return part
	}
	resolve := func(part ContentPart, provider config.Provider) (string, bool) {
		in, ok := mapPart(part, provider)
		if !ok {
			return "", false
		}
		f, ok := events.AsValue(in).(events.FileIdInput)
		if !ok {
			t.Fatalf("mapped %T, want a file id", in)
		}
		return f.FileID, true
	}

	// A Gemini run accepts both spellings of its own tag, and an untagged
	// handle, and nothing else.
	for _, tag := range []string{"gemini", "google", ""} {
		if _, ok := resolve(handle(tag), config.ProviderGemini); !ok {
			t.Fatalf("a handle tagged %q must resolve for a gemini run", tag)
		}
	}
	if _, ok := resolve(handle("openai"), config.ProviderGemini); ok {
		t.Fatal("a handle issued by another provider must be skipped")
	}
	if _, ok := resolve(handle("anthropic"), config.ProviderVertexAI); ok {
		t.Fatal("a vertexai run must not resolve another vendor's handle")
	}

	// An OpenAI run resolves its own tag only.
	if _, ok := resolve(handle("openai"), config.ProviderOpenAI); !ok {
		t.Fatal("an openai handle must resolve for an openai run")
	}
	if _, ok := resolve(handle("gemini"), config.ProviderOpenAI); ok {
		t.Fatal("an openai run must not resolve a gemini handle")
	}

	// With no readable provider only the untagged handle survives (the
	// pre-existing behavior).
	if _, ok := resolve(handle(""), ""); !ok {
		t.Fatal("an untagged handle must pass when the provider is unknown")
	}
	if _, ok := resolve(handle("gemini"), ""); ok {
		t.Fatal("a tagged handle must be skipped when the provider is unknown")
	}
}

func TestIsSameProvider(t *testing.T) {
	cases := []struct {
		tag      string
		provider config.Provider
		want     bool
	}{
		{"gemini", config.ProviderGemini, true},
		{"google", config.ProviderGemini, true},
		{"google", config.ProviderVertexAI, true},
		{"vertexai", config.ProviderVertexAI, true},
		{"openai", config.ProviderOpenAI, true},
		{"gemini", config.ProviderOpenAI, false},
		{"", config.ProviderGemini, false},
		{"gemini", "", false},
	}
	for _, tc := range cases {
		if got := isSameProvider(tc.tag, tc.provider); got != tc.want {
			t.Errorf("isSameProvider(%q, %q) = %v, want %v", tc.tag, tc.provider, got, tc.want)
		}
	}
}

// eventTypeOf reads an event's type through its wire form (the marker method is
// unexported).
func eventTypeOf(e Event) EventType {
	data, err := json.Marshal(e)
	if err != nil {
		return ""
	}
	var probe struct {
		Type EventType `json:"type"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return ""
	}
	return probe.Type
}

// End to end with the acceptance layer active: a run served by a real Gemini
// config (whose dialect the media table describes) resolves a handle tagged for
// Gemini and drops one tagged for another provider, while an untagged handle
// still passes. This is the path a client actually takes — provider filter and
// media-type table together.
func TestFileHandlesEndToEndWithAcceptance(t *testing.T) {
	cfg := gemini.NewGemini("test-key", "gemini-test")
	if d, described := config.DescribedDialect(cfg); !described || d != config.DialectGemini {
		t.Fatalf("the fixture must be a described gemini config, got %q (%v)", d, described)
	}
	input := RunAgentInput{
		ThreadID: "t", RunID: "r",
		Messages: []Message{{ID: "m1", Role: "user", Content: jsonRaw(`[
			{"type":"document","source":{"type":"file","value":"own","provider":"gemini"}},
			{"type":"document","source":{"type":"file","value":"vendor","provider":"google"}},
			{"type":"document","source":{"type":"file","value":"foreign","provider":"openai"}},
			{"type":"document","source":{"type":"file","value":"untagged"}},
			{"type":"text","text":"read these"}
		]`)}},
	}
	got := mapRunInputMessagesFor(input, cfg)
	var handles []string
	for _, in := range got.current {
		if f, ok := events.AsValue(in).(events.FileIdInput); ok {
			handles = append(handles, f.FileID)
		}
	}
	want := []string{"own", "vendor", "untagged"}
	if len(handles) != len(want) {
		t.Fatalf("handles = %v, want %v (the acceptance table must keep file ids for a gemini user turn)", handles, want)
	}
	for i := range want {
		if handles[i] != want[i] {
			t.Fatalf("handles = %v, want %v", handles, want)
		}
	}
	// The text part is still there, so the turn is not empty.
	var texts int
	for _, in := range got.current {
		if _, ok := events.AsValue(in).(events.TextInput); ok {
			texts++
		}
	}
	if texts != 1 {
		t.Fatalf("text inputs = %d, want 1", texts)
	}
}
