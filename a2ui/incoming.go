package a2ui

import (
	"fmt"
	"regexp"
	"strings"
)

// IncomingKind classifies a parsed client→server envelope.
type IncomingKind string

const (
	IncomingActionKind           IncomingKind = "action"
	IncomingFunctionCallKind     IncomingKind = "functionCall"
	IncomingErrorKind            IncomingKind = "error"
	IncomingFunctionResponseKind IncomingKind = "functionResponse"
	IncomingUnknownKind          IncomingKind = "unknown"
)

// IncomingAction is a client→server "action" envelope (a button click).
// An action is fire-and-forget: a renderer that wants a reply sends a
// callAgentFunction.
type IncomingAction struct {
	Name              string
	SurfaceID         string
	SourceComponentID string
	Timestamp         string
	Context           map[string]any
	UserMessage       string         // "" when absent
	Metadata          map[string]any // nil when absent
}

// IncomingFunctionCall is a client→server "callAgentFunction" envelope (v1.0):
// the renderer asks the server to run a registered action as a function.
type IncomingFunctionCall struct {
	FunctionCallID string
	SurfaceID      string
	Call           string
	Args           any // the raw wire value; only a JSON object may run a handler
	CatalogID      string
	// CatalogIDValid is false when "catalogId" was present but not a string,
	// so malformed input cannot select a handler by falling back silently.
	CatalogIDValid bool
}

// Arguments returns the call's keyword arguments, rejecting non-object args
// the way incoming.py's arguments property does.
func (c *IncomingFunctionCall) Arguments() (map[string]any, error) {
	if m, ok := c.Args.(map[string]any); ok {
		return m, nil
	}
	return nil, fmt.Errorf("function arguments must be a JSON object")
}

// IncomingSurfaceError is a client→server "error" reporting a surface
// validation/render failure. Path (a JSON pointer) is set only for
// VALIDATION_FAILED-style errors.
type IncomingSurfaceError struct {
	Code      string
	SurfaceID string
	Message   string
	Path      string // "" when absent
}

// IncomingFunctionError is the v1.0 generic error arm: a failed
// server-initiated callRendererFunction, carrying a FunctionCallID instead of
// a surface.
type IncomingFunctionError struct {
	Code           string
	FunctionCallID string
	Message        string
}

// IncomingError is the client→server "error" union; exactly one field is set.
type IncomingError struct {
	Surface  *IncomingSurfaceError
	Function *IncomingFunctionError
}

// IncomingFunctionResponse is a client→server "rendererFunctionResponse"
// (v1.0): the client's reply to a server-initiated callRendererFunction. It
// carries Value or Error — Error is set exactly when the call failed.
type IncomingFunctionResponse struct {
	FunctionCallID string
	Value          any
	Error          *FunctionError
}

// IncomingResult is the result of parsing one client→server envelope: a kind
// plus the matching payload (nil for the other arms; ParseError on unknown).
type IncomingResult struct {
	Kind             IncomingKind
	Action           *IncomingAction
	FunctionCall     *IncomingFunctionCall
	Error            *IncomingError
	FunctionResponse *IncomingFunctionResponse
	ParseError       string
}

func optionalString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// ParseIncomingMessage classifies a single client→server A2UI envelope
// (incoming.py parse_incoming_message): {"action": {...}} |
// {"callAgentFunction": {...}} | {"rendererFunctionResponse": {...}} |
// {"error": {...}}. Parsing is structural and version-neutral; it does NOT
// schema-validate the envelope.
//
// AGO-DEVIATION (D-076): Python coerces scalar fields with str() (a numeric
// name becomes "123"); non-string scalars here are treated as absent.
func ParseIncomingMessage(data any) IncomingResult {
	obj, ok := data.(map[string]any)
	if !ok {
		return IncomingResult{Kind: IncomingUnknownKind, ParseError: "envelope is not a JSON object"}
	}

	if actionObj, ok := obj["action"].(map[string]any); ok {
		rawContext, _ := actionObj["context"].(map[string]any)
		rawMetadata, _ := actionObj["metadata"].(map[string]any)
		return IncomingResult{Kind: IncomingActionKind, Action: &IncomingAction{
			Name:              optionalString(actionObj["name"]),
			SurfaceID:         optionalString(actionObj["surfaceId"]),
			SourceComponentID: optionalString(actionObj["sourceComponentId"]),
			Timestamp:         optionalString(actionObj["timestamp"]),
			Context:           orEmptyMap(rawContext),
			UserMessage:       optionalString(actionObj["userMessage"]),
			Metadata:          rawMetadata,
		}}
	}

	if callObj, ok := obj["callAgentFunction"].(map[string]any); ok {
		functionCallID, idIsStr := callObj["functionCallId"].(string)
		if !idIsStr {
			return unknownf("'callAgentFunction' has no 'functionCallId' to correlate a reply")
		}
		callObjInner, callIsObj := callObj["callFunction"].(map[string]any)
		name, nameIsStr := "", false
		if callIsObj {
			name, nameIsStr = callObjInner["@call"].(string)
		}
		if !callIsObj || !nameIsStr {
			return unknownf("'callAgentFunction' has no 'callFunction' with an '@call' name")
		}
		surfaceID, surfaceIsStr := callObj["surfaceId"].(string)
		if !surfaceIsStr {
			return unknownf("'callAgentFunction' has no string 'surfaceId'")
		}
		rawArgs, hasArgs := callObjInner["args"]
		if !hasArgs {
			rawArgs = map[string]any{}
		}
		catalogID, catalogOK := "", true
		if rawCatalog, present := callObjInner["catalogId"]; present {
			if s, isStr := rawCatalog.(string); isStr {
				catalogID = s
			} else {
				catalogOK = false
			}
		}
		return IncomingResult{Kind: IncomingFunctionCallKind, FunctionCall: &IncomingFunctionCall{
			FunctionCallID: functionCallID,
			SurfaceID:      surfaceID,
			Call:           name,
			Args:           rawArgs,
			CatalogID:      catalogID,
			CatalogIDValid: catalogOK,
		}}
	}

	if frObj, ok := obj["rendererFunctionResponse"].(map[string]any); ok {
		return parseFunctionResponse(frObj)
	}

	if errObj, ok := obj["error"].(map[string]any); ok {
		code := optionalString(errObj["code"])
		message := optionalString(errObj["message"])
		if fcID, ok := errObj["functionCallId"].(string); ok {
			return IncomingResult{Kind: IncomingErrorKind, Error: &IncomingError{
				Function: &IncomingFunctionError{Code: code, FunctionCallID: fcID, Message: message},
			}}
		}
		return IncomingResult{Kind: IncomingErrorKind, Error: &IncomingError{
			Surface: &IncomingSurfaceError{
				Code:      code,
				SurfaceID: optionalString(errObj["surfaceId"]),
				Message:   message,
				Path:      optionalString(errObj["path"]),
			},
		}}
	}

	return IncomingResult{Kind: IncomingUnknownKind,
		ParseError: "envelope has none of 'action', 'callAgentFunction', 'rendererFunctionResponse', or 'error' keys"}
}

func parseFunctionResponse(frObj map[string]any) IncomingResult {
	functionCallID, isStr := frObj["functionCallId"].(string)
	if !isStr {
		return unknownf("'rendererFunctionResponse' has no 'functionCallId' to correlate with a call")
	}
	_, hasValue := frObj["value"]
	_, hasError := frObj["error"]
	if hasValue == hasError {
		return unknownf("'rendererFunctionResponse' must carry exactly one of 'value' or 'error'")
	}
	if hasError {
		errObj, isObj := frObj["error"].(map[string]any)
		code, codeIsStr := "", false
		message, msgIsStr := "", false
		if isObj {
			code, codeIsStr = errObj["code"].(string)
			message, msgIsStr = errObj["message"].(string)
		}
		if !isObj || !codeIsStr || !msgIsStr {
			return unknownf("'rendererFunctionResponse' error must be an object with string 'code' and 'message'")
		}
		return IncomingResult{Kind: IncomingFunctionResponseKind, FunctionResponse: &IncomingFunctionResponse{
			FunctionCallID: functionCallID,
			Error:          &FunctionError{Code: code, Message: message},
		}}
	}
	return IncomingResult{Kind: IncomingFunctionResponseKind, FunctionResponse: &IncomingFunctionResponse{
		FunctionCallID: functionCallID,
		Value:          frObj["value"],
	}}
}

func unknownf(msg string) IncomingResult {
	return IncomingResult{Kind: IncomingUnknownKind, ParseError: msg}
}

func orEmptyMap(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}

// --- prompt sanitization (incoming.py sanitize_for_prompt) -------------------

const maxPromptFieldLen = 4000

var (
	controlCharsRe = regexp.MustCompile(`[\x00-\x08\x0b\x0c\x0e-\x1f\x7f]`)
	roleMarkerRe   = regexp.MustCompile(`(?im)^[ \t]*(system|assistant|user|developer|tool)[ \t]*:`)
)

// SanitizeForPrompt neutralizes client text spliced into an LLM prompt: it
// strips control characters, defuses the A2UI framing tags and role markers,
// and caps the length. Defense-in-depth, not a guarantee — treat all client
// text as untrusted.
func SanitizeForPrompt(text string) string {
	if text == "" {
		return ""
	}
	cleaned := controlCharsRe.ReplaceAllString(text, " ")
	for _, marker := range []string{JSONOpenTag, JSONCloseTag} {
		cleaned = strings.ReplaceAll(cleaned, marker,
			strings.ReplaceAll(strings.ReplaceAll(marker, "<", "‹"), ">", "›"))
	}
	cleaned = roleMarkerRe.ReplaceAllStringFunc(cleaned, func(m string) string {
		return strings.ReplaceAll(m, ":", "：")
	})
	if runes := []rune(cleaned); len(runes) > maxPromptFieldLen {
		cleaned = string(runes[:maxPromptFieldLen]) + "…[truncated]"
	}
	return cleaned
}

// --- envelope → prompt rewrites (incoming.py) -------------------------------

// ActionToPrompt rewrites a client A2UI action as a generic LLM instruction —
// the path taken only for clicks with NO registered server action. The second
// return is false only for a nameless (malformed) action, which is dropped.
func ActionToPrompt(action *IncomingAction) (string, bool) {
	if action.Name == "" {
		return "", false
	}
	name := SanitizeForPrompt(action.Name)
	ctxJSON := SanitizeForPrompt(pyJSON(action.Context))
	var originBits []string
	if action.SurfaceID != "" {
		originBits = append(originBits, "surface="+SanitizeForPrompt(action.SurfaceID))
	}
	if action.SourceComponentID != "" {
		originBits = append(originBits, "component="+SanitizeForPrompt(action.SourceComponentID))
	}
	if action.Timestamp != "" {
		originBits = append(originBits, "at="+SanitizeForPrompt(action.Timestamp))
	}
	origin := ""
	if len(originBits) > 0 {
		origin = fmt.Sprintf(" (%s)", strings.Join(originBits, ", "))
	}
	userMessage := ""
	if action.UserMessage != "" {
		userMessage = fmt.Sprintf(" User message: %s", SanitizeForPrompt(action.UserMessage))
	}
	return fmt.Sprintf("The user clicked the '%s' button%s. Context: %s%s", name, origin, ctxJSON, userMessage), true
}

// ErrorToPrompt rewrites a client-reported A2UI error as a corrective LLM
// instruction (both error arms).
func ErrorToPrompt(err *IncomingError) string {
	if err.Function != nil {
		fcID := SanitizeForPrompt(err.Function.FunctionCallID)
		code := "(none)"
		if err.Function.Code != "" {
			code = SanitizeForPrompt(err.Function.Code)
		}
		return fmt.Sprintf(
			"The client reported an A2UI error for function call '%s'. Code: %s. Message: %s. "+
				"Adjust your approach and continue without relying on that function's result.",
			fcID, code, SanitizeForPrompt(err.Function.Message))
	}
	surface := err.Surface
	code := "(none)"
	if surface.Code != "" {
		code = SanitizeForPrompt(surface.Code)
	}
	pathHint := "(unknown)"
	if surface.Path != "" {
		pathHint = SanitizeForPrompt(surface.Path)
	}
	return fmt.Sprintf(
		"The client reported an A2UI error on surface '%s'. Code: %s. Path: %s. Message: %s. "+
			"Please regenerate the UI with this issue corrected.",
		SanitizeForPrompt(surface.SurfaceID), code, pathHint, SanitizeForPrompt(surface.Message))
}

// FunctionResponseToPrompt rewrites a client rendererFunctionResponse as a
// continuation instruction. functionName (when the server remembers the call)
// names the function.
func FunctionResponseToPrompt(fr *IncomingFunctionResponse, functionName string) string {
	fcID := SanitizeForPrompt(fr.FunctionCallID)
	if functionName != "" {
		fcID = fmt.Sprintf("%s, function=%s", fcID, SanitizeForPrompt(functionName))
	}
	if fr.Error != nil {
		code := "(none)"
		if fr.Error.Code != "" {
			code = SanitizeForPrompt(fr.Error.Code)
		}
		return fmt.Sprintf(
			"The client's function call (id=%s) failed. Code: %s. Message: %s. "+
				"Adjust your approach and continue without relying on that function's result.",
			fcID, code, SanitizeForPrompt(fr.Error.Message))
	}
	valueJSON := SanitizeForPrompt(pyJSON(fr.Value))
	return fmt.Sprintf(
		"The client executed the function call (id=%s) and returned: %s. Use this result to continue.",
		fcID, valueJSON)
}

// IterIncomingPrompts rewrites client→server envelopes into prompt strings,
// the shared rewrite the HTTP server uses. Clicks on a REGISTERED action
// (resolveAction says so) are skipped — they run on the server. So is every
// callAgentFunction: the server answers it itself. Envelopes that cannot be
// safely mapped are dropped with a warning.
func IterIncomingPrompts(
	envelopes []any,
	resolveAction func(name string) bool,
	resolveFunction func(functionCallID string) (string, bool),
	logger Logger,
) []string {
	var prompts []string
	for _, envelope := range envelopes {
		result := ParseIncomingMessage(envelope)
		switch result.Kind {
		case IncomingActionKind:
			if resolveAction(result.Action.Name) {
				// Registered action → handled on the server; never rewritten.
				continue
			}
			prompt, ok := ActionToPrompt(result.Action)
			if !ok {
				warnf(logger, "Dropping A2UI action with no name.")
				continue
			}
			prompts = append(prompts, prompt)
		case IncomingFunctionCallKind:
			// Answered on the server, never by the LLM.
			continue
		case IncomingFunctionResponseKind:
			name := ""
			if resolveFunction != nil {
				if n, ok := resolveFunction(result.FunctionResponse.FunctionCallID); ok {
					name = n
				}
			}
			prompts = append(prompts, FunctionResponseToPrompt(result.FunctionResponse, name))
		case IncomingErrorKind:
			prompts = append(prompts, ErrorToPrompt(result.Error))
		default:
			warnf(logger, "Skipping A2UI envelope of unrecognized kind: %s", result.ParseError)
		}
	}
	return prompts
}

func warnf(logger Logger, format string, args ...any) {
	if logger != nil {
		logger.Printf(format, args...)
	}
}
