package a2ui

import (
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"
)

// This file renders JSON the way Python's json.dumps does, because client
// values flow verbatim into prompt strings whose format the Python
// implementation pins (incoming.py: json.dumps(action.context) and friends).
// AGO-DEVIATION: object keys are emitted sorted; Go maps have no order and the
// Python dicts these prompt strings were specified against are built in
// insertion order. Goldens captured from Python use sort_keys=True, so the two
// agree on every key set that matters for prompts.

// pyJSON renders v as Python json.dumps does with default separators
// (", " and ": ") and ensure_ascii=True.
func pyJSON(v any) string {
	var b strings.Builder
	pyWrite(&b, v, ", ", ": ")
	return b.String()
}

func pyWrite(b *strings.Builder, v any, sep, colon string) {
	switch t := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		if t {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case string:
		pyWriteString(b, t)
	case int:
		b.WriteString(strconv.Itoa(t))
	case int64:
		b.WriteString(strconv.FormatInt(t, 10))
	case float64:
		b.WriteString(pyFloat(t))
	case []any:
		b.WriteByte('[')
		for i, e := range t {
			if i > 0 {
				b.WriteString(sep)
			}
			pyWrite(b, e, sep, colon)
		}
		b.WriteByte(']')
	case map[string]any:
		b.WriteByte('{')
		first := true
		for _, k := range sortedKeys(t) {
			if !first {
				b.WriteString(sep)
			}
			first = false
			pyWriteString(b, k)
			b.WriteString(colon)
			pyWrite(b, t[k], sep, colon)
		}
		b.WriteByte('}')
	default:
		// No Python "default=str" fallback: callers only pass JSON-decoded
		// values. Render an obvious placeholder rather than a wrong number.
		b.WriteString(`"<non-serializable>"`)
	}
}

// pyFloat formats like Python repr(float) for the values that reach prompt
// strings. JSON-decoded numbers arrive as float64 with no int/float split, so
// whole values render as integers ("2") — the json.loads→json.dumps round-trip
// of the integer spelling that dominates prompt contexts.
// AGO-DEVIATION: a Python float written "2.0" would round-trip as "2.0"; a
// float64 cannot recover that distinction.
func pyFloat(f float64) string {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		// json.dumps refuses these; callers never produce them from JSON.
		return "null"
	}
	if f == math.Trunc(f) && math.Abs(f) < 1e15 {
		return strconv.FormatInt(int64(f), 10)
	}
	s := strconv.FormatFloat(f, 'g', -1, 64)
	if !strings.ContainsAny(s, ".eE") {
		s += ".0"
	}
	// Python writes exponents as e+NN / e-NN with at least two digits.
	if i := strings.IndexByte(s, 'e'); i >= 0 {
		mant, exp := s[:i], s[i+1:]
		sign := ""
		if len(exp) > 0 && (exp[0] == '+' || exp[0] == '-') {
			sign, exp = string(exp[0]), exp[1:]
		}
		if len(exp) < 2 {
			exp = "0" + exp
		}
		if sign == "" {
			sign = "+"
		}
		s = mant + "e" + sign + exp
	}
	return s
}

const pyHex = "0123456789abcdef"

// pyWriteString escapes like json.dumps with ensure_ascii=True: control
// characters, the quote and the backslash, and every non-ASCII code point as
// \uXXXX (surrogate pairs above the BMP).
func pyWriteString(b *strings.Builder, s string) {
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		default:
			if r < 0x20 || r > 0x7e {
				if r > 0xffff {
					r1, r2 := utf16.EncodeRune(r)
					writeU(b, r1)
					writeU(b, r2)
				} else {
					writeU(b, r)
				}
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
}

func writeU(b *strings.Builder, r rune) {
	b.WriteString(`\u`)
	b.WriteByte(pyHex[(r>>12)&0xf])
	b.WriteByte(pyHex[(r>>8)&0xf])
	b.WriteByte(pyHex[(r>>4)&0xf])
	b.WriteByte(pyHex[r&0xf])
}

// pyRepr renders a scalar the way Python repr would inside a diagnostic
// message ("unknown component 'Widget'"): strings in single quotes, JSON
// spellings for everything else.
func pyRepr(v any) string {
	if s, ok := v.(string); ok {
		return "'" + s + "'"
	}
	return pyJSON(v)
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
