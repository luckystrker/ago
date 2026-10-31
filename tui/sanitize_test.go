package tui

import (
	"strings"
	"testing"
)

// TestSanitizeTerminal: untrusted model text must lose every ESC-initiated
// sequence (OSC 52 clipboard writes above all) and every C0/C1 control except
// \n, \r, \t, while ordinary text — including multi-byte UTF-8 — passes
// through unchanged.
func TestSanitizeTerminal(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "plain text unchanged", in: "hello, world", want: "hello, world"},
		{name: "utf-8 text unchanged", in: "Привет — δ世界 —", want: "Привет — δ世界 —"},
		{name: "layout controls kept", in: "a\nb\r\nc\td", want: "a\nb\r\nc\td"},

		{name: "OSC 52 clipboard write, ST terminator",
			in:   "x\x1b]52;c;aGVsbG8=\x1b\\y",
			want: "xy"},
		{name: "OSC 52 clipboard write, BEL terminator",
			in:   "x\x1b]52;c;aGVsbG8=\x07y",
			want: "xy"},
		{name: "OSC 52 read request",
			in:   "\x1b]52;?;\x1b\\ok",
			want: "ok"},
		{name: "OSC window title",
			in:   "\x1b]0;evil title\x07body",
			want: "body"},
		{name: "unterminated OSC dropped at end of input",
			in:   "ok\x1b]52;c;aGVsbG8=",
			want: "ok"},

		{name: "CSI colors", in: "\x1b[31mred\x1b[0mtext", want: "redtext"},
		{name: "CSI erase screen and home cursor", in: "a\x1b[2J\x1b[Hb", want: "ab"},
		{name: "CSI private parameter", in: "\x1b[?25lx", want: "x"},
		{name: "CSI control byte inside aborts but keeps the control",
			in: "a\x1b[31\nmb", want: "a\nmb"},

		{name: "two-byte escapes (DECSC/DECRC/RIS)", in: "\x1b7x\x1b8y\x1bcz", want: "xyz"},
		{name: "charset escape ESC ( B", in: "\x1b(Bok", want: "ok"},
		{name: "DCS string dropped", in: "\x1bP1;2q#\x1b\\ok", want: "ok"},
		{name: "APC string dropped", in: "a\x1b_Gh0ST\x1b\\b", want: "ab"},

		{name: "C0 dropped (NUL BEL BS VT)", in: "a\x00b\x07c\x08d\x0be", want: "abcde"},
		{name: "C1 rune dropped (U+009B CSI)", in: "a\u009bb", want: "ab"},
		{name: "raw 8-bit C1 byte dropped", in: "a\x9bb", want: "ab"},
		{name: "lone ESC before non-final control keeps the control",
			in: "a\x1b\nb", want: "a\nb"},
		{name: "lone ESC before final-range byte consumes both",
			// Terminals eat ESC+0x30..0x7E as one two-byte sequence.
			in: "a\x1bq", want: "a"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sanitizeTerminal(tt.in); got != tt.want {
				t.Errorf("sanitizeTerminal(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestSanitizerPushSplitSequences: a sequence cut across streaming deltas is
// withheld until its terminator arrives and then dropped whole — a provider
// streaming a few characters at a time must not smuggle half a sequence into
// the terminal.
func TestSanitizerPushSplitSequences(t *testing.T) {
	var s sanitizer
	var got strings.Builder
	for _, delta := range []string{
		"x",                // plain
		"\x1b]5",           // OSC 52 opens, cut right after the introducer
		"2;c;aGVsbG8=\x1b", // payload arrives, ST is half-delivered
		"\\ done",          // ST completes; the whole OSC 52 is swallowed
		"\x1b[3",           // CSI cut between parameter bytes
		"1mred\x1b[0m.",    // CSI completes, colors vanish, text stays
	} {
		got.WriteString(s.push(delta))
	}
	if want := "x donered."; got.String() != want {
		t.Errorf("streamed output = %q, want %q", got.String(), want)
	}
}

// TestSanitizerPushSplitRune: a UTF-8 rune cut in half by chunking is passed
// through byte-by-byte and reassembles in the terminal — the sanitizer must
// not eat the halves as invalid input.
func TestSanitizerPushSplitRune(t *testing.T) {
	var s sanitizer
	first := s.push("при") // whole runes
	halves := "\xd0"       // first byte of 'в' (U+0432)
	rest := "\xb2"         // second byte, arrives in the next delta
	got := first + s.push(halves) + s.push(rest)
	if want := "прив"; got != want {
		t.Errorf("split-rune output = %q, want %q", got, want)
	}
}
