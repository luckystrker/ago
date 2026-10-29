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

	"ago/config"
	"ago/events"
	"ago/tools"
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
	for _, ev := range evs {
		switch e := events.AsValue(ev).(type) {
		case events.ModelResponse:
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
				turns = append(turns, turn("assistant", content))
			}

		case events.ModelRequest:
			t, err := userTurn(e.Parts)
			if err != nil {
				return nil, err
			}
			if t != nil {
				turns = append(turns, t)
			}

		case events.DrainedModelRequest:
			t, err := userTurn(e.Parts)
			if err != nil {
				return nil, err
			}
			if t != nil {
				turns = append(turns, t)
			}

		case events.ToolResultsEvent:
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
			turns = append(turns, turn("user", "[Summary of earlier conversation]\n"+e.Content))
		}
	}
	return turns, nil
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
			return imageBlock(p.MediaType, p.Data)
		case events.BinaryDocument:
			return documentBlock(p.MediaType, p.Data)
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
		// AGO-DEVIATION: Python references Files-API uploads (beta header +
		// files client); AGo has no Files client, so the reference fails fast.
		return nil, fmt.Errorf("%w: FileIdInput for the messages API", config.ErrUnsupportedInput)
	default:
		return nil, fmt.Errorf("%w: %T for the messages API", config.ErrUnsupportedInput, part)
	}
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

// --- tool schemas --------------------------------------------------------------

// ConvertTools maps the per-call tool schemas to the Messages-API tools array:
// function schemas become {"name","description","input_schema"}; hosted tools
// and anything else fail fast with config.ErrUnsupportedTool.
func ConvertTools(tools []any) ([]map[string]any, error) {
	out := make([]map[string]any, 0, len(tools))
	for _, t := range tools {
		w, err := toolToAPI(t)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, nil
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
		// Hosted/builtin schemas (web_search, code_execution, ...) have no AGo
		// port yet; fail fast instead of silently dropping, Python parity.
		return nil, fmt.Errorf("%w: hosted tool %q for the messages API", config.ErrUnsupportedTool, t.SchemaType())
	default:
		return nil, fmt.Errorf("%w: unsupported schema type %T", config.ErrUnsupportedTool, tool)
	}
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
	OutputTokensDetails      *struct {
		ThinkingTokens *int64 `json:"thinking_tokens"`
	} `json:"output_tokens_details"`
}

// NormalizeUsage maps the native usage keys onto events.Usage:
// input_tokens → PromptTokens, output_tokens → CompletionTokens, the cache
// counters as-is, and output_tokens_details.thinking_tokens → ThinkingTokens
// (a decomposition of output_tokens, not an addition — Python mapper parity).
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
