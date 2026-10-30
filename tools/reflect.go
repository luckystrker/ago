package tools

import (
	"reflect"
	"strconv"
	"strings"
)

var errorType = reflect.TypeOf((*error)(nil)).Elem()

// derefType removes pointer indirection.
func derefType(t reflect.Type) reflect.Type {
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t
}

func isIntKind(k reflect.Kind) bool {
	switch k {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return true
	}
	return false
}

// jsonFieldName returns the JSON name of a struct field (json tag or Go field name,
// matching encoding/json) and whether the field is skipped (`json:"-"`).
func jsonFieldName(f reflect.StructField) (name string, tagged bool, skip bool) {
	tag := f.Tag.Get("json")
	if tag == "-" {
		return "", false, true
	}
	if i := strings.Index(tag, ","); i >= 0 {
		tag = tag[:i]
	}
	if tag != "" {
		return tag, true, false
	}
	return f.Name, false, false
}

// structTagWith appends one `key:"value"` directive to a field tag.
func structTagWith(f reflect.StructField, key, value string) reflect.StructTag {
	base := strings.TrimSpace(string(f.Tag))
	directive := key + `:"` + value + `"`
	if base == "" {
		return reflect.StructTag(directive)
	}
	return reflect.StructTag(base + " " + directive)
}

// quoteGoName renders a reflect type for diagnostics.
func quoteGoName(t reflect.Type) string {
	if t == nil {
		return "<nil>"
	}
	return strconv.Quote(t.String())
}
