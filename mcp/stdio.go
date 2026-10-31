package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"time"
)

// stdioTransport runs the server as a subprocess and exchanges newline-delimited
// JSON-RPC 2.0 on its stdin/stdout (no Content-Length framing — the AGo minimum
// dialect). Requests are written one line each; replies are matched by id,
// skipping notifications. Server-initiated requests (sampling, ping) carry no
// answer in this minimum client and are skipped too.
//
// A dedicated reader goroutine owns the stdout pipe for the transport's whole
// lifetime, so no caller ever blocks on a bare read: every wait is a select on
// the reply queue, the exchange deadline, the context, and the transport's
// shutdown. A subprocess that goes silent fails the exchange at the deadline
// instead of pinning the Client's lock (and the agent turn) forever.
type stdioTransport struct {
	cfg *MCPStdioConfig

	// timeout caps one exchange (write + wait for the matching reply); 0 means
	// no cap. The default comes from defaultExchangeTimeout (http.go), the same
	// 30s Python's MCPServerConfig.connection_timeout gives the HTTP branch.
	timeout time.Duration

	cmd   *exec.Cmd
	stdin io.WriteCloser

	// responses carries the server's replies, parsed by the reader goroutine.
	// readerDone closes when the stdout pipe ends; dead closes exactly once,
	// when the transport shuts down. readerErr is written by the reader before
	// readerDone closes — read it only after receiving from readerDone.
	responses  chan wireMessage
	readerDone chan struct{}
	dead       chan struct{}
	readerErr  error

	// alive guards a single shutdown run; only the Client's serialized
	// call/close paths touch it.
	alive bool
}

const stdioScanBuffer = 8 << 20 // 8 MiB: tool results may carry base64 payloads
const killAfter = 5 * time.Second

// responseQueue bounds the reply queue. The Client keeps one exchange in
// flight, so the queue only ever holds the awaited reply plus stale answers to
// exchanges that already timed out; a full queue blocks the reader, which
// backpressures the server onto its own stdout write.
const responseQueue = 16

func newStdioTransport(cfg *MCPStdioConfig) (*stdioTransport, error) {
	if cfg.Command == "" {
		return nil, fmt.Errorf("%w: MCPStdioConfig.Command is empty", ErrNotProvided)
	}
	timeout := defaultExchangeTimeout
	switch {
	case cfg.Timeout > 0:
		timeout = cfg.Timeout
	case cfg.Timeout < 0:
		timeout = 0 // no cap
	}
	return &stdioTransport{cfg: cfg, timeout: timeout}, nil
}

// start launches the subprocess. The encoding is UTF-8 by fiat (os/exec pipes bytes).
func (t *stdioTransport) start() error {
	cmd := exec.Command(t.cfg.Command, t.cfg.Args...)
	cmd.Env = t.cfg.Env
	cmd.Dir = t.cfg.CWD
	// Server stderr is diagnostics the minimum client discards; keeping it silent
	// avoids interleaving it into a conversation's log stream.
	cmd.Stderr = io.Discard

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("mcp: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("mcp: stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("mcp: launch %s: %w", t.cfg.Command, err)
	}
	t.cmd, t.stdin = cmd, stdin
	t.responses = make(chan wireMessage, responseQueue)
	t.readerDone = make(chan struct{})
	t.dead = make(chan struct{})
	t.alive = true
	go t.read(stdout)
	return nil
}

// read is the transport's only reader of the subprocess stdout. It parses each
// line and queues the replies; notifications, server-to-client requests (the
// minimum client answers none) and garbled lines (a chatty banner must not kill
// the session) are dropped, so the queue holds replies only and their count is
// bounded by the requests the client sends.
func (t *stdioTransport) read(stdout io.ReadCloser) {
	defer close(t.readerDone)
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64*1024), stdioScanBuffer)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var msg wireMessage
		if err := json.Unmarshal(line, &msg); err != nil {
			continue
		}
		if msg.Method != "" || msg.ID == nil {
			continue // notification or server-to-client request
		}
		select {
		case t.responses <- msg:
		case <-t.dead:
			return
		}
	}
	if err := sc.Err(); err != nil {
		t.readerErr = err
	}
}

// writeLine writes one JSON message terminated by a newline (json.Marshal never
// emits raw newlines, so one Write is one message).
func (t *stdioTransport) writeLine(msg any) error {
	payload, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("%w: encode request: %v", ErrProtocol, err)
	}
	if _, err := t.stdin.Write(append(payload, '\n')); err != nil {
		return fmt.Errorf("mcp: server stdin: %w", err)
	}
	return nil
}

// call writes the request and waits for the reply with the awaited id. The wait
// ends on the deadline, on ctx cancellation or on transport shutdown — never on
// a blocked read (the reader goroutine owns the pipe).
func (t *stdioTransport) call(ctx context.Context, id int64, method string, params any) (json.RawMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := t.writeLine(rpcRequest{JSONRPC: "2.0", ID: id, Method: method, Params: params}); err != nil {
		return nil, err
	}
	return t.awaitReply(ctx, id)
}

// matchReply reports whether msg answers the request id, and with what outcome.
func matchReply(msg wireMessage, id int64) (json.RawMessage, bool, error) {
	if msg.ID == nil || *msg.ID != id {
		return nil, false, nil
	}
	if msg.Error != nil {
		return nil, true, fmt.Errorf("%w: %s", ErrProtocol, msg.Error.Error())
	}
	return msg.Result, true, nil
}

// awaitReply selects on the reply queue until the response to id shows up,
// skipping stale answers to exchanges that already timed out. A buffered reply
// wins even when the pipe has already ended.
func (t *stdioTransport) awaitReply(ctx context.Context, id int64) (json.RawMessage, error) {
	var deadline <-chan time.Time
	if t.timeout > 0 {
		timer := time.NewTimer(t.timeout)
		defer timer.Stop()
		deadline = timer.C
	}
	pipeGone := false
	for {
		select {
		case msg := <-t.responses:
			if res, done, err := matchReply(msg, id); done {
				return res, err
			}
			continue
		default:
		}
		if pipeGone {
			if t.readerErr != nil {
				return nil, fmt.Errorf("%w: server stdout: %v", ErrProtocol, t.readerErr)
			}
			return nil, fmt.Errorf("%w: server exited before answering request %d", ErrProtocol, id)
		}
		select {
		case msg := <-t.responses:
			if res, done, err := matchReply(msg, id); done {
				return res, err
			}
		case <-deadline:
			// A server that missed its deadline gets no grace: the session is
			// broken from the client's point of view, and the Client rebuilds
			// one on the next operation.
			t.shutdown(true)
			return nil, fmt.Errorf("%w: no reply to request %d within %s", ErrTimeout, id, t.timeout)
		case <-ctx.Done():
			// The caller gave up mid-exchange; kill the subprocess too, or the
			// reply (if any) would desynchronize the next session.
			t.shutdown(true)
			return nil, ctx.Err()
		case <-t.dead:
			return nil, fmt.Errorf("%w: transport closed while waiting for the reply to request %d", ErrClosed, id)
		case <-t.readerDone:
			pipeGone = true
		}
	}
}

func (t *stdioTransport) notify(ctx context.Context, method string, params any) error {
	if err := t.writeLine(rpcNotification{JSONRPC: "2.0", Method: method, Params: params}); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return nil
}

// close shuts the subprocess down: closing stdin lets a well-behaved server exit;
// after killAfter it is killed. Safe to call more than once. An exchange waiting
// on this transport wakes up with ErrClosed.
func (t *stdioTransport) close() error {
	t.shutdown(false)
	return nil
}

// shutdown tears the subprocess down exactly once. With kill set the process is
// terminated immediately (the deadline/cancellation path); otherwise it first
// gets the killAfter grace after its stdin closes.
func (t *stdioTransport) shutdown(kill bool) {
	if !t.alive {
		return
	}
	t.alive = false
	close(t.dead)
	t.stdin.Close()
	if kill {
		t.cmd.Process.Kill()
		t.cmd.Wait()
		return
	}
	done := make(chan struct{})
	go func() { t.cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(killAfter):
		t.cmd.Process.Kill()
		<-done
	}
}
