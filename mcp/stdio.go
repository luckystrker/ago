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
// dialect). Requests are written one line each; replies are read line by line
// until the awaited id shows up, skipping notifications. Server-initiated
// requests (sampling, ping) carry no answer in this minimum client and are
// skipped too.
type stdioTransport struct {
	cfg *MCPStdioConfig

	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Scanner
	alive  bool
}

const stdioScanBuffer = 8 << 20 // 8 MiB: tool results may carry base64 payloads
const killAfter = 5 * time.Second

func newStdioTransport(cfg *MCPStdioConfig) (*stdioTransport, error) {
	if cfg.Command == "" {
		return nil, fmt.Errorf("%w: MCPStdioConfig.Command is empty", ErrNotProvided)
	}
	return &stdioTransport{cfg: cfg}, nil
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
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64*1024), stdioScanBuffer)
	t.cmd, t.stdin, t.stdout, t.alive = cmd, stdin, sc, true
	return nil
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

// call writes the request and reads lines until the reply with the awaited id
// arrives. Reads are synchronous and serialized by the Client's lock, so exactly
// one scan loop is ever reading the pipe.
func (t *stdioTransport) call(ctx context.Context, id int64, method string, params any) (json.RawMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := t.writeLine(rpcRequest{JSONRPC: "2.0", ID: id, Method: method, Params: params}); err != nil {
		return nil, err
	}
	for t.stdout.Scan() {
		line := t.stdout.Bytes()
		if len(line) == 0 {
			continue
		}
		var msg wireMessage
		if err := json.Unmarshal(line, &msg); err != nil {
			// A garbled line is skipped: a chatty banner must not kill the
			// session, and the awaited reply may still follow.
			continue
		}
		if msg.Method != "" || msg.ID == nil || *msg.ID != id {
			continue
		}
		if msg.Error != nil {
			return nil, fmt.Errorf("%w: %s", ErrProtocol, msg.Error.Error())
		}
		return msg.Result, nil
	}
	if err := t.stdout.Err(); err != nil {
		return nil, fmt.Errorf("%w: server stdout: %v", ErrProtocol, err)
	}
	return nil, fmt.Errorf("%w: server exited before answering request %d", ErrProtocol, id)
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
// after killAfter it is killed. Safe to call more than once.
func (t *stdioTransport) close() error {
	if !t.alive {
		return nil
	}
	t.alive = false
	t.stdin.Close()
	done := make(chan struct{})
	go func() { t.cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(killAfter):
		t.cmd.Process.Kill()
		<-done
	}
	return nil
}
