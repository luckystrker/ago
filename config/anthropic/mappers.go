// History→wire, tool-schema→wire and usage normalization for the Anthropic
// Messages API. Parity with ag2/config/anthropic/mappers.py
// (convert_messages, tool_to_api, _tool_result_block, normalize_usage).

package anthropic

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/luckystrker/ago/config"
	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/tools"
)

// turn is one Messages-API message: {"role": ..., "content": string | []block}.
func turn(role string, content any) map[string]any {
	return map[string]any{"role": role, "content": content}
}

func textBlock(text string) map[string]any {
	return map[string]any{"type": "text", "text": text}
}

// ConvertMessages maps the event history to Messages-API turns. Only the
// conversational vocabulary is replayed (ModelRequest/DrainedModelRequest,
// ModelResponse, ToolResultsEvent and loose result events, CompactionSummary);
// everything else (usage telemetry, streaming chunks, standalone tool-call
// events) is skipped, Python parity. system is not built here: the client
// passes the prompt separately.
//
// Tool-use integrity: a tool_result whose tool_use was trimmed by a reduction
// policy is dropped (the API rejects an unmatched tool_result), and a tool_use
// whose result never arrived is dropped the same way (the API rejects tool_use
// ids without tool_result blocks immediately after) — Python parity, which
// keeps a compacted or partially-persisted transcript from failing the call.
func ConvertMessages(evs []events.Event) ([]map[string]any, error) {
	// Tool-use ids the transcript still shows.
	validToolIDs := map[string]bool{}
	for _, ev := range evs {
		if r, ok := events.AsValue(ev).(events.ModelResponse); ok {
			for _, call := range r.ToolCalls.Calls {
				validToolIDs[call.ID] = true
			}
		}
	}
	// Tool-use ids a result event answers. Loose ToolResultEvent/ToolErrorEvent
	// entries count too: the tool sends its own event and the executor adds the
	// wrapper, so a lost wrapper must not orphan the tool_use.
	resolvedToolIDs := map[string]bool{}
	for _, ev := range evs {
		switch e := events.AsValue(ev).(type) {
		case events.ToolResultsEvent:
			for _, r := range e.Results {
				if r.ParentID != "" {
					resolvedToolIDs[r.ParentID] = true
				}
			}
		case events.ToolResultEvent:
			if e.ParentID != "" {
				resolvedToolIDs[e.ParentID] = true
			}
		case events.ToolErrorEvent:
			if e.ParentID != "" {
				resolvedToolIDs[e.ParentID] = true
			}
		case events.ToolNotFoundEvent:
			if e.ParentID != "" {
				resolvedToolIDs[e.ParentID] = true
			}
		}
	}
	// Loose results already covered by a wrapper are not emitted twice (the
	// individual events reach the history before the wrapper that answers them).
	emitted := map[string]bool{}
	for _, ev := range evs {
		if e, ok := events.AsValue(ev).(events.ToolResultsEvent); ok {
			for _, r := range e.Results {
				if validToolIDs[r.ParentID] {
					emitted[r.ParentID] = true
				}
			}
		}
	}

	turns := make([]map[string]any, 0, len(evs))
	// openAssistant marks an assistant turn opened by a provider replay block,
	// so the ModelResponse that follows joins it instead of starting a second
	// assistant turn: the API wants thinking blocks before the turn's tool_use.
	openAssistant := false
	for _, ev := range evs {
		value := events.AsValue(ev)
		if block, ok := replayBlock(value); ok {
			turns, openAssistant = appendAssistantBlock(turns, openAssistant, block)
			continue
		}
		// A non-replay event only closes the open turn when it renders one:
		// telemetry between the blocks and the response (a UsageEvent, say) must
		// not split the assistant turn in two.
		open := openAssistant
		switch e := value.(type) {
		case events.ModelResponse:
			openAssistant = false
			content := make([]map[string]any, 0, 1+len(e.ToolCalls.Calls))
			if e.Message != nil {
				content = append(content, textBlock(e.Message.Content))
			}
			for _, call := range e.ToolCalls.Calls {
				if !resolvedToolIDs[call.ID] {
					// Orphan tool_use: the transcript lost its result. The
					// assistant text is still delivered.
					continue
				}
				input, err := decodeCallInput(call.Arguments)
				if err != nil {
					return nil, err
				}
				content = append(content, map[string]any{
					"type":  "tool_use",
					"id":    call.ID,
					"name":  call.Name,
					"input": input,
				})
			}
			if len(content) > 0 {
				if blocks, ok := openAssistantContent(turns, open); ok {
					turns[len(turns)-1]["content"] = append(blocks, content...)
				} else {
					turns = append(turns, turn("assistant", content))
				}
			}

		case events.ModelRequest:
			openAssistant = false
			t, err := userTurn(e.Parts)
			if err != nil {
				return nil, err
			}
			if t != nil {
				turns = append(turns, t)
			}

		case events.DrainedModelRequest:
			openAssistant = false
			t, err := userTurn(e.Parts)
			if err != nil {
				return nil, err
			}
			if t != nil {
				turns = append(turns, t)
			}

		case events.ToolResultsEvent:
			openAssistant = false
			blocks := make([]map[string]any, 0, len(e.Results))
			for _, r := range e.Results {
				// A result with no matching tool_use left in the transcript is
				// dropped only when the transcript has tool_use blocks at all
				// (a result-only history renders as-is, Python parity).
				if len(validToolIDs) > 0 && !validToolIDs[r.ParentID] {
					continue
				}
				b, err := toolResultBlock(r, false)
				if err != nil {
					return nil, err
				}
				blocks = append(blocks, b)
				emitted[r.ParentID] = true
			}
			if len(blocks) > 0 {
				turns = append(turns, turn("user", blocks))
			}

		case events.ToolResultEvent, events.ToolErrorEvent, events.ToolNotFoundEvent:
			// Fallback: a loose result event without a wrapper (the executor
			// batch was lost). Renders as its own user turn, Python parity.
			openAssistant = false
			var (
				parent string
				result events.ToolResult
				isErr  bool
			)
			switch r := e.(type) {
			case events.ToolResultEvent:
				parent, result = r.ParentID, r.Result
			case events.ToolErrorEvent:
				// The error flag keeps the model from reading the failure text
				// as a successful result.
				parent, result, isErr = r.ParentID, r.Result, true
			case events.ToolNotFoundEvent:
				parent, result, isErr = r.ParentID, r.Result, true
			}
			if parent == "" || !validToolIDs[parent] || emitted[parent] {
				continue
			}
			b, err := toolResultBlock(events.ToolResultEvent{ParentID: parent, Result: result}, isErr)
			if err != nil {
				return nil, err
			}
			emitted[parent] = true
			turns = append(turns, turn("user", []map[string]any{b}))

		case events.CompactionSummary:
			// A user turn keeps the summary visible and gives a valid opening turn.
			openAssistant = false
			turns = append(turns, turn("user", "[Summary of earlier conversation]\n"+e.Content))
		}
	}
	return turns, nil
}

// injectCacheBreakpoint marks the end of the last user turn as the end of the
// cacheable prefix, so every call after the first re-reads the conversation so
// far from the cache instead of re-billing it (Python parity:
// inject_cache_breakpoint). A string turn becomes a one-block turn; a block
// turn marks its last block, except thinking blocks, which cannot carry one.
//
// AGO-DEVIATION: a last block that already carries a caller-set cache_control
// (vendor_metadata, possibly with ttl "1h") keeps it; Python overwrites it with
// a plain 5-minute breakpoint, silently dropping the caller's choice.
func injectCacheBreakpoint(turns []map[string]any) {
	for i := len(turns) - 1; i >= 0; i-- {
		if turns[i]["role"] != "user" {
			continue
		}
		switch content := turns[i]["content"].(type) {
		case string:
			turns[i]["content"] = []map[string]any{{
				"type": "text", "text": content, "cache_control": map[string]any{"type": "ephemeral"},
			}}
		case []map[string]any:
			if n := len(content); n > 0 {
				last := content[n-1]
				_, marked := last["cache_control"]
				if t := last["type"]; t != "thinking" && t != "redacted_thinking" && !marked {
					last["cache_control"] = map[string]any{"type": "ephemeral"}
				}
			}
		}
		return
	}
}

// replayBlock renders one provider-native replay event as its assistant-turn
// block, in the original block order (Python parity: the
// AnthropicRedactedThinkingEvent / server-tool branch of convert_messages).
func replayBlock(ev events.Event) (map[string]any, bool) {
	switch e := ev.(type) {
	case ThinkingBlockEvent:
		return e.Block(), true
	case RedactedThinkingEvent:
		return e.Block(), true
	case events.BuiltinToolCallEvent, events.BuiltinToolResultEvent:
		// A server-tool call or result echoes back as the block the API sent;
		// the builtin events of another provider carry none and are skipped.
		if block := serverBlockOf(ev); block != nil {
			return withoutNulls(block).(map[string]any), true
		}
	}
	return nil, false
}

// openAssistantContent returns the content of the last turn when it is an
// assistant turn opened by a replay block.
func openAssistantContent(turns []map[string]any, open bool) ([]map[string]any, bool) {
	if !open || len(turns) == 0 {
		return nil, false
	}
	last := turns[len(turns)-1]
	if last["role"] != "assistant" {
		return nil, false
	}
	blocks, ok := last["content"].([]map[string]any)
	return blocks, ok
}

// appendAssistantBlock adds a replay block to the open assistant turn, or opens
// one. The API refuses a block that carries a cache breakpoint, so none is set.
func appendAssistantBlock(turns []map[string]any, open bool, block map[string]any) ([]map[string]any, bool) {
	if blocks, ok := openAssistantContent(turns, open); ok {
		turns[len(turns)-1]["content"] = append(blocks, block)
		return turns, true
	}
	return append(turns, turn("assistant", []map[string]any{block})), true
}

// userTurn maps one ModelRequest's parts to a user turn. A single text part
// collapses to a plain string (Python parity); an empty request yields no turn.
func userTurn(parts []events.Input) (map[string]any, error) {
	blocks := make([]map[string]any, 0, len(parts))
	for _, part := range parts {
		b, err := contentBlock(part)
		if err != nil {
			return nil, err
		}
		blocks = append(blocks, b)
	}
	if len(blocks) == 0 {
		return nil, nil
	}
	if len(blocks) == 1 && blocks[0]["type"] == "text" {
		return turn("user", blocks[0]["text"]), nil
	}
	return turn("user", blocks), nil
}

// contentBlock maps one input part to one wire content block. Shared by user
// turns and tool results. Unsupported parts fail fast (config.ErrUnsupportedInput),
// never silently dropped.
func contentBlock(part events.Input) (map[string]any, error) {
	switch p := events.AsValue(part).(type) {
	case events.TextInput:
		return textBlock(p.Content), nil
	case events.DataInput:
		// DataInput travels as its JSON encoding (spec §6 serializer parity).
		b, err := json.Marshal(p.Data)
		if err != nil {
			return nil, fmt.Errorf("anthropic: encoding DataInput: %w", err)
		}
		return textBlock(string(b)), nil
	case events.BinaryInput:
		switch p.Kind {
		case events.BinaryImage:
			b, err := imageBlock(p.MediaType, p.Data)
			if err != nil {
				return nil, err
			}
			// The API answers `image.citations: Extra inputs are not permitted`.
			if v, ok := p.VendorMetadata["citations"]; ok && v != nil {
				return nil, fmt.Errorf("%w: BinaryInput(image, citations) for the messages API", config.ErrUnsupportedInput)
			}
			if err := applyCacheControl(b, p.VendorMetadata); err != nil {
				return nil, err
			}
			return b, nil
		case events.BinaryDocument:
			b, err := documentBlock(p.MediaType, p.Data)
			if err != nil {
				return nil, err
			}
			if err := applyCacheControl(b, p.VendorMetadata); err != nil {
				return nil, err
			}
			if err := applyCitations(b, p.VendorMetadata); err != nil {
				return nil, err
			}
			return b, nil
		default:
			return nil, fmt.Errorf("%w: BinaryInput(%s) for the messages API", config.ErrUnsupportedInput, p.Kind)
		}
	case events.UrlInput:
		// A URL cannot be inlined as base64; it travels as a URL source
		// (images) or document source (documents/binaries) — the brief's
		// UrlInput mapping plus the Python document parity for the rest.
		switch p.Kind {
		case events.BinaryImage:
			return map[string]any{
				"type":   "image",
				"source": map[string]any{"type": "url", "url": p.URL},
			}, nil
		case events.BinaryDocument, events.BinaryBinary:
			return map[string]any{
				"type":   "document",
				"source": map[string]any{"type": "url", "url": p.URL},
			}, nil
		default:
			return nil, fmt.Errorf("%w: UrlInput(%s) for the messages API", config.ErrUnsupportedInput, p.Kind)
		}
	case events.FileIdInput:
		// An upload made through the Files API travels by reference. The
		// request then needs the files beta flag (see hasFileReferences).
		// AGO-DEVIATION: Python refuses a file uploaded through another
		// vendor's Files API; AGo's UploadedFile carries no provider tag, so
		// that check does not exist and the API answers a foreign file id.
		return fileIDBlock(p.FileID, p.Filename), nil
	default:
		return nil, fmt.Errorf("%w: %T for the messages API", config.ErrUnsupportedInput, part)
	}
}

// imageExtensions pick the block kind of a file reference: the Files API takes
// the same id for both, so the filename's extension decides (Python parity:
// _IMAGE_EXTENSIONS).
var imageExtensions = map[string]bool{".jpg": true, ".jpeg": true, ".png": true, ".gif": true, ".webp": true}

// fileIDBlock references an uploaded file: an image block when the filename
// has an image extension, a document block otherwise (Python parity:
// _file_id_block).
func fileIDBlock(fileID, filename string) map[string]any {
	kind := "document"
	if i := strings.LastIndex(filename, "."); i >= 0 && imageExtensions[strings.ToLower(filename[i:])] {
		kind = "image"
	}
	return map[string]any{
		"type":   kind,
		"source": map[string]any{"type": "file", "file_id": fileID},
	}
}

// hasFileReferences reports whether any request or tool result of the history
// references an uploaded file, which is what makes the request need the files
// beta flag (Python parity: has_file_id_references).
func hasFileReferences(evs []events.Event) bool {
	isFile := func(parts []events.Input) bool {
		for _, part := range parts {
			if _, ok := events.AsValue(part).(events.FileIdInput); ok {
				return true
			}
		}
		return false
	}
	for _, ev := range evs {
		switch e := events.AsValue(ev).(type) {
		case events.ModelRequest:
			if isFile(e.Parts) {
				return true
			}
		case events.DrainedModelRequest:
			if isFile(e.Parts) {
				return true
			}
		case events.ToolResultsEvent:
			for _, r := range e.Results {
				if isFile(r.Result.Parts) {
					return true
				}
			}
		case events.ToolResultEvent:
			if isFile(e.Result.Parts) {
				return true
			}
		case events.ToolErrorEvent:
			if isFile(e.Result.Parts) {
				return true
			}
		}
	}
	return false
}

// applyCacheControl copies the cache_control a caller set in vendor_metadata
// onto the block, in the shape the API accepts (Python parity: _cache_control).
// A missing or null value means the caller set none.
func applyCacheControl(block map[string]any, meta map[string]any) error {
	v, ok := meta["cache_control"]
	if !ok || v == nil {
		return nil
	}
	m, ok := v.(map[string]any)
	if !ok || m["type"] != "ephemeral" {
		return badVendorMetadata("cache_control", v)
	}
	out := map[string]any{"type": "ephemeral"}
	for key := range m {
		if key != "type" && key != "ttl" {
			return badVendorMetadata("cache_control", v)
		}
	}
	if ttl, ok := m["ttl"]; ok {
		s, ok := ttl.(string)
		if !ok || (s != "5m" && s != "1h") {
			return badVendorMetadata("cache_control", v)
		}
		out["ttl"] = s
	}
	block["cache_control"] = out
	return nil
}

// applyCitations copies the document citations config a caller set in
// vendor_metadata onto the block (Python parity: _citations). A missing or null
// value means the caller set none; an empty object is a value the API accepts.
func applyCitations(block map[string]any, meta map[string]any) error {
	v, ok := meta["citations"]
	if !ok || v == nil {
		return nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return badVendorMetadata("citations", v)
	}
	out := map[string]any{}
	for key := range m {
		if key != "enabled" {
			return badVendorMetadata("citations", v)
		}
	}
	if enabled, ok := m["enabled"]; ok {
		b, ok := enabled.(bool)
		if !ok {
			return badVendorMetadata("citations", v)
		}
		out["enabled"] = b
	}
	block["citations"] = out
	return nil
}

// badVendorMetadata refuses a vendor_metadata value the API would answer with a
// 400, naming the key and value (Python parity: the UnsupportedInputError text).
func badVendorMetadata(key string, value any) error {
	return fmt.Errorf("%w: BinaryInput(%s=%v) for the messages API", config.ErrUnsupportedInput, key, value)
}

// imageMediaTypes are the only inline image media types the API accepts; any
// other is answered with a 400, so it fails here first (Python parity).
var imageMediaTypes = map[string]bool{
	"image/jpeg": true,
	"image/png":  true,
	"image/gif":  true,
	"image/webp": true,
}

func imageBlock(mediaType string, data []byte) (map[string]any, error) {
	if !imageMediaTypes[mediaType] {
		return nil, fmt.Errorf("%w: BinaryInput(image, media_type=%s) for the messages API", config.ErrUnsupportedInput, mediaType)
	}
	return map[string]any{
		"type": "image",
		"source": map[string]any{
			"type":       "base64",
			"media_type": mediaType,
			"data":       base64.StdEncoding.EncodeToString(data),
		},
	}, nil
}

// documentBlock inlines a document: a PDF as base64, plain text as text. The
// API refuses base64 for anything but a PDF (Python parity).
func documentBlock(mediaType string, data []byte) (map[string]any, error) {
	switch mediaType {
	case "application/pdf":
		return map[string]any{
			"type": "document",
			"source": map[string]any{
				"type":       "base64",
				"media_type": "application/pdf",
				"data":       base64.StdEncoding.EncodeToString(data),
			},
		}, nil
	case "text/plain":
		if !utf8.Valid(data) {
			return nil, fmt.Errorf("%w: BinaryInput(document, media_type=text/plain, not UTF-8)", config.ErrUnsupportedInput)
		}
		return map[string]any{
			"type": "document",
			"source": map[string]any{
				"type":       "text",
				"media_type": "text/plain",
				"data":       string(data),
			},
		}, nil
	default:
		return nil, fmt.Errorf("%w: BinaryInput(document, media_type=%s) for the messages API", config.ErrUnsupportedInput, mediaType)
	}
}

// toolResultBlock renders one tool result as a tool_result block. A single
// text part collapses to a plain string content (Python parity).
func toolResultBlock(r events.ToolResultEvent, isError bool) (map[string]any, error) {
	blocks := make([]map[string]any, 0, len(r.Result.Parts))
	for _, part := range r.Result.Parts {
		b, err := contentBlock(part)
		if err != nil {
			return nil, err
		}
		blocks = append(blocks, b)
	}
	var content any = blocks
	if len(blocks) == 1 && blocks[0]["type"] == "text" {
		content = blocks[0]["text"]
	}
	block := map[string]any{
		"type":        "tool_result",
		"tool_use_id": r.ParentID,
		"content":     content,
	}
	if isError {
		block["is_error"] = true
	}
	return block, nil
}

// decodeCallInput parses replayed tool-call arguments into the JSON value the
// API expects in a tool_use block's input (Python parity:
// json.loads(call.arguments or "{}")); invalid JSON fails the call.
func decodeCallInput(arguments string) (any, error) {
	if strings.TrimSpace(arguments) == "" {
		return map[string]any{}, nil
	}
	var v any
	if err := json.Unmarshal([]byte(arguments), &v); err != nil {
		return nil, fmt.Errorf("anthropic: decoding tool call arguments: %w", err)
	}
	return v, nil
}

// outputConfig renders the request's native structured-output block from the
// response schema's JSON schema: {"format": {"type": "json_schema", "schema":
// ...}} (Python parity: response_proto_to_output_config). A schema without a
// JSON schema (a prompt-driven one) yields nil.
func outputConfig(schema map[string]any) map[string]any {
	if len(schema) == 0 {
		return nil
	}
	return map[string]any{"format": map[string]any{
		"type":   "json_schema",
		"schema": additionalPropertiesFalse(schema),
	}}
}

// additionalPropertiesFalse returns a copy of the schema with
// additionalProperties:false added to every object node that does not set it
// (the API requires it throughout output_config.format.schema).
func additionalPropertiesFalse(schema map[string]any) map[string]any {
	out := make(map[string]any, len(schema))
	for k, v := range schema {
		out[k] = v
	}
	if out["type"] == "object" {
		if _, ok := out["additionalProperties"]; !ok {
			out["additionalProperties"] = false
		}
	}
	for _, key := range []string{"properties", "$defs"} {
		if m, ok := out[key].(map[string]any); ok {
			sub := make(map[string]any, len(m))
			for name, v := range m {
				if node, ok := v.(map[string]any); ok {
					v = additionalPropertiesFalse(node)
				}
				sub[name] = v
			}
			out[key] = sub
		}
	}
	for _, key := range []string{"anyOf", "oneOf", "allOf"} {
		if items, ok := out[key].([]any); ok {
			sub := make([]any, len(items))
			for i, v := range items {
				if node, ok := v.(map[string]any); ok {
					v = additionalPropertiesFalse(node)
				}
				sub[i] = v
			}
			out[key] = sub
		}
	}
	if node, ok := out["items"].(map[string]any); ok {
		out["items"] = additionalPropertiesFalse(node)
	}
	return out
}

// --- tool schemas --------------------------------------------------------------

// ConvertTools maps the per-call tool schemas to the Messages-API tools array:
// function schemas become {"name","description","input_schema"}; the hosted
// types the ported Anthropic client can carry (web_search, code_execution,
// web_fetch, memory, mcp_server, tool_search) become their server-tool blocks;
// everything else fails fast with config.ErrUnsupportedTool.
func ConvertTools(tools []any) ([]map[string]any, error) {
	out := make([]map[string]any, 0, len(tools))
	for _, t := range tools {
		w, err := toolToAPI(t)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	// A web_fetch url_sources filter resolves its tool names against this
	// request's tools[] — checked against the mapped entries, so "declared"
	// means exactly what goes on the wire (Python parity: tools_to_api).
	if err := rejectUnknownURLSourceTools(tools, out); err != nil {
		return nil, err
	}
	return out, nil
}

// ExtractMCPServers renders the request's mcp_servers parameter from the hosted
// MCPServerToolSchema entries (Python parity: extract_mcp_servers; the Python
// client ships it as extra_body {"mcp_servers": [...]}). The tools[] mcp_toolset
// block produced by toolToAPI references the server by name, so a request with
// an MCP server needs both.
func ExtractMCPServers(schemas []any) []map[string]any {
	var servers []map[string]any
	for _, s := range schemas {
		t, ok := s.(tools.MCPServerToolSchema)
		if !ok {
			if p, isPtr := s.(*tools.MCPServerToolSchema); isPtr && p != nil {
				t = *p
			} else {
				continue
			}
		}
		server := map[string]any{"type": "url", "url": t.ServerURL, "name": t.ServerLabel}
		if t.AuthorizationToken != "" {
			server["authorization_token"] = t.AuthorizationToken
		}
		servers = append(servers, server)
	}
	return servers
}

func toolToAPI(tool any) (map[string]any, error) {
	switch t := tool.(type) {
	case tools.Schema:
		return functionToolToAPI(t)
	case *tools.Schema:
		if t == nil {
			return nil, fmt.Errorf("%w: nil function schema", config.ErrUnsupportedTool)
		}
		return functionToolToAPI(*t)
	case tools.ToolSchema:
		return hostedToolToAPI(t)
	default:
		return nil, fmt.Errorf("%w: unsupported schema type %T", config.ErrUnsupportedTool, tool)
	}
}

// hostedToolToAPI maps one hosted schema to its tools[] entry. Supported here:
// web_search, code_execution, web_fetch, memory, mcp_server and tool_search
// (Python parity: tool_to_api). file_search and image_generation are OpenAI
// Responses-only and fall through to the generic rejection; shell is rejected
// with the client-side explanation.
func hostedToolToAPI(t tools.ToolSchema) (map[string]any, error) {
	switch s := t.(type) {
	case tools.WebSearchToolSchema:
		return webSearchToolToAPI(s), nil
	case *tools.WebSearchToolSchema:
		if s == nil {
			return nil, nilSchemaErr(tools.WebSearchToolName)
		}
		return webSearchToolToAPI(*s), nil

	case tools.CodeExecutionToolSchema:
		return codeExecutionToAPI(s.Version), nil
	case *tools.CodeExecutionToolSchema:
		if s == nil {
			return nil, nilSchemaErr(tools.CodeExecutionToolName)
		}
		return codeExecutionToAPI(s.Version), nil

	case tools.WebFetchToolSchema:
		return webFetchToolToAPI(s)
	case *tools.WebFetchToolSchema:
		if s == nil {
			return nil, nilSchemaErr(tools.WebFetchToolName)
		}
		return webFetchToolToAPI(*s)

	case tools.MemoryToolSchema:
		// https://platform.claude.com/docs/en/agents-and-tools/tool-use/memory-tool
		return memoryToAPI(s.Version), nil
	case *tools.MemoryToolSchema:
		if s == nil {
			return nil, nilSchemaErr(tools.MemoryToolName)
		}
		return memoryToAPI(s.Version), nil

	case tools.MCPServerToolSchema:
		// https://platform.claude.com/docs/en/docs/agents-and-tools/mcp-connector
		// The one entry built by hand: the API ships no param type for an
		// mcp_toolset, so there is nothing to check this shape against. The
		// server definition itself travels in the request's mcp_servers
		// parameter (ExtractMCPServers).
		return mcpToolsetToAPI(s), nil
	case *tools.MCPServerToolSchema:
		if s == nil {
			return nil, nilSchemaErr(tools.MCPServerToolName)
		}
		return mcpToolsetToAPI(*s), nil

	case tools.ToolSearchToolSchema:
		return toolSearchToolToAPI(s), nil
	case *tools.ToolSearchToolSchema:
		if s == nil {
			return nil, nilSchemaErr(tools.ToolSearchToolName)
		}
		return toolSearchToolToAPI(*s), nil

	case tools.ShellToolSchema:
		return nil, shellRejected(t.SchemaType())
	case *tools.ShellToolSchema:
		if s == nil {
			return nil, nilSchemaErr(tools.ShellToolName)
		}
		return nil, shellRejected(s.SchemaType())
	}
	return nil, fmt.Errorf("%w: hosted tool %q for the messages API", config.ErrUnsupportedTool, t.SchemaType())
}

// shellRejected explains why a hosted shell schema cannot map to the Messages
// API (Python parity: the ShellToolSchema branch of tool_to_api): Anthropic's
// bash tool is client-side, the application executes the command.
func shellRejected(typeName string) error {
	return fmt.Errorf("%w: hosted tool %q for the messages API: Anthropic's bash tool is "+
		"client-side; run commands in the application (a sandboxed shell tool) and return the result",
		config.ErrUnsupportedTool, typeName)
}

// nilSchemaErr is the fail-fast for a typed nil hosted-schema pointer.
func nilSchemaErr(typeName string) error {
	return fmt.Errorf("%w: nil %s schema", config.ErrUnsupportedTool, typeName)
}

// codeExecutionToAPI builds the code_execution entry; an empty version resolves
// to the Python dataclass default (the Go zero value stands in for the default
// instance).
func codeExecutionToAPI(version string) map[string]any {
	if version == "" {
		version = tools.DefaultCodeExecutionVersion
	}
	// https://platform.claude.com/docs/en/agents-and-tools/tool-use/code-execution-tool
	return map[string]any{"type": version, "name": "code_execution"}
}

// memoryToAPI builds the memory entry; an empty version resolves to the Python
// dataclass default (see codeExecutionToAPI).
func memoryToAPI(version string) map[string]any {
	if version == "" {
		version = tools.MemoryVersion
	}
	// https://platform.claude.com/docs/en/agents-and-tools/tool-use/memory-tool
	return map[string]any{"type": version, "name": tools.MemoryToolName}
}

// webSearchToolToAPI builds the web_search entry. SearchContextSize is
// deliberately not sent: the Anthropic mapper ignores it (Python parity). The
// version literal replaces the newest param's on the way out. An empty version
// means the Go zero value, which stands in for the Python dataclass's default
// instance, so it resolves to the Python default literal.
func webSearchToolToAPI(t tools.WebSearchToolSchema) map[string]any {
	version := t.WebSearchVersion
	if version == "" {
		version = tools.DefaultWebSearchVersion
	}
	w := map[string]any{"type": version, "name": tools.WebSearchToolName}
	if t.MaxUses != nil {
		w["max_uses"] = *t.MaxUses
	}
	if t.UserLocation != nil {
		w["user_location"] = userLocationToAPI(t.UserLocation)
	}
	if t.AllowedDomains != nil {
		w["allowed_domains"] = t.AllowedDomains
	}
	if t.BlockedDomains != nil {
		w["blocked_domains"] = t.BlockedDomains
	}
	return w
}

// userLocationToAPI tags the location the way the API discriminates it; only
// the fields that were set travel.
func userLocationToAPI(l *tools.UserLocation) map[string]any {
	loc := map[string]any{"type": "approximate"}
	if l.City != nil {
		loc["city"] = *l.City
	}
	if l.Region != nil {
		loc["region"] = *l.Region
	}
	if l.Country != nil {
		loc["country"] = *l.Country
	}
	if l.Timezone != nil {
		loc["timezone"] = *l.Timezone
	}
	return loc
}

// Version literals an option first arrived in (Python parity:
// WEB_FETCH_USE_CACHE_SINCE / WEB_FETCH_RESPONSE_INCLUSION_SINCE; the literal
// formats compare lexicographically).
const (
	webFetchUseCacheSince          = "web_fetch_20260309"
	webFetchResponseInclusionSince = "web_fetch_20260318"
)

// webFetchToolToAPI builds the web_fetch entry, refusing an option the selected
// version predates (Python parity: _reject_unsupported_web_fetch_options) — a
// silently ignored option turns a caching or payload decision into a no-op. An
// empty version resolves to the Python dataclass default (the Go zero value
// stands in for the default instance).
func webFetchToolToAPI(t tools.WebFetchToolSchema) (map[string]any, error) {
	if t.WebFetchVersion == "" {
		t.WebFetchVersion = tools.DefaultWebFetchVersion
	}
	if t.UseCache != nil && t.WebFetchVersion < webFetchUseCacheSince {
		return nil, fmt.Errorf("%w: web_fetch option `use_cache` is not available on `%s`: it arrived in "+
			"`%s`. Pass version `%s` or later, or drop the option",
			config.ErrUnsupportedTool, t.WebFetchVersion, webFetchUseCacheSince, webFetchUseCacheSince)
	}
	if t.ResponseInclusion != "" && t.WebFetchVersion < webFetchResponseInclusionSince {
		return nil, fmt.Errorf("%w: web_fetch option `response_inclusion` is not available on `%s`: it arrived in "+
			"`%s`. Pass version `%s` or later, or drop the option",
			config.ErrUnsupportedTool, t.WebFetchVersion, webFetchResponseInclusionSince, webFetchResponseInclusionSince)
	}
	w := map[string]any{"type": t.WebFetchVersion, "name": tools.WebFetchToolName}
	if t.MaxUses != nil {
		w["max_uses"] = *t.MaxUses
	}
	if t.AllowedDomains != nil {
		w["allowed_domains"] = t.AllowedDomains
	}
	if t.BlockedDomains != nil {
		w["blocked_domains"] = t.BlockedDomains
	}
	if t.Citations != nil {
		w["citations"] = map[string]any{"enabled": *t.Citations}
	}
	if t.MaxContentTokens != nil {
		w["max_content_tokens"] = *t.MaxContentTokens
	}
	if t.Strict != nil {
		w["strict"] = *t.Strict
	}
	if t.UseCache != nil {
		w["use_cache"] = *t.UseCache
	}
	if t.ResponseInclusion != "" {
		w["response_inclusion"] = t.ResponseInclusion
	}
	if t.UrlSources != nil {
		if sources := urlSourcesToAPI(t.UrlSources); len(sources) > 0 {
			w["url_sources"] = sources
		}
	}
	return w, nil
}

// urlSourcesToAPI maps the sources that were set; a source left unset is the
// API's default, not a decision to send (Python parity: _url_sources_to_api).
func urlSourcesToAPI(u *tools.UrlSources) map[string]any {
	out := map[string]any{}
	if u.UserInput != "" {
		out["user_input"] = unfilteredSourceToAPI(u.UserInput)
	}
	if u.ClientToolResults != nil {
		out["client_tool_results"] = toolSourceToAPI(u.ClientToolResults)
	}
	if u.ServerToolResults != nil {
		out["server_tool_results"] = toolSourceToAPI(u.ServerToolResults)
	}
	return out
}

// unfilteredSourceToAPI tags a source that names no tools. A value outside the
// all/none pair is sent as written: this is a policy control, and a 400 naming
// the typo beats quietly substituting the closed policy (Python parity).
func unfilteredSourceToAPI(source string) map[string]any {
	return map[string]any{"type": source}
}

// toolSourceToAPI tags one url_sources entry the way the API discriminates it;
// a named filter refers to its tools as tool_reference entries.
func toolSourceToAPI(s *tools.ToolSources) map[string]any {
	w := map[string]any{"type": s.Kind}
	if len(s.Tools) > 0 {
		refs := make([]map[string]any, 0, len(s.Tools))
		for _, n := range s.Tools {
			refs = append(refs, map[string]any{"type": "tool_reference", "name": n})
		}
		w["tools"] = refs
	}
	return w
}

// mcpToolsetToAPI builds the tools[] mcp_toolset entry: the label names the
// server (defined in the request's mcp_servers parameter), and allowed/blocked
// tools become per-tool enable flags (Python parity).
func mcpToolsetToAPI(t tools.MCPServerToolSchema) map[string]any {
	w := map[string]any{"type": "mcp_toolset", "mcp_server_name": t.ServerLabel}
	if t.AllowedTools != nil {
		w["default_config"] = map[string]any{"enabled": false}
		configs := map[string]any{}
		for _, n := range t.AllowedTools {
			configs[n] = map[string]any{"enabled": true}
		}
		w["configs"] = configs
	}
	if t.BlockedTools != nil {
		configs, _ := w["configs"].(map[string]any)
		if configs == nil {
			configs = map[string]any{}
		}
		for _, n := range t.BlockedTools {
			configs[n] = map[string]any{"enabled": false}
		}
		w["configs"] = configs
	}
	return w
}

// toolSearchToolToAPI builds the tool_search entry from the variant's own param:
// the name is the variant id (the API rejects a generic "tool_search" and this
// is the name it puts on the server_tool_use block it sends back). A mode other
// than bm25 maps to the regex variant (Python parity).
func toolSearchToolToAPI(t tools.ToolSearchToolSchema) map[string]any {
	if t.Mode == "bm25" {
		return map[string]any{"type": "tool_search_tool_bm25_20251119", "name": "tool_search_tool_bm25"}
	}
	return map[string]any{"type": "tool_search_tool_regex_20251119", "name": "tool_search_tool_regex"}
}

// rejectUnknownURLSourceTools refuses a web_fetch url_sources filter naming a
// tool this request does not declare (Python parity:
// _reject_unknown_url_source_tools): the API resolves those names against this
// request's tools[], so an absent name is a policy that silently covers nothing.
func rejectUnknownURLSourceTools(schemas []any, mapped []map[string]any) error {
	declared := declaredToolNames(mapped)
	for _, s := range schemas {
		t, ok := s.(tools.WebFetchToolSchema)
		if !ok {
			if p, isPtr := s.(*tools.WebFetchToolSchema); isPtr && p != nil {
				t = *p
			} else {
				continue
			}
		}
		if t.UrlSources == nil {
			continue
		}
		for _, source := range []*tools.ToolSources{t.UrlSources.ClientToolResults, t.UrlSources.ServerToolResults} {
			if source == nil || (source.Kind != "only" && source.Kind != "except") {
				continue
			}
			for _, name := range source.Tools {
				if !declared[name] {
					return fmt.Errorf("%w: web_fetch `url_sources` names `%s`, which this request does not "+
						"declare. Give the agent that tool, or drop the name from `url_sources`",
						config.ErrUnsupportedTool, name)
				}
			}
		}
	}
	return nil
}

// declaredToolNames collects every tool name the mapped request puts on the
// wire: tools[] entries carry "name"; an mcp_toolset carries no name of its own
// but names the tools its configs switch on (Python parity:
// _declared_tool_names).
func declaredToolNames(mapped []map[string]any) map[string]bool {
	names := map[string]bool{}
	for _, entry := range mapped {
		if n, ok := entry["name"].(string); ok {
			names[n] = true
		}
		configs, _ := entry["configs"].(map[string]any)
		for n, c := range configs {
			if enabled, ok := c.(map[string]any)["enabled"].(bool); ok && enabled {
				names[n] = true
			}
		}
	}
	return names
}

func functionToolToAPI(t tools.Schema) (map[string]any, error) {
	w := map[string]any{
		"name":         t.Name,
		"input_schema": ensureObjectSchema(t.Parameters),
	}
	if t.Description != "" {
		w["description"] = t.Description
	}
	if t.DeferLoading {
		w["defer_loading"] = true
	}
	return w, nil
}

// ensureObjectSchema coerces the parameters into the object form the API
// requires (Python parity: _ensure_object_schema): type "object" and a
// properties map, leaving every other key untouched.
func ensureObjectSchema(params map[string]any) map[string]any {
	out := make(map[string]any, len(params)+2)
	for k, v := range params {
		out[k] = v
	}
	out["type"] = "object"
	if _, ok := out["properties"]; !ok {
		out["properties"] = map[string]any{}
	}
	return out
}

// --- usage -----------------------------------------------------------------------

// wireUsage is the usage object of a Message (or of the message_start /
// message_delta stream events).
type wireUsage struct {
	InputTokens              *int64 `json:"input_tokens"`
	OutputTokens             *int64 `json:"output_tokens"`
	CacheCreationInputTokens *int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     *int64 `json:"cache_read_input_tokens"`
	// CacheCreation carries the per-lifetime split of the cache writes; only
	// the 1-hour part maps onto Usage (the 5-minute rest is the total minus it).
	CacheCreation       *wireCacheCreation `json:"cache_creation"`
	OutputTokensDetails *struct {
		ThinkingTokens *int64 `json:"thinking_tokens"`
	} `json:"output_tokens_details"`
}

// wireCacheCreation is Anthropic's per-retention cache-write breakdown.
type wireCacheCreation struct {
	Ephemeral1hInputTokens *int64 `json:"ephemeral_1h_input_tokens"`
}

// NormalizeUsage maps the native usage keys onto events.Usage:
// input_tokens → PromptTokens, output_tokens → CompletionTokens, the cache
// counters as-is, cache_creation.ephemeral_1h_input_tokens →
// CacheCreationExtendedInputTokens (the extended-retention subset of the cache
// writes), and output_tokens_details.thinking_tokens → ThinkingTokens (a
// decomposition of output_tokens, not an addition — Python mapper parity).
// TotalTokens is the input+output sum when either was reported.
func NormalizeUsage(w wireUsage) events.Usage {
	var u events.Usage
	u.PromptTokens = w.InputTokens
	u.CompletionTokens = w.OutputTokens
	if w.InputTokens != nil || w.OutputTokens != nil {
		total := int64(0)
		if w.InputTokens != nil {
			total += *w.InputTokens
		}
		if w.OutputTokens != nil {
			total += *w.OutputTokens
		}
		u.TotalTokens = &total
	}
	u.CacheReadInputTokens = w.CacheReadInputTokens
	u.CacheCreationInputTokens = w.CacheCreationInputTokens
	if w.CacheCreation != nil {
		u.CacheCreationExtendedInputTokens = w.CacheCreation.Ephemeral1hInputTokens
	}
	if w.OutputTokensDetails != nil {
		u.ThinkingTokens = w.OutputTokensDetails.ThinkingTokens
	}
	return u
}

// MapStopReason renders the API's stop_reason in the OpenAI-style vocabulary
// the rest of AGo speaks (end_turn → stop, max_tokens → length, tool_use
// verbatim); unknown reasons pass through unchanged.
func MapStopReason(reason string) string {
	switch reason {
	case "end_turn":
		return "stop"
	case "tool_use":
		return "tool_use"
	case "max_tokens":
		return "length"
	default:
		return reason
	}
}
