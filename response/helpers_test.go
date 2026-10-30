package response

import (
	"encoding/json"
	"testing"

	"github.com/luckystrker/ago/stream"
)

// streamNewForTest builds a throwaway conversation stream for context arguments.
func streamNewForTest() stream.Stream {
	return stream.NewMemoryStream()
}

// mustJSON marshals v the way the prompt renderer does (indented, sorted keys).
func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatalf("MarshalIndent error: %v", err)
	}
	return string(b)
}
