package mcpui

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"ago/events"
)

// Goldens below are the by_alias/exclude_none wire forms of the Python objects
// (mcp-ui-server UIResource over mcp 2.x), captured by running the reference
// ag2/ag2/mcp_ui code against mcp-ui-server 1.0.0.

func marshalResource(t *testing.T, res Resource) string {
	t.Helper()
	b, err := Marshal(res)
	if err != nil {
		t.Fatalf("marshal resource: %v", err)
	}
	return string(b)
}

func TestRawHTMLGolden(t *testing.T) {
	res, err := RawHTML("ui://ag2/greeting", "<h1>Hi</h1>")
	if err != nil {
		t.Fatalf("RawHTML: %v", err)
	}
	want := `{"type":"resource","resource":{"uri":"ui://ag2/greeting","mimeType":"text/html","text":"<h1>Hi</h1>"}}`
	if got := marshalResource(t, res); got != want {
		t.Fatalf("golden mismatch:\n got %s\nwant %s", got, want)
	}
}

func TestRawHTMLBlobEncodingGolden(t *testing.T) {
	res, err := RawHTML("ui://ag2/greeting", "<h1>Hi</h1>", WithEncoding(EncodingBlob))
	if err != nil {
		t.Fatalf("RawHTML: %v", err)
	}
	want := `{"type":"resource","resource":{"uri":"ui://ag2/greeting","mimeType":"text/html","blob":"PGgxPkhpPC9oMT4="}}`
	if got := marshalResource(t, res); got != want {
		t.Fatalf("golden mismatch:\n got %s\nwant %s", got, want)
	}
}

func TestExternalURLGolden(t *testing.T) {
	res, err := ExternalURL("ui://ag2/docs", "https://docs.ag2.ai/")
	if err != nil {
		t.Fatalf("ExternalURL: %v", err)
	}
	want := `{"type":"resource","resource":{"uri":"ui://ag2/docs","mimeType":"text/uri-list","text":"https://docs.ag2.ai/"}}`
	if got := marshalResource(t, res); got != want {
		t.Fatalf("golden mismatch:\n got %s\nwant %s", got, want)
	}
}

func TestRemoteDOMGolden(t *testing.T) {
	t.Run("react", func(t *testing.T) {
		res, err := RemoteDOM("ui://ag2/dom", "root.appendChild(el)", WithFramework(FrameworkReact))
		if err != nil {
			t.Fatalf("RemoteDOM: %v", err)
		}
		want := `{"type":"resource","resource":{"uri":"ui://ag2/dom","mimeType":"application/vnd.mcp-ui.remote-dom+javascript; framework=react","text":"root.appendChild(el)"}}`
		if got := marshalResource(t, res); got != want {
			t.Fatalf("golden mismatch:\n got %s\nwant %s", got, want)
		}
	})
	t.Run("webcomponents", func(t *testing.T) {
		res, err := RemoteDOM("ui://ag2/dom", "root.appendChild(el)", WithFramework(FrameworkWebComponents))
		if err != nil {
			t.Fatalf("RemoteDOM: %v", err)
		}
		want := `{"type":"resource","resource":{"uri":"ui://ag2/dom","mimeType":"application/vnd.mcp-ui.remote-dom+javascript; framework=webcomponents","text":"root.appendChild(el)"}}`
		if got := marshalResource(t, res); got != want {
			t.Fatalf("golden mismatch:\n got %s\nwant %s", got, want)
		}
	})
	t.Run("default is react", func(t *testing.T) {
		res, err := RemoteDOM("ui://ag2/dom", "root.appendChild(el)")
		if err != nil {
			t.Fatalf("RemoteDOM: %v", err)
		}
		if res.Resource.MimeType != MIMERemoteDOMReact {
			t.Fatalf("default framework MIME: got %s", res.Resource.MimeType)
		}
	})
}

func TestUIMetadataPrefixedAndMetadataVerbatim(t *testing.T) {
	res, err := RawHTML("ui://ag2/greeting", "<h1>Hi</h1>",
		WithUIMetadata(map[string]any{"preferred-frame-size": []string{"800px", "600px"}}),
		WithMetadata(map[string]any{"title": "Greeting"}))
	if err != nil {
		t.Fatalf("RawHTML: %v", err)
	}
	want := `{"type":"resource","resource":{"uri":"ui://ag2/greeting","mimeType":"text/html",` +
		`"_meta":{"mcpui.dev/ui-preferred-frame-size":["800px","600px"],"title":"Greeting"},"text":"<h1>Hi</h1>"}}`
	if got := marshalResource(t, res); got != want {
		t.Fatalf("golden mismatch:\n got %s\nwant %s", got, want)
	}
}

func TestMetadataOverridesPrefixedUIKeys(t *testing.T) {
	// Caller metadata wins over the prefixed UI keys on a collision (Python:
	// `meta |= metadata`).
	res, err := RawHTML("ui://ag2/greeting", "<h1>Hi</h1>",
		WithUIMetadata(map[string]any{"size": "1px"}),
		WithMetadata(map[string]any{"mcpui.dev/ui-size": "100px"}))
	if err != nil {
		t.Fatalf("RawHTML: %v", err)
	}
	if got := res.Resource.Meta["mcpui.dev/ui-size"]; got != "100px" {
		t.Fatalf("verbatim metadata must win: got %v", got)
	}
}

func TestEmptyMetadataIsOmitted(t *testing.T) {
	res, err := RawHTML("ui://ag2/greeting", "<h1>Hi</h1>")
	if err != nil {
		t.Fatalf("RawHTML: %v", err)
	}
	if !strings.Contains(marshalResource(t, res), `"mimeType":"text/html","text"`) {
		t.Fatalf("_meta must drop off the wire when empty: %s", marshalResource(t, res))
	}
}

func TestURIWithoutUISchemeIsRejected(t *testing.T) {
	for _, tc := range []struct {
		name string
		call func() (Resource, error)
	}{
		{"raw_html", func() (Resource, error) { return RawHTML("https://ag2.ai/greeting", "<h1>Hi</h1>") }},
		{"external_url", func() (Resource, error) { return ExternalURL("https://ag2.ai/greeting", "https://docs.ag2.ai/") }},
		{"remote_dom", func() (Resource, error) { return RemoteDOM("https://ag2.ai/greeting", "root.appendChild(el)") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.call()
			if !errors.Is(err, ErrInvalidURI) {
				t.Fatalf("want ErrInvalidURI, got %v", err)
			}
			if want := "URI must start with 'ui://' but got: https://ag2.ai/greeting"; !strings.Contains(err.Error(), want) {
				t.Fatalf("error text: got %q, want prefix %q", err.Error(), want)
			}
		})
	}
}

func TestEmptyContentIsRejected(t *testing.T) {
	for _, tc := range []struct {
		name        string
		contentName string
		call        func() (Resource, error)
	}{
		{"raw_html", "html", func() (Resource, error) { return RawHTML("ui://ag2/greeting", "") }},
		{"external_url", "url", func() (Resource, error) { return ExternalURL("ui://ag2/docs", "") }},
		{"remote_dom", "script", func() (Resource, error) { return RemoteDOM("ui://ag2/dom", "") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.call()
			if !errors.Is(err, ErrInvalidContent) {
				t.Fatalf("want ErrInvalidContent, got %v", err)
			}
			if want := tc.contentName + " must be provided as a non-empty string"; !strings.HasPrefix(err.Error(), want) {
				t.Fatalf("error text: got %q, want prefix %q", err.Error(), want)
			}
		})
	}
}

func TestRemoteDOMRejectsUnknownFramework(t *testing.T) {
	_, err := RemoteDOM("ui://ag2/dom", "root.appendChild(el)", WithFramework("svelte"))
	if !errors.Is(err, ErrInvalidContent) {
		t.Fatalf("want ErrInvalidContent, got %v", err)
	}
	if want := "framework must be 'react' or 'webcomponents', got: svelte"; !strings.Contains(err.Error(), want) {
		t.Fatalf("error text: got %q, want %q", err.Error(), want)
	}
}

func TestUnknownEncodingIsRejected(t *testing.T) {
	_, err := RawHTML("ui://ag2/greeting", "<h1>Hi</h1>", WithEncoding("base64"))
	if !errors.Is(err, ErrInvalidContent) {
		t.Fatalf("want ErrInvalidContent, got %v", err)
	}
	if want := "Invalid encoding type: base64"; !strings.Contains(err.Error(), want) {
		t.Fatalf("error text: got %q, want %q", err.Error(), want)
	}
}

func TestTheURIIsCheckedBeforeTheFramework(t *testing.T) {
	// Both wrong: the shared uri guard must be the error the caller trips first.
	_, err := RemoteDOM("https://ag2.ai/dom", "root.appendChild(el)", WithFramework("svelte"))
	if !errors.Is(err, ErrInvalidURI) {
		t.Fatalf("want ErrInvalidURI first, got %v", err)
	}
}

// WithUIResource carries the resource block as a DataInput part of the result.
// The embedded bytes go through the framework's default encoder, which — like
// for any content part, TextInput included — HTML-escapes (< → \u003c) and
// stamps the zero created_at, so the assertion is semantic: the part decodes
// back to exactly the resource the constructor built.
func TestWithUIResourceAppendsResourceParts(t *testing.T) {
	res, err := ExternalURL("ui://ag2/docs", "https://docs.ag2.ai/")
	if err != nil {
		t.Fatalf("ExternalURL: %v", err)
	}
	result := events.NewToolResult(false, "added to cart")
	out := WithUIResource(result, res)
	if len(out.Parts) != 2 {
		t.Fatalf("want 2 parts, got %d", len(out.Parts))
	}
	if out.Parts[0].EventType() != "TextInput" {
		t.Fatalf("existing parts must be kept, got %s", out.Parts[0].EventType())
	}
	b, err := events.MarshalEvent(out.Parts[1])
	if err != nil {
		t.Fatalf("marshal part: %v", err)
	}
	var part struct {
		Type string `json:"@type"`
		Data struct {
			Type     string `json:"type"`
			Resource struct {
				URI      string `json:"uri"`
				MimeType string `json:"mimeType"`
				Text     string `json:"text"`
			} `json:"resource"`
		} `json:"data"`
	}
	if err := json.Unmarshal(b, &part); err != nil {
		t.Fatalf("unmarshal part: %v", err)
	}
	if part.Type != "DataInput" || part.Data.Type != "resource" ||
		part.Data.Resource.URI != "ui://ag2/docs" ||
		part.Data.Resource.MimeType != "text/uri-list" ||
		part.Data.Resource.Text != "https://docs.ag2.ai/" {
		t.Fatalf("embedded resource mismatch: %s", b)
	}
}
