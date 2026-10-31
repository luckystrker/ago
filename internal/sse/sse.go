// Package sse parses Server-Sent Events streams line by line. It is the shared
// transport for provider clients that stream Chat-Completions-style chunks.
package sse

import (
	"bufio"
	"errors"
	"io"
	"strings"
)

// maxLineSize bounds one SSE line (data lines carry base64 payloads and can be large).
const maxLineSize = 8 << 20 // 8 MiB

// DefaultMaxEventSize bounds the total size of the data lines accumulated for a
// single event (the block between blank lines). A single line is already capped
// by maxLineSize, but the SSE format lets a peer grow an event without bound by
// appending data lines and never sending the blank line — the cap turns that
// into a readable stream error instead of unbounded memory growth.
const DefaultMaxEventSize = 32 << 20 // 32 MiB

// ErrEventTooLarge is reported by Err when the accumulated event exceeds the
// configured cap. Consumers should treat the stream as hostile or broken.
var ErrEventTooLarge = errors.New("sse: event too large")

// Scanner iterates the events of an SSE stream. It follows the event-block rules of
// the SSE spec: a blank line dispatches the accumulated event; consecutive "data:"
// lines join with "\n"; lines starting with ":" are comments; "event:" names the
// event. A "data: [DONE]" sentinel is ordinary data — filtering it is the caller's
// job. Next must be called before Event.
type Scanner struct {
	sc           *bufio.Scanner
	name         string
	data         []string
	eventSize    int
	maxEventSize int
	done         bool
	err          error
}

// Option configures a Scanner.
type Option func(*Scanner)

// WithMaxEventSize sets the cap on the total size of the data lines accumulated
// for one event. Values <= 0 keep the DefaultMaxEventSize. The cap counts the
// payload bytes only (the "\n" separators added when joining are not counted);
// it is a guard against hostile streams, not an exact payload budget.
func WithMaxEventSize(n int) Option {
	return func(s *Scanner) {
		if n > 0 {
			s.maxEventSize = n
		}
	}
}

// NewScanner wraps r.
func NewScanner(r io.Reader, opts ...Option) *Scanner {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), maxLineSize)
	s := &Scanner{sc: sc, maxEventSize: DefaultMaxEventSize}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Next advances to the next complete event and reports whether one was found.
// The trailing block of a stream that ends without a blank line is still delivered.
// An event whose accumulated data lines exceed the cap terminates the scan with
// ErrEventTooLarge.
func (s *Scanner) Next() bool {
	if s.done {
		return false
	}
	// Reset the accumulation from any previously delivered event.
	s.name, s.data, s.eventSize = "", nil, 0
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
				// Bound the event before storing: the check must happen on
				// every line, or a peer that never sends the blank line grows
				// s.data without limit.
				// AGO-DEVIATION: Python AG2 relies on httpx-sse/openai client
				// parsing with no event-size cap; we refuse oversize events
				// because an LLM endpoint is not trusted input.
				s.eventSize += len(value)
				if s.eventSize > s.maxEventSize {
					s.err = ErrEventTooLarge
					s.done = true
					return false
				}
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
