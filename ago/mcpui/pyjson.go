package mcpui

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
)

// A minimal serializer reproducing Python's json.dumps for the JSON-basic value
// set, so PostMessage is byte-identical to the Python reference: ", " between
// items and ": " after keys (json.dumps default separators), and
// ensure_ascii=True string escaping. Unexported: it exists only to keep
// PostMessage parity with ag2/ag2/mcp_ui/actions.py.
//
// AGO-DEVIATION: Go maps have no insertion order, so object keys serialize in
// sorted order where Python preserves dict insertion order (docs/DECISIONS.md
// D-044). Floats render via Go's shortest representation, not Python's repr
// ("1" vs "1.0").

// pythonJSON renders v the way json.dumps would.
func pythonJSON(v any) string {
	var b strings.Builder
	writePythonJSON(&b, v)
	return b.String()
}

func writePythonJSON(b *strings.Builder, v any) {
	switch t := v.(type) {
	case nil:
		b.WriteString("null")
	case string:
		b.WriteString(pythonString(t))
	case bool:
		if t {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case int:
		b.WriteString(strconv.FormatInt(int64(t), 10))
	case int8:
		b.WriteString(strconv.FormatInt(int64(t), 10))
	case int16:
		b.WriteString(strconv.FormatInt(int64(t), 10))
	case int32:
		b.WriteString(strconv.FormatInt(int64(t), 10))
	case int64:
		b.WriteString(strconv.FormatInt(t, 10))
	case uint:
		b.WriteString(strconv.FormatUint(uint64(t), 10))
	case uint8:
		b.WriteString(strconv.FormatUint(uint64(t), 10))
	case uint16:
		b.WriteString(strconv.FormatUint(uint64(t), 10))
	case uint32:
		b.WriteString(strconv.FormatUint(uint64(t), 10))
	case uint64:
		b.WriteString(strconv.FormatUint(t, 10))
	case float32:
		b.WriteString(strconv.FormatFloat(float64(t), 'g', -1, 32))
	case float64:
		b.WriteString(strconv.FormatFloat(t, 'g', -1, 64))
	case map[string]any:
		keys := make([]string, 0, len(t))
		for key := range t {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		b.WriteByte('{')
		for i, key := range keys {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(pythonString(key))
			b.WriteString(": ")
			writePythonJSON(b, t[key])
		}
		b.WriteByte('}')
	case []any:
		b.WriteByte('[')
		for i, item := range t {
			if i > 0 {
				b.WriteString(", ")
			}
			writePythonJSON(b, item)
		}
		b.WriteByte(']')
	default:
		writeFallback(b, v)
	}
}

// writeFallback serves the value shapes the native cases miss: named slices and
// arrays expand element-wise so they keep the json.dumps ", " separator, and
// anything else embeds as compact encoding/json output — exotic parameter
// values only, never the payload shapes PostMessage carries.
func writeFallback(b *strings.Builder, v any) {
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Slice, reflect.Array:
		b.WriteByte('[')
		for i := 0; i < rv.Len(); i++ {
			if i > 0 {
				b.WriteString(", ")
			}
			writePythonJSON(b, rv.Index(i).Interface())
		}
		b.WriteByte(']')
	default:
		// A marshaling failure renders the literal null, as json.dumps has no
		// other way to represent an unserializable value.
		raw, err := json.Marshal(v)
		if err != nil {
			raw = []byte("null")
		}
		b.Write(raw)
	}
}

// pythonString escapes s the way json.dumps(ensure_ascii=True) would: JSON
// mandatory escapes for the quote and backslash, short escapes for the five
// named control characters, \uXXXX (lowercase hex, surrogate pairs beyond the
// BMP) for other control characters and anything non-ASCII. <, > and ' stay
// verbatim — html escaping happens later in PostMessage.
func pythonString(s string) string {
	var b strings.Builder
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
			switch {
			case r >= 0x20 && r <= 0x7e:
				b.WriteRune(r)
			case r > 0xffff:
				// UTF-16 surrogate pair, as Python encodes non-BMP code points.
				r -= 0x10000
				hi := 0xd800 + (r >> 10)
				lo := 0xdc00 + (r & 0x3ff)
				fmt.Fprintf(&b, `\u%04x\u%04x`, hi, lo)
			default:
				fmt.Fprintf(&b, `\u%04x`, r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

// htmlEscape reproduces Python's html.escape(quote=True): & first, then < > "
// and ', with &#x27; for the apostrophe (Go's html.EscapeString picks different
// entities, &#39; and &#34;).
func htmlEscape(s string) string {
	return strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
		`"`, "&quot;",
		"'", "&#x27;",
	).Replace(s)
}
