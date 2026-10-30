package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/luckystrker/ago/config/ollama"
)

// envCheck is the outcome of one provider environment probe.
type envCheck struct {
	Provider string // human-readable provider name
	EnvVar   string // environment variable the config reads
	Set      bool   // variable present and non-empty
	Value    string // shown only for non-secret vars; always "" for secrets
	Secret   bool   // API keys: the value must never be echoed
	Hint     string // which config consumes the variable
}

// checkProviderEnv probes the provider environment variables. getenv is
// injected so the check is testable without touching the real process
// environment. API keys are secrets: their values are never echoed, only
// their presence. OLLAMA_HOST is not secret, so its value — or the built-in
// default from the ollama config — is shown. No network access happens here;
// reachability of the Ollama host is deliberately not probed.
func checkProviderEnv(getenv func(string) string) []envCheck {
	return []envCheck{
		{
			Provider: "openai",
			EnvVar:   "OPENAI_API_KEY",
			Set:      getenv("OPENAI_API_KEY") != "",
			Secret:   true,
			Hint:     "github.com/luckystrker/ago/config/openai.NewOpenAICompletions(model)",
		},
		{
			Provider: "anthropic",
			EnvVar:   "ANTHROPIC_API_KEY",
			Set:      getenv("ANTHROPIC_API_KEY") != "",
			Secret:   true,
			Hint:     "github.com/luckystrker/ago/config/anthropic.NewAnthropic(model)",
		},
		{
			Provider: "gemini",
			EnvVar:   "GEMINI_API_KEY",
			Set:      getenv("GEMINI_API_KEY") != "",
			Secret:   true,
			Hint:     "github.com/luckystrker/ago/config/gemini.NewGemini(apiKey, model)",
		},
		{
			Provider: "ollama",
			EnvVar:   "OLLAMA_HOST",
			Set:      getenv("OLLAMA_HOST") != "",
			Value:    orDefault(getenv("OLLAMA_HOST"), ollama.DefaultHost),
			Hint:     "github.com/luckystrker/ago/config/ollama.NewOllama(model)",
		},
		{
			Provider: "zai",
			EnvVar:   "ZAI_API_KEY",
			Set:      getenv("ZAI_API_KEY") != "",
			Secret:   true,
			Hint:     "github.com/luckystrker/ago/config/zai.NewZAI(model)",
		},
		{
			Provider: "mistral",
			EnvVar:   "MISTRAL_API_KEY",
			Set:      getenv("MISTRAL_API_KEY") != "",
			Secret:   true,
			Hint:     "github.com/luckystrker/ago/config/mistral.NewMistral(model)",
		},
		{
			Provider: "dashscope",
			EnvVar:   "DASHSCOPE_API_KEY",
			Set:      getenv("DASHSCOPE_API_KEY") != "",
			Secret:   true,
			Hint:     "github.com/luckystrker/ago/config/dashscope.NewDashScope(model)",
		},
	}
}

func orDefault(v, def string) string {
	if v != "" {
		return v
	}
	return def
}

// doctor prints an environment report: this build's version, the go
// toolchain and the provider environment table. It is a report only — it
// never exits non-zero over missing keys.
func doctor(out io.Writer) {
	fmt.Fprintln(out, "ago doctor")
	fmt.Fprintln(out)

	const mark = "  ✓ "
	const cross = "  ✗ "

	label := "ago"
	detail := fmt.Sprintf("%s (%s/%s)", Version, runtime.GOOS, runtime.GOARCH)
	fmt.Fprintf(out, "%s%-14s %s\n", mark, label, detail)

	ok, detail := goToolchain()
	if ok {
		fmt.Fprintf(out, "%s%-14s %s\n", mark, "go toolchain", detail)
	} else {
		fmt.Fprintf(out, "%s%-14s %s\n", cross, "go toolchain", detail)
	}

	fmt.Fprintln(out)
	fmt.Fprintln(out, "  provider environment (presence only; no network calls):")

	checks := checkProviderEnv(os.Getenv)
	w := 0
	for _, c := range checks {
		if len(c.EnvVar) > w {
			w = len(c.EnvVar)
		}
	}
	for _, c := range checks {
		status := "not set"
		if c.Set {
			status = "set"
		}
		if !c.Secret {
			status = c.Value
			if !c.Set {
				status += " (default)"
			}
		}
		m := cross
		if c.Set {
			m = mark
		}
		fmt.Fprintf(out, "%s%-*s  %-9s  → %s\n", m, w, c.EnvVar, status, c.Hint)
	}
}

// goToolchain runs "go version" and reports the toolchain string. The lookup
// failure and the run failure are both reported as not-ok with the reason.
func goToolchain() (ok bool, detail string) {
	out, err := exec.Command("go", "version").Output()
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return false, "go not found in PATH — install Go 1.24+ to build AGo"
		}
		return false, fmt.Sprintf("go version failed: %v", err)
	}
	return true, strings.TrimSpace(string(out))
}
