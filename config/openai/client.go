package openai

import (
	"context"

	"ago/config"
	agocontext "ago/context"
	"ago/events"
)

// Client is the Chat Completions client built by Create: one configured
// endpoint with its auth and headers, stateless per call. Safe for concurrent
// use. Implements config.LLMClient.
type Client struct {
	params config.CompatParams
}

// Call performs one model call over the shared Chat-Completions engine
// (config.RunChatCompat): the full event history replays to the provider,
// streaming progress is published on conv, and the returned ModelResponse is
// the durable answer.
func (c *Client) Call(ctx context.Context, messages []events.Event, conv *agocontext.Context, opts config.CallOptions) (*events.ModelResponse, error) {
	p := c.params
	// The strict response_format is built here — through this package's
	// StrictifySchema — rather than left to the engine's inner mapping, so the
	// brief's key stripping is what reaches the wire. The precedence is the
	// engine's own: an explicit config ResponseFormat wins over the per-call
	// schema.
	if p.ResponseFormat == nil && opts.ResponseSchema != nil {
		p.ResponseFormat = ResponseFormat(opts.ResponseSchema)
	}
	return config.RunChatCompat(ctx, p, messages, conv, opts)
}
