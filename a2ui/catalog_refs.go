package a2ui

import (
	"fmt"
	"strings"
)

// A v1.0 catalog may $ref only the components, functions and leaf $defs it
// declares, plus the standard types in common_types.json (a2ui_protocol.md,
// "Restricted $ref Targets"). Any other target — another catalog's component,
// a reserved union definition, an undeclared common type — makes the catalog
// invalid; it is rejected when loaded rather than left to fail later as an
// unresolvable reference.

// catalogCommonTypes lists the common_types.json definitions a v1.0 catalog may
// $ref (the spec's restricted list; the reserved unions and the internal
// building blocks are absent on purpose).
var catalogCommonTypes = map[string]bool{
	"ComponentId":             true,
	"Child":                   true,
	"ChildList":               true,
	"DynamicString":           true,
	"DynamicNumber":           true,
	"DynamicBoolean":          true,
	"DynamicStringList":       true,
	"DynamicValue":            true,
	"AccessibilityAttributes": true,
	"CheckRule":               true,
	"Checkable":               true,
	"Action":                  true,
	"DataBinding":             true,
	"FunctionCall":            true,
}

// commonTypesRefPrefix is the only external spelling the rule accepts: the
// catalog's own $id directory is not part of it.
const commonTypesRefPrefix = "common_types.json#/$defs/"

// catalogSections are the catalog sections whose entries are checked and may be
// $ref-ed as "#/<section>/<name>".
var catalogSections = []string{"components", "functions", "$defs"}

// nonSchemaKeys hold annotations or literal data rather than subschemas, so a
// "$ref" inside them is not a reference. Mirrors upstream run_tests.py.
var nonSchemaKeys = map[string]bool{
	"metadata": true, "examples": true, "const": true,
	"default": true, "enum": true, "description": true,
}

func pointerToken(key string) string {
	return strings.ReplaceAll(strings.ReplaceAll(key, "~", "~0"), "/", "~1")
}

func pointerKey(token string) string {
	return strings.ReplaceAll(strings.ReplaceAll(token, "~1", "/"), "~0", "~")
}

func asObject(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

// checkCatalogReferences rejects a v1.0 catalog carrying a $ref the spec does
// not allow. The port has no custom-catalog load path (D-076), so the only
// caller is the schema manager's load of the vendored basic catalog: the same
// rule keeps a bad re-vendor from reaching the registry.
//
// AGO-DEVIATION (D-088): Go maps are unordered, so definitions and properties
// are walked in sorted-name order; Python walks declaration order and may
// report a different first offending $ref. The rule and the message shape are
// the same.
func checkCatalogReferences(catalog map[string]any) error {
	for _, section := range catalogSections {
		definitions := asObject(catalog[section])
		for _, name := range sortedKeys(definitions) {
			pointer := "/" + section + "/" + pointerToken(name)
			if err := checkDefinitionReferences(catalog, definitions[name], pointer); err != nil {
				return err
			}
		}
	}
	return nil
}

// isAllowedCatalogReference reports whether a v1.0 catalog may $ref ref: a
// local target declared in one of the three sections (never the reserved
// unions) or a listed standard type from common_types.json.
func isAllowedCatalogReference(catalog map[string]any, ref string) bool {
	if strings.HasPrefix(ref, commonTypesRefPrefix) {
		return catalogCommonTypes[strings.TrimPrefix(ref, commonTypesRefPrefix)]
	}
	for _, section := range catalogSections {
		prefix := "#/" + section + "/"
		if !strings.HasPrefix(ref, prefix) {
			continue
		}
		name := pointerKey(strings.TrimPrefix(ref, prefix))
		if section == "$defs" && (name == "anyComponent" || name == "anyFunction") {
			return false
		}
		_, declared := asObject(catalog[section])[name]
		return declared
	}
	return false
}

func checkDefinitionReferences(catalog map[string]any, schema any, pointer string) error {
	switch s := schema.(type) {
	case []any:
		for i, item := range s {
			if err := checkDefinitionReferences(catalog, item, fmt.Sprintf("%s/%d", pointer, i)); err != nil {
				return err
			}
		}
		return nil
	case map[string]any:
		if ref, ok := s["$ref"].(string); ok && !isAllowedCatalogReference(catalog, ref) {
			return fmt.Errorf(
				"a2ui: unsupported $ref '%s' at %s/$ref: a v1.0 catalog may only reference the "+
					"components, functions and leaf $defs it declares (#/components/..., "+
					"#/functions/..., #/$defs/...) and the standard types in %s...",
				ref, pointer, commonTypesRefPrefix)
		}
		for _, key := range sortedKeys(s) {
			value := s[key]
			if key == "properties" {
				if properties, ok := value.(map[string]any); ok {
					// Declared properties are subschemas whatever they are named.
					for _, name := range sortedKeys(properties) {
						sub := pointer + "/properties/" + pointerToken(name)
						if err := checkDefinitionReferences(catalog, properties[name], sub); err != nil {
							return err
						}
					}
					continue
				}
			}
			if nonSchemaKeys[key] {
				continue
			}
			if err := checkDefinitionReferences(catalog, value, pointer+"/"+pointerToken(key)); err != nil {
				return err
			}
		}
	}
	return nil
}
