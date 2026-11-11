package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	agocontext "github.com/luckystrker/ago/context"
)

func TestResourceTemplateMatching(t *testing.T) {
	read := func(_ context.Context, v map[string]string) (ResourceContent, error) {
		return ResourceContent{Text: "ok"}, nil
	}
	p, err := newResourceProvider(nil, []ResourceTemplate{
		{URITemplate: "weather://{city}", Name: "w", Read: read},
		{URITemplate: "files:///{+path}", Name: "f", Read: read},
		{URITemplate: "repo://{owner}/{name}", Name: "r", Read: read},
	})
	if err != nil {
		t.Fatal(err)
	}
	match := func(uri string) (map[string]string, bool) {
		for _, c := range p.templates {
			if v, ok := c.match(uri); ok {
				return v, true
			}
		}
		return nil, false
	}
	for _, tc := range []struct {
		uri  string
		want map[string]string // nil: addresses nothing
	}{
		{"weather://Paris", map[string]string{"city": "Paris"}},
		{"weather://New%20York", map[string]string{"city": "New York"}},
		{"weather://a+b", map[string]string{"city": "a+b"}},   // a literal + is preserved
		{"weather://%2520", map[string]string{"city": "%20"}}, // decoded once
		{"weather://a/b", nil},   // {var} is one segment
		{"weather://a%2Fb", nil}, // and stays one after decoding
		{"weather://%ff", nil},   // not UTF-8
		{"weather://%zz", nil},   // not an escape at all
		{"files:///a/b/c.txt", map[string]string{"path": "a/b/c.txt"}},
		{"files:///a%2Fb", map[string]string{"path": "a/b"}}, // {+var} may span /
		{"files:///../etc/passwd", map[string]string{"path": "../etc/passwd"}},
		{"repo://ago/mcp", map[string]string{"owner": "ago", "name": "mcp"}},
		{"repo://ago", nil},
		{"other://x", nil},
	} {
		got, ok := match(tc.uri)
		if tc.want == nil {
			if ok {
				t.Errorf("%s matched %v, want nothing", tc.uri, got)
			}
			continue
		}
		if !ok || len(got) != len(tc.want) {
			t.Errorf("%s = %v (%v), want %v", tc.uri, got, ok, tc.want)
			continue
		}
		for k, v := range tc.want {
			if got[k] != v {
				t.Errorf("%s: %s = %q, want %q", tc.uri, k, got[k], v)
			}
		}
	}
}

func TestResourceRegistrationRefusals(t *testing.T) {
	ok := func(context.Context, *agocontext.Context) (ResourceContent, error) { return ResourceContent{}, nil }
	if _, err := newResourceProvider([]Resource{{URI: "a://x", Name: "x", Read: ok}, {URI: "a://x", Name: "y", Read: ok}}, nil); !errors.Is(err, ErrConflict) {
		t.Errorf("duplicate URI = %v, want ErrConflict", err)
	}
	if _, err := newResourceProvider([]Resource{{Name: "x", Read: ok}}, nil); !errors.Is(err, ErrNotProvided) {
		t.Errorf("no URI = %v, want ErrNotProvided", err)
	}
	if _, err := newPromptProvider([]Prompt{{Name: "p"}}); !errors.Is(err, ErrNotProvided) {
		t.Errorf("no Render = %v, want ErrNotProvided", err)
	}
	r := func(context.Context, map[string]string) ([]PromptMessage, error) { return nil, nil }
	if _, err := newPromptProvider([]Prompt{{Name: "p", Render: r}, {Name: "p", Render: r}}); !errors.Is(err, ErrConflict) {
		t.Errorf("duplicate prompt = %v, want ErrConflict", err)
	}
	if _, err := NewServer(ServerConfig{Ask: func(context.Context, string) (string, error) { return "", nil }, ElicitationPolicy: "sometimes"}); !errors.Is(err, ErrNotProvided) {
		t.Errorf("unknown elicitation policy = %v, want ErrNotProvided", err)
	}
}

func TestResourceReadFailureIsAnInternalErrorNotNotFound(t *testing.T) {
	_, ts := fullServer(t, TransportConfig{Stateful: true}, func(c *ServerConfig) {
		c.Resources = append(c.Resources, Resource{URI: "mem://broken", Name: "broken",
			Read: func(context.Context, *agocontext.Context) (ResourceContent, error) {
				return ResourceContent{}, errors.New("disk on fire")
			}})
	})
	sid := openSession(t, ts.URL, `{}`, "")
	_, e := call(t, ts.URL, sid, "resources/read", 3, `{"uri":"mem://broken"}`)
	if e == nil || e["code"] != -32603.0 || e["message"] != "disk on fire" {
		t.Errorf("error = %v, want internal error carrying the reader's message", e)
	}
}

func TestMemoryEventStore(t *testing.T) {
	st := NewMemoryEventStore(3)
	ctx := context.Background()
	var ids []string
	for i := 0; i < 5; i++ {
		id, err := st.StoreEvent(ctx, "s1", []byte(`{"n":`+string(rune('0'+i))+`}`))
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	other, _ := st.StoreEvent(ctx, "s2", []byte(`{"other":true}`))
	// Ids are unique across streams, and a replay stays on its stream.
	if other == ids[0] || other == ids[4] {
		t.Error("event ids collide across streams")
	}
	var got []string
	stream, err := st.ReplayEventsAfter(ctx, ids[2], func(id string, msg json.RawMessage) error {
		got = append(got, string(msg))
		return nil
	})
	if err != nil || stream != "s1" || len(got) != 2 || got[0] != `{"n":3}` || got[1] != `{"n":4}` {
		t.Errorf("replay = %v stream %q err %v", got, stream, err)
	}
	// The bound dropped the oldest events; resuming from one is an unknown id.
	if _, err := st.ReplayEventsAfter(ctx, ids[0], func(string, json.RawMessage) error { return nil }); !errors.Is(err, ErrUnknownEvent) {
		t.Errorf("replay from an evicted id = %v, want ErrUnknownEvent", err)
	}
	// A priming event is a cursor: stored, never replayed — even when a client
	// resumes from an id that precedes it.
	first, _ := st.StoreEvent(ctx, "s3", []byte(`{"before":"priming"}`))
	p, _ := st.StoreEvent(ctx, "s3", nil)
	st.StoreEvent(ctx, "s3", []byte(`{"after":"priming"}`))
	got = nil
	st.ReplayEventsAfter(ctx, p, func(_ string, msg json.RawMessage) error { got = append(got, string(msg)); return nil })
	if len(got) != 1 || got[0] != `{"after":"priming"}` {
		t.Errorf("replay after priming = %v", got)
	}
	got = nil
	st.ReplayEventsAfter(ctx, first, func(_ string, msg json.RawMessage) error { got = append(got, string(msg)); return nil })
	if len(got) != 1 || got[0] != `{"after":"priming"}` {
		t.Errorf("replay across a priming event = %v, want only the real message", got)
	}
}
