package a2ui

import (
	"encoding/json"
	"strings"
	"testing"
)

// NewSchemaManager applies the restricted-$ref rule to the catalog it loads.
// The port has no custom-catalog path (D-076), so a bad re-vendor is the only
// way a rejected catalog can reach the loader; overriding the embedded text
// pins that the load-time check is actually wired in, not just unit-tested.
func TestSchemaManagerRejectsForbiddenCatalogReference(t *testing.T) {
	original := basicCatalogJSON
	t.Cleanup(func() { basicCatalogJSON = original })

	var catalog map[string]any
	if err := json.Unmarshal([]byte(original), &catalog); err != nil {
		t.Fatalf("decode vendored catalog: %v", err)
	}
	components, ok := catalog["components"].(map[string]any)
	if !ok {
		t.Fatal("vendored catalog has no components object")
	}
	components["Broken"] = map[string]any{
		"properties": map[string]any{
			"text": map[string]any{"$ref": "another_catalog.json#/$defs/Shared"},
		},
	}
	corrupt, err := json.Marshal(catalog)
	if err != nil {
		t.Fatalf("encode catalog: %v", err)
	}
	basicCatalogJSON = string(corrupt)

	if _, err := NewSchemaManager(); err == nil {
		t.Fatal("NewSchemaManager accepted a catalog with a forbidden $ref")
	} else if !strings.Contains(err.Error(), "another_catalog.json#/$defs/Shared") {
		t.Errorf("load error %q must name the offending $ref", err)
	}
}
