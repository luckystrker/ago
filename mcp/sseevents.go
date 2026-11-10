package mcp

import (
	"bufio"
	"io"
	"math"
	"strconv"
	"strings"

	"github.com/luckystrker/ago/internal/sse"
)

// sseEvent is one dispatched server-sent event with the two fields streamable
// HTTP leans on beyond the payload: the event id (the cursor a resuming client
// sends back as Last-Event-ID) and the retry hint (how long to wait before
// reconnecting).
type sseEvent struct {
	ID    string
	Retry int // milliseconds; -1 when the event carried no retry field
	Name  string
	Data  []byte
	// HasData is false for a priming event: an id with an empty data line.
	HasData bool
}

// eventReader parses an SSE stream like internal/sse.Scanner but keeps the id
// and retry fields and delivers events whose payload is empty (the priming event
// a resumable stream opens with). The per-event size cap fails with the shared
// scanner's sse.ErrEventTooLarge, so an oversized event fails the same way.
type eventReader struct {
	sc   *bufio.Scanner
	max  int
	cur  sseEvent
	err  error
	done bool
}

// newEventReader wraps r; max is the per-event payload cap in bytes
// (math.MaxInt for none).
func newEventReader(r io.Reader, max int) *eventReader {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 8<<20)
	if max <= 0 {
		max = math.MaxInt
	}
	return &eventReader{sc: sc, max: max}
}

// next advances to the next event and reports whether there is one.
func (r *eventReader) next() bool {
	if r.done {
		return false
	}
	var (
		ev     = sseEvent{Retry: -1}
		data   []string
		size   int
		fields bool
	)
	flush := func() bool {
		ev.Data = []byte(strings.Join(data, "\n"))
		ev.HasData = len(ev.Data) > 0
		r.cur = ev
		return true
	}
	for r.sc.Scan() {
		line := r.sc.Text()
		switch {
		case line == "":
			if fields {
				return flush()
			}
		case strings.HasPrefix(line, ":"):
		default:
			field, value, _ := strings.Cut(line, ":")
			value = strings.TrimPrefix(value, " ")
			switch field {
			case "event":
				ev.Name, fields = value, true
			case "id":
				ev.ID, fields = value, true
			case "retry":
				if n, err := strconv.Atoi(value); err == nil && n >= 0 {
					ev.Retry = n
				}
				fields = true
			case "data":
				size += len(value)
				if size > r.max {
					r.err, r.done = sse.ErrEventTooLarge, true
					return false
				}
				data, fields = append(data, value), true
			}
		}
	}
	r.done = true
	if err := r.sc.Err(); err != nil {
		r.err = err
		return false
	}
	if fields {
		return flush()
	}
	return false
}

func (r *eventReader) event() sseEvent { return r.cur }

// err reports the first read error (end of stream is not one).
func (r *eventReader) failure() error { return r.err }
