package agui

import "github.com/luckystrker/ago/config"

// The provider a run is served from, in AGo's vocabulary and in AG-UI's
// (Python parity: ag_ui/provider.py).
//
// AG-UI names a file handle's provider by vendor ("openai", "anthropic",
// "google"); AGo names the API, and Gemini and Vertex AI are both Google's.

// geminiFamily reports whether calls through p carry a Gemini thought
// signature: Vertex AI runs on the same client (Python parity:
// provider.GEMINI_FAMILY).
func geminiFamily(p config.Provider) bool {
	return p == config.ProviderGemini || p == config.ProviderVertexAI
}

// vendorOf maps an AGo provider onto the vendor name AG-UI uses for a file
// handle, or "" when the two vocabularies coincide.
func vendorOf(p config.Provider) string {
	switch p {
	case config.ProviderGemini, config.ProviderVertexAI:
		return "google"
	}
	return ""
}

// isSameProvider reports whether a file handle tagged tag was issued by the
// run's provider, in either vocabulary (Python parity: provider.is_same_provider).
// A run with no readable provider matches nothing: a handle it cannot verify
// is someone else's.
func isSameProvider(tag string, provider config.Provider) bool {
	if tag == "" || provider == "" {
		return false
	}
	return tag == string(provider) || tag == vendorOf(provider)
}

// providerOf reads the provider a model config serves from; false when the
// config is nil or does not say (Python parity: provider.provider_of, which
// catches NotImplementedError).
func providerOf(cfg config.ModelConfig) (config.Provider, bool) {
	if cfg == nil {
		return "", false
	}
	p := cfg.Provider()
	if p == "" {
		return "", false
	}
	return p, true
}
