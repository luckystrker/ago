package searchtools

import (
	"net/http"
	"sync/atomic"
	"testing"

	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/middleware"
)

// countingMiddleware counts tool-execution hops, so a toolkit that attached its
// middleware twice would show up as two.
type countingMiddleware struct {
	middleware.Base
	calls *int32
}

func (m countingMiddleware) OnToolExecution(next middleware.ToolExecFunc, ev *events.ToolCallEvent, ctx *agocontext.Context) (events.Event, error) {
	atomic.AddInt32(m.calls, 1)
	return next(ev, ctx)
}

// TestToolkitMiddlewareRunsOnce asserts the toolkit middleware wraps a call
// exactly once (Python parity: the toolkit's middleware= is applied once to each
// member).
func TestToolkitMiddlewareRunsOnce(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		respondJSON(t, w, `{"results":[]}`)
	})
	var calls int32
	tk, err := NewExaToolkit("k", WithExaBaseURL(srv.URL), WithExaMiddleware(countingMiddleware{calls: &calls}))
	if err != nil {
		t.Fatalf("NewExaToolkit: %v", err)
	}
	_, cx := newTestContext()
	ev := callTool(t, tk, cx, "exa_search", `{"query":"q"}`)
	if _, ok := events.AsValue(ev).(events.ToolResultEvent); !ok {
		t.Fatalf("event %T is not a result", ev)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("middleware calls = %d, want exactly 1", got)
	}
}

// TestNilMiddlewareRejected asserts a nil middleware entry fails construction.
func TestNilMiddlewareRejected(t *testing.T) {
	if _, err := NewExaToolkit("k", WithExaMiddleware(nil)); err == nil {
		t.Fatal("a nil middleware must be rejected")
	}
	if _, err := NewSerplySearchToolkit("k", WithSerplyMiddleware(nil)); err == nil {
		t.Fatal("a nil middleware must be rejected")
	}
}
