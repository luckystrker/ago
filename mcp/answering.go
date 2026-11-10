package mcp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"sort"

	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
)

// Errors reported while answering a server's own requests. Match with errors.Is.
var (
	// ErrAnswering reports a server request the client could not answer — no
	// human channel for an elicitation, a sampling handler that failed, a
	// malformed request — and which therefore fails the tool call it belongs to
	// (the same outcome as Python, where such an answer aborts the call).
	ErrAnswering = errors.New("mcp: cannot answer the server's request")
	// ErrMaxRounds reports a server that kept answering a tool call with
	// input_required after AnswerPolicy.MaxRounds rounds of answers.
	ErrMaxRounds = errors.New("mcp: server exceeded the input_required round limit")
)

// ElicitationPolicy decides what happens to a server's question for the user.
type ElicitationPolicy string

const (
	// ElicitationDecline (the zero value) advertises no elicitation capability
	// at all: the server is never able to ask.
	ElicitationDecline ElicitationPolicy = "decline"
	// ElicitationAsk advertises the capability and routes each question to the
	// conversation's human input (Context.Input, i.e. the agent's HITL hook).
	// Only single-field "form" elicitations are answered; anything else is
	// declined on the wire, because one free-text answer cannot be split across
	// fields without fabricating data.
	ElicitationAsk ElicitationPolicy = "ask"
)

// DefaultMaxRounds is AnswerPolicy.MaxRounds when it is left zero.
const DefaultMaxRounds = 10

// AnswerPolicy is which of the operator's own resources a server may use. Every
// field is off by default, so connecting to a server you do not control hands it
// nothing you did not name; each capability is advertised to the server only
// when enabled here (Python parity: MCPAnswerPolicy).
type AnswerPolicy struct {
	// Elicitation routes a server's questions to the conversation's human.
	Elicitation ElicitationPolicy
	// Sampling lends the calling agent's own model (the model config the agent
	// publishes in the conversation context) to the server, so it can run a
	// completion on the operator's budget. Deprecated upstream (SEP-2577):
	// prefer giving the server direct access to an LLM provider of its own.
	Sampling bool
	// SamplingHandler, when set, answers sampling requests instead of the
	// agent's model and enables the capability on its own.
	SamplingHandler SamplingHandler
	// Roots are the directories reported to a server that scopes its work to
	// them (deprecated upstream, SEP-2577). Relative paths are made absolute.
	Roots []string
	// MaxRounds bounds how many times one tool call may come back with
	// input_required before the call is abandoned with ErrMaxRounds. Zero means
	// DefaultMaxRounds; negative is rejected by New.
	MaxRounds int
}

func (p AnswerPolicy) elicits() bool { return p.Elicitation == ElicitationAsk }
func (p AnswerPolicy) samples() bool { return p.Sampling || p.SamplingHandler != nil }

func (p AnswerPolicy) maxRounds() int {
	if p.MaxRounds == 0 {
		return DefaultMaxRounds
	}
	return p.MaxRounds
}

func (p AnswerPolicy) validate() error {
	switch p.Elicitation {
	case "", ElicitationDecline, ElicitationAsk:
	default:
		return fmt.Errorf("%w: unknown elicitation policy %q", ErrUnsupportedConfig, p.Elicitation)
	}
	if p.MaxRounds < 0 {
		return fmt.Errorf("%w: AnswerPolicy.MaxRounds must not be negative", ErrUnsupportedConfig)
	}
	return nil
}

// capabilities is the client capability object the handshake advertises: one
// entry per enabled request type, nothing else.
func (p AnswerPolicy) capabilities() map[string]any {
	caps := map[string]any{}
	if p.elicits() {
		// Both modes are advertised, as the SDK does for any elicitation
		// callback; url mode is then declined in elicit (no out-of-band flow).
		caps["elicitation"] = map[string]any{"form": map[string]any{}, "url": map[string]any{}}
	}
	if p.samples() {
		caps["sampling"] = map[string]any{}
	}
	if len(p.Roots) > 0 {
		// listChanged=true is what the SDK advertises for any roots callback.
		// AGO-DEVIATION: AGo never sends notifications/roots/list_changed (the
		// roots are fixed at construction), so the flag promises nothing it
		// will use; it is kept for wire parity with the SDK.
		caps["roots"] = map[string]any{"listChanged": true}
	}
	return caps
}

// SamplingRequest is a server's request for one model completion.
type SamplingRequest struct {
	// SystemPrompt is the server's system prompt ("" when it sent none).
	SystemPrompt string
	// Messages is the conversation to complete, user and assistant turns.
	Messages []SamplingMessage
	// MaxTokens is the server's requested ceiling (0 when absent). Generation
	// parameters are not forwarded to the agent's model — see sampleWithModel.
	MaxTokens int
}

// SamplingMessage is one turn of a sampling conversation.
type SamplingMessage struct {
	// Role is "user" or "assistant".
	Role string
	// Parts are the turn's content, mapped like tool results (text, binary).
	Parts []events.Input
}

// SamplingResult is the completion handed back to the server.
type SamplingResult struct {
	Text       string
	Model      string
	StopReason string
}

// SamplingHandler answers a sampling request. cx is the conversation of the tool
// call the server is serving (nil when the request arrives outside one). An
// error fails the tool call with ErrAnswering.
type SamplingHandler func(ctx context.Context, cx *agocontext.Context, req SamplingRequest) (SamplingResult, error)

// callScope is what a server's request is answered against: the conversation of
// the tool call in flight. Set and read only while the Client's lock is held (the
// transports invoke the handler on the goroutine waiting for its call's reply).
type callScope struct {
	cx *agocontext.Context
	// failure records the first request that could not be answered; the call
	// fails with it however the server then replies.
	failure error
}

// handleServerRequest answers one server-initiated request: ping, elicitation,
// sampling or roots; anything else is method-not-found. A refusal that must
// abort the tool call (Python: the ErrorData arms of answering.py) is also
// recorded on the scope.
func (c *Client) handleServerRequest(ctx context.Context, method string, params json.RawMessage) (any, *rpcError) {
	switch method {
	case "ping":
		return map[string]any{}, nil
	case "elicitation/create":
		return c.answer(c.elicit(ctx, params))
	case "sampling/createMessage":
		return c.answer(c.sample(ctx, params))
	case "roots/list":
		return c.answer(c.listRoots())
	default:
		return nil, &rpcError{Code: codeMethodNotFound, Message: "method not found: " + method}
	}
}

// answer turns an internal outcome into the wire reply, recording an error as
// the failure of the call in flight.
func (c *Client) answer(result any, err error) (any, *rpcError) {
	if err == nil {
		return result, nil
	}
	if c.scope != nil && c.scope.failure == nil {
		c.scope.failure = fmt.Errorf("%w: %w", ErrAnswering, err)
	}
	code := codeInternalError
	var pe *paramsError
	if errors.As(err, &pe) {
		code = codeInvalidParams
	}
	var ne *notEnabledError
	if errors.As(err, &ne) {
		code = codeInvalidRequest
	}
	return nil, &rpcError{Code: code, Message: err.Error()}
}

// paramsError marks a request the server malformed (JSON-RPC invalid params).
type paramsError struct{ msg string }

func (e *paramsError) Error() string { return e.msg }

// notEnabledError marks a request for a capability this client never advertised.
type notEnabledError struct{ msg string }

func (e *notEnabledError) Error() string { return e.msg }

type elicitParams struct {
	Mode            string          `json:"mode"`
	Message         string          `json:"message"`
	RequestedSchema json.RawMessage `json:"requestedSchema"`
}

func declined() map[string]any { return map[string]any{"action": "decline"} }

func (c *Client) elicit(ctx context.Context, raw json.RawMessage) (any, error) {
	if !c.policy.elicits() {
		return declined(), nil
	}
	var p elicitParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, &paramsError{"malformed elicitation request: " + err.Error()}
	}
	if p.Mode != "" && p.Mode != "form" {
		// Context.Input is a text channel and cannot confirm that an
		// out-of-band (url-mode) flow happened: declining beats accepting on a
		// promise.
		return declined(), nil
	}
	field, ok := soleField(p.RequestedSchema)
	if !ok {
		// Splitting one free-text answer across fields is fabricating data.
		return declined(), nil
	}
	if c.scope == nil || c.scope.cx == nil {
		// An absent channel is not a refusal: declining would hand the server
		// a decision the operator never made, so the call fails instead.
		return nil, errors.New("elicitation needs a conversation (use CallToolWith or Tools) to reach a human")
	}
	// A hitl error (no hook, hook failure, timeout) is deliberately not turned
	// into a decline, for the same reason.
	answer, err := c.scope.cx.Input(ctx, p.Message, 0)
	if err != nil {
		return nil, fmt.Errorf("human input: %w", err)
	}
	return map[string]any{"action": "accept", "content": map[string]any{field: answer}}, nil
}

// soleField is the name of the form's single property.
func soleField(schema json.RawMessage) (string, bool) {
	var s struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(schema, &s); err != nil || len(s.Properties) != 1 {
		return "", false
	}
	for name := range s.Properties {
		return name, true
	}
	return "", false
}

type samplingParams struct {
	Messages []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"messages"`
	SystemPrompt string `json:"systemPrompt"`
	MaxTokens    int    `json:"maxTokens"`
}

func (c *Client) sample(ctx context.Context, raw json.RawMessage) (any, error) {
	if !c.policy.samples() {
		return nil, &notEnabledError{"this client does not lend its model for sampling"}
	}
	var p samplingParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, &paramsError{"malformed sampling request: " + err.Error()}
	}
	if len(p.Messages) == 0 {
		return nil, &paramsError{"a sampling request must carry at least one message"}
	}
	req := SamplingRequest{SystemPrompt: p.SystemPrompt, MaxTokens: p.MaxTokens}
	for _, m := range p.Messages {
		parts, err := samplingParts(m.Content)
		if err != nil {
			return nil, &paramsError{err.Error()}
		}
		req.Messages = append(req.Messages, SamplingMessage{Role: m.Role, Parts: parts})
	}
	var cx *agocontext.Context
	if c.scope != nil {
		cx = c.scope.cx
	}
	handler := c.policy.SamplingHandler
	if handler == nil {
		handler = sampleWithModel
	}
	res, err := handler(ctx, cx, req)
	if err != nil {
		return nil, fmt.Errorf("sampling: %w", err)
	}
	out := map[string]any{
		"role":    "assistant",
		"content": map[string]any{"type": "text", "text": res.Text},
		"model":   res.Model,
	}
	if out["model"] == "" {
		out["model"] = "unknown"
	}
	if res.StopReason != "" {
		out["stopReason"] = res.StopReason
	}
	return out, nil
}

// samplingParts maps a sampling message's content (one block or an array) to
// typed inputs; a malformed base64 payload is an error, not a silent drop.
func samplingParts(raw json.RawMessage) ([]events.Input, error) {
	var blocks []map[string]any
	if len(raw) > 0 && raw[0] == '[' {
		if err := json.Unmarshal(raw, &blocks); err != nil {
			return nil, fmt.Errorf("sampling content: %w", err)
		}
	} else {
		var one map[string]any
		if err := json.Unmarshal(raw, &one); err != nil {
			return nil, fmt.Errorf("sampling content: %w", err)
		}
		blocks = []map[string]any{one}
	}
	parts := make([]events.Input, 0, len(blocks))
	for _, b := range blocks {
		switch t, _ := b["type"].(string); t {
		case "text":
			text, _ := b["text"].(string)
			parts = append(parts, &events.TextInput{Content: text})
		case "image", "audio":
			kind := events.BinaryImage
			if t == "audio" {
				kind = events.BinaryAudio
			}
			encoded, _ := b["data"].(string)
			data, err := base64.StdEncoding.DecodeString(encoded)
			if err != nil {
				return nil, fmt.Errorf("a server sent a %s block that is not base64: %w", t, err)
			}
			mime, _ := b["mimeType"].(string)
			parts = append(parts, &events.BinaryInput{Data: data, MediaType: mime, Kind: kind})
		default:
			// A block of a flow this client does not declare (tool use):
			// preserved as text rather than dropped.
			enc, err := json.Marshal(b)
			if err != nil {
				return nil, fmt.Errorf("sampling content: %w", err)
			}
			parts = append(parts, &events.TextInput{Content: string(enc)})
		}
	}
	return parts, nil
}

func (c *Client) listRoots() (any, error) {
	if len(c.policy.Roots) == 0 {
		return nil, &notEnabledError{"this client reports no roots"}
	}
	roots := make([]map[string]any, 0, len(c.policy.Roots))
	for _, dir := range c.policy.Roots {
		abs, err := filepath.Abs(dir)
		if err != nil {
			return nil, fmt.Errorf("root %q: %w", dir, err)
		}
		u := url.URL{Scheme: "file", Path: filepath.ToSlash(abs)}
		root := map[string]any{"uri": u.String()}
		if name := filepath.Base(abs); name != "" && name != string(filepath.Separator) && name != "." {
			root["name"] = name
		}
		roots = append(roots, root)
	}
	return map[string]any{"roots": roots}, nil
}

// --- input_required (the modern era) -------------------------------------------

// inputRequired is the incomplete result a modern-era server returns instead of
// answering a call: questions for the client plus a state token to echo back.
type inputRequired struct {
	ResultType    string                     `json:"resultType"`
	InputRequests map[string]json.RawMessage `json:"inputRequests"`
	RequestState  string                     `json:"requestState"`
}

// asInputRequired reports whether a tools/call result is an input_required
// result rather than a completed one.
func asInputRequired(res json.RawMessage) (inputRequired, bool) {
	var ir inputRequired
	if err := json.Unmarshal(res, &ir); err != nil {
		return inputRequired{}, false
	}
	if ir.ResultType == "input_required" || len(ir.InputRequests) > 0 {
		return ir, true
	}
	return inputRequired{}, false
}

// answerInputRequests dispatches every embedded request through the same
// handler the standalone server-to-client requests use, so the two eras cannot
// disagree about who answers what. A request that cannot be answered aborts the
// call (the server cannot continue without the answer).
//
// AGO-DEVIATION: the SDK dispatches the requests of one round concurrently; AGo
// answers them one by one in key order, so two questions never reach the one
// human (a single Context.Input channel) at the same time.
func (c *Client) answerInputRequests(ctx context.Context, ir inputRequired) (map[string]any, error) {
	keys := make([]string, 0, len(ir.InputRequests))
	for k := range ir.InputRequests {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	responses := make(map[string]any, len(keys))
	for _, key := range keys {
		var req struct {
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(ir.InputRequests[key], &req); err != nil || req.Method == "" {
			return nil, fmt.Errorf("%w: input request %q is malformed", ErrProtocol, key)
		}
		result, rpcErr := c.handleServerRequest(ctx, req.Method, req.Params)
		if rpcErr != nil {
			if c.scope != nil && c.scope.failure != nil {
				return nil, c.scope.failure
			}
			return nil, fmt.Errorf("%w: input request %q (%s): %s", ErrAnswering, key, req.Method, rpcErr.Message)
		}
		responses[key] = result
	}
	return responses, nil
}
