package a2ui

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

func TestToJSONLError(t *testing.T) {
	// A prop value encoding/json cannot express (a func) must surface as an
	// error, not a panic, and must not emit partial output.
	bad := []Message{{Version: VersionV1, UpdateComponents: &UpdateComponents{
		SurfaceID: "s1",
		Components: []Component{
			{ID: "c1", Kind: "Text", Props: map[string]any{"text": func() {}}},
		},
	}}}
	got, err := ToJSONL(bad)
	if err == nil {
		t.Fatalf("non-JSON prop: want error, got %q", got)
	}
	var unsupported *json.UnsupportedTypeError
	if !errors.As(err, &unsupported) {
		t.Errorf("want json.UnsupportedTypeError, got %v", err)
	}
	if got != "" {
		t.Errorf("partial output on error: %q", got)
	}

	// A payload-less message is reported with its sentinel.
	got, err = ToJSONL([]Message{{}})
	if !errors.Is(err, ErrNoPayload) {
		t.Errorf("no payload: want ErrNoPayload, got %v", err)
	}
	if got != "" {
		t.Errorf("partial output on error: %q", got)
	}

	// The failing message is named by index even when others are fine.
	good := Message{Version: VersionV1, DeleteSurface: &DeleteSurface{SurfaceID: "s1"}}
	if _, err = ToJSONL([]Message{good, {}}); err == nil || !errors.Is(err, ErrNoPayload) {
		t.Errorf("second message failure: %v", err)
	}
}

func TestServerUnserializableMessageFailsCleanly(t *testing.T) {
	// The HTTP path must not panic either: the frame builder turns a
	// marshal failure into a 500 before the status is committed.
	srv := NewServer(WithPromptHandler(func(context.Context, string) (Turn, error) {
		return Turn{Messages: []Message{{Version: VersionV1, UpdateComponents: &UpdateComponents{
			SurfaceID: "s1",
			Components: []Component{
				{ID: "c1", Kind: "Text", Props: map[string]any{"text": make(chan int)}},
			},
		}}}}, nil
	}))
	resp := serve(t, srv, `{"messages": [{"role": "user", "content": "x"}]}`)
	if resp.code != 500 {
		t.Fatalf("status: %d (%s)", resp.code, resp.body)
	}
	if !containsJSONString(resp.body, "could not serialize response message") {
		t.Errorf("error body: %q", resp.body)
	}
}

func containsJSONString(body, needle string) bool {
	var decoded struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &decoded); err != nil {
		return false
	}
	return decoded.Error == needle
}
