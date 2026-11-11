package a2a

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// Wave-4 invariant 7, property test (spec/tla/A2AResubscribe.tla):
//
//	after a reconnect the client's view converges to the server's current task
//	state (because a resubscribe opens with the current task); the terminal
//	state is always observed; no events after terminal; no event belongs to
//	another task.
//
// The schedules mix the server's progress (artifact chunks, an intermediate
// status), the turn's terminal transition, and connection drops at random
// points of random streams — including inside the opening stream, inside a
// resubscribe, and after the terminal event was written.

// streamScript is one seeded turn: the chunks to emit and how it ends.
type streamScript struct {
	chunks       []chunkSpec
	intermediate bool
	fail         bool
}

// randomStreamScript draws one turn.
func randomStreamScript(rng *rand.Rand) *streamScript {
	script := &streamScript{
		intermediate: rng.Intn(3) == 0,
		fail:         rng.Intn(5) == 0,
	}
	for n, i := rng.Intn(4), 0; i < n; i++ {
		id := "a1"
		if rng.Intn(2) == 0 {
			id = "a2"
		}
		script.chunks = append(script.chunks, chunkSpec{
			artifact: Artifact{
				ArtifactID: id,
				Name:       "artifact-" + id,
				Parts:      []Part{NewTextPart(fmt.Sprintf("chunk-%d", i))},
			},
			appendParts: rng.Intn(2) == 0,
			lastChunk:   rng.Intn(3) == 0,
		})
	}
	return script
}

// randomDropPlan draws which of the client's streams break and after how many
// events. The opening stream never breaks before its first event: a client
// without a task id has nothing to resubscribe to.
func randomDropPlan(rng *rand.Rand, chunks int) []int {
	total := chunks + 2 // working, chunks, terminal
	plan := make([]int, 0, 3)
	for n, i := rng.Intn(4), 0; i < n; i++ {
		after := rng.Intn(total)
		if i == 0 && after == 0 {
			after = 1
		}
		plan = append(plan, after)
	}
	return plan
}

// stateRank orders the lifecycle the way the TLA model does, for the
// ViewMonotone check.
func stateRank(state TaskState) int {
	switch state {
	case "", TaskStateUnknown:
		return 0
	case TaskStateSubmitted:
		return 1
	case TaskStateWorking, TaskStateInputRequired, TaskStateAuthRequired:
		return 2
	}
	if terminalTaskState(state) {
		return 3
	}
	return 2
}

// TestStreamConvergesAfterDrops is the invariant-7 property test, run on both
// bindings: the stream a caller sees must converge whether it rode JSON-RPC SSE
// or the HTTP+JSON twin.
func TestStreamConvergesAfterDrops(t *testing.T) {
	for _, binding := range []string{TransportJSONRPC, TransportREST} {
		t.Run(binding, func(t *testing.T) { runStreamConvergence(t, binding) })
	}
}

// runStreamConvergence drives the seeded schedules of one binding.
func runStreamConvergence(t *testing.T, binding string) {
	const seeds = 2000

	store := NewMemoryTaskStore()
	var scripts sync.Map
	srv := NewServer(ServerConfig{
		Card:  testCard(true),
		Store: store,
		HandleStream: func(_ context.Context, msg *Message, emit StreamEmitter) (*Message, error) {
			loaded, ok := scripts.Load(textOf(msg))
			if !ok {
				return nil, fmt.Errorf("no scripted turn for %q", textOf(msg))
			}
			script := loaded.(*streamScript)
			for i, chunk := range script.chunks {
				if err := emit.Artifact(chunk.artifact, chunk.appendParts, chunk.lastChunk); err != nil {
					return nil, err
				}
				if script.intermediate && i == 0 {
					if err := emit.Status(TaskStateWorking, NewMessage(RoleAgent, NewTextPart("still working"))); err != nil {
						return nil, err
					}
				}
			}
			if script.fail {
				return nil, errors.New("scripted failure")
			}
			return NewMessage(RoleAgent, NewTextPart("reply")), nil
		},
	})
	server := httptest.NewServer(srv.Handler())
	defer server.Close()

	var totalBreaks, totalSnapshots, droppedSeeds int
	for seed := 0; seed < seeds; seed++ {
		rng := rand.New(rand.NewSource(int64(seed)))
		token := fmt.Sprintf("run-%s-%d", binding, seed)
		script := randomStreamScript(rng)
		scripts.Store(token, script)
		transport := &recordingTransport{base: http.DefaultTransport, plan: randomDropPlan(rng, len(script.chunks))}
		client := testClient(t, server.URL, binding, transport,
			WithMaxReconnects(8), WithReconnectBackoff(time.Microsecond), WithPollingInterval(time.Microsecond))

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		stream, err := client.MessageStream(ctx, NewMessage(RoleUser, NewTextPart(token)))
		if err != nil {
			cancel()
			t.Fatalf("seed %d: MessageStream: %v", seed, err)
		}

		var events []*TaskEvent
		previousRank := 0
		for {
			event, err := stream.Next(ctx)
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				stream.Close()
				cancel()
				t.Fatalf("seed %d: Next: %v", seed, err)
			}
			events = append(events, event)
			if rank := stateRank(stream.view.Status.State); rank < previousRank {
				stream.Close()
				cancel()
				t.Fatalf("seed %d: view moved back from rank %d to %d at %+v", seed, previousRank, rank, event)
			} else {
				previousRank = rank
			}
		}
		if len(events) == 0 {
			stream.Close()
			cancel()
			t.Fatalf("seed %d: the stream delivered nothing", seed)
		}

		// The terminal state is observed exactly once, and nothing follows it.
		terminals := 0
		for i, event := range events {
			if event.settled() {
				terminals++
				if i != len(events)-1 {
					stream.Close()
					cancel()
					t.Fatalf("seed %d: event %d (%s) followed the terminal state", seed, i, eventKind(event))
				}
			}
		}
		if terminals != 1 {
			stream.Close()
			cancel()
			t.Fatalf("seed %d: observed the terminal state %d times, want exactly once", seed, terminals)
		}
		if _, err := stream.Next(ctx); !errors.Is(err, io.EOF) {
			stream.Close()
			cancel()
			t.Fatalf("seed %d: Next after the terminal state = %v, want io.EOF", seed, err)
		}

		// No event belongs to another task, and the view equals the server's
		// record in status and artifacts.
		view := stream.View()
		if view == nil || view.ID == "" {
			stream.Close()
			cancel()
			t.Fatalf("seed %d: the view names no task", seed)
		}
		for i, event := range events {
			if id := event.taskID(); id != "" && id != view.ID {
				stream.Close()
				cancel()
				t.Fatalf("seed %d: event %d belongs to task %q, the stream to %q", seed, i, id, view.ID)
			}
		}
		stored, err := store.Get(ctx, view.ID)
		if err != nil {
			stream.Close()
			cancel()
			t.Fatalf("seed %d: store.Get(%q): %v", seed, view.ID, err)
		}
		if view.Status.State != stored.Status.State {
			stream.Close()
			cancel()
			t.Fatalf("seed %d: view state %q, stored %q", seed, view.Status.State, stored.Status.State)
		}
		if got, want := textOf(view.Status.Message), textOf(stored.Status.Message); got != want {
			stream.Close()
			cancel()
			t.Fatalf("seed %d: view status.message %q, stored %q", seed, got, want)
		}
		gotArtifacts, err := json.Marshal(view.Artifacts)
		if err != nil {
			t.Fatalf("seed %d: marshal view artifacts: %v", seed, err)
		}
		wantArtifacts, err := json.Marshal(stored.Artifacts)
		if err != nil {
			t.Fatalf("seed %d: marshal stored artifacts: %v", seed, err)
		}
		if !bytes.Equal(gotArtifacts, wantArtifacts) {
			stream.Close()
			cancel()
			t.Fatalf("seed %d: view artifacts %s, stored %s", seed, gotArtifacts, wantArtifacts)
		}

		// Every break that happened was answered by a resubscribe, and a
		// resubscribe opens with the server's current task (A1/A3): a stream
		// that broke before the terminal state cannot finish without one.
		if breaks := transport.breaks.Load(); breaks > 0 {
			droppedSeeds++
			totalBreaks += int(breaks)
			snapshots := 0
			for _, event := range events {
				if event.Task != nil {
					snapshots++
				}
			}
			totalSnapshots += snapshots
			if snapshots == 0 {
				stream.Close()
				cancel()
				t.Fatalf("seed %d: %d break(s) but no resubscribe snapshot", seed, breaks)
			}
		}

		stream.Close()
		cancel()
	}
	t.Logf("%d seeded sequences: %d broke a stream, %d breaks total, %d resubscribe snapshots",
		seeds, droppedSeeds, totalBreaks, totalSnapshots)
}
