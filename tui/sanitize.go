package tui

import (
	"strings"
	"unicode/utf8"
)

// esc is the single byte every terminal control sequence is built from.
const esc = '\x1b'

// sanitizer strips terminal control data from untrusted model text before it
// reaches the terminal (audit H-10, ANSI/OSC injection). Model output is
// attacker-controllable via prompt injection: an OSC 52 sequence makes the
// terminal overwrite the user's clipboard, CSI sequences can rewrite screen
// contents, colors and the window title. Everything ESC-initiated (CSI, OSC,
// DCS, SOS/PM/APC, two- and three-byte escapes) is dropped, and so are C0/C1
// control characters, except \n, \r and \t which carry normal text layout.
//
// The sanitizer is stateful so streaming works: a sequence split across
// deltas is held back until its terminator arrives and swallowed whole; an
// unterminated sequence at end of input is dropped — fail closed. It is not
// safe for concurrent use; each output stream owns one instance.
//
// AGO-DEVIATION: the Python TUI renders through textual widgets, which keep
// escape bytes out of the drawn output; this stdlib REPL writes to a raw
// io.Writer, so it must sanitize the text itself.
type sanitizer struct {
	// pending is an incomplete escape sequence carried over from the
	// previous push, waiting for its terminator.
	pending string
}

// sanitizeTerminal is the one-shot form of sanitizer for complete strings
// (final reply bodies, error texts). An incomplete trailing sequence is
// dropped rather than emitted.
func sanitizeTerminal(s string) string {
	var sn sanitizer
	return sn.push(s)
}

// push consumes one chunk and returns exactly the bytes that are safe to
// write to the terminal now. Bytes of an incomplete trailing escape sequence
// are withheld until a later push completes them (or they are dropped).
func (s *sanitizer) push(chunk string) string {
	buf := s.pending + chunk
	s.pending = ""
	var b strings.Builder
	b.Grow(len(chunk))
	i := 0
	for i < len(buf) {
		if buf[i] == esc {
			n, complete := escSeqLen(buf[i:])
			if !complete {
				s.pending = buf[i:]
				return b.String()
			}
			i += n
			continue
		}
		r, size := utf8.DecodeRuneInString(buf[i:])
		switch {
		case r == utf8.RuneError && size == 1:
			// Invalid byte. Keep it — a streaming delta may cut a multi-byte
			// rune in half and the next push completes it — unless it is a
			// raw 8-bit C1 control, which some terminals act on.
			if c := buf[i]; c >= 0x80 && c <= 0x9f {
				i++
				continue
			}
			b.WriteByte(buf[i])
			i++
		case r < 0x20 || (r >= 0x80 && r <= 0x9f):
			// C0/C1 controls: layout controls pass through, the rest drops.
			if r == '\n' || r == '\r' || r == '\t' {
				b.WriteString(buf[i : i+size])
			}
			i += size
		default:
			b.WriteString(buf[i : i+size])
			i += size
		}
	}
	return b.String()
}

// escSeqLen returns the length of the escape sequence starting at p[0] == ESC
// and whether that sequence is complete. When p ends mid-sequence, complete
// is false and the caller must wait for more input before emitting anything
// from it.
func escSeqLen(p string) (n int, complete bool) {
	if len(p) == 1 {
		return 0, false // lone ESC: the next byte decides its shape
	}
	switch c := p[1]; {
	case c == '[':
		// CSI: parameter/intermediate bytes 0x20–0x3F, then a final byte
		// 0x40–0x7E.
		j := 2
		for j < len(p) && p[j] >= 0x20 && p[j] <= 0x3f {
			j++
		}
		if j == len(p) {
			return 0, false
		}
		if p[j] >= 0x40 && p[j] <= 0x7e {
			return j + 1, true
		}
		// A control byte inside CSI: terminals execute it and abort the
		// sequence, so drop the parsed part and handle that byte on its own
		// (this keeps \n, \r, \t working inside a broken sequence).
		return j, true
	case c == ']' || c == 'P' || c == 'X' || c == '^' || c == '_':
		// String sequences (OSC, DCS, SOS, PM, APC): run to BEL or ST (ESC \).
		for j := 2; j < len(p); j++ {
			switch p[j] {
			case 0x07:
				return j + 1, true
			case esc:
				if j+1 == len(p) {
					return 0, false // may still turn into ST
				}
				if p[j+1] == '\\' {
					return j + 2, true
				}
				// A fresh ESC aborts the string; the new sequence starts here.
				return j, true
			}
		}
		return 0, false
	case c >= 0x20 && c <= 0x2f:
		// Intermediate-first escapes (charset selection, e.g. ESC ( B):
		// intermediates 0x20–0x2F, then one final byte 0x30–0x7E.
		j := 1
		for j < len(p) && p[j] >= 0x20 && p[j] <= 0x2f {
			j++
		}
		if j == len(p) {
			return 0, false
		}
		if p[j] >= 0x30 && p[j] <= 0x7e {
			return j + 1, true
		}
		return j, true
	case c >= 0x30 && c <= 0x7e:
		// Two-byte escape (ESC 7, ESC =, ESC M, ...). The byte after a lone
		// ESC in this range is consumed by real terminals as part of the
		// sequence, so it must go with the ESC.
		return 2, true
	default:
		// ESC followed by a control or 8-bit byte forms no sequence; drop
		// only the ESC itself.
		return 1, true
	}
}
