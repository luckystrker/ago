// Command ago is the command-line helper for AGo: a version banner, an
// environment doctor for provider API keys and a fully offline scripted demo
// of the agent turn loop. Arguments are parsed by hand; the module carries no
// external dependencies.
//
// Usage:
//
//	ago version  — print the build version and platform
//	ago doctor   — check the environment (go toolchain, provider env vars)
//	ago demo     — offline agent run with a tool call (scripted model, no network)
//	ago help     — this text
//
// The version string is stamped at build time:
//
//	go build -ldflags "-X main.Version=v0.5.0" ./cmd/ago
package main

import (
	"fmt"
	"io"
	"os"
	"runtime"
)

// Version is the build stamp; "dev" unless overridden with
// -ldflags "-X main.Version=vX.Y.Z" (install.sh and CI pass it in).
var Version = "dev"

func main() {
	args := os.Args[1:]
	if len(args) == 0 {
		printUsage(os.Stdout)
		return
	}
	switch args[0] {
	case "version":
		printVersion(os.Stdout)
	case "doctor":
		doctor(os.Stdout)
	case "demo":
		if err := demo(os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "ago demo:", err)
			os.Exit(1)
		}
	case "help", "-h", "--help":
		printUsage(os.Stdout)
	default:
		fmt.Fprintf(os.Stderr, "ago: unknown command %q\n\n", args[0])
		printUsage(os.Stderr)
		os.Exit(2)
	}
}

func printVersion(out io.Writer) {
	fmt.Fprintf(out, "ago %s (%s/%s)\n", Version, runtime.GOOS, runtime.GOARCH)
}

func printUsage(out io.Writer) {
	fmt.Fprint(out, `ago — command-line helper for the AGo framework

Usage:

  ago            show this help
  ago version    print build version and platform
  ago doctor     check the environment: go toolchain, provider API keys
  ago demo       offline scripted agent run with a tool call (no network)
  ago help       show this help

Provider credentials live in the environment; "ago doctor" lists which
variable each config reads (OPENAI_API_KEY, ANTHROPIC_API_KEY,
GEMINI_API_KEY, OLLAMA_HOST, ZAI_API_KEY, MISTRAL_API_KEY,
DASHSCOPE_API_KEY).

The version is stamped at build time:
  go build -ldflags "-X main.Version=vX.Y.Z" ./cmd/ago
`)
}
