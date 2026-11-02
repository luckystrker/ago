package events

import (
	"bytes"
	"encoding/json"
	"math/rand"
	"reflect"
	"testing"
	"testing/quick"
	"time"
)

// wireOrigin extracts the origin key from marshaled request JSON.
func wireOrigin(t *testing.T, data []byte) string {
	t.Helper()
	var w map[string]any
	if err := json.Unmarshal(data, &w); err != nil {
		t.Fatalf("unmarshal %s: %v", data, err)
	}
	o, ok := w["origin"].(string)
	if !ok {
		t.Fatalf("origin key missing or not a string in %s", data)
	}
	return o
}

// TestRequestOriginMarshalNormalization pins the wire contract: origin always
// rides the JSON, and an empty in-memory origin normalizes to the Python field
// defaults ("user" for requests, "service" for drained final merges).
func TestRequestOriginMarshalNormalization(t *testing.T) {
	tests := []struct {
		name string
		ev   Event
		want string
	}{
		{"request zero origin", &ModelRequest{Parts: []Input{TextInput{Content: "x"}}}, "user"},
		{"request explicit service", &ModelRequest{Origin: OriginService}, "service"},
		{"request explicit user", &ModelRequest{Origin: OriginUser}, "user"},
		{"drained zero origin", &DrainedModelRequest{}, "service"},
		{"drained explicit user", &DrainedModelRequest{ModelRequest{Origin: OriginUser}}, "user"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			data, err := MarshalEvent(tc.ev)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if got := wireOrigin(t, data); got != tc.want {
				t.Fatalf("origin = %q, want %q (%s)", got, tc.want, data)
			}
		})
	}
}

// TestRequestOriginUnmarshalDefaults pins the legacy decoding: a record without
// origin is a user request; a drained record without origin is a service merge.
func TestRequestOriginUnmarshalDefaults(t *testing.T) {
	tests := []struct {
		name string
		json string
		want RequestOrigin
	}{
		{"request legacy", `{"@type":"ModelRequest","parts":[]}`, OriginUser},
		{"request explicit", `{"@type":"ModelRequest","parts":[],"origin":"service"}`, OriginService},
		{"drained legacy", `{"@type":"DrainedModelRequest","parts":[]}`, OriginService},
		{"drained explicit", `{"@type":"DrainedModelRequest","parts":[],"origin":"user"}`, OriginUser},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ev, err := UnmarshalEvent([]byte(tc.json))
			if err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			var got RequestOrigin
			switch r := ev.(type) {
			case *ModelRequest:
				got = r.Origin
			case *DrainedModelRequest:
				got = r.Origin
			default:
				t.Fatalf("unexpected type %T", ev)
			}
			if got != tc.want {
				t.Fatalf("origin = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestRequestOriginRoundTrip pins that an explicit origin survives a full
// marshal/unmarshal cycle (wire compatibility of the new field).
//
// Equality note: the Python field carries compare=False — origin is excluded
// from dataclass equality. Go compares every field, so tests comparing
// requests structurally (reflect.DeepEqual or ==) must set Origin explicitly
// on both sides.
func TestRequestOriginRoundTrip(t *testing.T) {
	req := &ModelRequest{Parts: []Input{TextInput{Content: "hi"}}, Origin: OriginService}
	data, err := MarshalEvent(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	back, err := UnmarshalEvent(data)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got, ok := back.(*ModelRequest)
	if !ok {
		t.Fatalf("type: %T", back)
	}
	if got.Origin != OriginService {
		t.Fatalf("origin = %q, want service", got.Origin)
	}
	again := wireOrigin(t, mustMarshal(t, got))
	if again != "service" {
		t.Fatalf("re-marshaled origin = %q, want service", again)
	}
}

func mustMarshal(t *testing.T, ev Event) []byte {
	t.Helper()
	b, err := MarshalEvent(ev)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// --- generative property (testing/quick) ------------------------------------

// quickOrigins enumerates every representable in-memory origin state, including
// the legacy empty one the wire normalizes.
var quickOrigins = []RequestOrigin{"", OriginUser, OriginService}

// quickStrings alphabet mixes ASCII, spaces and multi-byte runes so a
// round-trip exercises UTF-8 and JSON escaping, not just plain words.
var quickStrings = []rune("abAB01 ·é—")

func quickString(r *rand.Rand) string {
	out := make([]rune, r.Intn(8))
	for i := range out {
		out[i] = quickStrings[r.Intn(len(quickStrings))]
	}
	return string(out)
}

// quickJSONValue produces JSON-native values only (string, float64, bool,
// nil): Metadata decodes into map[string]any, so a generated value must keep
// its Go type across a JSON round-trip for the property to compare the two
// sides. Arbitrary Go values (int64, nested structs) are out of the wire
// domain by construction.
func quickJSONValue(r *rand.Rand) any {
	switch r.Intn(4) {
	case 0:
		return quickString(r)
	case 1:
		return float64(r.Intn(1000)) / 8
	case 2:
		return r.Intn(2) == 0
	default:
		return nil
	}
}

var quickBinaryTypes = []BinaryType{BinaryBinary, BinaryImage, BinaryAudio, BinaryDocument, BinaryVideo, ""}

var quickMediaTypes = []string{"text/plain", "image/png", ""}

// quickPart generates one Input of a random kind with a random payload.
func quickPart(r *rand.Rand) Input {
	switch r.Intn(5) {
	case 0:
		return &TextInput{Content: quickString(r)}
	case 1:
		return &DataInput{Data: quickJSONValue(r)}
	case 2:
		var meta map[string]any
		if r.Intn(2) == 0 {
			meta = map[string]any{quickString(r): quickJSONValue(r)}
		}
		data := make([]byte, r.Intn(16))
		r.Read(data)
		return &BinaryInput{
			Data:           data,
			MediaType:      quickMediaTypes[r.Intn(len(quickMediaTypes))],
			Kind:           quickBinaryTypes[r.Intn(len(quickBinaryTypes))],
			VendorMetadata: meta,
		}
	case 3:
		return &UrlInput{URL: quickString(r), Kind: quickBinaryTypes[r.Intn(len(quickBinaryTypes))]}
	default:
		name := ""
		if r.Intn(2) == 0 {
			name = quickString(r)
		}
		return &FileIdInput{FileID: quickString(r), Filename: name}
	}
}

// quickRequest generates a request event in every representable state: either
// wire shape (ModelRequest / DrainedModelRequest), all three origin states,
// zero to three parts of every Input kind and optional metadata. CreatedAt is
// always a non-zero UTC timestamp: a zero Base cannot survive a JSON
// round-trip under DeepEqual (the zero time carries a nil location, the parsed
// one is UTC), so the generative domain here is stamped events.
type quickRequest struct {
	drained bool
	req     ModelRequest
}

func (quickRequest) Generate(r *rand.Rand, size int) reflect.Value {
	req := ModelRequest{Origin: quickOrigins[r.Intn(len(quickOrigins))]}
	for n := r.Intn(4); n > 0; n-- {
		req.Parts = append(req.Parts, quickPart(r))
	}
	if n := r.Intn(3); n > 0 {
		req.Metadata = make(map[string]any, n)
		for i := 0; i < n; i++ {
			req.Metadata[quickString(r)] = quickJSONValue(r)
		}
	}
	req.CreatedAt = time.Date(2026, 1, 2, 3, 4, r.Intn(60), 0, time.UTC)
	return reflect.ValueOf(quickRequest{drained: r.Intn(2) == 0, req: req})
}

// wireOriginOf extracts the origin key from marshaled request JSON; ok=false
// when it is missing or not a string (a property must report, not Fatal).
func wireOriginOf(data []byte) (string, bool) {
	var w map[string]json.RawMessage
	if err := json.Unmarshal(data, &w); err != nil {
		return "", false
	}
	raw, ok := w["origin"]
	if !ok {
		return "", false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false
	}
	return s, true
}

// TestQuickRequestOriginWireRoundTrip sweeps the whole representable request
// domain (generative companion of the fixed tables above) and checks the three
// wire invariants of the origin field at once:
//
//  1. the origin always rides the JSON and is normalized to the Python field
//     defaults — an empty in-memory origin reads as "user" for a request and
//     "service" for a drained final merge;
//  2. the decoded event equals the original with that normalization applied,
//     including every part payload;
//  3. the wire form is idempotent — marshaling the decoded event reproduces
//     the exact bytes.
func TestQuickRequestOriginWireRoundTrip(t *testing.T) {
	prop := func(c quickRequest) bool {
		var ev Event = &c.req
		if c.drained {
			ev = &DrainedModelRequest{ModelRequest: c.req}
		}
		data, err := MarshalEvent(ev)
		if err != nil {
			return false
		}
		origin, ok := wireOriginOf(data)
		if !ok || origin == "" {
			return false
		}
		wantOrigin := c.req.Origin
		if wantOrigin == "" {
			wantOrigin = OriginUser
			if c.drained {
				wantOrigin = OriginService
			}
		}
		if origin != string(wantOrigin) {
			return false
		}
		back, err := UnmarshalEvent(data)
		if err != nil {
			return false
		}
		want := c.req
		want.Origin = wantOrigin
		if len(want.Parts) == 0 {
			want.Parts = []Input{} // decode of "parts":[] is a non-nil empty slice
		}
		switch got := back.(type) {
		case *ModelRequest:
			if c.drained || !reflect.DeepEqual(got, &want) {
				return false
			}
		case *DrainedModelRequest:
			if !c.drained || !reflect.DeepEqual(got, &DrainedModelRequest{ModelRequest: want}) {
				return false
			}
		default:
			return false
		}
		again, err := MarshalEvent(back)
		return err == nil && bytes.Equal(data, again)
	}
	cfg := &quick.Config{MaxCount: 1000, Rand: rand.New(rand.NewSource(1))}
	if err := quick.Check(prop, cfg); err != nil {
		t.Fatalf("request origin wire property failed: %v", err)
	}
}
