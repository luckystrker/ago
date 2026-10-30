package gemini

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"path"
	"strings"

	"ago/config"
	agocontext "ago/context"
	"ago/events"
	"ago/internal/uuid"
	"ago/tools"
)

// --- history → contents -------------------------------------------------------

// wireContent is one Gemini Content: a role plus its parts.
type wireContent struct {
	Role  string           `json:"role"`
	Parts []map[string]any `json:"parts"`
}

// mimeByExtension infers a MIME type from a URL path extension (Python parity:
// the gemini mapper's _URL_EXTENSION_TO_MIME; entries Gemini accepts as file data).
var mimeByExtension = map[string]string{
	".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".png": "image/png", ".gif": "image/gif",
	".webp": "image/webp",
	".wav":  "audio/wav", ".mp3": "audio/mpeg", ".ogg": "audio/ogg", ".flac": "audio/flac",
	".aac": "audio/aac", ".aiff": "audio/aiff", ".aif": "audio/aiff",
	".mp4": "video/mp4", ".webm": "video/webm", ".mov": "video/quicktime",
	".mkv": "video/x-matroska", ".mpeg": "video/mpeg", ".mpg": "video/mpeg",
	".flv": "video/x-flv", ".wmv": "video/x-ms-wmv", ".3gp": "video/3gpp",
	".pdf": "application/pdf", ".txt": "text/plain", ".html": "text/html",
	".csv": "text/csv", ".json": "application/json", ".xml": "text/xml",
	".md": "text/markdown",
}

// mimeFromURL guesses the MIME type of a URL from its path extension ("" if unknown).
func mimeFromURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return mimeByExtension[strings.ToLower(path.Ext(u.Path))]
}

// buildContents converts the event history into Gemini contents (Python parity:
// gemini/mappers.py convert_messages). Events outside the conversational
// transcript (usage events, streaming chunks, human traffic) are skipped.
func buildContents(evs []events.Event) ([]wireContent, error) {
	var contents []wireContent
	for _, ev := range evs {
		switch e := events.AsValue(ev).(type) {
		case events.ModelResponse:
			var parts []map[string]any
			if e.Message != nil && e.Message.Content != "" {
				parts = append(parts, map[string]any{"text": e.Message.Content})
			}
			for _, call := range e.ToolCalls.Calls {
				args, err := parseCallArguments(call.Arguments)
				if err != nil {
					return nil, err
				}
				parts = append(parts, map[string]any{
					"functionCall": map[string]any{"name": call.Name, "args": args},
				})
			}
			if len(parts) > 0 {
				contents = append(contents, wireContent{Role: "model", Parts: parts})
			}

		case events.ToolResultsEvent:
			parts := make([]map[string]any, 0, len(e.Results))
			for _, r := range e.Results {
				// Text parts collapse into response.result: a single string, or a
				// list when the result carries several (Python parity).
				var texts []string
				for _, part := range r.Result.Parts {
					switch p := events.AsValue(part).(type) {
					case events.TextInput:
						texts = append(texts, p.Content)
					case events.DataInput:
						b, err := json.Marshal(p.Data)
						if err != nil {
							return nil, fmt.Errorf("gemini: encoding DataInput tool result: %w", err)
						}
						texts = append(texts, string(b))
					default:
						return nil, fmt.Errorf("%w: %T in a gemini tool result", config.ErrUnsupportedInput, part)
					}
				}
				var resultValue any
				switch len(texts) {
				case 0:
					resultValue = map[string]any{}
				case 1:
					resultValue = texts[0]
				default:
					resultValue = texts
				}
				resp := map[string]any{}
				if len(texts) > 0 {
					resp["result"] = resultValue
				}
				parts = append(parts, map[string]any{
					"functionResponse": map[string]any{
						"name":     r.Name,
						"response": resp,
					},
				})
			}
			contents = append(contents, wireContent{Role: "user", Parts: parts})

		case events.ModelRequest:
			parts, err := requestParts(e.Parts)
			if err != nil {
				return nil, err
			}
			if len(parts) > 0 {
				contents = append(contents, wireContent{Role: "user", Parts: parts})
			}

		case events.DrainedModelRequest:
			parts, err := requestParts(e.Parts)
			if err != nil {
				return nil, err
			}
			if len(parts) > 0 {
				contents = append(contents, wireContent{Role: "user", Parts: parts})
			}

		case events.CompactionSummary:
			// A user turn keeps the summary visible and gives a valid opening turn.
			contents = append(contents, wireContent{Role: "user", Parts: []map[string]any{
				{"text": "[Summary of earlier conversation]\n" + e.Content},
			}})
		}
	}
	return contents, nil
}

// requestParts maps one ModelRequest's parts to Gemini parts.
func requestParts(parts []events.Input) ([]map[string]any, error) {
	out := make([]map[string]any, 0, len(parts))
	for _, part := range parts {
		switch p := events.AsValue(part).(type) {
		case events.TextInput:
			out = append(out, map[string]any{"text": p.Content})
		case events.DataInput:
			// Python encodes DataInput through the serializer as text; AGo uses
			// its JSON encoding (docs/specs/config-llm.md §6).
			b, err := json.Marshal(p.Data)
			if err != nil {
				return nil, fmt.Errorf("gemini: encoding DataInput: %w", err)
			}
			out = append(out, map[string]any{"text": string(b)})
		case events.BinaryInput:
			// AGO-DEVIATION: vendor_metadata (media_resolution, video_metadata,
			// display_name) has no raw-REST mapping in this wave and is dropped.
			out = append(out, map[string]any{
				"inlineData": map[string]any{
					"mimeType": p.MediaType,
					"data":     base64.StdEncoding.EncodeToString(p.Data),
				},
			})
		case events.UrlInput:
			fd := map[string]any{"fileUri": p.URL}
			if mt := mimeFromURL(p.URL); mt != "" {
				fd["mimeType"] = mt
			}
			out = append(out, map[string]any{"fileData": fd})
		case events.FileIdInput:
			// AGO-DEVIATION: Python resolves file references through the Gemini
			// Files API; this port has no Files client, so they fail fast.
			return nil, fmt.Errorf("%w: FileIdInput for the generateContent API", config.ErrUnsupportedInput)
		default:
			return nil, fmt.Errorf("%w: %T for the generateContent API", config.ErrUnsupportedInput, part)
		}
	}
	return out, nil
}

// parseCallArguments turns a ToolCallEvent's raw arguments JSON into the args
// object the functionCall wire part expects (Python parity: json.loads or {}).
func parseCallArguments(arguments string) (map[string]any, error) {
	if strings.TrimSpace(arguments) == "" {
		return map[string]any{}, nil
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(arguments), &m); err != nil {
		return nil, fmt.Errorf("gemini: invalid tool call arguments %q: %w", arguments, err)
	}
	if m == nil {
		m = map[string]any{}
	}
	return m, nil
}

// --- tool schemas ---------------------------------------------------------------

// buildTools renders the call's tool schemas as Gemini tools: function schemas
// become one functionDeclarations block; the hosted types the ported Gemini
// client can carry (web_search, web_fetch, code_execution, file_search) become
// one server-side tool block each, after the function declarations (Python
// parity: build_tools). Everything else fails fast (config.ErrUnsupportedTool).
//
// AGO-DEVIATION: Python also builds a ToolConfig for mixed requests
// (build_tool_config: include_server_side_tool_invocations) and replays the
// server-side invocations from the response (GeminiServerToolCallEvent); the
// AGo client does not parse grounding metadata, so no ToolConfig is emitted and
// grounding travels only as ordinary response text (see docs/DECISIONS.md).
func buildTools(schemas []any) ([]map[string]any, error) {
	declarations := make([]map[string]any, 0, len(schemas))
	var extras []map[string]any
	for _, schema := range schemas {
		switch t := schema.(type) {
		case tools.Schema:
			declarations = append(declarations, functionDeclaration(t))
		case *tools.Schema:
			if t == nil {
				return nil, fmt.Errorf("%w: nil function schema", config.ErrUnsupportedTool)
			}
			declarations = append(declarations, functionDeclaration(*t))
		case tools.ToolSchema:
			entries, err := hostedToolsToAPI(t)
			if err != nil {
				return nil, err
			}
			extras = append(extras, entries...)
		default:
			return nil, fmt.Errorf("%w: unsupported schema type %T", config.ErrUnsupportedTool, schema)
		}
	}
	if len(declarations) == 0 && len(extras) == 0 {
		return nil, nil
	}
	out := make([]map[string]any, 0, 1+len(extras))
	if len(declarations) > 0 {
		out = append(out, map[string]any{"functionDeclarations": declarations})
	}
	return append(out, extras...), nil
}

// hostedToolsToAPI renders one hosted schema as the Gemini tools[] entries it
// maps to (zero or one). Wire keys follow the google-genai SDK's camelCase REST
// aliases — the shape the ported Python client actually puts on the wire.
func hostedToolsToAPI(t tools.ToolSchema) ([]map[string]any, error) {
	switch s := t.(type) {
	case tools.WebSearchToolSchema:
		return []map[string]any{{"googleSearch": googleSearch(s)}}, nil
	case *tools.WebSearchToolSchema:
		if s == nil {
			return nil, nilSchemaErr(tools.WebSearchToolName)
		}
		return []map[string]any{{"googleSearch": googleSearch(*s)}}, nil

	case tools.WebFetchToolSchema:
		// Gemini ignores every web_fetch field and enables its url_context tool
		// (Python parity).
		return []map[string]any{{"urlContext": map[string]any{}}}, nil
	case *tools.WebFetchToolSchema:
		if s == nil {
			return nil, nilSchemaErr(tools.WebFetchToolName)
		}
		return []map[string]any{{"urlContext": map[string]any{}}}, nil

	case tools.CodeExecutionToolSchema:
		return []map[string]any{{"codeExecution": map[string]any{}}}, nil
	case *tools.CodeExecutionToolSchema:
		if s == nil {
			return nil, nilSchemaErr(tools.CodeExecutionToolName)
		}
		return []map[string]any{{"codeExecution": map[string]any{}}}, nil

	case tools.FileSearchToolSchema:
		w, err := fileSearchToAPI(s)
		if err != nil {
			return nil, err
		}
		return []map[string]any{{"fileSearch": w}}, nil
	case *tools.FileSearchToolSchema:
		if s == nil {
			return nil, nilSchemaErr(tools.FileSearchToolName)
		}
		w, err := fileSearchToAPI(*s)
		if err != nil {
			return nil, err
		}
		return []map[string]any{{"fileSearch": w}}, nil
	}
	return nil, fmt.Errorf("%w: hosted tool %q for the generateContent API", config.ErrUnsupportedTool, t.SchemaType())
}

// googleSearch maps the web_search fields Gemini honours: only the blocked
// domains travel, as excludeDomains (Python parity: exclude_domains=blocked or
// None — an empty list is not sent).
func googleSearch(t tools.WebSearchToolSchema) map[string]any {
	g := map[string]any{}
	if len(t.BlockedDomains) > 0 {
		g["excludeDomains"] = t.BlockedDomains
	}
	return g
}

// fileSearchToAPI maps file_search over the Gemini file-search stores.
// vector_store_ids / filters are OpenAI-only and rejected here (Python parity:
// UnsupportedToolError without store_names).
func fileSearchToAPI(t tools.FileSearchToolSchema) (map[string]any, error) {
	if len(t.StoreNames) == 0 {
		return nil, fmt.Errorf("%w: hosted tool %q for the generateContent API requires store_names "+
			"(vector_store_ids and filters are OpenAI-only)", config.ErrUnsupportedTool, t.SchemaType())
	}
	w := map[string]any{"fileSearchStoreNames": t.StoreNames}
	if t.MaxNumResults != nil {
		w["topK"] = *t.MaxNumResults
	}
	if t.MetadataFilter != "" {
		w["metadataFilter"] = t.MetadataFilter
	}
	return w, nil
}

// nilSchemaErr is the fail-fast for a typed nil hosted-schema pointer.
func nilSchemaErr(typeName string) error {
	return fmt.Errorf("%w: nil %s schema", config.ErrUnsupportedTool, typeName)
}

func functionDeclaration(t tools.Schema) map[string]any {
	decl := map[string]any{
		"name":       t.Name,
		"parameters": geminiParameters(t.Parameters),
	}
	if t.Description != "" {
		decl["description"] = t.Description
	}
	return decl
}

// geminiParameters normalizes a function-parameters schema for Gemini:
// parameterless (or type-less) schemas become the empty object schema, and
// additionalProperties is stripped everywhere — the API rejects it inside
// anyOf/oneOf branches and does not enforce it anyway (Python parity:
// _ensure_object_schema + _strip_additional_properties).
func geminiParameters(params map[string]any) map[string]any {
	rawType := ""
	if t, ok := params["type"].(string); ok {
		rawType = strings.ToLower(t)
	}
	if len(params) == 0 || rawType == "" || rawType == "null" || rawType == "none" {
		return map[string]any{"type": "object", "properties": map[string]any{}}
	}
	stripped, ok := stripAdditionalProperties(params).(map[string]any)
	if !ok { // unreachable: stripping a map yields a map
		return map[string]any{"type": "object", "properties": map[string]any{}}
	}
	return stripped
}

func stripAdditionalProperties(node any) any {
	switch v := node.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, val := range v {
			if k == "additionalProperties" {
				continue
			}
			out[k] = stripAdditionalProperties(val)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, val := range v {
			out[i] = stripAdditionalProperties(val)
		}
		return out
	default:
		return node
	}
}

// --- response parsing -------------------------------------------------------------

type genPart struct {
	Text       string `json:"text"`
	Thought    bool   `json:"thought"`
	InlineData *struct {
		MimeType string `json:"mimeType"`
		Data     string `json:"data"`
	} `json:"inlineData"`
	FunctionCall *struct {
		ID   string         `json:"id"`
		Name string         `json:"name"`
		Args map[string]any `json:"args"`
	} `json:"functionCall"`
}

type genUsage struct {
	PromptTokenCount        *int64 `json:"promptTokenCount"`
	CandidatesTokenCount    *int64 `json:"candidatesTokenCount"`
	TotalTokenCount         *int64 `json:"totalTokenCount"`
	ThoughtsTokenCount      *int64 `json:"thoughtsTokenCount"`
	ToolUsePromptTokenCount *int64 `json:"toolUsePromptTokenCount"`
	CachedContentTokenCount *int64 `json:"cachedContentTokenCount"`
}

type genResponse struct {
	Candidates []struct {
		Content *struct {
			Parts []genPart `json:"parts"`
			Role  string    `json:"role"`
		} `json:"content"`
		FinishReason string `json:"finishReason"`
	} `json:"candidates"`
	UsageMetadata *genUsage `json:"usageMetadata"`
	ModelVersion  string    `json:"modelVersion"`
	ResponseID    string    `json:"responseId"`
}

// normalizeUsage maps Gemini usageMetadata onto events.Usage.
//
// AGO-DEVIATION: the porting brief maps candidatesTokenCount straight to
// CompletionTokens and keeps thoughtsTokenCount as ThinkingTokens only; the
// Python mapper additionally folds thinking into completion_tokens so that
// prompt + completion == total. Tool-use prompt tokens are added into the
// prompt (Python parity), and cachedContentTokenCount lands on
// CacheReadInputTokens (Python parity).
func normalizeUsage(m *genUsage) events.Usage {
	var u events.Usage
	if m == nil {
		return u
	}
	u.PromptTokens = m.PromptTokenCount
	if m.ToolUsePromptTokenCount != nil {
		// Tool outputs billed as prompt are counted apart by the API but belong
		// in the prompt total: leaving them out puts prompt+completion below
		// totalTokenCount.
		p := int64(0)
		if m.PromptTokenCount != nil {
			p = *m.PromptTokenCount
		}
		p += *m.ToolUsePromptTokenCount
		u.PromptTokens = &p
	}
	u.CompletionTokens = m.CandidatesTokenCount
	u.TotalTokens = m.TotalTokenCount
	u.ThinkingTokens = m.ThoughtsTokenCount
	u.CacheReadInputTokens = m.CachedContentTokenCount
	return u
}

// genAccumulator collects the parts of one response (whole or streamed).
type genAccumulator struct {
	content strings.Builder
	calls   []events.ToolCallEvent
	files   []events.BinaryResult
}

// addPart folds one content part into the accumulator. When stream is true,
// text parts also emit ModelMessageChunk on the conversation (Python parity:
// the streaming loop); thought parts always emit ModelReasoning.
func (a *genAccumulator) addPart(p genPart, conv *agocontext.Context, stream bool) error {
	switch {
	case p.Thought && p.Text != "":
		return sendEvent(conv, &events.ModelReasoning{Content: p.Text})
	case p.Text != "":
		a.content.WriteString(p.Text)
		if stream {
			return sendEvent(conv, &events.ModelMessageChunk{Content: p.Text})
		}
	case p.InlineData != nil && p.InlineData.Data != "":
		data, err := base64.StdEncoding.DecodeString(p.InlineData.Data)
		if err != nil {
			return fmt.Errorf("gemini: decoding inlineData: %w", err)
		}
		metadata := map[string]any{}
		if p.InlineData.MimeType != "" {
			metadata["media_type"] = p.InlineData.MimeType
		}
		a.files = append(a.files, events.BinaryResult{Data: data, Metadata: metadata})
	case p.FunctionCall != nil:
		id := p.FunctionCall.ID
		if id == "" {
			// The generateContent response carries no call id; every consumer
			// (tool result matching) needs one.
			id = uuid.New()
		}
		args, err := json.Marshal(p.FunctionCall.Args)
		if err != nil {
			return fmt.Errorf("gemini: encoding function call args: %w", err)
		}
		if p.FunctionCall.Args == nil {
			args = []byte("{}")
		}
		a.calls = append(a.calls, events.ToolCallEvent{
			ID:        id,
			Name:      p.FunctionCall.Name,
			Arguments: string(args),
		})
	}
	return nil
}
