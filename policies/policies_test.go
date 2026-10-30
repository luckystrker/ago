package policies

import (
	"testing"

	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/stream"
)

// testContext builds a conversation context over a fresh in-memory stream
// (used by policies that send events, i.e. AlertPolicy's fatal halt).
func testContext(t *testing.T) *agocontext.Context {
	t.Helper()
	return agocontext.New(stream.NewMemoryStream())
}
