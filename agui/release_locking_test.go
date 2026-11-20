package agui

import (
	stdcontext "context"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/luckystrker/ago/agent"
	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/internal/sse"
	agotest "github.com/luckystrker/ago/testing"
	"github.com/luckystrker/ago/tools"
)

// A turn is released without waiting for it to wind down, and the registry
// never waits on a turn that waits on the registry's lock (the winding-down
// turn takes it in discard). These tests drive each place that releases a turn
// through the public handler: a release that waited under that lock would wedge
// the request — and every later one — so each test bounds its requests with a
// timer of its own and fails with a goroutine dump instead of hanging the
// package. The two waiting kinds (server shutdown, a cancelled resume) are
// pinned too, and so is the turn's own end after a release that does not wait.

const releaseTestBound = 5 * time.Second

// selfReleaseBound is far under the question timeout the self-release test
// asks with, so a turn that waited for its own question to time out fails it.
const selfReleaseBound = 500 * time.Millisecond

// settle runs fn on its own goroutine and fails the test, with every
// goroutine's stack, when fn has not returned within the bound.
func settle(t *testing.T, what string, fn func()) time.Duration {
	t.Helper()
	start := time.Now()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
		return time.Since(start)
	case <-time.After(releaseTestBound):
		buf := make([]byte, 1<<20)
		buf = buf[:runtime.Stack(buf, true)]
		t.Fatalf("%s did not return within %v; goroutines:\n%s", what, releaseTestBound, buf)
		return 0
	}
}

// awaitUnwound waits, up to the bound, for a released turn's run scope to close
// and its driver to return: a release that does not wait must still end the
// turn, or the goroutine leaks. The wait is on the turn's own signals, not on a
// delay.
func awaitUnwound(t *testing.T, what string, turn *servedTurn) {
	t.Helper()
	awaitUnwoundWithin(t, what, turn, releaseTestBound)
}

// awaitUnwoundWithin is awaitUnwound with a bound of the caller's choosing, for
// a turn that must end sooner than something else would have ended it.
func awaitUnwoundWithin(t *testing.T, what string, turn *servedTurn, bound time.Duration) {
	t.Helper()
	if turn == nil || turn.closed == nil {
		t.Fatalf("%s was never released", what)
	}
	for _, c := range []chan struct{}{turn.closed, turn.driverDone} {
		select {
		case <-c:
		case <-time.After(bound):
			buf := make([]byte, 1<<20)
			buf = buf[:runtime.Stack(buf, true)]
			t.Fatalf("%s did not wind down within %v; goroutines:\n%s", what, bound, buf)
		}
	}
}

// postRunQuiet posts a run input and reads the response to its end. Unlike
// postRunURL it reports through its return values, so it can run off the test
// goroutine.
func postRunQuiet(url, body string) ([]Event, error) {
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var evs []Event
	sc := sse.NewScanner(resp.Body)
	for sc.Next() {
		_, data := sc.Event()
		ev, err := UnmarshalEvent(data)
		if err != nil {
			return evs, err
		}
		evs = append(evs, ev)
	}
	return evs, sc.Err()
}

func runBodyFor(threadID, runID string) string {
	return `{"threadId":"` + threadID + `","runId":"` + runID +
		`","messages":[{"id":"m1","role":"user","content":"hi"}]}`
}

// releaseHooks are the points a test can hold a run at: gate runs in the tool
// before it asks (so runs can be lined up before any of them asks), unwind runs
// in the tool once its question failed — the turn is winding down, and the
// hook holds that up until the test lets it go.
type releaseHooks struct {
	gate   func()
	unwind func()
}

// releaseRig is what a test can see of the served runs: the turn behind each
// question, in asking order.
type releaseRig struct {
	mu    sync.Mutex
	turns []*servedTurn
}

func (r *releaseRig) record(t *servedTurn) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.turns = append(r.turns, t)
}

func (r *releaseRig) turn(i int) *servedTurn {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.turns[i]
}

// releaseServer is a served agent whose every run asks one question: the tool
// puts it with the timeout in seconds that the asking order picks from timeouts
// (0 = the retention TTL; the last entry repeats). The script has a tool call
// for each of runs turns and plain replies after them.
func releaseServer(t *testing.T, runs int, timeouts []float64, hooks releaseHooks, opts ...ServerOption) (*Server, *httptest.Server, *releaseRig) {
	t.Helper()
	srv, err := NewServer(opts...)
	if err != nil {
		t.Fatalf("server: %v", err)
	}
	rig := &releaseRig{}
	var (
		mu    sync.Mutex
		asked int
	)
	tool, err := tools.NewFunctionTool("ask_human", "Ask the human",
		func(args struct{}, cx *agocontext.Context) (string, error) {
			if hooks.gate != nil {
				hooks.gate()
			}
			rig.record(srv.turns.turnForStream(cx.Stream.ID()))
			mu.Lock()
			timeout := 0.0
			if len(timeouts) > 0 {
				timeout = timeouts[min(asked, len(timeouts)-1)]
			}
			asked++
			mu.Unlock()
			answer, err := cx.Input(stdcontext.Background(), testQuestion, time.Duration(timeout*float64(time.Second)))
			if err != nil {
				if hooks.unwind != nil {
					hooks.unwind()
				}
				return "", err
			}
			return "heard: " + answer, nil
		})
	if err != nil {
		t.Fatalf("tool: %v", err)
	}
	var script []*events.ModelResponse
	for i := 0; i < runs; i++ {
		script = append(script, &events.ModelResponse{ToolCalls: events.ToolCallsEvent{Calls: []events.ToolCallEvent{
			{ID: "c1", Name: "ask_human", Arguments: "{}"},
		}}})
	}
	for i := 0; i < 2*runs+2; i++ {
		script = append(script, &events.ModelResponse{Message: &events.ModelMessage{Content: "noted"}})
	}
	base := append([]agent.Option{agent.WithTools(tool)}, srv.Options()...)
	h := srv.Handler(agent.New("served", "You are served.",
		append(base, agent.WithConfig(agotest.NewTestConfig(script...)))...))
	ts := httptest.NewServer(h)
	// Close waits for every outstanding request: a handler wedged on the
	// registry lock would hang the cleanup behind the failure it reports.
	t.Cleanup(func() {
		closed := make(chan struct{})
		go func() { ts.Close(); close(closed) }()
		select {
		case <-closed:
		case <-time.After(2 * time.Second):
		}
	})
	return srv, ts, rig
}

func requireInterrupted(t *testing.T, what string, evs []Event, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
	if len(evs) == 0 {
		t.Fatalf("%s: no events", what)
	}
	fin, ok := evs[len(evs)-1].(RunFinishedEvent)
	if !ok {
		t.Fatalf("%s: last event = %T, want RUN_FINISHED", what, evs[len(evs)-1])
	}
	if _, ok := fin.Outcome.(InterruptOutcome); !ok {
		t.Fatalf("%s: outcome = %#v, want an interrupt", what, fin.Outcome)
	}
}

// The expired question is released when the next plain request for its thread
// sweeps the registry (holds → evictExpiredLocked).
func TestServerReleasesExpiredTurnOutsideRegistryLock(t *testing.T) {
	clock := newDrivenClock()
	srv, ts, rig := releaseServer(t, 1, nil, releaseHooks{},
		WithRetention(Retention{TTL: time.Minute, MaxHeld: 4}),
		WithClock(clock.Now))

	first, err := postRunQuiet(ts.URL, runBodyFor("t1", "r1"))
	requireInterrupted(t, "first POST", first, err)
	if !srv.turns.holds("t1") {
		t.Fatal("the question is not held")
	}
	clock.Advance(time.Minute)

	var (
		evs  []Event
		rerr error
	)
	settle(t, "the POST that meets the expired question", func() {
		evs, rerr = postRunQuiet(ts.URL, runBodyFor("t1", "r2"))
	})
	if rerr != nil {
		t.Fatalf("second POST: %v", rerr)
	}
	if len(evs) == 0 {
		t.Fatal("second POST: no events")
	}
	awaitUnwound(t, "the expired turn", rig.turn(0))
}

// An expired question is also swept by a resume for another thread (take →
// evictExpiredLocked).
func TestServerReleasesExpiredTurnOnResumeOutsideRegistryLock(t *testing.T) {
	clock := newDrivenClock()
	srv, ts, rig := releaseServer(t, 2, nil, releaseHooks{},
		WithRetention(Retention{TTL: time.Minute, MaxHeld: 4}),
		WithClock(clock.Now))

	first, err := postRunQuiet(ts.URL, runBodyFor("t1", "r1"))
	requireInterrupted(t, "first POST", first, err)
	clock.Advance(time.Minute)

	var (
		evs  []Event
		rerr error
	)
	settle(t, "the resume that sweeps another thread's expired question", func() {
		evs, rerr = postRunQuiet(ts.URL, resumeBody("t2", "r2", "nothing-held", "blue"))
	})
	if rerr != nil {
		t.Fatalf("resume POST: %v", rerr)
	}
	if len(evs) == 0 {
		t.Fatal("resume POST: no events")
	}
	if srv.turns.holds("t1") {
		t.Fatal("the expired question is still held")
	}
	awaitUnwound(t, "the expired turn", rig.turn(0))
}

// A question that pushes the registry past MaxHeld releases the held turn
// nearest its deadline (hold's eviction loop).
func TestServerReleasesOverflowTurnOutsideRegistryLock(t *testing.T) {
	srv, ts, rig := releaseServer(t, 2, nil, releaseHooks{},
		WithRetention(Retention{TTL: time.Minute, MaxHeld: 1}))

	first, err := postRunQuiet(ts.URL, runBodyFor("t1", "r1"))
	requireInterrupted(t, "first POST", first, err)

	var (
		second []Event
		serr   error
	)
	settle(t, "the POST whose question overflows MaxHeld", func() {
		second, serr = postRunQuiet(ts.URL, runBodyFor("t2", "r2"))
	})
	requireInterrupted(t, "second POST", second, serr)
	if srv.turns.holds("t1") {
		t.Fatal("the overflowed question is still held")
	}
	if !srv.turns.holds("t2") {
		t.Fatal("the new question is not held")
	}
	awaitUnwound(t, "the overflowed turn", rig.turn(0))
}

// A new question on a thread supersedes the turn that thread held before (hold
// replacing the previous turn). Two runs of one thread ask at once: the
// handler admits both, because neither holds a question yet.
func TestServerReleasesSupersededTurnOutsideRegistryLock(t *testing.T) {
	var (
		mu      sync.Mutex
		arrived int
		both    = make(chan struct{})
	)
	gate := func() {
		mu.Lock()
		arrived++
		if arrived == 2 {
			close(both)
		}
		mu.Unlock()
		<-both
	}
	srv, ts, rig := releaseServer(t, 2, nil, releaseHooks{gate: gate},
		WithRetention(Retention{TTL: time.Minute, MaxHeld: 4}))

	type result struct {
		evs []Event
		err error
	}
	results := make(chan result, 2)
	for _, run := range []string{"r1", "r2"} {
		go func(run string) {
			evs, err := postRunQuiet(ts.URL, runBodyFor("t1", run))
			results <- result{evs, err}
		}(run)
	}
	for i := 0; i < 2; i++ {
		settle(t, "a POST of two runs asking on one thread", func() {
			res := <-results
			if res.err != nil {
				t.Errorf("POST: %v", res.err)
			}
		})
	}
	if !srv.turns.holds("t1") {
		t.Fatal("the thread holds no question after both runs asked")
	}
	// Whichever asked first was superseded by the other: released, and ended.
	released := 0
	for i := 0; i < 2; i++ {
		if turn := rig.turn(i); turn.closed != nil {
			released++
			awaitUnwound(t, "the superseded turn", turn)
		}
	}
	if released != 1 {
		t.Fatalf("%d of the two turns were released, want the superseded one only", released)
	}
}

// A question whose own deadline is the nearest while the registry is full is
// the overflow victim: the turn that is asking is released. It cannot wait for
// its own run to wind down — that run is parked on this very question, so the
// wait would last until the question's own timeout — and the POST answers with
// the interrupt at once.
func TestServerAnswersPromptlyWhenAskingTurnIsOverflowVictim(t *testing.T) {
	srv, ts, rig := releaseServer(t, 2, []float64{0, 3}, releaseHooks{},
		WithRetention(Retention{TTL: time.Minute, MaxHeld: 1}))

	first, err := postRunQuiet(ts.URL, runBodyFor("t1", "r1"))
	requireInterrupted(t, "first POST", first, err)

	// Asked with a three-second timeout against t1's minute, so t2's deadline
	// is the nearest and t2 is the overflow victim.
	var (
		second []Event
		serr   error
	)
	took := settle(t, "the POST whose own question is the overflow victim", func() {
		second, serr = postRunQuiet(ts.URL, runBodyFor("t2", "r2"))
	})
	requireInterrupted(t, "second POST", second, serr)
	if took > selfReleaseBound {
		t.Fatalf("second POST answered after %v, want under %v: the turn waited on its own release", took, selfReleaseBound)
	}
	if srv.turns.holds("t2") {
		t.Fatal("the released question is still held")
	}
	awaitUnwound(t, "the asking turn", rig.turn(1))
}

// Shutting the server down waits for every turn to wind down: a turn's cleanup
// has run before Close returns. The tool is held at its unwinding step, so a
// Close that only cancelled would return at once and be seen to.
func TestServerCloseWaitsForHeldTurnsToUnwind(t *testing.T) {
	var (
		entered = make(chan struct{}, 1)
		proceed = make(chan struct{})
		unwound atomic.Bool
	)
	srv, ts, rig := releaseServer(t, 1, nil, releaseHooks{unwind: func() {
		entered <- struct{}{}
		<-proceed
		unwound.Store(true)
	}}, WithRetention(Retention{TTL: time.Minute, MaxHeld: 4}))

	first, err := postRunQuiet(ts.URL, runBodyFor("t1", "r1"))
	requireInterrupted(t, "first POST", first, err)

	closed := make(chan bool, 1)
	go func() {
		srv.Close()
		closed <- unwound.Load()
	}()
	select {
	case <-entered:
	case <-time.After(releaseTestBound):
		t.Fatal("the held turn never began to wind down after Close")
	}
	// The turn cannot finish until proceed is closed. A Close that returns in
	// this window did not wait for it; a waiting one is still blocked when the
	// window ends.
	select {
	case <-closed:
		t.Fatal("Close returned while a turn was still winding down")
	case <-time.After(200 * time.Millisecond):
	}
	close(proceed)
	select {
	case done := <-closed:
		if !done {
			t.Fatal("Close returned before the turn finished winding down")
		}
	case <-time.After(releaseTestBound):
		t.Fatal("Close did not return after the turn finished winding down")
	}
	awaitUnwound(t, "the held turn", rig.turn(0))
}

// A cancelled resume answers the client only after the abandoned turn has
// wound down: the closing events it owes are taken from a turn that has stopped
// producing them. The tool is held at its unwinding step, so an answer sent
// without the wait would arrive while the turn is still running.
func TestServerCancelledResumeWaitsForTurnToUnwind(t *testing.T) {
	var (
		entered = make(chan struct{}, 1)
		proceed = make(chan struct{})
		unwound atomic.Bool
	)
	_, ts, rig := releaseServer(t, 1, nil, releaseHooks{unwind: func() {
		entered <- struct{}{}
		<-proceed
		unwound.Store(true)
	}}, WithRetention(Retention{TTL: time.Minute, MaxHeld: 4}))

	first, err := postRunQuiet(ts.URL, runBodyFor("t1", "r1"))
	requireInterrupted(t, "first POST", first, err)
	interrupt := soleInterrupt(t, first)

	type answered struct {
		evs       []Event
		err       error
		unwoundAt bool
	}
	got := make(chan answered, 1)
	go func() {
		body := `{"threadId":"t1","runId":"r2","messages":[],"resume":[{"interruptId":"` +
			interrupt.ID + `","status":"cancelled"}]}`
		evs, err := postRunQuiet(ts.URL, body)
		got <- answered{evs, err, unwound.Load()}
	}()
	select {
	case <-entered:
	case <-time.After(releaseTestBound):
		t.Fatal("the abandoned turn never began to wind down")
	}
	select {
	case <-got:
		t.Fatal("the cancelled resume was answered while the turn was still winding down")
	case <-time.After(200 * time.Millisecond):
	}
	close(proceed)
	select {
	case a := <-got:
		if a.err != nil {
			t.Fatalf("cancelled resume: %v", a.err)
		}
		if !a.unwoundAt {
			t.Fatal("the cancelled resume was answered before the turn finished winding down")
		}
		if n := len(a.evs); n == 0 {
			t.Fatal("cancelled resume: no events")
		} else if fin, ok := a.evs[n-1].(RunFinishedEvent); !ok {
			t.Fatalf("last event = %T, want RUN_FINISHED", a.evs[n-1])
		} else if _, ok := fin.Outcome.(CancelledOutcome); !ok {
			t.Fatalf("outcome = %#v, want cancelled", fin.Outcome)
		}
	case <-time.After(releaseTestBound):
		t.Fatal("the cancelled resume was not answered after the turn wound down")
	}
	awaitUnwound(t, "the abandoned turn", rig.turn(0))
}

// A resume that answers nothing — here, a question already past its deadline —
// releases the turn it found parked, rather than leaving it to its own timer:
// the turn ends, and its question closes, well before the question's timeout.
func TestServerResumeOfExpiredQuestionReleasesParkedTurn(t *testing.T) {
	clock := newDrivenClock()
	srv, ts, rig := releaseServer(t, 2, []float64{3, 0}, releaseHooks{},
		WithRetention(Retention{TTL: time.Minute, MaxHeld: 4}),
		WithClock(clock.Now))

	first, err := postRunQuiet(ts.URL, runBodyFor("t1", "r1"))
	requireInterrupted(t, "first POST", first, err)
	interrupt := soleInterrupt(t, first)
	turn := rig.turn(0)

	// Past the three-second deadline the question advertised, while the turn's
	// own timer (real time) still has about three seconds to run.
	clock.Advance(4 * time.Second)
	evs, err := postRunQuiet(ts.URL, resumeBody("t1", "r2", interrupt.ID, "blue"))
	if err != nil {
		t.Fatalf("late resume: %v", err)
	}
	if len(evs) == 0 {
		t.Fatal("late resume: no events")
	}
	awaitUnwoundWithin(t, "the turn parked on the expired question", turn, selfReleaseBound)
	if turn.outstanding() != nil {
		t.Fatal("the expired question is still open")
	}
	if srv.turns.turnForStream(turn.streamID) == turn {
		t.Fatal("the released turn is still registered")
	}
}
