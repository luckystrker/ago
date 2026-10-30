// Package config carries the LLM configuration model: the provider vocabulary,
// the ModelConfig factory contract and the LLMClient call contract. Provider
// configs live in config/<provider>/ and build on the shared OpenAI-compatible
// engine in openaicompat.go or their own transport.
//
// Parity with Python AG2 ag2/config/config.py (ModelProvider, ModelConfig) and
// client.py (LLMClient). See docs/specs/config-llm.md §1–2.
package config

import (
	"context"

	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
)

// Provider names a model vendor. The string value is the Python enum value and
// travels on events (ModelResponse.Provider) and in usage reports.
type Provider string

// Supported providers (Python parity: ModelProvider enum).
const (
	ProviderOpenAI    Provider = "openai"
	ProviderAnthropic Provider = "anthropic"
	ProviderGemini    Provider = "gemini"
	ProviderBedrock   Provider = "bedrock"
	ProviderDashScope Provider = "dashscope"
	ProviderMistral   Provider = "mistral"
	ProviderOllama    Provider = "ollama"
	ProviderTypeSafe  Provider = "typesafe"
	ProviderVertexAI  Provider = "vertexai"
	ProviderXAI       Provider = "xai"
	ProviderZAI       Provider = "zai"
)

// ModelConfig is one configured model: identity plus a factory for fresh
// clients. Implementations are immutable after construction and safe for
// concurrent use (Python parity: the typed dataclass configs).
type ModelConfig interface {
	// Provider names the vendor this config talks to.
	Provider() Provider
	// Model returns the model identifier sent to the API.
	Model() string
	// Create builds a ready-to-call client; called per turn (Python parity:
	// fresh client per create()). Missing model or key → error wrapping
	// ErrNotProvided.
	Create() (LLMClient, error)
	// Clone returns a copy with field overrides applied (Python parity:
	// dataclasses.replace). Unknown override keys → error. Values keep the
	// config's field types (e.g. *float64 for temperature).
	Clone(overrides map[string]any) (ModelConfig, error)
}

// LLMClient performs one model call over the full event history. The returned
// ModelResponse is the durable transcript entry; streaming progress goes to
// conv.Send (ModelMessageChunk / ModelReasoning) before it returns.
type LLMClient interface {
	Call(ctx context.Context, messages []events.Event, conv *agocontext.Context, opts CallOptions) (*events.ModelResponse, error)
}
