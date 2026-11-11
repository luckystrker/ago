package a2a

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// Server-side streaming text: the message/stream response body (JSON-RPC and
// the REST twin) and the shared tasks/resubscribe body (:subscribe on REST).
//
// Both bindings speak Server-Sent Events: one "data:" line per stream item.
// The JSON-RPC binding wraps each item in a JSON-RPC response carrying the
// request id; the HTTP+JSON binding writes the bare item. The items are the
// A2A StreamResponse shapes: a Task snapshot, a StatusUpdate, an
// ArtifactUpdate, or a Message.

// streamFraming adapts one binding to the SSE body and to the reply of a
// request that failed before the body was committed.
type streamFraming struct {
	// frame encodes one stream item as the JSON of its SSE data line.
	frame func(item any) ([]byte, error)
	// fail answers a request that never opened its stream.
	fail func(*RPCError)
}

// write emits one SSE event. Write errors (a client that hung up) end the
// stream: the turn keeps running and its record stays authoritative, but there
// is nobody left to deliver to.
func (f streamFraming) write(w io.Writer, flusher http.Flusher, item any) bool {
	data, err := f.frame(item)
	if err != nil {
		return false
	}
	if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
		return false
	}
	flusher.Flush()
	return true
}

// jsonrpcStreamFraming frames the SSE body of the JSON-RPC binding.
func jsonrpcStreamFraming(w http.ResponseWriter, id json.RawMessage) streamFraming {
	return streamFraming{
		frame: func(item any) ([]byte, error) {
			result, err := json.Marshal(item)
			if err != nil {
				return nil, err
			}
			return json.Marshal(rpcResponse{JSONRPC: "2.0", ID: idOr(id), Result: result})
		},
		fail: func(rpcErr *RPCError) { writeRPCError(w, id, rpcErr) },
	}
}

// restStreamFraming frames the SSE body of the HTTP+JSON binding.
func restStreamFraming(w http.ResponseWriter) streamFraming {
	return streamFraming{
		frame: func(item any) ([]byte, error) { return json.Marshal(item) },
		fail:  func(rpcErr *RPCError) { writeREST(w, rpcErr) },
	}
}

// startSSE commits the streaming response headers.
func startSSE(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
}

// serveMessageStream drives one message/stream turn on either binding: the
// working transition, the handler's progress, and the terminal update.
//
// The turn's working record is stored before the first event, so tasks/get and
// a resubscribe resolve the task while it runs, and every later transition is
// persisted through the hub together with its broadcast.
func (s *Server) serveMessageStream(ctx context.Context, w http.ResponseWriter, flusher http.Flusher, msg *Message, framing streamFraming) {
	startSSE(w)
	deliver := func(item any) { framing.write(w, flusher, item) }

	task, rpcErr := s.beginTurn(ctx, msg)
	if rpcErr != nil {
		s.failedStream(s.newTask(msg), deliver, rpcErr)
		return
	}
	turn, stored, refused, err := s.beginStream(ctx, task, deliver)
	if err != nil {
		s.failedStream(task, deliver, &RPCError{Code: codeInternalError, Message: msgInternalError})
		return
	}
	if refused {
		// The stored record is already terminal (a cancel won): report its
		// status instead of opening a turn on it.
		deliver(&StatusUpdate{
			Kind:      KindStatusUpdate,
			TaskID:    stored.ID,
			ContextID: stored.ContextID,
			Final:     true,
			Status:    stored.Status,
		})
		return
	}
	if s.cfg.HandleStream != nil {
		reply, state, handleErr := s.runStreamTurn(ctx, task, msg, StreamEmitter{turn: turn})
		s.commitTurnOutcome(ctx, task, msg, turn, reply, state, handleErr)
		return
	}
	s.finishTurnOn(ctx, task, msg, turn)
}

// failedStream reports a turn that could not open as a failed stream: the SSE
// body is already committed, so the failure travels as a final failed event.
// task supplies the ids the stream reports.
func (s *Server) failedStream(task *Task, deliver func(any), rpcErr *RPCError) {
	deliver(&StatusUpdate{
		Kind: KindStatusUpdate, TaskID: task.ID, ContextID: task.ContextID,
		Status: TaskStatus{State: TaskStateWorking},
	})
	deliver(&StatusUpdate{
		Kind: KindStatusUpdate, TaskID: task.ID, ContextID: task.ContextID, Final: true,
		Status: TaskStatus{State: TaskStateFailed, Message: errorMessage(errors.New(rpcErr.Message))},
	})
}

// beginStream opens a streaming turn: the task is stored in the working state
// and the working update is published to the hub and delivered to the
// originating response. refused reports that the stored record was already
// terminal, in which case stored carries that record.
func (s *Server) beginStream(ctx context.Context, task *Task, deliver func(any)) (*streamTurn, *Task, bool, error) {
	turn := &streamTurn{ctx: ctx, srv: s, task: task, out: deliver}
	working := func(t *Task) any {
		return &StatusUpdate{
			Kind:      KindStatusUpdate,
			TaskID:    t.ID,
			ContextID: t.ContextID,
			Status:    TaskStatus{State: TaskStateWorking},
		}
	}
	stored, refused, err := s.hub.commit(ctx, task.ID, func() (*Task, bool, error) {
		return s.updateOrCreateTask(ctx, task, func(current *Task) bool {
			if terminalTaskState(current.Status.State) {
				return false
			}
			current.Status = TaskStatus{State: TaskStateWorking}
			return true
		})
	}, working)
	if err != nil {
		return nil, nil, false, err
	}
	turn.task = stored
	if !refused {
		deliver(working(stored))
	}
	return turn, stored, refused, nil
}

// serveResubscribe is the body shared by tasks/resubscribe and the REST
// :subscribe route: it opens with the task as the server currently holds it,
// then forwards every published event. The stream ends at the terminal state
// (the hub closes it right after the terminal event), when the caller goes
// away, or when the task is unknown (an error before the body is committed).
func (s *Server) serveResubscribe(ctx context.Context, w http.ResponseWriter, flusher http.Flusher, taskID string, framing streamFraming) {
	snapshot, events, remove, err := s.hub.subscribe(ctx, taskID, func() (*Task, error) {
		store, storeErr := s.storeFor(ctx)
		if storeErr != nil {
			return nil, storeErr
		}
		return store.Get(ctx, taskID)
	})
	if err != nil {
		framing.fail(storeError(err))
		return
	}
	defer remove()
	startSSE(w)
	if !framing.write(w, flusher, snapshot) {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-events:
			if !ok {
				return
			}
			if !framing.write(w, flusher, event.result) {
				return
			}
		}
	}
}
