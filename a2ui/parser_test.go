package a2ui

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type recorder struct {
	header http.Header
	code   int
	body   string
}

func (r *recorder) Header() http.Header { return r.header }
func (r *recorder) Write(b []byte) (int, error) {
	r.body += string(b)
	return len(b), nil
}
func (r *recorder) WriteHeader(code int) { r.code = code }

type serverResponse struct {
	code   int
	body   string
	header http.Header
}

func serve(t *testing.T, srv *Server, body string) serverResponse {
	t.Helper()
	return serveWithAuth(t, srv, body, "")
}

// serveWithAuth is serve with an Authorization header ("" sends none).
func serveWithAuth(t *testing.T, srv *Server, body, authz string) serverResponse {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, "/a2ui", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if authz != "" {
		req.Header.Set("Authorization", authz)
	}
	rec := &recorder{header: make(http.Header)}
	srv.ServeHTTP(rec, req)
	return serverResponse{code: rec.code, body: rec.body, header: rec.header}
}

// httptest round-trip: the full server path over a real HTTP client.
func TestServerHTTPRoundTrip(t *testing.T) {
	registry := NewActionRegistry()
	registry.Register(Action{Name: "echo",
		Fn: func(_ context.Context, params map[string]any) (any, error) {
			return map[string]any{
				"updateDataModel": map[string]any{"surfaceId": "s1", "path": "/last", "value": params["v"]},
			}, nil
		}})
	srv := NewServer(WithActions(registry), WithEncoding(EncodingJSONL))
	ts := httptest.NewServer(srv)
	defer ts.Close()

	body := `{"messages": [], "a2ui": [{"version": "v1.0", "action": {"name": "echo",
		"surfaceId": "s1", "sourceComponentId": "btn1", "timestamp": "",
		"context": {"v": "round-trip"}}}]}`
	resp, err := http.Post(ts.URL, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(resp.Body); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: %d (%s)", resp.StatusCode, buf.String())
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/x-ndjson" {
		t.Errorf("content type: %s", ct)
	}
	want := `{"version":"v1.0","updateDataModel":{"surfaceId":"s1","path":"/last","value":"round-trip"}}` + "\n"
	if buf.String() != want {
		t.Errorf("round-trip body:\n  want %q\n  got  %q", want, buf.String())
	}
}

func TestMessageEnvelopeErrors(t *testing.T) {
	var m Message
	err := json.Unmarshal([]byte(`{"createSurface": {"surfaceId": "s"}, "deleteSurface": {"surfaceId": "s"}}`), &m)
	if err == nil || !strings.Contains(err.Error(), "both present") {
		t.Errorf("ambiguous payload: %v", err)
	}
	err = json.Unmarshal([]byte(`{"version": "v1.0"}`), &m)
	if err == nil || !strings.Contains(err.Error(), "exactly one of") {
		t.Errorf("no payload: %v", err)
	}
	if _, err := m.MarshalJSON(); err != ErrNoPayload {
		t.Errorf("marshal with no payload: %v", err)
	}
	two := Message{CreateSurface: &CreateSurface{SurfaceID: "s"}, DeleteSurface: &DeleteSurface{SurfaceID: "s"}}
	if _, err := two.MarshalJSON(); err != ErrAmbiguousPayload {
		t.Errorf("marshal with two payloads: %v", err)
	}
}

func TestComponentWire(t *testing.T) {
	// Flat component round-trip with props outside the envelope.
	raw := `{"id":"img1","component":"Image","url":"https://x/y.png","fit":"cover"}`
	var c Component
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		t.Fatal(err)
	}
	if c.ID != "img1" || c.Kind != "Image" || c.Props["url"] != "https://x/y.png" || c.Props["fit"] != "cover" {
		t.Errorf("unmarshal: %+v", c)
	}
	out, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	// Props outside the envelope are emitted sorted (the Go map rule; Python
	// golden comparisons are structural where this differs).
	want := `{"id":"img1","component":"Image","fit":"cover","url":"https://x/y.png"}`
	if string(out) != want {
		t.Errorf("wire form:\n  want %s\n  got  %s", want, out)
	}

	// Children template form (v1.0 ChildList).
	var tpl Component
	if err := json.Unmarshal([]byte(`{"id":"row","component":"Row","children":{"componentId":"item","path":"/items"}}`), &tpl); err != nil {
		t.Fatal(err)
	}
	if tpl.Children.Template == nil || tpl.Children.Template.ComponentID != "item" || tpl.Children.Template.Path != "/items" {
		t.Errorf("children template: %+v", tpl.Children)
	}
	out, err = json.Marshal(tpl)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"children":{"componentId":"item","path":"/items"}`) {
		t.Errorf("children template wire: %s", out)
	}

	// Missing id/component are rejected (fresh values, not the reused var).
	if err := json.Unmarshal([]byte(`{"component":"Text"}`), new(Component)); err == nil {
		t.Error("missing id must be rejected")
	}
	if err := json.Unmarshal([]byte(`{"id":"x"}`), new(Component)); err == nil {
		t.Error("missing component must be rejected")
	}
}

func TestStripMarkdownFences(t *testing.T) {
	cases := []struct{ in, want string }{
		{"plain", "plain"},
		{"```json\n{}\n```", "{}"},
		{"```\n{}\n```", "{}"},
		{"```json {\"a\": 1}```", `{"a": 1}`},
		{"```{\"a\": 1}```", `{"a": 1}`},
		// A greedy info string followed by "{" is stripped (verified against
		// the Python regex).
		{"```jsonx{\"a\": 1}```", `{"a": 1}`},
		{"```json\n{}\n```\n  ", "{}"},
	}
	for _, tc := range cases {
		if got := StripMarkdownFences(tc.in); got != tc.want {
			t.Errorf("StripMarkdownFences(%q): want %q, got %q", tc.in, tc.want, got)
		}
	}
}

func TestParseJSONLAndArrayForms(t *testing.T) {
	mgr, err := NewSchemaManager()
	if err != nil {
		t.Fatal(err)
	}
	parser := NewParser(mgr)

	// A bare object (not wrapped in an array) parses as one operation.
	r := parser.Parse(`<a2ui-json>{"version": "v1.0", "deleteSurface": {"surfaceId": "s"}}</a2ui-json>`)
	if len(r.Operations) != 1 {
		t.Fatalf("bare object: %+v", r)
	}

	// A scalar block is rejected with the type name in the error.
	r = parser.Parse(`<a2ui-json>42</a2ui-json>`)
	if r.ParseError == "" || !strings.Contains(r.ParseError, "Expected JSON array or object, got") {
		t.Errorf("scalar block: %q", r.ParseError)
	}

	// Empty block: no A2UI content.
	r = parser.Parse(`<a2ui-json>   </a2ui-json>`)
	if r.HasA2UI {
		t.Errorf("empty block: %+v", r)
	}

	// JSONL with a malformed line reports the whole-block error only when
	// line parsing also fails; a single valid line parses.
	r = parser.Parse("<a2ui-json>\nnot json\n</a2ui-json>")
	if r.ParseError == "" {
		t.Error("malformed block must set ParseError")
	}
}

func TestParseMultiPayloadOperationRejected(t *testing.T) {
	mgr, err := NewSchemaManager()
	if err != nil {
		t.Fatal(err)
	}
	parser := NewParser(mgr)
	r := parser.Parse(`<a2ui-json>[{"createSurface": {"surfaceId": "a"}, "deleteSurface": {"surfaceId": "b"}}]</a2ui-json>`)
	if r.ParseError == "" || !strings.HasPrefix(r.ParseError, "Operation 0 is not an A2UI message:") {
		t.Errorf("multi-payload op: %q", r.ParseError)
	}
}
