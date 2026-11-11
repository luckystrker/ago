package acp

// Presets: a Config carrying the launch command of a known ACP adapter
// (Python parity: ClaudeCodeConfig, CodexConfig, OpenCodeConfig,
// KiloCodeConfig). Each returns a fresh *Config to adjust before use — Env
// for API keys, ModelName, the policies — or to rebuild a launch line (set
// Command to {"npx", "-y", "@agentclientprotocol/claude-agent-acp"} to run an
// adapter without a global install). Only HOME, LOGNAME, PATH, SHELL, TERM and
// USER are inherited from this process (see Config.Env): a shell `export` of a
// key does not reach the agent, so pass it in Env.

// ClaudeCodeConfig launches the @agentclientprotocol/claude-agent-acp bin,
// which must be on PATH. Authenticate with ANTHROPIC_API_KEY in Env (billed
// per token), or through an existing Claude Code login under $HOME
// (~/.claude, or a directory named by CLAUDE_CONFIG_DIR in Env), which uses
// that login's plan. Select the model with ModelName (one of the adapter's
// advertised ids) or the adapter's ANTHROPIC_MODEL env var.
func ClaudeCodeConfig() *Config { return &Config{Command: []string{"claude-agent-acp"}} }

// CodexConfig launches the @agentclientprotocol/codex-acp bin, which must be
// on PATH. Authenticate with CODEX_API_KEY (takes precedence) or
// OPENAI_API_KEY in Env — billed per token — or with an existing `codex
// login` on the host (credentials under ~/.codex, inherited through $HOME),
// whose billing follows that login. Select the model with ModelName or the
// adapter's MODEL_PROVIDER env var.
func CodexConfig() *Config { return &Config{Command: []string{"codex-acp"}} }

// OpenCodeConfig launches `opencode acp`, which must be on PATH. Authenticate
// with `opencode auth login` (credentials under $HOME, inherited, so no Env is
// needed). Select the model with ModelName ("provider/model" as listed by
// `opencode models`) or in OpenCode's own config.
func OpenCodeConfig() *Config { return &Config{Command: []string{"opencode", "acp"}} }

// KiloCodeConfig launches `kilo acp`; the kilo CLI must be on PATH (install
// with `npm install -g @kilocode/cli`). Authenticate with `kilo auth login`
// (credentials under $HOME, so no Env is needed). Always set ModelName
// ("provider/model" as listed by `kilo models`, for example
// "kilo/anthropic/claude-haiku-4.5"): a fresh Kilo ACP session may default to
// a model that cannot do text, which ends every turn with an empty reply.
func KiloCodeConfig() *Config { return &Config{Command: []string{"kilo", "acp"}} }
