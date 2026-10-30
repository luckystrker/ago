package mcpserver

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
)

// stdioScanBuffer bounds one stdin line (tool arguments may carry base64
// payloads); the same cap the mcp stdio client uses.
const stdioScanBuffer = 8 << 20 // 8 MiB

// Stdio serves newline-delimited JSON-RPC 2.0 on stdin/stdout until the
// client disconnects (stdin EOF returns nil) — the transport local clients
// (Claude Desktop, Cursor, the MCP Inspector) speak, and the same no-framing
// dialect the ago/mcp stdio client speaks. One request runs at a time: a slow
// Ask blocks the loop, which is the stdio transport's whole concurrency story.
//
// Garbled lines are skipped rather than fatal: a chatty banner must not kill
// the session, and the next valid request still gets its reply.
func (s *Server) Stdio() error {
	return s.serveIO(os.Stdin, os.Stdout)
}

// serveIO is Stdio over explicit streams; the tests drive it through pipes.
func (s *Server) serveIO(in io.Reader, out io.Writer) error {
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 0, 64*1024), stdioScanBuffer)
	w := bufio.NewWriter(out)
	// The serve loop is the only writer, and it never writes while a request
	// handler runs, so the buffer needs no lock.
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var req rpcRequest
		if err := json.Unmarshal(line, &req); err != nil {
			// No trustworthy reply id inside an unparseable line; skip it.
			continue
		}
		if req.JSONRPC != "2.0" {
			if req.ID == nil || isNullID(req.ID) {
				continue
			}
			if err := writeLine(w, &rpcResponse{JSONRPC: "2.0", ID: req.ID,
				Error: rpcErr(codeInvalidRequest, "not a JSON-RPC 2.0 message")}); err != nil {
				return err
			}
			continue
		}
		reply := s.handle(context.Background(), &req)
		if reply == nil {
			continue // a notification: never answered
		}
		if err := writeLine(w, reply); err != nil {
			return fmt.Errorf("mcpserver: write stdout: %w", err)
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("mcpserver: read stdin: %w", err)
	}
	return nil
}

// writeLine flushes one JSON message terminated by a newline (json.Marshal
// never emits raw newlines, so one write is one message).
func writeLine(w *bufio.Writer, msg any) error {
	payload, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("mcpserver: encode response: %w", err)
	}
	if _, err := w.Write(append(payload, '\n')); err != nil {
		return err
	}
	return w.Flush()
}
