package mcpserver

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/luckystrker/ago/config"
	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/hitl"
)

// ElicitationPolicy says whether a served agent's questions reach the calling
// client's human (Python parity: ag2.hitl.ElicitationPolicy).
type ElicitationPolicy string

const (
	// ElicitationAsk (the zero value) sends a question the agent puts to its
	// human (Context.Input) to the client as elicitation/create, when the client
	// declared the capability and the request has a back-channel; otherwise the
	// agent's own HITL hook answers, as before.
	ElicitationAsk ElicitationPolicy = "ask"
	// ElicitationDecline never sends a question to the client.
	ElicitationDecline ElicitationPolicy = "decline"
)

// ElicitationDeclinedError reports that the calling client refused (declined or
// cancelled) a served agent's question, or accepted it without the answer the
// form asked for. It wraps hitl.ErrNotProvided, as the Python error subclasses
// HumanInputNotProvidedError: a refusal is the same "no answer" outcome as an
// absent channel, distinct only so a host can tell them apart.
type ElicitationDeclinedError struct{ Action string }

func (e *ElicitationDeclinedError) Error() string {
	return fmt.Sprintf("the calling MCP client answered the agent's question with %q, so there is no answer to continue from", e.Action)
}

func (e *ElicitationDeclinedError) Unwrap() error { return hitl.ErrNotProvided }

// answerField is what AG2's served side names its single answer field
// (ag2.mcp.elicitation.ANSWER_FIELD).
const answerField = "answer"

// inputFormSchema is the requested schema of a Context.Input question: one
// string property, since Input is a string in, string out.
func inputFormSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			answerField: map[string]any{
				"type":        "string",
				"title":       "Answer",
				"description": "Your answer to the agent's question.",
			},
		},
		"required": []string{answerField},
	}
}

// canElicit is Python's can_answer: the deployment's policy, the client's own
// declaration, and a channel to ask on.
//
// AGO-DEVIATION: the channel exists only on a stateful transport answering as SSE
// (sess != nil). Python also reaches a stdio client, whose session can send
// requests; the stdio loop here handles one message at a time and has no way to
// read the client's reply while a turn runs, so a served agent over stdio is
// answered by its own HITL hook.
func canElicit(policy ElicitationPolicy, ex *exchange, sess *mcpSession) bool {
	if policy == ElicitationDecline || ex == nil || !ex.canRequest || sess == nil {
		return false
	}
	return sess.clientCapabilities().elicitation
}

// elicitationHook is the HITL hook that puts the agent's questions to the client.
func elicitationHook(ex *exchange) hitl.HumanHook {
	return func(ctx context.Context, req *events.HumanInputRequest) (*events.HumanMessage, error) {
		raw, err := ex.request(ctx, "elicitation/create", map[string]any{
			"mode":            "form",
			"message":         req.Content,
			"requestedSchema": inputFormSchema(),
		})
		if err != nil {
			return nil, err
		}
		var res struct {
			Action  string         `json:"action"`
			Content map[string]any `json:"content"`
		}
		if err := json.Unmarshal(raw, &res); err != nil {
			return nil, fmt.Errorf("mcpserver: malformed elicitation result: %w", err)
		}
		if res.Action != "accept" {
			return nil, &ElicitationDeclinedError{Action: res.Action}
		}
		answer, ok := res.Content[answerField].(string)
		if !ok {
			// The channel worked and the human is done, so re-asking would loop:
			// the same "no answer" outcome as a decline.
			return nil, &ElicitationDeclinedError{Action: "accept"}
		}
		return &events.HumanMessage{Content: answer}, nil
	}
}

// --- sampling: the agent's model is the client's -------------------------------------

// ErrSamplingUnavailable reports a server told to run its agent on the calling
// client's model (ServerConfig.ClientModel) whose client advertised no sampling
// capability and which has no model of its own to fall back on. It reaches the
// client as JSON-RPC error -32021 naming the missing capability.
var ErrSamplingUnavailable = errors.New("mcpserver: the calling client cannot lend its model")

// ErrSamplingRefused reports a turn needing more of a model than a borrowed one
// can give: tools or a structured response, which sampling carries neither of,
// or a completion with no text.
var ErrSamplingRefused = errors.New("mcpserver: cannot run this turn on the calling client's model")

// clientModelMaxTokens is the generation bound sent with each borrowed request,
// and the only generation parameter sent: the rest belong to a model
// configuration, and a borrowed model has none to take them from.
const clientModelMaxTokens = 4096

// codeMissingRequiredClientCapability is mcp_types.MISSING_REQUIRED_CLIENT_CAPABILITY.
const codeMissingRequiredClientCapability = -32021

// samplingUnavailableError is the JSON-RPC error of ErrSamplingUnavailable.
func samplingUnavailableError() *rpcError {
	return &rpcError{
		Code: codeMissingRequiredClientCapability,
		Message: "This server runs the agent on the calling client's model, and this client advertised no " +
			"sampling capability. Connect with sampling enabled, or ask the operator to configure a model " +
			"for the agent to fall back to.",
		Data: json.RawMessage(`{"requiredCapabilities":{"sampling":{}}}`),
	}
}

// clientModelConfig is a config.ModelConfig whose completions run on the calling
// client (sampling/createMessage). Built per call: the exchange it holds belongs
// to one request.
type clientModelConfig struct{ ex *exchange }

func (clientModelConfig) Provider() config.Provider { return "mcp-client" }

// Model is not known until a completion names one, and it may name another next
// time.
func (clientModelConfig) Model() string { return "mcp-client" }

func (c clientModelConfig) Create() (config.LLMClient, error) {
	return clientModelClient{ex: c.ex}, nil
}

func (c clientModelConfig) Clone(map[string]any) (config.ModelConfig, error) { return c, nil }

type clientModelClient struct{ ex *exchange }

// unknownModel is what the completion's model is reported as when the client
// returns none.
const unknownModel = "unknown"

func (c clientModelClient) Call(ctx context.Context, messages []events.Event, conv *agocontext.Context, opts config.CallOptions) (*events.ModelResponse, error) {
	if len(opts.Tools) > 0 {
		return nil, fmt.Errorf("%w: a served agent with tools cannot borrow the calling client's model: the sampling request carries no tools this deployment can offer", ErrSamplingRefused)
	}
	if opts.ResponseSchema != nil {
		return nil, fmt.Errorf("%w: a served agent with a response schema cannot borrow the calling client's model: sampling returns free text, which nothing here could validate against the schema", ErrSamplingRefused)
	}
	params := map[string]any{
		"messages":  toSamplingMessages(messages),
		"maxTokens": clientModelMaxTokens,
	}
	if conv != nil {
		if sys := strings.Join(conv.PromptLines(), "\n"); sys != "" {
			params["systemPrompt"] = sys
		}
	}
	raw, err := c.ex.request(ctx, "sampling/createMessage", params)
	if err != nil {
		return nil, err
	}
	var res struct {
		Content    json.RawMessage `json:"content"`
		Model      string          `json:"model"`
		StopReason string          `json:"stopReason"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, fmt.Errorf("%w: the calling client answered with something that is not a completion: %v", ErrSamplingRefused, err)
	}
	text, err := completionText(res.Content)
	if err != nil {
		return nil, err
	}
	msg := &events.ModelMessage{Content: text}
	// Published like any other model reply, so a served turn borrowing a model
	// streams and records the same way one on its own model does.
	if conv != nil {
		_ = conv.Send(msg)
	}
	model := res.Model
	if model == "" {
		model = unknownModel
	}
	return &events.ModelResponse{Message: msg, Model: model, FinishReason: res.StopReason}, nil
}

// completionText joins the completion's text blocks; one with none is refused (an
// empty text block is a model allowed to say nothing).
func completionText(raw json.RawMessage) (string, error) {
	var blocks []map[string]any
	if len(raw) > 0 && raw[0] == '[' {
		_ = json.Unmarshal(raw, &blocks)
	} else {
		var one map[string]any
		if json.Unmarshal(raw, &one) == nil && one != nil {
			blocks = []map[string]any{one}
		}
	}
	var sb strings.Builder
	spoken := false
	kinds := map[string]bool{}
	for _, b := range blocks {
		t, _ := b["type"].(string)
		kinds[t] = true
		if t == "text" {
			spoken = true
			s, _ := b["text"].(string)
			sb.WriteString(s)
		}
	}
	if !spoken {
		var names []string
		for k := range kinds {
			names = append(names, k)
		}
		return "", fmt.Errorf("%w: the calling client's completion carried no text, only %s", ErrSamplingRefused, strings.Join(names, ", "))
	}
	return sb.String(), nil
}

// toSamplingMessages renders the agent's conversation as the sampling messages
// the client receives: text, images and audio under the protocol's two roles,
// each user block its own message (as Python does); anything else is dropped
// rather than guessed at.
func toSamplingMessages(msgs []events.Event) []map[string]any {
	out := []map[string]any{}
	for _, m := range msgs {
		switch v := events.AsValue(m).(type) {
		case events.ModelResponse:
			out = append(out, map[string]any{"role": "assistant", "content": textBlockOf(v.Content())})
		case events.ModelMessage:
			out = append(out, map[string]any{"role": "assistant", "content": textBlockOf(v.Content)})
		case events.ModelRequest:
			for _, p := range v.Parts {
				switch part := events.AsValue(p).(type) {
				case events.TextInput:
					out = append(out, map[string]any{"role": "user", "content": textBlockOf(part.Content)})
				case events.BinaryInput:
					enc := base64.StdEncoding.EncodeToString(part.Data)
					switch part.Kind {
					case events.BinaryImage:
						out = append(out, map[string]any{"role": "user", "content": map[string]any{"type": "image", "data": enc, "mimeType": part.MediaType}})
					case events.BinaryAudio:
						out = append(out, map[string]any{"role": "user", "content": map[string]any{"type": "audio", "data": enc, "mimeType": part.MediaType}})
					}
				}
			}
		}
	}
	return out
}

func textBlockOf(text string) map[string]any { return map[string]any{"type": "text", "text": text} }
