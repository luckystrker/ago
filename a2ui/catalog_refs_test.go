package a2ui

import (
	"strings"
	"testing"
)

// catalogForRefs is a minimal catalog declaration: the three sections whose
// entries a v1.0 catalog may $ref plus the two reserved union definitions.
func catalogForRefs() map[string]any {
	return map[string]any{
		"components": map[string]any{
			"Text": map[string]any{},
			"A/B":  map[string]any{},
		},
		"functions": map[string]any{"required": map[string]any{}},
		"$defs": map[string]any{
			"Weight":       map[string]any{},
			"anyComponent": map[string]any{},
			"anyFunction":  map[string]any{},
		},
	}
}

// withRef returns a catalog whose Text component carries the given $ref inside
// a property subschema (the shape the checker walks).
func withRef(ref string) map[string]any {
	c := catalogForRefs()
	c["components"].(map[string]any)["Text"] = map[string]any{
		"properties": map[string]any{
			"weight": map[string]any{"$ref": ref},
		},
	}
	return c
}

func TestCheckCatalogReferencesAllowed(t *testing.T) {
	refs := []string{
		"#/components/Text",
		"#/components/A~1B",
		"#/functions/required",
		"#/$defs/Weight",
		"common_types.json#/$defs/DynamicString",
		"common_types.json#/$defs/AccessibilityAttributes",
	}
	for _, ref := range refs {
		if err := checkCatalogReferences(withRef(ref)); err != nil {
			t.Errorf("$ref %q must be allowed: %v", ref, err)
		}
	}
}

func TestCheckCatalogReferencesRejected(t *testing.T) {
	cases := []struct {
		name string
		ref  string
		want string
	}{
		{"reserved anyComponent", "#/$defs/anyComponent", "#/$defs/anyComponent"},
		{"reserved anyFunction", "#/$defs/anyFunction", "#/$defs/anyFunction"},
		{"undeclared component", "#/components/Missing", "#/components/Missing"},
		{"undeclared function", "#/functions/missing", "#/functions/missing"},
		{"undeclared common type", "common_types.json#/$defs/Unknown", "common_types.json#/$defs/Unknown"},
		{"another catalog", "https://a2ui.org/specification/v0_9/catalogs/basic/catalog.json#/components/Text",
			"https://a2ui.org/specification/v0_9/catalogs/basic/catalog.json#/components/Text"},
		{"unknown section", "#/unknown/Text", "#/unknown/Text"},
		{"bare document", "catalog.json", "catalog.json"},
	}
	for _, tc := range cases {
		err := checkCatalogReferences(withRef(tc.ref))
		if err == nil {
			t.Errorf("%s: $ref %q must be rejected", tc.name, tc.ref)
			continue
		}
		if !strings.Contains(err.Error(), "$ref '"+tc.want+"'") {
			t.Errorf("%s: error %q does not name the offending $ref %q", tc.name, err, tc.want)
		}
		if !strings.Contains(err.Error(), "/components/Text/properties/weight/$ref") {
			t.Errorf("%s: error %q must point at the rejected location", tc.name, err)
		}
	}
}

func TestCheckCatalogReferencesIgnoresAnnotationValues(t *testing.T) {
	c := catalogForRefs()
	c["components"].(map[string]any)["Text"] = map[string]any{
		"description": "see $ref: another_catalog.json#/components/Other",
		"examples": []any{
			map[string]any{"$ref": "another_catalog.json#/components/Other"},
		},
		"const":   map[string]any{"$ref": "another_catalog.json#/components/Other"},
		"default": map[string]any{"$ref": "another_catalog.json#/components/Other"},
		"enum":    []any{"another_catalog.json#/components/Other"},
		"metadata": map[string]any{
			"$ref": "another_catalog.json#/components/Other",
		},
	}
	if err := checkCatalogReferences(c); err != nil {
		t.Errorf("$ref inside annotation values is data, not a reference: %v", err)
	}
}

func TestVendoredCatalogPassesReferenceCheck(t *testing.T) {
	mgr, err := NewSchemaManager()
	if err != nil {
		t.Fatalf("the vendored basic catalog must load: %v", err)
	}
	if err := checkCatalogReferences(mgr.catalog); err != nil {
		t.Errorf("the vendored basic catalog must satisfy the restricted-$ref rule: %v", err)
	}
}
