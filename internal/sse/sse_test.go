package sse

import (
	"errors"
	"io"
	"strings"
	"testing"
)

func collect(t *testing.T, stream string) ([]struct {
	name string
	data string
}, error) {
	t.Helper()
	sc := NewScanner(strings.NewReader(stream))
	var out []struct {
		name string
		data string
	}
	for sc.Next() {
		name, data := sc.Event()
		out = append(out, struct {
			name string
			data string
		}{name, string(data)})
	}
	return out, sc.Err()
}

func TestScannerBasic(t *testing.T) {
	stream := "data: hello\n" +
		"\n" +
		"data: world\n" +
		"\n"
	events, err := collect(t, stream)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("want 2 events, got %d: %+v", len(events), events)
	}
	if events[0].data != "hello" || events[1].data != "world" {
		t.Fatalf("bad data: %+v", events)
	}
	if events[0].name != "" {
		t.Fatalf("name should be empty without an event line, got %q", events[0].name)
	}
}

func TestScannerMultiLineData(t *testing.T) {
	stream := "data: line1\n" +
		"data: line2\n" +
		"\n"
	events, err := collect(t, stream)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("want 1 event, got %d", len(events))
	}
	if events[0].data != "line1\nline2" {
		t.Fatalf("want joined data %q, got %q", "line1\nline2", events[0].data)
	}
}

func TestScannerEventNameAndComments(t *testing.T) {
	stream := ": keep-alive comment\n" +
		"event: message\n" +
		"data: payload\n" +
		"id: 42\n" +
		"retry: 3000\n" +
		"\n" +
		": another comment\n" +
		"\n"
	events, err := collect(t, stream)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("comments must not produce events, got %d: %+v", len(events), events)
	}
	if events[0].name != "message" || events[0].data != "payload" {
		t.Fatalf("bad event: %+v", events[0])
	}
}

func TestScannerDonePassesThrough(t *testing.T) {
	stream := "data: {\"x\":1}\n" +
		"\n" +
		"data: [DONE]\n" +
		"\n"
	events, err := collect(t, stream)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("want 2 events, got %d", len(events))
	}
	if events[1].data != "[DONE]" {
		t.Fatalf("[DONE] must travel as data, got %q", events[1].data)
	}
}

func TestScannerFieldWithoutSpaceAndValuelessField(t *testing.T) {
	stream := "data:no-space\n" +
		"data:\n" +
		"\n"
	events, err := collect(t, stream)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("want 1 event, got %d", len(events))
	}
	// "data:no-space" keeps its value verbatim; the valueless "data:" adds an
	// empty line to the payload.
	if events[0].data != "no-space\n" {
		t.Fatalf("want %q, got %q", "no-space\n", events[0].data)
	}
}

func TestScannerTrailingBlockWithoutBlankLine(t *testing.T) {
	stream := "data: first\n" +
		"\n" +
		"data: tail"
	events, err := collect(t, stream)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("trailing block must be delivered, got %d events", len(events))
	}
	if events[1].data != "tail" {
		t.Fatalf("bad tail data: %q", events[1].data)
	}
}

func TestScannerError(t *testing.T) {
	sc := NewScanner(brokenReader{})
	if sc.Next() {
		t.Fatal("expected no event from a failing reader")
	}
	if sc.Err() != errBroken {
		t.Fatalf("expected Err to report the read failure, got %v", sc.Err())
	}
	// After a failure Next stays false.
	if sc.Next() {
		t.Fatal("Next must not restart after an error")
	}
}

type brokenReader struct{}

func (brokenReader) Read([]byte) (int, error) { return 0, errBroken }

var errBroken = errors.New("broken")

func TestScannerEventTooLarge(t *testing.T) {
	stream := "event: msg\n" +
		"data: " + strings.Repeat("a", 200) + "\n" +
		"data: " + strings.Repeat("b", 200) + "\n" +
		"\n" +
		"data: after\n" +
		"\n"
	sc := NewScanner(strings.NewReader(stream), WithMaxEventSize(300))
	if sc.Next() {
		t.Fatal("an oversize event must not be delivered")
	}
	if !errors.Is(sc.Err(), ErrEventTooLarge) {
		t.Fatalf("want ErrEventTooLarge, got %v", sc.Err())
	}
	// After the failure Next stays false; the rest of the stream is not parsed.
	if sc.Next() {
		t.Fatal("Next must not restart after the error")
	}
}

func TestScannerEventTooLargeUnterminated(t *testing.T) {
	// The audit scenario: a peer sends endless small data lines and never the
	// blank line, so a per-line cap alone does not bound memory.
	var sb strings.Builder
	for i := 0; i < 1200; i++ {
		sb.WriteString("data: chunk\n")
	}
	sc := NewScanner(strings.NewReader(sb.String()), WithMaxEventSize(5000))
	if sc.Next() {
		t.Fatal("an unterminated oversize event must not be delivered")
	}
	if !errors.Is(sc.Err(), ErrEventTooLarge) {
		t.Fatalf("want ErrEventTooLarge, got %v", sc.Err())
	}
}

func TestScannerEventSizeResetsBetweenEvents(t *testing.T) {
	// Each block stays under the cap while the total exceeds it: the budget
	// must restart with every dispatched event.
	block := "data: " + strings.Repeat("a", 400) + "\n\n"
	sc := NewScanner(strings.NewReader(strings.Repeat(block, 3)), WithMaxEventSize(600))
	count := 0
	for sc.Next() {
		_, data := sc.Event()
		count++
		if len(data) != 400 {
			t.Fatalf("event %d: want 400 payload bytes, got %d", count, len(data))
		}
	}
	if count != 3 {
		t.Fatalf("want 3 delivered events, got %d", count)
	}
	if sc.Err() != nil {
		t.Fatalf("events under the cap must not error, got %v", sc.Err())
	}
}

func TestScannerDefaultEventCap(t *testing.T) {
	// Without options the default cap applies: just over DefaultMaxEventSize of
	// payload in one unterminated event must fail, not accumulate.
	r := &repeatingLineReader{
		line:  "data: " + strings.Repeat("a", 1000) + "\n",
		limit: int64(40 << 20), // hard stop so the test cannot loop forever
	}
	sc := NewScanner(r)
	if sc.Next() {
		t.Fatal("an event above DefaultMaxEventSize must not be delivered")
	}
	if !errors.Is(sc.Err(), ErrEventTooLarge) {
		t.Fatalf("want ErrEventTooLarge, got %v", sc.Err())
	}
}

func TestWithMaxEventSizeNonPositiveKeepsScanning(t *testing.T) {
	// A non-positive option must neither reject every event nor panic; it
	// falls back to the default cap.
	sc := NewScanner(strings.NewReader("data: hi\n\n"), WithMaxEventSize(0))
	if !sc.Next() {
		t.Fatalf("normal event rejected: %v", sc.Err())
	}
	if _, data := sc.Event(); string(data) != "hi" {
		t.Fatalf("bad data: %q", data)
	}
}

// repeatingLineReader yields the same complete line on every Read until the
// byte limit is reached, then EOF. The line is far smaller than the scanner
// read buffer, so a Read never splits it.
type repeatingLineReader struct {
	line  string
	sent  int64
	limit int64
}

func (r *repeatingLineReader) Read(p []byte) (int, error) {
	if r.sent >= r.limit {
		return 0, io.EOF
	}
	n := copy(p, r.line)
	r.sent += int64(n)
	return n, nil
}
