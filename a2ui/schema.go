package a2ui

import (
	_ "embed"
	"fmt"
	"sort"
	"strings"
)

// Vendored v1.0 A2UI schemas (byte-identical copies of the upstream spec
// files the Python implementation loads from ag2/a2ui/v1_0/spec/). The
// validator is a hand-rolled subset engine — the Python side uses jsonschema
// + referencing, which do not carry over into the zero-dependency core.
var (
	//go:embed spec/v1_0/agent_to_renderer.json
	agentToRendererJSON string
	//go:embed spec/v1_0/renderer_to_agent.json
	rendererToAgentJSON string
	//go:embed spec/v1_0/common_types.json
	commonTypesJSON string
	//go:embed spec/v1_0/basic_catalog.json
	basicCatalogJSON string
	//go:embed spec/v1_0/prompt_example.json
	promptExampleJSON string
	//go:embed spec/v1_0/prompt_message_types.md
	promptMessageTypesMD string
)

const schemaBaseURI = "https://a2ui.org/specification/v1_0/"

// SchemaManager loads the vendored v1.0 schemas and keeps the registry of
// documents $refs resolve against. There is exactly one catalog in this port
// (the v1.0 basic catalog); custom catalogs are deferred (D-076).
type SchemaManager struct {
	catalogID    string
	docs         map[string]map[string]any
	catalog      map[string]any
	instructions string
}

// NewSchemaManager decodes the vendored v1.0 schemas. It fails only if a
// vendored file is corrupt (a build-time constant in practice).
func NewSchemaManager() (*SchemaManager, error) {
	m := &SchemaManager{docs: map[string]map[string]any{}}
	for _, raw := range []string{agentToRendererJSON, rendererToAgentJSON, commonTypesJSON, basicCatalogJSON} {
		var doc map[string]any
		if err := unmarshalJSON(raw, &doc); err != nil {
			return nil, fmt.Errorf("a2ui: vendored schema is not valid JSON: %w", err)
		}
		id, _ := doc["$id"].(string)
		if id == "" {
			return nil, fmt.Errorf("a2ui: vendored schema has no $id")
		}
		if err := m.registerDoc(doc); err != nil {
			return nil, err
		}
	}
	cat := m.docs[basicCatalogJSONURI()]
	m.catalog = cat
	m.catalogID = DefaultCatalogID
	instructions, _ := cat["instructions"].(string)
	m.instructions = strings.TrimSpace(instructions)
	// Refs the vendored files use, beyond absolute $ids:
	// "catalog.json" → the (only) catalog, bare file names → their documents,
	// and the base-URI-prefixed spellings. common_types.json also resolves
	// next to the catalog's own $id (v1.0 catalogs reference it bare).
	aliases := map[string]string{
		"catalog.json":                           basicCatalogJSONURI(),
		"common_types.json":                      commonTypesJSONURI(),
		"agent_to_renderer.json":                 agentToRendererURI(),
		"renderer_to_agent.json":                 rendererToAgentURI(),
		"basic_catalog.json":                     basicCatalogJSONURI(),
		schemaBaseURI + "common_types.json":      commonTypesJSONURI(),
		schemaBaseURI + "agent_to_renderer.json": agentToRendererURI(),
		schemaBaseURI + "renderer_to_agent.json": rendererToAgentURI(),
		schemaBaseURI + "basic_catalog.json":     basicCatalogJSONURI(),
		schemaBaseURI + "catalog.json":           basicCatalogJSONURI(),
		catalogNeighbor("common_types.json"):     commonTypesJSONURI(),
	}
	for alias, target := range aliases {
		m.docs[alias] = m.docs[target]
	}
	return m, nil
}

func agentToRendererURI() string  { return schemaBaseURI + "agent_to_renderer.json" }
func rendererToAgentURI() string  { return schemaBaseURI + "renderer_to_agent.json" }
func commonTypesJSONURI() string  { return schemaBaseURI + "common_types.json" }
func basicCatalogJSONURI() string { return schemaBaseURI + "catalogs/basic/catalog.json" }

// catalogNeighbor resolves a bare reference from next to the catalog's $id
// (urljoin(catalogURI, "common_types.json") in Python).
func catalogNeighbor(name string) string {
	return strings.TrimSuffix(basicCatalogJSONURI(), "catalog.json") + name
}

func (m *SchemaManager) registerDoc(doc map[string]any) error {
	id, _ := doc["$id"].(string)
	if _, dup := m.docs[id]; dup {
		return fmt.Errorf("a2ui: duplicate schema $id %q", id)
	}
	m.docs[id] = doc
	return nil
}

// VersionString reports the protocol version ("v1.0").
func (m *SchemaManager) VersionString() string { return VersionV1 }

// CatalogID reports the active catalog's $id.
func (m *SchemaManager) CatalogID() string { return m.catalogID }

// Catalog returns the registry entry for a surface's catalog id. The second
// return is false for an unknown id — the parser reports that as an unresolvable
// reference, the way Python surfaces referencing.Unresolvable.
func (m *SchemaManager) Catalog(catalogID string) (*Catalog, bool) {
	if _, ok := m.docs[catalogID]; !ok {
		return nil, false
	}
	return &Catalog{ID: catalogID, mgr: m}, true
}

// Catalog is one registered surface catalog bound to the schema registry.
type Catalog struct {
	ID  string
	mgr *SchemaManager
}

// Instructions returns the catalog's embedded guidance text (v1.0 embeds it
// in the catalog's own "instructions" field).
func (c *Catalog) Instructions() string { return c.mgr.instructions }

// ComponentNames lists the catalog's component names, sorted.
func (c *Catalog) ComponentNames() []string {
	components, _ := c.mgr.docs[basicCatalogJSONURI()]["components"].(map[string]any)
	names := make([]string, 0, len(components))
	for name := range components {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// validateOp validates one message against the agent_to_renderer schema,
// narrowed to the definition the message's single payload key selects (the
// MessageChecker narrowing of diagnostics.py), and enforces the v1.0
// agent-caller policy on callRendererFunction.
func (c *Catalog) validateOp(op map[string]any) []leaf {
	present := presentPayloadKeys(op)
	if len(present) != 1 {
		return []leaf{{nil, fmt.Sprintf("Not an A2UI message: expected an object with exactly one of %s",
			pyStringList(payloadKeys[:])), "message"}}
	}
	key := present[0]
	v := &validator{mgr: c.mgr, agentInitiated: key == "callRendererFunction"}
	leaves := v.check(op, map[string]any{"$ref": "#/$defs/" + payloadDefName(key)}, agentToRendererDoc(c.mgr), nil)
	if key == "callRendererFunction" {
		leaves = append(leaves, v.agentCallerIssues(op)...)
	}
	return leaves
}

func presentPayloadKeys(op map[string]any) []string {
	var present []string
	for _, k := range payloadKeys {
		if _, ok := op[k]; ok {
			present = append(present, k)
		}
	}
	return present
}

func payloadDefName(key string) string {
	switch key {
	case "createSurface":
		return "CreateSurfaceMessage"
	case "updateComponents":
		return "UpdateComponentsMessage"
	case "updateDataModel":
		return "UpdateDataModelMessage"
	case "deleteSurface":
		return "DeleteSurfaceMessage"
	case "callRendererFunction":
		return "CallRendererFunctionMessage"
	default:
		return "AgentFunctionResponseMessage"
	}
}

func agentToRendererDoc(m *SchemaManager) map[string]any {
	return m.docs[agentToRendererURI()]
}

// agentCallerIssues enforces the v1.0 caller policy the raw JSON schema cannot
// express without Python's catalog-reference surgery: against the basic
// catalog every function is rendererOnly, so an agent-initiated
// callRendererFunction naming a catalog function (or the @index system
// function) is rejected with the diagnostics unknown-tag message.
// AGO-DEVIATION: Python's jsonschema narrowing here reports a less actionable
// const-intersection error; the abbreviated diagnostics give the same
// "unknown @call '<name>'" wording the unknown-name case produces verbatim.
func (v *validator) agentCallerIssues(op map[string]any) []leaf {
	payload, _ := op["callRendererFunction"].(map[string]any)
	call, _ := payload["callFunction"].(map[string]any)
	name, _ := call["@call"].(string)
	if name == "" {
		return nil
	}
	catalog := v.mgr.docs[basicCatalogJSONURI()]
	functions, _ := catalog["functions"].(map[string]any)
	path := []any{"callRendererFunction", "callFunction"}
	if def, ok := functions[name].(map[string]any); ok {
		callers, _ := def["allowedCallers"].(string)
		if callers == "" {
			callers = "rendererOnly"
		}
		if callers != "agentOnly" && callers != "rendererOrAgent" {
			return []leaf{{path, fmt.Sprintf("unknown @call %s", pyRepr(name)), "oneOf"}}
		}
		return nil
	}
	if name == "@index" {
		// The index system function is renderer-template-only.
		return []leaf{{path, fmt.Sprintf("unknown @call %s", pyRepr(name)), "oneOf"}}
	}
	return nil
}
