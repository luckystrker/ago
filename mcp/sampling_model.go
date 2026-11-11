package mcp

import (
	"context"
	"errors"
	"fmt"

	"github.com/luckystrker/ago/config"
	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/stream"
)

// modelConfigDependency is where a running agent publishes the model config of
// its turn (agent/run.go): the model AnswerPolicy.Sampling lends to a server.
const modelConfigDependency = "ago:config"

// sampleWithModel runs one completion on the calling agent's own model.
//
// One completion, not a turn of the calling agent: its tools, history and
// response schema stay out, so a borrowed model cannot reach back into the agent
// that lent it. The server's generation parameters (max tokens, temperature, ...)
// are not forwarded either — the operator's configuration governs a call they
// pay for.
func sampleWithModel(ctx context.Context, cx *agocontext.Context, req SamplingRequest) (SamplingResult, error) {
	if cx == nil {
		return SamplingResult{}, errors.New("no model configured to sample with: the call has no conversation")
	}
	v, _ := cx.Dependency(modelConfigDependency)
	mc, ok := v.(config.ModelConfig)
	if !ok || mc == nil {
		return SamplingResult{}, errors.New("no model configured to sample with")
	}
	client, err := mc.Create()
	if err != nil {
		return SamplingResult{}, fmt.Errorf("create model client: %w", err)
	}
	history := make([]events.Event, 0, len(req.Messages))
	for _, m := range req.Messages {
		if m.Role == "user" {
			history = append(history, &events.ModelRequest{Parts: m.Parts})
			continue
		}
		var text string
		for _, p := range m.Parts {
			if ti, ok := events.AsValue(p).(events.TextInput); ok {
				text += ti.Content
			}
		}
		history = append(history, &events.ModelResponse{Message: &events.ModelMessage{Content: text}})
	}
	// A fresh conversation: the lent completion must not leak into the lender's
	// own stream or transcript.
	sampling := agocontext.New(stream.NewMemoryStream())
	if req.SystemPrompt != "" {
		sampling.Prompt = []string{req.SystemPrompt}
	}
	resp, err := client.Call(ctx, history, sampling, config.CallOptions{})
	if err != nil {
		return SamplingResult{}, err
	}
	model := resp.Model
	if model == "" {
		model = mc.Model()
	}
	return SamplingResult{Text: resp.Content(), Model: model, StopReason: resp.FinishReason}, nil
}
