package sse

import (
	"errors"
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
