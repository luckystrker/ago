package a2ui

import (
	"fmt"
	"strings"
)

// PromptSectionOptions mirror generate_prompt_section's toggles.
type PromptSectionOptions struct {
	// IncludeSchema appends the full vendored message schema to the section.
	IncludeSchema bool
	// IncludeRules appends the catalog's embedded component rules.
	IncludeRules bool
}

// DefaultPromptSectionOptions returns the Python defaults (both on).
func DefaultPromptSectionOptions() PromptSectionOptions {
	return PromptSectionOptions{IncludeSchema: true, IncludeRules: true}
}

// PromptExample renders the v1.0 example response the prompt section shows
// (schema_manager.py _build_prompt_example): the vendored prompt_example
// template re-emitted with Python's separators (",", ": ") and the active
// catalog id substituted in. The template's literal key order survives via
// the typed message marshal.
func (m *SchemaManager) PromptExample() string {
	var example []Message
	if err := unmarshalJSON(promptExampleJSON, &example); err != nil {
		return ""
	}
	lines := make([]string, 0, len(example))
	for i := range example {
		b, err := example[i].MarshalJSON()
		if err != nil {
			return ""
		}
		lines = append(lines, spacedColonsJSON(string(b)))
	}
	rendered := "[" + strings.Join(lines, ",") + "]"
	return strings.ReplaceAll(rendered, "{catalog_id}", m.catalogID)
}

// PromptSection generates the A2UI portion of the system prompt for the
// v1.0 basic catalog (schema_manager.py generate_prompt_section): the
// response format with the worked example, the message-type notes, the
// component list, the catalog rules, the server actions, and optionally the
// full vendored schema.
func (m *SchemaManager) PromptSection(actions []Action, opts PromptSectionOptions) string {
	catalog, _ := m.Catalog(m.catalogID)
	var sections []string

	exampleJSON := m.PromptExample()
	sections = append(sections, fmt.Sprintf(
		"## A2UI Response Format (%s)\n\n"+
			"You can generate rich UI responses using the A2UI %s protocol. "+
			"When you want to display a UI, write your conversational text first, "+
			"then wrap a JSON array of A2UI message objects between the "+
			"`%s` and `%s` tags.\n\n"+
			"Example response format:\n"+
			"```\n"+
			"Here is the UI you requested.\n"+
			"%s\n"+
			"%s\n"+
			"%s\n"+
			"```",
		VersionV1, VersionV1, JSONOpenTag, JSONCloseTag, JSONOpenTag, exampleJSON, JSONCloseTag))

	messageTypes := strings.ReplaceAll(promptMessageTypesMD, "{catalog_id}", m.catalogID)
	messageTypes = strings.ReplaceAll(messageTypes, "{version_string}", VersionV1)
	sections = append(sections, fmt.Sprintf("\n\n## A2UI Message Types\n\n%s", messageTypes))

	if catalog != nil {
		names := catalog.ComponentNames()
		if len(names) > 0 {
			compSection := fmt.Sprintf("\n\n## Available Components\n\n**Basic catalog:** %s\n\n",
				strings.Join(names, ", "))
			compSection += "Each component uses a flat discriminator format:\n" +
				"```json\n{\"id\": \"myId\", \"component\": \"Text\", \"text\": \"Hello\"}\n```\n\n" +
				"Components are referenced by `id`. Use `children` (array of ids) for layout components (Column, Row). " +
				"Use `child` (single id) for wrapper components (Card, Button)."
			sections = append(sections, compSection)
		}
	}

	if opts.IncludeRules && m.instructions != "" {
		sections = append(sections, "\n\n## Component Rules\n\n"+m.instructions)
	}

	if len(actions) > 0 {
		var actionLines []string
		actionLines = append(actionLines, "\n\n## Available Actions")
		actionLines = append(actionLines, "The following actions can be triggered by buttons in the UI:\n")
		actionLines = append(actionLines, "### Server Events")
		for _, a := range actions {
			line := "- `" + a.Name + "`"
			if a.Description != "" {
				line += ": " + a.Description
			}
			actionLines = append(actionLines, line)
			if len(a.ExampleContext) > 0 {
				actionLines = append(actionLines, fmt.Sprintf("  Context: %s", pyJSONMapAny(a.ExampleContext)))
			}
		}
		actionLines = append(actionLines,
			"\nTo create a button that triggers a server event:\n"+
				"```json\n"+
				`{"id": "action_btn", "component": "Button", "child": "action_btn_text", `+
				`"action": {"event": {"name": "<action_name>", "context": {...}}}},`+"\n"+
				`{"id": "action_btn_text", "component": "Text", "text": "Button Label"}`+"\n"+
				"```")
		sections = append(sections, strings.Join(actionLines, "\n"))
	}

	if opts.IncludeSchema {
		var schema any
		if err := unmarshalJSON(agentToRendererJSON, &schema); err == nil {
			rendered, err := marshalNoHTMLEscapeIndent(schema)
			if err == nil {
				sections = append(sections, fmt.Sprintf(
					"\n\n## A2UI Message Schema (%s)\n\n```json\n%s\n```", VersionV1, rendered))
			}
		}
	}

	return strings.Join(sections, "")
}

// pyJSONMapAny renders an example context the way json.dumps would in the
// actions prompt (sorted keys — see the pyjson.go note).
func pyJSONMapAny(m map[string]any) string {
	return pyJSON(m)
}

// marshalNoHTMLEscapeIndent is json.MarshalIndent without HTML escaping. Key
// order follows Go's sorted-map rule, not Python's insertion order — prompt
// text only, never a wire form. AGO-DEVIATION (prompt text cosmetics).
func marshalNoHTMLEscapeIndent(v any) (string, error) {
	raw, err := marshalNoHTMLEscape(v)
	if err != nil {
		return "", err
	}
	var buf strings.Builder
	if err := indentJSON(&buf, string(raw)); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// indentJSON re-renders compact JSON with two-space indent (Python
// json.dumps(indent=2) shape) without decoding — a scanner over the compact
// encoding, tracking string state.
func indentJSON(out *strings.Builder, compact string) error {
	depth := 0
	inString := false
	escaped := false
	for i := 0; i < len(compact); i++ {
		c := compact[i]
		if inString {
			out.WriteByte(c)
			if escaped {
				escaped = false
			} else if c == '\\' {
				escaped = true
			} else if c == '"' {
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
			out.WriteByte(c)
		case '{', '[':
			out.WriteByte(c)
			// Empty container stays on one line.
			if i+1 < len(compact) && (compact[i+1] == '}' || compact[i+1] == ']') {
				out.WriteByte(compact[i+1])
				i++
				continue
			}
			depth++
			out.WriteString("\n" + strings.Repeat("  ", depth))
		case '}', ']':
			depth--
			out.WriteString("\n" + strings.Repeat("  ", depth))
			out.WriteByte(c)
		case ',':
			out.WriteString(",\n" + strings.Repeat("  ", depth))
		case ':':
			out.WriteString(": ")
		default:
			out.WriteByte(c)
		}
	}
	if inString {
		return fmt.Errorf("unterminated string in JSON")
	}
	return nil
}

// spacedColonsJSON re-renders compact JSON with the separators
// schema_manager.py renders the prompt example with (",", ": "): commas stay
// tight, colons gain a space. A scanner over the compact encoding, so
// document key order survives. Used only for prompt rendering, never wire
// bytes.
func spacedColonsJSON(compact string) string {
	var b strings.Builder
	inString := false
	escaped := false
	for i := 0; i < len(compact); i++ {
		c := compact[i]
		if inString {
			b.WriteByte(c)
			if escaped {
				escaped = false
			} else if c == '\\' {
				escaped = true
			} else if c == '"' {
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
			b.WriteByte(c)
		case ':':
			b.WriteString(": ")
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}
