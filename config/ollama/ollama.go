// Package ollama implements the Ollama provider over the raw /api/chat REST
// API — no SDK. It maps the AGo event history to chat messages (plain-string
// content, base64 images, object tool-call arguments) and the response back
// onto a ModelResponse. See docs/specs/config-llm.md §1/§6.
package ollama

import (
	"fmt"
	"os"

	"github.com/luckystrker/ago/config"
	"github.com/luckystrker/ago/internal/httpx"
)

// DefaultHost is the local Ollama server root used when no Host is configured.
//
// AGO-DEVIATION: Python's OLLAMA_DEFAULT_HOST is "http://localhost:11434"; the
// porting brief pins 127.0.0.1 (the address Ollama binds, immune to an IPv6
// localhost resolution).
const DefaultHost = "http://127.0.0.1:11434"

// EnvAPIKey names the environment variable read when no API key is configured
// (Ollama Cloud deployments authenticate with a bearer token).
const EnvAPIKey = "OLLAMA_API_KEY"

// OllamaConfig is one configured Ollama model (Python parity: the
// OllamaConfig dataclass). A config is immutable after construction and safe
// for concurrent use; Create builds a fresh client per turn.
//
// AGO-DEVIATION: the brief sketches exported struct fields, but the ModelConfig
// contract requires a Model() accessor and Go forbids a field and a method with
// the same name on one type. Fields are unexported and set through NewOllama
// and the Option funcs.
type OllamaConfig struct {
	model       string
	host        string
	apiKey      string
	temperature *float64
	topP        *float64
	maxTokens   *int64
	stop        []string
	seed        *int64
	streaming   bool
}

// Option sets one field of an OllamaConfig at construction time.
type Option func(*OllamaConfig)

// WithHost overrides the server root ("" keeps the default).
func WithHost(host string) Option { return func(c *OllamaConfig) { c.host = host } }

// WithAPIKey sets the bearer token (Ollama Cloud); empty means unauthenticated
// local access, with the OLLAMA_API_KEY env var as fallback.
func WithAPIKey(key string) Option { return func(c *OllamaConfig) { c.apiKey = key } }

// WithTemperature sets the sampling temperature.
func WithTemperature(v float64) Option { return func(c *OllamaConfig) { c.temperature = &v } }

// WithTopP sets the nucleus-sampling probability mass.
func WithTopP(v float64) Option { return func(c *OllamaConfig) { c.topP = &v } }

// WithMaxTokens caps the generated tokens (wire name num_predict).
func WithMaxTokens(v int64) Option { return func(c *OllamaConfig) { c.maxTokens = &v } }

// WithStop sets the stop sequences.
func WithStop(v ...string) Option {
	return func(c *OllamaConfig) { c.stop = append([]string(nil), v...) }
}

// WithSeed pins the sampling seed.
func WithSeed(v int64) Option { return func(c *OllamaConfig) { c.seed = &v } }

// WithStreaming toggles streaming. Streaming is not implemented yet in this
// port: Create fails on a streaming config.
func WithStreaming(v bool) Option { return func(c *OllamaConfig) { c.streaming = v } }

// NewOllama builds a config for one Ollama model.
func NewOllama(model string, opts ...Option) *OllamaConfig {
	c := &OllamaConfig{model: model, host: DefaultHost}
	for _, o := range opts {
		o(c)
	}
	return c
}

// Provider names the vendor (Python parity: ModelProvider.OLLAMA).
func (c *OllamaConfig) Provider() config.Provider { return config.ProviderOllama }

// Model returns the model identifier sent to the API.
func (c *OllamaConfig) Model() string { return c.model }

// Create resolves the env fallbacks and builds a ready-to-call client
// (Python parity: a fresh client per create()).
func (c *OllamaConfig) Create() (config.LLMClient, error) {
	if c.model == "" {
		return nil, fmt.Errorf("%w: ollama model", config.ErrNotProvided)
	}
	// AGO-DEVIATION: the brief defers Ollama streaming (Python streams); a
	// streaming config refuses to create a client instead of silently
	// degrading to non-streaming.
	if c.streaming {
		return nil, fmt.Errorf("%w: ollama streaming is not implemented (non-streaming only)", config.ErrUnsupportedInput)
	}
	host := c.host
	if host == "" {
		host = DefaultHost
	}
	key := c.apiKey
	if key == "" {
		key = os.Getenv(EnvAPIKey)
	}
	// A missing key is fine: local Ollama needs no auth; the header is
	// omitted rather than failing (Python parity). No transport retries: the
	// Python ollama SDK posts plain httpx without retry logic.
	return &client{
		cfg:    *c,
		host:   host,
		apiKey: key,
		http:   &httpx.Client{},
	}, nil
}

// Clone returns a copy with overrides applied (Python parity:
// dataclasses.replace). Keys are the Python field names; values keep the
// config's field types (*float64/*int64 or the plain scalars, nil clears the
// field). Unknown keys are an error.
func (c *OllamaConfig) Clone(overrides map[string]any) (config.ModelConfig, error) {
	cc := *c
	cc.stop = append([]string(nil), c.stop...)
	cc.temperature = cloneFloat(c.temperature)
	cc.topP = cloneFloat(c.topP)
	cc.maxTokens = cloneInt(c.maxTokens)
	cc.seed = cloneInt(c.seed)
	for k, v := range overrides {
		switch k {
		case "model":
			s, err := stringOverride(k, v)
			if err != nil {
				return nil, err
			}
			cc.model = s
		case "host":
			s, err := stringOverride(k, v)
			if err != nil {
				return nil, err
			}
			cc.host = s
		case "api_key":
			s, err := stringOverride(k, v)
			if err != nil {
				return nil, err
			}
			cc.apiKey = s
		case "temperature":
			f, err := floatOverride(k, v)
			if err != nil {
				return nil, err
			}
			cc.temperature = f
		case "top_p":
			f, err := floatOverride(k, v)
			if err != nil {
				return nil, err
			}
			cc.topP = f
		case "max_tokens":
			i, err := intOverride(k, v)
			if err != nil {
				return nil, err
			}
			cc.maxTokens = i
		case "stop":
			ss, err := stringsOverride(k, v)
			if err != nil {
				return nil, err
			}
			cc.stop = ss
		case "seed":
			i, err := intOverride(k, v)
			if err != nil {
				return nil, err
			}
			cc.seed = i
		case "streaming":
			b, err := boolOverride(k, v)
			if err != nil {
				return nil, err
			}
			cc.streaming = b
		default:
			return nil, fmt.Errorf("ollama: unknown config override %q", k)
		}
	}
	return &cc, nil
}

// --- override value coercion (shared shape with the gemini config) ------------

func overrideErr(key string, v any) error {
	return fmt.Errorf("ollama: override %q expects the config field type, got %T", key, v)
}

func stringOverride(key string, v any) (string, error) {
	s, ok := v.(string)
	if !ok {
		return "", overrideErr(key, v)
	}
	return s, nil
}

func floatOverride(key string, v any) (*float64, error) {
	switch n := v.(type) {
	case nil:
		return nil, nil
	case *float64:
		return cloneFloat(n), nil
	case float64:
		return &n, nil
	default:
		return nil, overrideErr(key, v)
	}
}

func intOverride(key string, v any) (*int64, error) {
	switch n := v.(type) {
	case nil:
		return nil, nil
	case *int64:
		return cloneInt(n), nil
	case int64:
		return &n, nil
	case int:
		i := int64(n)
		return &i, nil
	default:
		return nil, overrideErr(key, v)
	}
}

func stringsOverride(key string, v any) ([]string, error) {
	switch s := v.(type) {
	case nil:
		return nil, nil
	case []string:
		return append([]string(nil), s...), nil
	default:
		return nil, overrideErr(key, v)
	}
}

func boolOverride(key string, v any) (bool, error) {
	switch b := v.(type) {
	case bool:
		return b, nil
	case *bool:
		if b == nil {
			return false, nil
		}
		return *b, nil
	default:
		return false, overrideErr(key, v)
	}
}

func cloneFloat(v *float64) *float64 {
	if v == nil {
		return nil
	}
	c := *v
	return &c
}

func cloneInt(v *int64) *int64 {
	if v == nil {
		return nil
	}
	c := *v
	return &c
}
