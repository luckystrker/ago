package a2ui

import (
	"strings"
	"testing"
)

// The protocol is still selected as "v1.0" (spec directory, A2A extension URI)
// while a stable v1.0 message carries the bare "1.0". Conflating the two emits
// a wire version no renderer accepts.
func TestProtocolAndWireVersionsAreDistinct(t *testing.T) {
	if VersionV1 != "v1.0" {
		t.Errorf("VersionV1 = %q, want the protocol selector %q", VersionV1, "v1.0")
	}
	if WireVersionV1 != "1.0" {
		t.Errorf("WireVersionV1 = %q, want the message version %q", WireVersionV1, "1.0")
	}
}

func TestMessageDefaultsToStableWireVersion(t *testing.T) {
	raw, err := Message{DeleteSurface: &DeleteSurface{SurfaceID: "s1"}}.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	want := `{"version":"1.0","deleteSurface":{"surfaceId":"s1"}}`
	if string(raw) != want {
		t.Errorf("default wire form:\n  want %s\n  got  %s", want, raw)
	}

	// An explicit version is caller-owned: stamping never rewrites it.
	raw, err = Message{Version: "v1.0", DeleteSurface: &DeleteSurface{SurfaceID: "s1"}}.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"version":"v1.0"`) {
		t.Errorf("explicit version clobbered: %s", raw)
	}
}

func TestSchemaManagerSeparatesProtocolAndWireVersions(t *testing.T) {
	mgr, err := NewSchemaManager()
	if err != nil {
		t.Fatal(err)
	}
	if got, want := mgr.ProtocolVersion(), VersionV1; got != want {
		t.Errorf("SchemaManager.ProtocolVersion() = %q, want %q", got, want)
	}
	if got, want := mgr.VersionString(), WireVersionV1; got != want {
		t.Errorf("SchemaManager.VersionString() = %q, want the wire version %q", got, want)
	}

	parser := NewParser(mgr)
	if got, want := parser.VersionString(), WireVersionV1; got != want {
		t.Errorf("Parser.VersionString() = %q, want the wire version %q", got, want)
	}
	if got, want := parser.ProtocolVersion(), VersionV1; got != want {
		t.Errorf("Parser.ProtocolVersion() = %q, want the protocol selector %q", got, want)
	}
}

func TestPromptCarriesWireVersionNotProtocolVersion(t *testing.T) {
	mgr, err := NewSchemaManager()
	if err != nil {
		t.Fatal(err)
	}
	example := mgr.PromptExample()
	if !strings.Contains(example, `"version": "1.0"`) {
		t.Errorf("prompt example must carry version 1.0: %s", example)
	}
	if strings.Contains(example, `"v1.0"`) || strings.Contains(example, "{version_string}") {
		t.Errorf("prompt example leaked the protocol selector or an unsubstituted placeholder: %s", example)
	}

	section := mgr.PromptSection(nil, PromptSectionOptions{})
	// Headings name the protocol, the wire values name the message version.
	if !strings.Contains(section, "## A2UI Response Format (v1.0)") {
		t.Error("prompt section must name the protocol v1.0")
	}
	if !strings.Contains(section, `"version": "1.0"`) {
		t.Error("prompt section message types must show the wire version 1.0")
	}
	if strings.Contains(section, "{version_string}") {
		t.Error("prompt section has an unsubstituted version placeholder")
	}
}

func TestValidationRejectsProtocolSelectorAsWireVersion(t *testing.T) {
	mgr, err := NewSchemaManager()
	if err != nil {
		t.Fatal(err)
	}
	parser := NewParser(mgr)

	op := func(version string) map[string]any {
		return map[string]any{
			"version":       version,
			"deleteSurface": map[string]any{"surfaceId": "s1"},
		}
	}
	if vr := parser.Validate([]map[string]any{op(WireVersionV1)}); !vr.IsValid {
		t.Errorf("version 1.0 must validate: %v", vr.Errors)
	}
	if vr := parser.Validate([]map[string]any{op(VersionV1)}); vr.IsValid {
		t.Errorf("version v1.0 must be rejected by the stable v1.0 schema: %v", vr.Errors)
	}
}

func TestTextFieldValueRequired(t *testing.T) {
	mgr, err := NewSchemaManager()
	if err != nil {
		t.Fatal(err)
	}
	parser := NewParser(mgr)

	textField := func(props map[string]any) map[string]any {
		return map[string]any{
			"version": WireVersionV1,
			"updateComponents": map[string]any{
				"surfaceId": "s1",
				"components": []any{
					map[string]any{"id": "root", "component": "Card", "child": "f"},
					mergeProps(map[string]any{"id": "f", "component": "TextField"}, props),
				},
			},
		}
	}
	cases := []struct {
		name  string
		props map[string]any
		valid bool
	}{
		{name: "with literal value", props: map[string]any{"label": "Qty", "value": "2"}, valid: true},
		{name: "with binding value", props: map[string]any{"label": "Qty", "value": map[string]any{"@path": "/qty"}}, valid: true},
		{name: "missing value", props: map[string]any{"label": "Qty"}, valid: false},
	}
	for _, tc := range cases {
		vr := parser.Validate([]map[string]any{textField(tc.props)})
		if vr.IsValid != tc.valid {
			t.Errorf("%s: is_valid = %v, want %v (%v)", tc.name, vr.IsValid, tc.valid, vr.Errors)
		}
	}
}

func TestCatalogWeightReferenceResolves(t *testing.T) {
	mgr, err := NewSchemaManager()
	if err != nil {
		t.Fatal(err)
	}
	parser := NewParser(mgr)

	text := func(weight any) map[string]any {
		props := map[string]any{"text": "hi"}
		if weight != nil {
			props["weight"] = weight
		}
		return map[string]any{
			"version": WireVersionV1,
			"updateComponents": map[string]any{
				"surfaceId": "s1",
				"components": []any{
					map[string]any{"id": "root", "component": "Card", "child": "t"},
					mergeProps(map[string]any{"id": "t", "component": "Text"}, props),
				},
			},
		}
	}
	if vr := parser.Validate([]map[string]any{text(2.0)}); !vr.IsValid {
		t.Errorf("numeric weight must validate through #/$defs/Weight: %v", vr.Errors)
	}
	if vr := parser.Validate([]map[string]any{text("heavy")}); vr.IsValid {
		t.Error("a non-numeric weight must fail the catalog's Weight type")
	}
}

func mergeProps(base, extra map[string]any) map[string]any {
	out := make(map[string]any, len(base)+len(extra))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}
