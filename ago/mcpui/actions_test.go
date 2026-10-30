package mcpui

import (
	"encoding/json"
	"strings"
	"testing"
)

// Goldens are byte-identical to the Python reference outputs captured by running
// ag2/ag2/mcp_ui/actions.py against mcp-ui-server 1.0.0: json.dumps default
// separators (": " after keys, ", " between items), html.escape(quote=True)
// entities (&quot;, &#x27;), ensure_ascii escaping.

func TestActionWireGolden(t *testing.T) {
	// The struct wire form hosts receive through encoding/json (compact).
	for _, tc := range []struct {
		name string
		act  Action
		want string
	}{
		{"tool_call", ToolCall("add_to_cart", map[string]any{"good_id": "42"}),
			`{"type":"tool","payload":{"toolName":"add_to_cart","params":{"good_id":"42"}}}`},
		{"prompt", Prompt("What is the weather?"),
			`{"type":"prompt","payload":{"prompt":"What is the weather?"}}`},
		{"link", Link("https://docs.ag2.ai/"),
			`{"type":"link","payload":{"url":"https://docs.ag2.ai/"}}`},
		{"intent", Intent("checkout", map[string]any{"cart_id": "7"}),
			`{"type":"intent","payload":{"intent":"checkout","params":{"cart_id":"7"}}}`},
		{"notify", Notify("Widget loaded"),
			`{"type":"notify","payload":{"message":"Widget loaded"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, err := json.Marshal(tc.act)
			if err != nil {
				t.Fatalf("marshal %s: %v", tc.name, err)
			}
			if string(b) != tc.want {
				t.Fatalf("golden mismatch:\n got %s\nwant %s", b, tc.want)
			}
		})
	}
}

func TestNilParamsSerializeAsEmptyObject(t *testing.T) {
	b, err := json.Marshal(ToolCall("ping", nil))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if want := `{"type":"tool","payload":{"toolName":"ping","params":{}}}`; string(b) != want {
		t.Fatalf("golden mismatch:\n got %s\nwant %s", b, want)
	}
}

func TestPostMessageGolden(t *testing.T) {
	got := PostMessage(ToolCall("add_to_cart", map[string]any{"good_id": "42"}))
	want := "window.parent.postMessage(" +
		`{&quot;type&quot;: &quot;tool&quot;, &quot;payload&quot;: ` +
		`{&quot;toolName&quot;: &quot;add_to_cart&quot;, &quot;params&quot;: {&quot;good_id&quot;: &quot;42&quot;}}}` +
		", &#x27;*&#x27;)"
	if got != want {
		t.Fatalf("golden mismatch:\n got %s\nwant %s", got, want)
	}
}

func TestPostMessageApostropheGolden(t *testing.T) {
	// An apostrophe in the payload must not terminate a quoted HTML attribute.
	got := PostMessage(Prompt("What's the status of my order?"))
	want := "window.parent.postMessage(" +
		`{&quot;type&quot;: &quot;prompt&quot;, &quot;payload&quot;: ` +
		`{&quot;prompt&quot;: &quot;What&#x27;s the status of my order?&quot;}}` +
		", &#x27;*&#x27;)"
	if got != want {
		t.Fatalf("golden mismatch:\n got %s\nwant %s", got, want)
	}
	if strings.ContainsRune(got, '\'') || strings.ContainsRune(got, '"') {
		t.Fatalf("quotes must all be escaped: %s", got)
	}
}

func TestPostMessageEnsureASCII(t *testing.T) {
	// Non-ASCII escapes as lowercase \uXXXX, non-BMP as a UTF-16 surrogate pair —
	// the byte forms json.dumps(ensure_ascii=True) produces.
	got := PostMessage(Prompt("Привет 𝒜"))
	want := "window.parent.postMessage(" +
		`{&quot;type&quot;: &quot;prompt&quot;, &quot;payload&quot;: ` +
		`{&quot;prompt&quot;: &quot;\u041f\u0440\u0438\u0432\u0435\u0442 \ud835\udc9c&quot;}}` +
		", &#x27;*&#x27;)"
	if got != want {
		t.Fatalf("golden mismatch:\n got %s\nwant %s", got, want)
	}
}

func TestPostMessageSortedKeysInParams(t *testing.T) {
	// AGO-DEVIATION (D-044): Go maps have no insertion order, so params keys ride
	// in sorted order where Python keeps dict insertion order.
	got := PostMessage(ToolCall("t", map[string]any{"z": 1, "a": 2}))
	want := "window.parent.postMessage(" +
		`{&quot;type&quot;: &quot;tool&quot;, &quot;payload&quot;: ` +
		`{&quot;toolName&quot;: &quot;t&quot;, &quot;params&quot;: {&quot;a&quot;: 2, &quot;z&quot;: 1}}}` +
		", &#x27;*&#x27;)"
	if got != want {
		t.Fatalf("sorted-order mismatch:\n got %s\nwant %s", got, want)
	}
}

func TestPythonStringEscapes(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want string // the quoted form json.dumps would print
	}{
		{"plain", `"plain"`},
		{"quote: \" back: \\", `"quote: \" back: \\"`},
		{"nl:\n\ttab", `"nl:\n\ttab"`},
		{"\x01\x1f", `"\u0001\u001f"`},
		{"<b>&'</b>", `"<b>&'</b>"`}, // left for the html pass
		{"é", `"\u00e9"`},
	} {
		if got := pythonString(tc.in); got != tc.want {
			t.Fatalf("pythonString(%q) = %s, want %s", tc.in, got, tc.want)
		}
	}
}

func TestMessageIDRidesFirstWhenSet(t *testing.T) {
	a := ToolCall("t", nil)
	a.MessageID = "m-1"
	payload := actionJSON(a)
	if !strings.HasPrefix(payload, `{"messageId": "m-1", "type": "tool"`) {
		t.Fatalf("messageId must lead the wire form: %s", payload)
	}
	// Fresh actions carry no messageId, as pydantic's exclude_none drops it.
	if strings.Contains(actionJSON(ToolCall("t", nil)), "messageId") {
		t.Fatalf("unset messageId must not serialize")
	}
}
