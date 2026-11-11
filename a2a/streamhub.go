package a2a

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// Live task streams: the server side of tasks/resubscribe (:subscribe on REST)
// and of the artifact progress of message/stream.
//
// A task stream is a fan-out of one task's progress to its subscribers. The
// hub owns the write path of a streamed task: a transition (a status, an
// artifact chunk) is persisted and broadcast under one lock, so a subscriber
// that opens in the middle either reads a snapshot that already contains the
// change or receives the change as an event — never both, never neither. That
// is what makes a resubscribe open with the current task and then continue
// live (wave-4 invariant 7, spec/tla/A2AResubscribe.tla).
//
// The terminal state closes the stream: the event that carries it is the last
// one a subscriber sees, and a later subscriber gets the final task as its
// snapshot and an already-closed channel.

// hubBuffer caps the events buffered for one subscriber. A client that falls
// this far behind is disconnected instead of stalling the turn that is
// producing the events; it reconnects through tasks/resubscribe, which opens
// with the current task, so nothing is lost.
//
// AGO-DEVIATION: the a2a-sdk event queue blocks the producer when a consumer
// falls behind; the reconnect path makes dropping the slow stream the cheaper
// and converging option here.
const hubBuffer = 32

var (
	// ErrStreamClosed reports progress reported for a task whose stream has
	// already ended: the task reached a terminal state (a cancel usually), so
	// nothing further can be published for it.
	ErrStreamClosed = errors.New("a2a: task stream closed")
	// ErrReconnect reports a task stream this client could not re-establish:
	// the connection broke and the task was unknown (nothing to resubscribe
	// to) or the reconnect budget ran out (parity: A2AReconnectError).
	ErrReconnect = errors.New("a2a: task stream reconnect failed")
	// ErrUnsupportedOperation reports an operation the remote server refuses
	// as unsupported (JSON-RPC -32004); the client's stream falls back to
	// polling on it, the way the Python client treats the SDK's
	// UnsupportedOperationError (spec §3.1.6).
	ErrUnsupportedOperation = errors.New("a2a: unsupported operation")
)

// taskHub fans task events out to live subscribers, keyed by tenant and task
// id. Safe for concurrent use.
type taskHub struct {
	mu      sync.Mutex
	streams map[string]*hubStream
}

// hubStream is one task's stream while it has subscribers.
type hubStream struct {
	current *Task
	closed  bool
	subs    map[*hubSubscriber]struct{}
}

// hubSubscriber is one open stream: a buffered channel of the events published
// after it subscribed.
type hubSubscriber struct {
	ch chan hubEvent
}

// hubEvent carries one item for a subscriber: a *Task snapshot, a
// *StatusUpdate, an *ArtifactUpdate or a *Message.
type hubEvent struct {
	result any
}

// hubKey scopes a stream to the tenant of the request: two tenants can never
// share a stream even if their stores hold records with the same id.
func hubKey(ctx context.Context, id string) string {
	return tenantOf(ctx) + "\x00" + id
}

// commit runs write under the hub lock, then publishes the stored record to
// every subscriber of the task. write performs the store transition and
// returns the record as stored plus whether the store refused it. event builds
// the item subscribers receive; nil publishes the task snapshot itself, which
// is how a synchronous message/send continuation advances a live stream. A
// record stored in a terminal state closes the stream after the event, and a
// closed stream publishes nothing: the terminal state is the last thing a
// stream carries.
func (h *taskHub) commit(ctx context.Context, id string, write func() (*Task, bool, error), event func(*Task) any) (*Task, bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	ts := h.streams[hubKey(ctx, id)]
	stored, refused, err := write()
	if err != nil {
		return stored, refused, err
	}
	if ts == nil || refused {
		return stored, refused, nil
	}
	if ts.closed {
		// The stream of this task ended; a terminal record is final, so a
		// write that produced one publishes nothing. Anything else is a new
		// turn on the same task id (a continuation after the terminal state),
		// which reopens the stream.
		if terminalTaskState(stored.Status.State) {
			return stored, refused, nil
		}
		ts.closed = false
	}
	ts.current = stored
	item := any(stored)
	if event != nil {
		item = event(stored)
	}
	h.broadcastLocked(ts, item)
	if terminalTaskState(stored.Status.State) {
		h.closeLocked(ts)
	}
	return stored, refused, nil
}

// subscribe opens one subscriber on the current state of id. load reads the
// record from the tenant's store (it authorizes the request and seeds a stream
// the hub has not seen). The returned snapshot is the first item the stream
// carries; the channel is already closed when the task is terminal, so a
// resubscribe on a finished task returns its final state and ends. remove must
// be called when the reader stops (it is idempotent).
func (h *taskHub) subscribe(ctx context.Context, id string, load func() (*Task, error)) (*Task, <-chan hubEvent, func(), error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	key := hubKey(ctx, id)
	ts := h.streams[key]
	var snapshot *Task
	if ts == nil || ts.closed {
		// A stream the hub has not seen opens on the stored record; a stream
		// that already ended is re-read too, because its id may have been
		// reopened by a continuation after the terminal state. The snapshot is
		// cloned before the hub holds anything, so a malformed record cannot
		// leave a half-registered stream behind.
		task, err := load()
		if err != nil {
			return nil, nil, nil, err
		}
		snapshot, err = cloneTask(task)
		if err != nil {
			return nil, nil, nil, err
		}
		if ts == nil {
			ts = &hubStream{subs: make(map[*hubSubscriber]struct{})}
			if h.streams == nil {
				h.streams = make(map[string]*hubStream)
			}
			h.streams[key] = ts
		}
		ts.current = task
		ts.closed = terminalTaskState(task.Status.State)
	} else {
		var err error
		snapshot, err = cloneTask(ts.current)
		if err != nil {
			return nil, nil, nil, err
		}
	}
	sub := &hubSubscriber{ch: make(chan hubEvent, hubBuffer)}
	ts.subs[sub] = struct{}{}
	if ts.closed {
		close(sub.ch)
	}
	var once sync.Once
	remove := func() {
		once.Do(func() {
			h.mu.Lock()
			defer h.mu.Unlock()
			delete(ts.subs, sub)
			if len(ts.subs) == 0 {
				// The store was written before the hub state and the write
				// and the broadcast share the hub lock, so dropping the entry
				// loses nothing: the next subscribe reloads.
				delete(h.streams, key)
			}
		})
	}
	return snapshot, sub.ch, remove, nil
}

// broadcastLocked queues item on every subscriber, dropping — and closing —
// a subscriber whose buffer is full: that client is disconnected and its next
// resubscribe starts from the current task. Caller holds h.mu.
func (h *taskHub) broadcastLocked(ts *hubStream, item any) {
	for sub := range ts.subs {
		select {
		case sub.ch <- hubEvent{result: item}:
		default:
			delete(ts.subs, sub)
			close(sub.ch)
		}
	}
}

// closeLocked ends the stream: every subscriber is closed after the last
// event. Caller holds h.mu.
func (h *taskHub) closeLocked(ts *hubStream) {
	ts.closed = true
	for sub := range ts.subs {
		delete(ts.subs, sub)
		close(sub.ch)
	}
}

// StreamEmitter reports the progress of one streaming turn to the server's
// message/stream response and to every tasks/resubscribe subscriber. A handler
// configured as ServerConfig.HandleStream receives it. The zero value is inert:
// its methods refuse with ErrStreamClosed.
type StreamEmitter struct {
	turn *streamTurn
}

// Artifact publishes one artifact chunk: the chunk is folded into the task
// record under the A2A append rules (see ApplyArtifactUpdate) and streamed as
// an ArtifactUpdate. appendParts extends an existing artifact with the same
// id instead of replacing its parts; lastChunk marks the artifact complete.
// ErrStreamClosed means the task turned terminal (a cancel usually) and the
// handler should stop.
func (e StreamEmitter) Artifact(artifact Artifact, appendParts, lastChunk bool) error {
	if e.turn == nil {
		return ErrStreamClosed
	}
	return e.turn.artifact(artifact, appendParts, lastChunk)
}

// Status publishes one progress transition (working) of the running turn. The
// turn's outcome — the terminal state or an interruption — is set by the
// handler's return value, so a settled or unknown state here is refused with
// ErrWire.
func (e StreamEmitter) Status(state TaskState, message *Message) error {
	if e.turn == nil {
		return ErrStreamClosed
	}
	return e.turn.status(state, message)
}

// streamTurn is the server's side of one streaming turn: the task it belongs
// to, the hub it publishes through, and the originating response it writes to.
// Safe for concurrent use: a handler that emits from several goroutines
// serializes here, which the originating http.ResponseWriter requires.
type streamTurn struct {
	mu   sync.Mutex
	ctx  context.Context
	srv  *Server
	task *Task
	out  func(any)
}

// artifact persists one chunk and publishes it.
func (t *streamTurn) artifact(artifact Artifact, appendParts, lastChunk bool) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if artifact.ArtifactID == "" {
		return fmt.Errorf("%w: artifact update without an artifactId", ErrWire)
	}
	update := &ArtifactUpdate{
		Kind:      KindArtifactUpdate,
		TaskID:    t.task.ID,
		ContextID: t.task.ContextID,
		Artifact:  artifact,
		Append:    appendParts,
		LastChunk: lastChunk,
	}
	stored, _, err := t.srv.hub.commit(t.ctx, t.task.ID, func() (*Task, bool, error) {
		return t.srv.updateOrCreateTask(t.ctx, t.task, func(current *Task) bool {
			if terminalTaskState(current.Status.State) {
				return false
			}
			ApplyArtifactUpdate(current, update)
			return true
		})
	}, func(*Task) any { return update })
	if err != nil {
		return err
	}
	if terminalTaskState(stored.Status.State) {
		return ErrStreamClosed
	}
	t.task = stored
	t.write(update)
	return nil
}

// status persists one non-terminal transition and publishes it.
func (t *streamTurn) status(state TaskState, message *Message) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if settledTaskState(state) || state == TaskStateUnknown {
		return fmt.Errorf("%w: Status(%q): the handler's return value ends the turn", ErrWire, state)
	}
	update := &StatusUpdate{
		Kind:      KindStatusUpdate,
		TaskID:    t.task.ID,
		ContextID: t.task.ContextID,
		Status:    TaskStatus{State: state, Message: normalizeReply(message, t.task)},
	}
	stored, _, err := t.srv.hub.commit(t.ctx, t.task.ID, func() (*Task, bool, error) {
		return t.srv.updateOrCreateTask(t.ctx, t.task, func(current *Task) bool {
			if terminalTaskState(current.Status.State) {
				return false
			}
			current.Status = update.Status
			return true
		})
	}, func(*Task) any { return update })
	if err != nil {
		return err
	}
	if terminalTaskState(stored.Status.State) {
		return ErrStreamClosed
	}
	t.task = stored
	t.write(update)
	return nil
}

// deliver writes one item to the originating message/stream response.
func (t *streamTurn) deliver(item any) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.write(item)
}

// write delivers item while the turn's lock is held; it is nil-safe so a turn
// can run without an HTTP response (tests, embedders).
func (t *streamTurn) write(item any) {
	if t.out != nil {
		t.out(item)
	}
}
