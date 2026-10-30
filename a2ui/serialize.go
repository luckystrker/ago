package a2ui

import (
	"strings"
	"unicode/utf16"
)

// ToJSONL serializes messages to canonical JSONL (serialize.py): one compact
// JSON object per line, no trailing newline, empty input → empty string. It is
// the single canonical encoding; every transport frame builds on it.
func ToJSONL(messages []Message) string {
	lines := make([]string, 0, len(messages))
	for i := range messages {
		b, err := messages[i].MarshalJSON()
		if err != nil {
			// MarshalJSON only fails on a caller bug (no/ambiguous payload);
			// surface it loudly instead of emitting a broken line.
			panic(err)
		}
		lines = append(lines, escapeNonASCII(string(b)))
	}
	return strings.Join(lines, "\n")
}

// escapeNonASCII rewrites every non-ASCII rune as \uXXXX (surrogate pairs
// above the BMP), matching json.dumps' ensure_ascii=True so the wire bytes are
// identical to the Python serializer's. ASCII bytes pass through untouched,
// so this is safe to apply to already-encoded JSON.
func escapeNonASCII(s string) string {
	needs := false
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			needs = true
			break
		}
	}
	if !needs {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 16)
	for _, r := range s {
		if r < 0x80 {
			b.WriteByte(byte(r))
			continue
		}
		if r > 0xffff {
			r1, r2 := utf16.EncodeRune(r)
			writeU(&b, r1)
			writeU(&b, r2)
		} else {
			writeU(&b, r)
		}
	}
	return b.String()
}
