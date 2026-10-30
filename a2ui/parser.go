package a2ui

import (
	"encoding/json"
	"fmt"

	"strings"
)

// ParseResult is the result of parsing an agent response for A2UI content
// (parser.py A2UIParseResult).
type ParseResult struct {
	// Text is the conversational portion of the response.
	Text string
	// Operations are the parsed A2UI operation objects.
	Operations []map[string]any
	// HasA2UI reports whether the response contained A2UI content.
	HasA2UI bool
	// RawJSON is the raw JSON string extracted from the response ("" if none).
	RawJSON string
	// ParseError is the error message if JSON parsing failed ("" if none).
	ParseError string
}

// ValidationResult is the result of validating A2UI operations against the
// schema (parser.py A2UIValidationResult).
type ValidationResult struct {
	IsValid bool
	Errors  []string
}

// Parser extracts and validates A2UI messages from agent responses: the block
// the LLM wraps between the official A2UI tags is the JSON; everything outside
// is conversational prose. With a SchemaManager it also validates the messages
// against the v1.0 schema.
type Parser struct {
	version string
	mgr     *SchemaManager // nil disables schema validation
}

// NewParser builds a v1.0 parser. mgr may be nil to skip schema validation.
func NewParser(mgr *SchemaManager) *Parser {
	return &Parser{version: VersionV1, mgr: mgr}
}

// VersionString reports the protocol version the parser speaks.
func (p *Parser) VersionString() string { return p.version }

// stripSingleLineFenceInfo removes the info string of a single-line fence,
// reproducing Python's `^[A-Za-z][A-Za-z0-9_-]*(?=\s|[{[])` substitution (Go's
// regexp has no lookahead): an alphabetic run is removed only when the next
// character is whitespace, "{" or "[".
func stripSingleLineFenceInfo(rest string) string {
	if rest == "" {
		return rest
	}
	c := rest[0]
	if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z') {
		return rest
	}
	i := 1
	for i < len(rest) {
		b := rest[i]
		if b >= 'A' && b <= 'Z' || b >= 'a' && b <= 'z' || b >= '0' && b <= '9' || b == '_' || b == '-' {
			i++
			continue
		}
		break
	}
	if i < len(rest) && (isSpaceByte(rest[i]) || rest[i] == '{' || rest[i] == '[') {
		return rest[i:]
	}
	return rest
}

func isSpaceByte(b byte) bool {
	switch b {
	case ' ', '\t', '\n', '\r', '\v', '\f':
		return true
	}
	return false
}

// StripMarkdownFences removes markdown code fences (```json ... ```) wrapping
// JSON content: multi-line fences (info string on the opening line) and
// single-line fences with an optional info string.
func StripMarkdownFences(text string) string {
	text = strings.TrimSpace(text)
	if strings.HasPrefix(text, "```") {
		rest := text[3:]
		if strings.Contains(rest, "\n") {
			text = rest[strings.Index(rest, "\n")+1:]
		} else {
			text = stripSingleLineFenceInfo(rest)
		}
	}
	text = strings.TrimSuffix(text, "```")
	return strings.TrimSpace(text)
}

// Parse extracts conversational text and A2UI messages from a response.
// Text before/after the <a2ui-json> block is prose; the block content parses
// as a JSON array/object or JSONL. A missing closing tag is tolerated — the
// block runs to the end of the response.
func (p *Parser) Parse(response string) ParseResult {
	openIdx := strings.Index(response, JSONOpenTag)
	if openIdx == -1 {
		return ParseResult{Text: strings.TrimSpace(response)}
	}

	textBefore := response[:openIdx]
	afterOpen := response[openIdx+len(JSONOpenTag):]

	var jsonPart, textAfter string
	closeIdx := strings.Index(afterOpen, JSONCloseTag)
	if closeIdx == -1 {
		// Tolerate a missing closing tag: take everything to the end.
		jsonPart = afterOpen
	} else {
		jsonPart = afterOpen[:closeIdx]
		textAfter = afterOpen[closeIdx+len(JSONCloseTag):]
	}

	var textParts []string
	for _, part := range []string{strings.TrimSpace(textBefore), strings.TrimSpace(textAfter)} {
		if part != "" {
			textParts = append(textParts, part)
		}
	}
	text := strings.Join(textParts, " ")
	jsonPart = StripMarkdownFences(jsonPart)

	if jsonPart == "" {
		return ParseResult{Text: text, HasA2UI: false}
	}

	operations, parseError := parseJSONBlock(jsonPart)
	if parseError != "" {
		return ParseResult{Text: text, HasA2UI: true, RawJSON: jsonPart, ParseError: parseError}
	}
	return ParseResult{Text: text, Operations: operations, HasA2UI: true, RawJSON: jsonPart}
}

// parseJSONBlock parses an <a2ui-json> block (JSON array/object, or JSONL)
// into A2UI messages, mirroring parser.py: a whole-block JSON failure falls
// back to line-by-line parsing and, when that also fails, reports the
// whole-block error (the more informative one).
func parseJSONBlock(jsonPart string) ([]map[string]any, string) {
	var parsed any
	if err := json.Unmarshal([]byte(jsonPart), &parsed); err != nil {
		operations, jsonlError := parseJSONL(jsonPart)
		if jsonlError == "" {
			return operations, ""
		}
		return nil, fmt.Sprintf("Invalid JSON: %v", err)
	}

	switch t := parsed.(type) {
	case map[string]any:
		return asMessages([]any{t})
	case []any:
		return asMessages(t)
	default:
		return nil, fmt.Sprintf("Expected JSON array or object, got %s", pyTypeName(parsed))
	}
}

func parseJSONL(jsonPart string) ([]map[string]any, string) {
	var values []any
	for _, line := range strings.Split(jsonPart, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var value any
		if err := json.Unmarshal([]byte(line), &value); err != nil {
			return nil, fmt.Sprintf("Invalid JSON: %v", err)
		}
		if _, ok := value.(map[string]any); !ok {
			return nil, fmt.Sprintf("Expected JSON object per line, got %s", pyTypeName(value))
		}
		values = append(values, value)
	}
	if len(values) == 0 {
		return nil, "No JSON objects found"
	}
	return asMessages(values)
}

// isMessageEnvelope reports whether value has the envelope of a message: an
// object with exactly one payload key. The payload contents are left to the
// schema validation.
func isMessageEnvelope(value any) bool {
	obj, ok := value.(map[string]any)
	if !ok {
		return false
	}
	return len(presentPayloadKeys(obj)) == 1
}

func asMessages(values []any) ([]map[string]any, string) {
	messages := make([]map[string]any, 0, len(values))
	for i, value := range values {
		if !isMessageEnvelope(value) {
			return nil, fmt.Sprintf(
				"Operation %d is not an A2UI message: expected an object with exactly one of %s",
				i, pyStringList(payloadKeys[:]))
		}
		messages = append(messages, value.(map[string]any))
	}
	return messages, ""
}

// Validate validates messages, inheriting catalogs from surfaces created in
// this response (parser.py validate). For a surface created in an earlier
// response the default catalog is used; an explicit unknown catalog is
// reported as an unresolvable reference.
func (p *Parser) Validate(operations []map[string]any) ValidationResult {
	if p.mgr == nil {
		return ValidationResult{IsValid: true}
	}

	var errors []string
	surfaceCatalogs := map[string]string{}
	for i, op := range operations {
		catalogID, ok := surfaceCatalogOf(op, surfaceCatalogs, p.mgr.CatalogID())
		if ok && catalogID != "" {
			catalog, known := p.mgr.Catalog(catalogID)
			if !known {
				errors = append(errors, fmt.Sprintf(
					"Operation %d: could not resolve schema reference (%s)", i, catalogID))
				continue
			}
			for _, l := range catalog.validateOp(op) {
				issue := Issue{Path: l.path, Message: l.message}
				errors = append(errors, fmt.Sprintf("Operation %d: %s", i, issue.Render(op)))
			}
			continue
		}
		defaultCatalog, _ := p.mgr.Catalog(p.mgr.CatalogID())
		for _, l := range defaultCatalog.validateOp(op) {
			issue := Issue{Path: l.path, Message: l.message}
			errors = append(errors, fmt.Sprintf("Operation %d: %s", i, issue.Render(op)))
		}
	}

	// v1.0 rule the JSON schema cannot express here: the agent never sends
	// agentFunctionResponse — the server does, answering a callAgentFunction.
	for i, op := range operations {
		if _, ok := op["agentFunctionResponse"]; ok {
			errors = append(errors, fmt.Sprintf(
				"Operation %d: 'agentFunctionResponse' is sent by the server in reply to a "+
					"'callAgentFunction'; do not emit it.", i))
		}
	}

	// Spec rule (server_to_client.json + a2ui.org): across all
	// component-carrying ops, at least one component must have id 'root'.
	// v1.0's createSurface can carry the initial components itself, so those
	// count alongside updateComponents.
	var componentLists [][]any
	hasComponentOps := false
	for _, op := range operations {
		if payload, ok := op["updateComponents"].(map[string]any); ok {
			hasComponentOps = true
			componentLists = append(componentLists, componentsOf(payload))
		}
		if payload, ok := op["createSurface"].(map[string]any); ok {
			list := componentsOf(payload)
			if len(list) > 0 {
				hasComponentOps = true
			}
			componentLists = append(componentLists, list)
		}
	}
	if hasComponentOps {
		hasRoot := false
		for _, list := range componentLists {
			for _, c := range list {
				if m, ok := c.(map[string]any); ok && m["id"] == "root" {
					hasRoot = true
				}
			}
		}
		if !hasRoot {
			errors = append(errors,
				"No component with id 'root' found across the surface's components — "+
					"the A2UI spec requires the component tree to have a 'root' node.")
		}
	}

	return ValidationResult{IsValid: len(errors) == 0, Errors: errors}
}

// surfaceCatalogOf tracks surface → catalog within one validation call
// (parser.py _surface_catalog): createSurface binds (catalogId or the default),
// deleteSurface unbinds, and the op's catalog is the binding at its surface.
func surfaceCatalogOf(operation map[string]any, surfaces map[string]string, defaultCatalogID string) (string, bool) {
	for _, key := range []string{"createSurface", "updateComponents", "callRendererFunction", "deleteSurface"} {
		payload, ok := operation[key].(map[string]any)
		if !ok {
			continue
		}
		surfaceID, ok := payload["surfaceId"].(string)
		if !ok {
			continue
		}
		if key == "createSurface" {
			catalogID, ok := payload["catalogId"].(string)
			if !ok {
				catalogID = defaultCatalogID
			}
			surfaces[surfaceID] = catalogID
		} else if key == "deleteSurface" {
			delete(surfaces, surfaceID)
		}
		catalog, bound := surfaces[surfaceID]
		return catalog, bound
	}
	return "", false
}

func componentsOf(payload map[string]any) []any {
	if payload == nil {
		return nil
	}
	list, _ := payload["components"].([]any)
	return list
}

// FormatValidationError formats validation errors as feedback for the LLM to
// self-correct (parser.py format_validation_error).
func (p *Parser) FormatValidationError(parseResult *ParseResult, validationResult *ValidationResult) string {
	lines := []string{"Your A2UI output had validation errors:"}
	for _, err := range validationResult.Errors {
		lines = append(lines, "- "+err)
	}
	if parseResult.ParseError != "" {
		// AGO-DEVIATION (D-076): the parse-error line carries the Go decoder's
		// message, not Python's; the prefix matches and the detail is
		// engine-specific (golden-tested on the prefix).
		lines = append(lines, "- JSON parse error: "+parseResult.ParseError)
	}
	lines = append(lines, "")
	lines = append(lines,
		"Please fix these errors and regenerate the A2UI JSON. "+
			fmt.Sprintf(`Make sure each message includes "version": %q and all required properties. `, p.version)+
			"The user never sees this exchange: do not mention the errors or the correction in your reply.")
	return strings.Join(lines, "\n")
}
