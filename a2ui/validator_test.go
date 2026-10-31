package a2ui

import (
	"fmt"
	"testing"
)

// Leaf paths must be independent slices. A plain append(path, part) reuses
// the parent's backing array when it has spare capacity (which growslice
// grants from depth 3 on), so two sibling failures would share one buffer
// and each would overwrite the other's last segment. These schemas put
// failing siblings at that depth for both the properties and items walks.
func TestValidatorLeafPathsNotAliased(t *testing.T) {
	v := &validator{mgr: &SchemaManager{}}
	strType := func() map[string]any { return map[string]any{"type": "string"} }

	t.Run("properties", func(t *testing.T) {
		schema := map[string]any{
			"properties": map[string]any{
				"p1": map[string]any{
					"properties": map[string]any{
						"p2": map[string]any{
							"properties": map[string]any{
								"obj": map[string]any{
									"properties": map[string]any{
										"a": strType(),
										"b": strType(),
									},
								},
							},
						},
					},
				},
			},
		}
		instance := map[string]any{
			"p1": map[string]any{
				"p2": map[string]any{
					"obj": map[string]any{"a": 1.0, "b": 2.0},
				},
			},
		}
		leaves := v.check(instance, schema, schema, nil)
		if len(leaves) != 2 {
			t.Fatalf("leaves: %d, want 2 (%+v)", len(leaves), leaves)
		}
		want := []string{"[p1 p2 obj a]", "[p1 p2 obj b]"}
		for i, l := range leaves {
			if got := fmt.Sprint(l.path); got != want[i] {
				t.Errorf("leaf %d path: want %s, got %s", i, want[i], got)
			}
		}
	})

	t.Run("items", func(t *testing.T) {
		schema := map[string]any{
			"properties": map[string]any{
				"p1": map[string]any{
					"properties": map[string]any{
						"p2": map[string]any{
							"properties": map[string]any{
								"p3": map[string]any{
									"items": strType(),
								},
							},
						},
					},
				},
			},
		}
		instance := map[string]any{
			"p1": map[string]any{
				"p2": map[string]any{
					"p3": []any{1.0, 2.0},
				},
			},
		}
		leaves := v.check(instance, schema, schema, nil)
		if len(leaves) != 2 {
			t.Fatalf("leaves: %d, want 2 (%+v)", len(leaves), leaves)
		}
		want := []string{"[p1 p2 p3 0]", "[p1 p2 p3 1]"}
		for i, l := range leaves {
			if got := fmt.Sprint(l.path); got != want[i] {
				t.Errorf("leaf %d path: want %s, got %s", i, want[i], got)
			}
		}
	})
}

// additionalProperties leaves share the same aliased-append surface.
func TestValidatorAdditionalPropertiesPathsNotAliased(t *testing.T) {
	v := &validator{mgr: &SchemaManager{}}
	schema := map[string]any{
		"properties": map[string]any{
			"p1": map[string]any{
				"properties": map[string]any{
					"p2": map[string]any{
						"properties": map[string]any{
							"obj": map[string]any{
								"properties": map[string]any{
									"known": map[string]any{"type": "string"},
								},
								"additionalProperties": false,
							},
						},
					},
				},
			},
		},
	}
	instance := map[string]any{
		"p1": map[string]any{
			"p2": map[string]any{
				"obj": map[string]any{"extra1": 1.0, "extra2": 2.0},
			},
		},
	}
	leaves := v.check(instance, schema, schema, nil)
	if len(leaves) != 2 {
		t.Fatalf("leaves: %d, want 2 (%+v)", len(leaves), leaves)
	}
	want := []string{"[p1 p2 obj extra1]", "[p1 p2 obj extra2]"}
	for i, l := range leaves {
		if got := fmt.Sprint(l.path); got != want[i] {
			t.Errorf("leaf %d path: want %s, got %s", i, want[i], got)
		}
	}
}
