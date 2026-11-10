// Server-side tool blocks of the Messages API (web search, web fetch, code
// execution, tool search, the MCP connector): the model's call and the
// provider's answer arrive inside one response, run at Anthropic, and must be
// echoed back on the next request. They surface as the provider-neutral
// events.BuiltinToolCallEvent / BuiltinToolResultEvent with the verbatim block
// kept for replay (the route OpenAI hosted items take, see config/openai).
//
// AGO-DEVIATION: Python keeps the SDK block object on provider event subtypes
// (AnthropicServerToolCallEvent / AnthropicServerToolResultEvent); AGo has no
// subtyping of events, so the JSON block rides in
// VendorMetadata["block"] (call) and Result.Metadata["block"] (result).

package anthropic

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/tools"
)

// serverBlockKey is the VendorMetadata / Result.Metadata key carrying the
// verbatim wire block.
const serverBlockKey = "block"

// serverToolName maps a server_tool_use block's tool name to the
// provider-neutral tool name; unknown names map to "".
func serverToolName(block map[string]any) string {
	if block["type"] == "mcp_tool_use" {
		// AGO-DEVIATION: Python drops MCP connector blocks (they fall into its
		// "unhandled block" warning); AGo records them like the other server
		// tools so the connector's calls are not invisible and replay intact.
		return tools.MCPServerToolName
	}
	name, _ := block["name"].(string)
	switch name {
	case "web_search":
		return tools.WebSearchToolName
	case "web_fetch":
		return tools.WebFetchToolName
	case "code_execution", "bash_code_execution", "text_editor_code_execution":
		return tools.CodeExecutionToolName
	case "tool_search_tool_regex", "tool_search_tool_bm25":
		return tools.ToolSearchToolName
	}
	return ""
}

// serverToolCallEvent maps a server_tool_use / mcp_tool_use block to its call
// event; nil for a tool name AGo does not know (the block is then not replayed
// either, and neither is its result).
func serverToolCallEvent(block map[string]any) *events.BuiltinToolCallEvent {
	name := serverToolName(block)
	if name == "" {
		return nil
	}
	id, _ := block["id"].(string)
	args := "{}"
	if in, ok := block["input"]; ok && in != nil {
		if b, err := json.Marshal(in); err == nil {
			args = string(b)
		}
	}
	return &events.BuiltinToolCallEvent{ToolCallEvent: events.ToolCallEvent{
		ID:             id,
		Name:           name,
		Arguments:      args,
		VendorMetadata: map[string]any{serverBlockKey: block},
	}}
}

// serverToolResultEvent maps a server-tool result block to its result event; nil
// for any block that is not one.
func serverToolResultEvent(block map[string]any) *events.BuiltinToolResultEvent {
	typ, _ := block["type"].(string)
	var (
		name  string
		parts []events.Input
		meta  = map[string]any{}
	)
	content := block["content"]
	object, _ := content.(map[string]any)

	switch typ {
	case "web_search_tool_result":
		name = tools.WebSearchToolName
		if object != nil { // an error object; success is a list
			parts, meta = errorResult(object)
			break
		}
		results, _ := content.([]any)
		listed := make([]any, 0, len(results))
		for _, r := range results {
			res, ok := r.(map[string]any)
			if !ok || res["type"] != "web_search_result" {
				continue
			}
			url, _ := res["url"].(string)
			parts = append(parts, events.UrlInput{URL: url, Kind: events.BinaryBinary})
			// AGO-DEVIATION: Python puts title/page_age on the UrlInput's
			// metadata; AGo's UrlInput has none, so they ride in the result
			// metadata, one entry per part.
			listed = append(listed, map[string]any{"url": url, "title": res["title"], "page_age": res["page_age"]})
		}
		meta["count"] = len(results)
		meta["results"] = listed

	case "web_fetch_tool_result":
		name = tools.WebFetchToolName
		switch {
		case object == nil:
		case object["type"] == "web_fetch_result":
			url, _ := object["url"].(string)
			parts = append(parts, events.UrlInput{URL: url, Kind: events.BinaryBinary})
			doc, _ := object["content"].(map[string]any)
			source, _ := doc["source"].(map[string]any)
			data, _ := source["data"].(string)
			switch source["type"] {
			case "base64":
				if raw, err := base64.StdEncoding.DecodeString(data); err == nil {
					parts = append(parts, events.BinaryInput{Data: raw, MediaType: "application/pdf", Kind: events.BinaryDocument})
				}
			case "text":
				parts = append(parts, events.TextInput{Content: data})
			}
			meta["retrieved_at"] = object["retrieved_at"]
			meta["title"] = doc["title"]
		default:
			parts, meta = errorResult(object)
		}

	case "code_execution_tool_result", "bash_code_execution_tool_result":
		name = tools.CodeExecutionToolName
		switch {
		case object == nil:
		case strings.HasSuffix(fmt.Sprint(object["type"]), "_error"):
			parts, meta = errorResult(object)
		case object["type"] == "encrypted_code_execution_result":
			parts = fileParts(object["content"])
			meta["return_code"] = object["return_code"]
			meta["encrypted"] = true
		default:
			if s, _ := object["stdout"].(string); s != "" {
				parts = append(parts, events.TextInput{Content: s})
			}
			if s, _ := object["stderr"].(string); s != "" {
				parts = append(parts, events.TextInput{Content: s})
			}
			parts = append(parts, fileParts(object["content"])...)
			meta["return_code"] = object["return_code"]
		}

	case "text_editor_code_execution_tool_result":
		name = tools.CodeExecutionToolName
		switch {
		case object == nil:
		case strings.HasSuffix(fmt.Sprint(object["type"]), "_error"):
			parts, meta = errorResult(object)
			if msg, _ := object["error_message"].(string); msg != "" {
				parts = []events.Input{events.TextInput{Content: fmt.Sprintf("%v: %v: %s", object["type"], object["error_code"], msg)}}
			}
			meta["error_message"] = object["error_message"]
		case object["type"] == "text_editor_code_execution_view_result":
			if s, ok := object["content"].(string); ok {
				parts = []events.Input{events.TextInput{Content: s}}
			}
			for _, k := range []string{"file_type", "num_lines", "start_line", "total_lines"} {
				meta[k] = object[k]
			}
		case object["type"] == "text_editor_code_execution_create_result":
			meta["is_file_update"] = object["is_file_update"]
		case object["type"] == "text_editor_code_execution_str_replace_result":
			if lines, ok := object["lines"].([]any); ok {
				texts := make([]string, 0, len(lines))
				for _, l := range lines {
					texts = append(texts, fmt.Sprint(l))
				}
				parts = []events.Input{events.TextInput{Content: strings.Join(texts, "\n")}}
			}
			for _, k := range []string{"new_lines", "new_start", "old_lines", "old_start"} {
				meta[k] = object[k]
			}
		}

	case "tool_search_tool_result":
		name = tools.ToolSearchToolName
		switch {
		case object == nil:
		case strings.HasSuffix(fmt.Sprint(object["type"]), "_error"):
			parts, meta = errorResult(object)
		default:
			refs, _ := object["tool_references"].([]any)
			names := make([]string, 0, len(refs))
			for _, r := range refs {
				if ref, ok := r.(map[string]any); ok {
					names = append(names, fmt.Sprint(ref["tool_name"]))
				}
			}
			if len(names) > 0 {
				parts = []events.Input{events.TextInput{Content: strings.Join(names, ", ")}}
			}
			meta["tool_references"] = names
		}

	case "mcp_tool_result":
		// AGO-DEVIATION: Python does not map MCP connector results (see
		// serverToolName).
		name = tools.MCPServerToolName
		switch c := content.(type) {
		case string:
			parts = []events.Input{events.TextInput{Content: c}}
		case []any:
			for _, item := range c {
				if m, ok := item.(map[string]any); ok && m["type"] == "text" {
					text, _ := m["text"].(string)
					parts = append(parts, events.TextInput{Content: text})
				}
			}
		}
		if isErr, _ := block["is_error"].(bool); isErr {
			meta["error"] = true
		}

	default:
		return nil
	}

	meta[serverBlockKey] = block
	parent, _ := block["tool_use_id"].(string)
	return &events.BuiltinToolResultEvent{ToolResultEvent: events.ToolResultEvent{
		ParentID: parent,
		Name:     name,
		Result:   events.ToolResult{Parts: parts, Metadata: meta},
	}}
}

// errorResult renders a `*_error` result object as its one-line text and the
// error metadata (Python parity: "{type}: {error_code}").
func errorResult(object map[string]any) ([]events.Input, map[string]any) {
	text := fmt.Sprintf("%v: %v", object["type"], object["error_code"])
	return []events.Input{events.TextInput{Content: text}}, map[string]any{
		"error": true, "error_code": object["error_code"], "type": object["type"],
	}
}

// fileParts turns a code-execution result's output list into file references.
func fileParts(content any) []events.Input {
	items, _ := content.([]any)
	var parts []events.Input
	for _, it := range items {
		if m, ok := it.(map[string]any); ok {
			if id, _ := m["file_id"].(string); id != "" {
				parts = append(parts, events.FileIdInput{FileID: id})
			}
		}
	}
	return parts
}

// serverBlockOf returns the verbatim wire block a server-tool event carries, or
// nil for a builtin event of another provider (an OpenAI hosted item, say).
func serverBlockOf(ev events.Event) map[string]any {
	var block map[string]any
	switch e := ev.(type) {
	case events.BuiltinToolCallEvent:
		block, _ = e.VendorMetadata[serverBlockKey].(map[string]any)
	case events.BuiltinToolResultEvent:
		block, _ = e.Result.Metadata[serverBlockKey].(map[string]any)
	}
	if t, _ := block["type"].(string); t == "" {
		return nil
	}
	return block
}

// withoutNulls returns a deep copy of a JSON value with every null object
// field removed, which is what the block is replayed as (Python parity:
// model_dump(exclude_none=True)); the API may refuse a null it sent itself,
// page_age on a search result for one.
func withoutNulls(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			if val != nil {
				out[k] = withoutNulls(val)
			}
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = withoutNulls(val)
		}
		return out
	}
	return v
}
