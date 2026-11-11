// Package gemini implements the Google Gemini provider over the raw
// generateContent REST API (v1beta) — no SDK. It maps the AGo event history to
// contents parts, function schemas to functionDeclarations and the response
// back onto a ModelResponse. See docs/specs/config-llm.md §1/§6.
package gemini

import (
	"fmt"
	"net/http"
	"os"

	"github.com/luckystrker/ago/config"
	"github.com/luckystrker/ago/internal/httpx"
)

// DefaultBaseURL is the Gemini API root used when no BaseURL is configured.
const DefaultBaseURL = "https://generativelanguage.googleapis.com/v1beta"

// EnvAPIKey names the environment variable read when no API key is configured
// (Python parity: the SDK resolves the key from the environment).
const EnvAPIKey = "GEMINI_API_KEY"

// GeminiConfig is one configured Gemini model (Python parity: the GeminiConfig
// dataclass). A config is immutable after construction and safe for concurrent
// use; Create builds a fresh client per turn.
//
// AGO-DEVIATION: the brief sketches exported struct fields, but the ModelConfig
// contract requires a Model() accessor and Go forbids a field and a method with
// the same name on one type. Fields are unexported and set through NewGemini
// and the Option funcs.
type GeminiConfig struct {
	model            string
	apiKey           string
	baseURL          string
	temperature      *float64
	topP             *float64
	topK             *int64
	maxOutputTokens  *int64
	stopSequences    []string
	streaming        bool
	presencePenalty  *float64
	frequencyPenalty *float64
	seed             *int64
	thinkingLevel    *string
	thinkingBudget   *int64
	// thinkingConfig is a raw thinkingConfig object; when set it wins over
	// thinkingLevel/thinkingBudget (Python parity: thinking_config).
	thinkingConfig map[string]any
	// cachedContent names a cached content resource made with the Caches API
	// (Python parity: cached_content; top-level "cachedContent" on the wire).
	cachedContent string
	// responseModalities asks for the output kinds ("TEXT", "IMAGE"); the
	// imageConfig block then tunes image generation (Python parity:
	// response_modalities + image_config).
	responseModalities []string
	imageConfig        *ImageConfig
	// automaticContinuation resumes answers the server truncated at its
	// per-request token limit (Python parity: automatic_continuation
	// bool|None = None; the SDK default is enabled). nil = enabled.
	automaticContinuation *bool
	// httpClient carries the requests when non-nil (Python parity:
	// http_options client args); nil = default client.
	httpClient *http.Client
}

// Option sets one field of a GeminiConfig at construction time.
type Option func(*GeminiConfig)

// WithAPIKey overrides the API key ("" keeps the constructor value).
func WithAPIKey(key string) Option { return func(c *GeminiConfig) { c.apiKey = key } }

// WithBaseURL overrides the API root ("" keeps the default).
func WithBaseURL(url string) Option { return func(c *GeminiConfig) { c.baseURL = url } }

// WithTemperature sets the sampling temperature.
func WithTemperature(v float64) Option { return func(c *GeminiConfig) { c.temperature = &v } }

// WithTopP sets the nucleus-sampling probability mass.
func WithTopP(v float64) Option { return func(c *GeminiConfig) { c.topP = &v } }

// WithTopK sets the top-k candidate count.
func WithTopK(v int64) Option { return func(c *GeminiConfig) { c.topK = &v } }

// WithMaxOutputTokens caps the generated tokens.
func WithMaxOutputTokens(v int64) Option { return func(c *GeminiConfig) { c.maxOutputTokens = &v } }

// WithStopSequences sets the stop sequences.
func WithStopSequences(v ...string) Option {
	return func(c *GeminiConfig) { c.stopSequences = append([]string(nil), v...) }
}

// WithStreaming toggles the streaming transport (streamGenerateContent?alt=sse).
func WithStreaming(v bool) Option { return func(c *GeminiConfig) { c.streaming = v } }

// WithPresencePenalty sets the presence penalty.
func WithPresencePenalty(v float64) Option { return func(c *GeminiConfig) { c.presencePenalty = &v } }

// WithFrequencyPenalty sets the frequency penalty.
func WithFrequencyPenalty(v float64) Option { return func(c *GeminiConfig) { c.frequencyPenalty = &v } }

// WithSeed pins the sampling seed.
func WithSeed(v int64) Option { return func(c *GeminiConfig) { c.seed = &v } }

// WithThinkingLevel sets the reasoning-effort level; one of "low", "medium",
// "high" (Python parity: the ThinkingLevel literal).
func WithThinkingLevel(v string) Option { return func(c *GeminiConfig) { c.thinkingLevel = &v } }

// WithThinkingBudget caps the thinking token budget.
func WithThinkingBudget(v int64) Option { return func(c *GeminiConfig) { c.thinkingBudget = &v } }

// WithThinkingConfig sets the thinkingConfig object verbatim, for fields the
// level/budget pair does not cover (includeThoughts, thinkingLevel values of
// newer models, numTokens). When set it wins over WithThinkingLevel and
// WithThinkingBudget, exactly as Python resolves thinking_config first.
// The map is copied.
func WithThinkingConfig(raw map[string]any) Option {
	return func(c *GeminiConfig) { c.thinkingConfig = cloneAnyMap(raw) }
}

// WithCachedContent names a content cache created through the Caches API; the
// name travels as the top-level cachedContent of generateContent. An empty
// name keeps the field off the wire.
func WithCachedContent(name string) Option {
	return func(c *GeminiConfig) { c.cachedContent = name }
}

// WithResponseModalities selects the output kinds, e.g. "TEXT" and "IMAGE"
// (Python parity: response_modalities).
func WithResponseModalities(modalities ...string) Option {
	return func(c *GeminiConfig) { c.responseModalities = append([]string(nil), modalities...) }
}

// WithImageConfig tunes image generation; it only takes effect together with
// an "IMAGE" response modality.
func WithImageConfig(cfg ImageConfig) Option {
	return func(c *GeminiConfig) { c.imageConfig = &cfg }
}

// ImageConfig is the image-generation block of generationConfig (Python
// parity: google.genai.types.ImageConfig). Every zero field stays off the
// wire; the names match Python's snake_case fields, the JSON keys are the
// REST camelCase ones.
type ImageConfig struct {
	// AspectRatio is one of "1:1", "2:3", "3:2", "3:4", "4:3", "9:16",
	// "16:9", "21:9".
	AspectRatio string
	// ImageSize is "1K", "2K" or "4K".
	ImageSize string
	// PersonGeneration is ALLOW_ALL, ALLOW_ADULT or ALLOW_NONE.
	PersonGeneration string
	// ProminentPeople controls celebrity generation (Vertex AI only).
	ProminentPeople string
	// OutputMimeType is the MIME type of the generated images.
	OutputMimeType string
	// OutputCompressionQuality is the JPEG/WebP quality (Vertex AI only).
	OutputCompressionQuality *int64
	// ImageOutputOptions carries the output image format (Vertex AI only).
	ImageOutputOptions map[string]any
}

// wire renders the block under the REST aliases; unset fields are omitted.
func (i ImageConfig) wire() map[string]any {
	out := map[string]any{}
	set := func(key, value string) {
		if value != "" {
			out[key] = value
		}
	}
	set("aspectRatio", i.AspectRatio)
	set("imageSize", i.ImageSize)
	set("personGeneration", i.PersonGeneration)
	set("prominentPeople", i.ProminentPeople)
	set("outputMimeType", i.OutputMimeType)
	if i.OutputCompressionQuality != nil {
		out["outputCompressionQuality"] = *i.OutputCompressionQuality
	}
	if len(i.ImageOutputOptions) > 0 {
		out["imageOutputOptions"] = i.ImageOutputOptions
	}
	return out
}

// WithHTTPClient routes every request through c (proxy, custom root CAs,
// mTLS, test transports). The client owns its own Timeout and redirect
// policy. Nil keeps the default client.
func WithHTTPClient(c *http.Client) Option { return func(cfg *GeminiConfig) { cfg.httpClient = c } }

// WithAutomaticContinuation toggles the automatic resumption of answers the
// server cut off at its per-request token limit. The flag never travels on the
// wire; it only drives the client-side hop loop (Python parity:
// automatic_continuation, default enabled).
func WithAutomaticContinuation(v bool) Option {
	return func(c *GeminiConfig) { c.automaticContinuation = &v }
}

// continuationEnabled reports whether truncated answers are resumed
// (Python parity: should_enable_automatic_continuation — unset means enabled).
func (c *GeminiConfig) continuationEnabled() bool {
	return c.automaticContinuation == nil || *c.automaticContinuation
}

// NewGemini builds a config for one Gemini model.
func NewGemini(apiKey, model string, opts ...Option) *GeminiConfig {
	c := &GeminiConfig{model: model, apiKey: apiKey, baseURL: DefaultBaseURL}
	for _, o := range opts {
		o(c)
	}
	return c
}

// Provider names the vendor (Python parity: ModelProvider.GEMINI).
func (c *GeminiConfig) Provider() config.Provider { return config.ProviderGemini }

// Model returns the model identifier sent to the API.
func (c *GeminiConfig) Model() string { return c.model }

// Create resolves the env fallbacks and builds a ready-to-call client
// (Python parity: a fresh client per create()).
func (c *GeminiConfig) Create() (config.LLMClient, error) {
	if c.model == "" {
		return nil, fmt.Errorf("%w: gemini model", config.ErrNotProvided)
	}
	key := c.apiKey
	if key == "" {
		key = os.Getenv(EnvAPIKey)
	}
	if key == "" {
		return nil, fmt.Errorf("%w: gemini api key (set %s)", config.ErrNotProvided, EnvAPIKey)
	}
	base := c.baseURL
	if base == "" {
		base = DefaultBaseURL
	}
	// No transport retries: the Python google-genai SDK posts plain httpx
	// without retry logic; callers cancel via ctx.
	return &client{
		cfg:     *c,
		apiKey:  key,
		baseURL: base,
		http:    &httpx.Client{HTTP: c.httpClient},
	}, nil
}

// Clone returns a copy with overrides applied (Python parity:
// dataclasses.replace). Keys are the Python field names; values keep the
// config's field types (*float64/*int64 or the plain scalars, nil clears the
// field). Unknown keys are an error.
func (c *GeminiConfig) Clone(overrides map[string]any) (config.ModelConfig, error) {
	cc := *c
	cc.stopSequences = append([]string(nil), c.stopSequences...)
	cc.temperature = cloneFloat(c.temperature)
	cc.topP = cloneFloat(c.topP)
	cc.topK = cloneInt(c.topK)
	cc.maxOutputTokens = cloneInt(c.maxOutputTokens)
	cc.presencePenalty = cloneFloat(c.presencePenalty)
	cc.frequencyPenalty = cloneFloat(c.frequencyPenalty)
	cc.seed = cloneInt(c.seed)
	cc.thinkingLevel = cloneString(c.thinkingLevel)
	cc.thinkingBudget = cloneInt(c.thinkingBudget)
	cc.thinkingConfig = cloneAnyMap(c.thinkingConfig)
	cc.responseModalities = append([]string(nil), c.responseModalities...)
	if c.imageConfig != nil {
		img := *c.imageConfig
		img.ImageOutputOptions = cloneAnyMap(c.imageConfig.ImageOutputOptions)
		cc.imageConfig = &img
	}
	cc.automaticContinuation = cloneBool(c.automaticContinuation)
	for k, v := range overrides {
		switch k {
		case "model":
			s, err := stringOverride(k, v)
			if err != nil {
				return nil, err
			}
			cc.model = s
		case "api_key":
			s, err := stringOverride(k, v)
			if err != nil {
				return nil, err
			}
			cc.apiKey = s
		case "base_url":
			s, err := stringOverride(k, v)
			if err != nil {
				return nil, err
			}
			cc.baseURL = s
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
		case "top_k":
			i, err := intOverride(k, v)
			if err != nil {
				return nil, err
			}
			cc.topK = i
		case "max_output_tokens":
			i, err := intOverride(k, v)
			if err != nil {
				return nil, err
			}
			cc.maxOutputTokens = i
		case "stop_sequences":
			ss, err := stringsOverride(k, v)
			if err != nil {
				return nil, err
			}
			cc.stopSequences = ss
		case "streaming":
			b, err := boolOverride(k, v)
			if err != nil {
				return nil, err
			}
			cc.streaming = b
		case "presence_penalty":
			f, err := floatOverride(k, v)
			if err != nil {
				return nil, err
			}
			cc.presencePenalty = f
		case "frequency_penalty":
			f, err := floatOverride(k, v)
			if err != nil {
				return nil, err
			}
			cc.frequencyPenalty = f
		case "seed":
			i, err := intOverride(k, v)
			if err != nil {
				return nil, err
			}
			cc.seed = i
		case "thinking_level":
			// AGO-DEVIATION: Python enforces the ThinkingLevel literal
			// statically; the dynamic Clone boundary checks the value instead
			// (D-035). A nil clears the field.
			if v == nil {
				cc.thinkingLevel = nil
			} else {
				s, err := stringOverride(k, v)
				if err != nil {
					return nil, err
				}
				if !validThinkingLevel(s) {
					return nil, fmt.Errorf("gemini: override %q must be one of \"low\", \"medium\", \"high\", got %q", k, s)
				}
				cc.thinkingLevel = &s
			}
		case "thinking_budget":
			i, err := intOverride(k, v)
			if err != nil {
				return nil, err
			}
			cc.thinkingBudget = i
		case "thinking_config":
			m, err := anyMapOverride(k, v)
			if err != nil {
				return nil, err
			}
			cc.thinkingConfig = m
		case "cached_content":
			s, err := stringOverride(k, v)
			if err != nil {
				return nil, err
			}
			cc.cachedContent = s
		case "response_modalities":
			ss, err := stringsOverride(k, v)
			if err != nil {
				return nil, err
			}
			cc.responseModalities = ss
		case "image_config":
			img, err := imageConfigOverride(k, v)
			if err != nil {
				return nil, err
			}
			cc.imageConfig = img
		case "http_client":
			switch h := v.(type) {
			case nil:
				cc.httpClient = nil
			case *http.Client:
				cc.httpClient = h
			default:
				return nil, overrideErr(k, v)
			}
		case "automatic_continuation":
			b, err := boolPtrOverride(k, v)
			if err != nil {
				return nil, err
			}
			cc.automaticContinuation = b
		default:
			return nil, fmt.Errorf("gemini: unknown config override %q", k)
		}
	}
	return &cc, nil
}

// --- override value coercion (Python parity: replace() type errors) ----------

func overrideErr(key string, v any) error {
	return fmt.Errorf("gemini: override %q expects the config field type, got %T", key, v)
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

// anyMapOverride accepts an object override and copies it, so a cloned config
// never shares a mutable map with the caller.
func anyMapOverride(key string, v any) (map[string]any, error) {
	switch m := v.(type) {
	case nil:
		return nil, nil
	case map[string]any:
		return cloneAnyMap(m), nil
	default:
		return nil, overrideErr(key, v)
	}
}

// imageConfigOverride accepts the typed block or its pointer (Python parity:
// replace() takes types.ImageConfig); nil clears it.
func imageConfigOverride(key string, v any) (*ImageConfig, error) {
	switch img := v.(type) {
	case nil:
		return nil, nil
	case ImageConfig:
		c := img
		c.ImageOutputOptions = cloneAnyMap(img.ImageOutputOptions)
		return &c, nil
	case *ImageConfig:
		if img == nil {
			return nil, nil
		}
		c := *img
		c.ImageOutputOptions = cloneAnyMap(img.ImageOutputOptions)
		return &c, nil
	default:
		return nil, overrideErr(key, v)
	}
}

func cloneAnyMap(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
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

// boolPtrOverride keeps the three-state shape of a bool|None field: nil clears
// the override (back to the default), a bool or non-nil *bool sets it, and a
// nil *bool clears it too (boolOverride cannot express that: it maps a nil
// *bool to false).
func boolPtrOverride(key string, v any) (*bool, error) {
	switch b := v.(type) {
	case nil:
		return nil, nil
	case bool:
		return &b, nil
	case *bool:
		return cloneBool(b), nil
	default:
		return nil, overrideErr(key, v)
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

func cloneString(v *string) *string {
	if v == nil {
		return nil
	}
	c := *v
	return &c
}

func cloneBool(v *bool) *bool {
	if v == nil {
		return nil
	}
	c := *v
	return &c
}

// validThinkingLevel reports whether level is an accepted thinking level
// (Python parity: ThinkingLevel = Literal["low", "medium", "high"]).
func validThinkingLevel(level string) bool {
	return level == "low" || level == "medium" || level == "high"
}
