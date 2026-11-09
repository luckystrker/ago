package a2ui

import "testing"

// IsMessageEnvelope is the exported predicate handlers use to tell a message
// object from a plain JSON value; it keys off the payload vocabulary, not the
// version (the parser and validator own that).
func TestIsMessageEnvelope(t *testing.T) {
	cases := []struct {
		name  string
		value any
		want  bool
	}{
		{"deleteSurface", map[string]any{"deleteSurface": map[string]any{"surfaceId": "s1"}}, true},
		{"agentFunctionResponse", map[string]any{"agentFunctionResponse": map[string]any{}}, true},
		{"version only", map[string]any{"version": "1.0"}, false},
		{"unknown payload", map[string]any{"wat": map[string]any{}}, false},
		{"empty object", map[string]any{}, false},
		{"not an object", []any{map[string]any{"deleteSurface": map[string]any{}}}, false},
		{"nil", nil, false},
	}
	for _, tc := range cases {
		if got := IsMessageEnvelope(tc.value); got != tc.want {
			t.Errorf("%s: IsMessageEnvelope = %v, want %v", tc.name, got, tc.want)
		}
	}
}
