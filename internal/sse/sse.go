// Package sse parses Server-Sent Events streams line by line. It is the shared
// transport for provider clients that stream Chat-Completions-style chunks.
package sse

import (
	"bufio"
	"io"
	"strings"
)

// maxLineSize bounds one SSE line (data lines carry base64 payloads and can be large).
const maxLineSize = 8 << 20 // 8 MiB

// Scanner iterates the events of an SSE stream. It follows the event-block rules of
// the SSE spec: a blank line dispatches the accumulated event; consecutive "data:"
// lines join with "\n"; lines starting with ":" are comments; "event:" names the
// event. A "data: [DONE]" sentinel is ordinary data — filtering it is the caller's
// job. Next must be called before Event.
type Scanner struct {
	sc   *bufio.Scanner
	name string
	data []string
	done bool
	err  error
}

// NewScanner wraps r.
func NewScanner(r io.Reader) *Scanner {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), maxLineSize)
	return &Scanner{sc: sc}
}

// Next advances to the next complete event and reports whether one was found.
// The trailing block of a stream that ends without a blank line is still delivered.
func (s *Scanner) Next() bool {
	if s.done {
		return false
	}
	// Reset the accumulation from any previously delivered event.
	s.name, s.data = "", nil
	for s.sc.Scan() {
		line := s.sc.Text()
		switch {
		case line == "":
			// Blank line: dispatch the block when it carried data.
			if len(s.data) > 0 {
				return true
			}
			s.name = ""
		case strings.HasPrefix(line, ":"):
			// Comment: ignored.
		default:
			field, value, _ := strings.Cut(line, ":")
			value = strings.TrimPrefix(value, " ")
			switch field {
			case "event":
				s.name = value
			case "data":
				s.data = append(s.data, value)
			default:
				// "id" and "retry" carry no meaning for a one-shot response stream.
			}
		}
	}
	if err := s.sc.Err(); err != nil {
		s.err = err
		s.done = true
		return false
	}
	// EOF: deliver a pending block once (streams without the final blank line).
	s.done = true
	return len(s.data) > 0
}

// Event returns the name and payload of the current event. data is nil when the
// block carried no data lines. Name is "" when no "event:" line preceded the data.
func (s *Scanner) Event() (name string, data []byte) {
	if len(s.data) == 0 {
		return s.name, nil
	}
	joined := strings.Join(s.data, "\n")
	return s.name, []byte(joined)
}

// Err reports the first read error (io.EOF is not an error).
func (s *Scanner) Err() error { return s.err }
