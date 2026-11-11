package searchtools

import (
	"net/http"
	"testing"

	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/stream"
)

// TestGlasserMaxCharactersZero asserts a zero budget is legal (Python parity: a
// non-negative max_characters) and cuts every content field to nothing.
func TestGlasserMaxCharactersZero(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		respondJSON(t, w, `{"status":"DONE","output":"payload","error":"boom"}`)
	})
	tk, err := NewGlasserToolkit("k", WithGlasserBaseURL(srv.URL), WithGlasserMaxCharacters(0))
	if err != nil {
		t.Fatalf("WithGlasserMaxCharacters(0): %v", err)
	}
	_, cx := newTestContext()
	ev := callTool(t, tk, cx, "glasser_stock_quote", `{"symbol":"A"}`)
	sameJSON(t, firstDataJSON(t, ev),
		`{"status":"DONE","output":"","error":"","truncated":true}`)
}

// TestGlasserVariablesBothNamesRequired asserts a variable binding needs both a
// parameter name and a variable name.
func TestGlasserVariablesBothNamesRequired(t *testing.T) {
	if _, err := NewGlasserToolkit("k", WithGlasserVariable("", "v")); err == nil {
		t.Fatal("an empty parameter name must be rejected")
	}
	if _, err := NewGlasserToolkit("k", WithGlasserVariable("limit", "")); err == nil {
		t.Fatal("an empty variable name must be rejected")
	}
	tk, err := NewGlasserToolkit("k", WithGlasserVariable("limit", "v"))
	if err != nil {
		t.Fatalf("a valid binding must be accepted: %v", err)
	}
	_, cx := newTestContext()
	if err := cx.SetVariable("v", 3); err != nil {
		t.Fatalf("SetVariable: %v", err)
	}
	if v, ok, err := tk.params.intParam(cx, "limit"); err != nil || !ok || v != 3 {
		t.Fatalf("resolved = %v, %v, %v; want 3 from the variable", v, ok, err)
	}
}

// TestIdempotencyKeyDeterminism asserts the key is stable per (stream, call,
// capability) and random when the call has no id.
func TestIdempotencyKeyDeterminism(t *testing.T) {
	streamA := stream.NewMemoryStream()
	streamB := stream.NewMemoryStream()
	cxA := agocontext.New(streamA).WithToolCallID("call-1")
	cxB := agocontext.New(streamB).WithToolCallID("call-1")

	first, err := idempotencyKey(cxA, "web_research")
	if err != nil {
		t.Fatalf("idempotencyKey: %v", err)
	}
	again, _ := idempotencyKey(cxA, "web_research")
	if first != again {
		t.Fatalf("keys = %q and %q, want the same key for the same call", first, again)
	}
	// A different stream with the same call id must derive a different key: LLM
	// call ids repeat across conversations.
	if same, _ := idempotencyKey(cxB, "web_research"); same == first {
		t.Fatalf("keys for two streams are equal (%q), want stream-scoped keys", same)
	}
	// A different capability is a different call.
	if other, _ := idempotencyKey(cxA, "market_data"); other == first {
		t.Fatalf("keys for two capabilities are equal (%q)", other)
	}
	// Without a tool call id the key must be random, not derived: two runs of the
	// same conversation would otherwise collide.
	noID := agocontext.New(streamA)
	one, err := idempotencyKey(noID, "web_research")
	if err != nil {
		t.Fatalf("idempotencyKey: %v", err)
	}
	two, _ := idempotencyKey(noID, "web_research")
	if one == two {
		t.Fatalf("keys = %q and %q, want a random key without a call id", one, two)
	}
}

// TestValidRunID pins the accepted Run id alphabet: hex digits and dashes, with
// both letter cases and the digit boundaries included.
func TestValidRunID(t *testing.T) {
	valid := []string{"0", "9", "a", "f", "A", "F", "-", "ab12cd34-ef56", "0-0"}
	for _, id := range valid {
		if !validRunID(id) {
			t.Fatalf("validRunID(%q) = false, want true", id)
		}
	}
	// An empty id is vacuously valid here; the caller rejects it separately.
	if !validRunID("") {
		t.Fatal(`validRunID("") = false; the emptiness check lives at the call site`)
	}
	invalid := []string{"run-abc", "g", "G", "z", "abc def", "abc/def", "é"}
	for _, id := range invalid {
		if validRunID(id) {
			t.Fatalf("validRunID(%q) = true, want false", id)
		}
	}
}

// TestJSONDumpsKeepsStringContent asserts the Python-style spacing pass never
// touches characters inside a string: a comma or colon there must not grow the
// rendered length and shift the truncation budget.
func TestJSONDumpsKeepsStringContent(t *testing.T) {
	value := map[string]any{"a": "x,y:z"}
	encoded, err := pythonJSONDumps(value)
	if err != nil {
		t.Fatalf("pythonJSONDumps: %v", err)
	}
	if encoded != `{"a": "x,y:z"}` {
		t.Fatalf("encoded = %s, want the Python default separators and the string intact", encoded)
	}
	_, remaining, cut := glasserContent(value, 100)
	if cut || remaining != 100-runeLen(`{"a": "x,y:z"}`) {
		t.Fatalf("remaining = %d, cut = %v; want %d", remaining, cut, 100-runeLen(`{"a": "x,y:z"}`))
	}
	// An escaped quote must not end the string state either.
	if got, err := pythonJSONDumps(map[string]any{"a": `x"y,z`}); err != nil || got != `{"a": "x\"y,z"}` {
		t.Fatalf("encoded = %q, %v", got, err)
	}
}
