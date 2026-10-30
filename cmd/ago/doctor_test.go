package main

import (
	"testing"

	"github.com/luckystrker/ago/config/ollama"
)

func fakeGetenv(env map[string]string) func(string) string {
	return func(key string) string { return env[key] }
}

// wantOrder matches the report order the ask fixes: openai, anthropic,
// gemini, ollama, zai, mistral, dashscope.
var wantOrder = []string{
	"OPENAI_API_KEY",
	"ANTHROPIC_API_KEY",
	"GEMINI_API_KEY",
	"OLLAMA_HOST",
	"ZAI_API_KEY",
	"MISTRAL_API_KEY",
	"DASHSCOPE_API_KEY",
}

func TestCheckProviderEnv(t *testing.T) {
	allKeys := map[string]string{
		"OPENAI_API_KEY":    "sk-openai-test",
		"ANTHROPIC_API_KEY": "sk-ant-test",
		"GEMINI_API_KEY":    "gm-test",
		"OLLAMA_HOST":       "http://192.168.1.10:11434",
		"ZAI_API_KEY":       "zai-test",
		"MISTRAL_API_KEY":   "mistral-test",
		"DASHSCOPE_API_KEY": "dash-test",
	}

	tests := []struct {
		name      string
		env       map[string]string
		wantSet   map[string]bool
		wantValue map[string]string // non-secret vars only (OLLAMA_HOST)
	}{
		{
			name: "nothing set: no key present, ollama falls back to the default host",
			env:  map[string]string{},
			wantSet: map[string]bool{
				"OPENAI_API_KEY":    false,
				"ANTHROPIC_API_KEY": false,
				"GEMINI_API_KEY":    false,
				"OLLAMA_HOST":       false,
				"ZAI_API_KEY":       false,
				"MISTRAL_API_KEY":   false,
				"DASHSCOPE_API_KEY": false,
			},
			wantValue: map[string]string{"OLLAMA_HOST": ollama.DefaultHost},
		},
		{
			name: "every provider variable set",
			env:  allKeys,
			wantSet: map[string]bool{
				"OPENAI_API_KEY":    true,
				"ANTHROPIC_API_KEY": true,
				"GEMINI_API_KEY":    true,
				"OLLAMA_HOST":       true,
				"ZAI_API_KEY":       true,
				"MISTRAL_API_KEY":   true,
				"DASHSCOPE_API_KEY": true,
			},
			wantValue: map[string]string{"OLLAMA_HOST": "http://192.168.1.10:11434"},
		},
		{
			name: "only ollama host set",
			env:  map[string]string{"OLLAMA_HOST": "http://box.lan:11434"},
			wantSet: map[string]bool{
				"OPENAI_API_KEY":    false,
				"ANTHROPIC_API_KEY": false,
				"GEMINI_API_KEY":    false,
				"OLLAMA_HOST":       true,
				"ZAI_API_KEY":       false,
				"MISTRAL_API_KEY":   false,
				"DASHSCOPE_API_KEY": false,
			},
			wantValue: map[string]string{"OLLAMA_HOST": "http://box.lan:11434"},
		},
		{
			name: "empty string counts as unset",
			env:  map[string]string{"OPENAI_API_KEY": ""},
			wantSet: map[string]bool{
				"OPENAI_API_KEY":    false,
				"ANTHROPIC_API_KEY": false,
				"GEMINI_API_KEY":    false,
				"OLLAMA_HOST":       false,
				"ZAI_API_KEY":       false,
				"MISTRAL_API_KEY":   false,
				"DASHSCOPE_API_KEY": false,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := checkProviderEnv(fakeGetenv(tt.env))

			if len(got) != len(wantOrder) {
				t.Fatalf("len(checks) = %d, want %d", len(got), len(wantOrder))
			}
			for i, c := range got {
				if c.EnvVar != wantOrder[i] {
					t.Errorf("check[%d].EnvVar = %q, want %q", i, c.EnvVar, wantOrder[i])
				}
				if c.Hint == "" {
					t.Errorf("%s: Hint is empty", c.EnvVar)
				}
			}

			byVar := map[string]envCheck{}
			for _, c := range got {
				byVar[c.EnvVar] = c
			}
			for envVar, want := range tt.wantSet {
				c := byVar[envVar]
				if c.Set != want {
					t.Errorf("%s: Set = %v, want %v", envVar, c.Set, want)
				}
				if c.Secret && c.Value != "" {
					t.Errorf("%s: secret value must never be echoed, got %q", envVar, c.Value)
				}
				if wantVal, ok := tt.wantValue[envVar]; ok && c.Value != wantVal {
					t.Errorf("%s: Value = %q, want %q", envVar, c.Value, wantVal)
				}
			}

			// The API-key values from the fixture must not leak anywhere.
			for envVar, c := range byVar {
				if c.Secret {
					for _, v := range allKeys {
						if c.Value == v {
							t.Errorf("%s: leaked fixture value %q", envVar, v)
						}
					}
				}
			}
		})
	}
}
