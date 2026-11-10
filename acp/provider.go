package acp

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/luckystrker/ago/config"
)

// The ACP provider: an external ACP agent (Claude Code, Codex, OpenCode, Kilo
// Code, ...) as an AGo agent's *model* (Python parity: acp/config.py ACPConfig
// and acp/client.py ACPClient). The agent runs as a subprocess speaking ACP
// over stdio; one model turn is one session/prompt, during which the agent
// runs its own tool loop, and session/update notifications stream onto the
// run's event stream. Config is the config.ModelConfig; Create returns the
// config.LLMClient, whose live ACP session is kept per conversation stream in
// the config's registry and reused across the run's model turns.

// ProviderACP names this provider on ModelResponse.Provider and usage events.
const ProviderACP config.Provider = "acp"

// PermissionPolicy decides how session/request_permission is answered.
type PermissionPolicy string

const (
	// PermissionAsk (the default) routes the question to the run's human
	// input hook (Context.Input).
	PermissionAsk PermissionPolicy = "ask"
	// PermissionAuto allows every request.
	PermissionAuto PermissionPolicy = "auto"
	// PermissionDeny rejects every request.
	PermissionDeny PermissionPolicy = "deny"
)

// ElicitationPolicy decides how elicitation/create is answered. It is
// deliberately two-valued: there is no "auto", because nothing may answer a
// question on the user's behalf.
type ElicitationPolicy string

const (
	// ElicitationAsk (the default) routes the question to the run's human
	// input hook.
	ElicitationAsk ElicitationPolicy = "ask"
	// ElicitationDecline declines every question.
	ElicitationDecline ElicitationPolicy = "decline"
)

// Provider-level errors. Match with errors.Is.
var (
	// ErrTransport reports a connection to the agent that broke while a turn
	// was in flight (the process crashed or closed its pipes). The error is a
	// *TransportError. A turn that fails this way has no answer and the
	// session is gone; a later turn starts a new one (Python parity:
	// ACPTransportError).
	ErrTransport = errors.New("acp: agent connection failed")
	// ErrChannelFailed reports a turn ended because something had to ask a
	// person — a permission, a question, a tool — and no human input hook was
	// reachable. The original hitl error is wrapped alongside.
	ErrChannelFailed = errors.New("acp: human input channel failed during the turn")
	// ErrModelNotOffered reports a Config.ModelName the agent's model picker
	// does not offer (Python parity: select_model's ValueError).
	ErrModelNotOffered = errors.New("acp: model not offered by the agent")
)

// TransportError is the typed failure of a dropped agent connection.
type TransportError struct {
	// Transport names the transport ("stdio").
	Transport string
	// Err is the underlying failure.
	Err error
}

func (e *TransportError) Error() string {
	return fmt.Sprintf("the ACP %s connection to the agent failed during the turn (%v); the turn has no answer",
		e.Transport, e.Err)
}

// Unwrap lets errors.Is match both ErrTransport and the cause.
func (e *TransportError) Unwrap() []error { return []error{ErrTransport, e.Err} }

// Defaults of the zero Config fields (Python parity: ACPConfig defaults).
const (
	defaultStartupTimeout = 30 * time.Second
	defaultCancelTimeout  = 5 * time.Second
)

// envWhitelist is the part of this process's environment a launched agent
// inherits (Python parity: the ACP SDK's default_environment); everything
// else, API keys included, has to be passed in Config.Env explicitly.
var envWhitelist = []string{"HOME", "LOGNAME", "PATH", "SHELL", "TERM", "USER"}

// Config drives a CLI coding agent over ACP, launching it as a local
// subprocess. It implements config.ModelConfig. Build one with a preset
// (ClaudeCodeConfig, CodexConfig, OpenCodeConfig, KiloCodeConfig) or fill
// Command yourself, set the optional fields, and hand it to agent.WithConfig.
//
// A Config owns the live ACP sessions started from it — one per conversation
// stream — and is safe for concurrent use; do not copy it after first use
// (Clone returns a distinct config with its own sessions). Close ends every
// session and its subprocess: a session outlives the Ask that created it, so
// the config's scope, not the run's, is the conversation's lifetime.
//
// AGO-DEVIATION: Python names two switches allow_terminal / expose_tools with
// default True. Go's zero value is the useful configuration, so they are
// DisableTerminal / DisableTools (false by default, same behavior).
//
// AGO-DEVIATION: Python reclaims a session whose stream was garbage-collected
// (weakref.finalize); Go has no such hook, so a Config must be Closed.
//
// AGO-DEVIATION: only the local-subprocess configuration is ported. The
// remote transports (ACPRemoteConfig over HTTP/WebSocket) and a non-loopback
// tool gateway address stay deferred (D-070).
type Config struct {
	// Command is the executable and its base arguments launching the agent in
	// ACP mode, e.g. {"claude-agent-acp"}; the first element is the
	// executable. Required.
	Command []string
	// CWD is the workspace root passed to session/new and the subprocess's
	// working directory; "" means ".". It is made absolute.
	CWD string
	// Env adds environment variables for the subprocess. The subprocess does
	// NOT inherit the full parent environment: only HOME, LOGNAME, PATH,
	// SHELL, TERM and USER are, merged with this map — so API-key auth must be
	// passed here explicitly.
	Env map[string]string
	// ModelName selects the agent's model through session/set_config_option
	// when the agent advertises a model picker; it must be one of the ids the
	// agent offers. "" keeps the agent's default. Without a picker the value
	// is response metadata only.
	ModelName string
	// PermissionPolicy answers session/request_permission, and consents the
	// mediated fs writes and terminal launches; "" means PermissionAsk.
	PermissionPolicy PermissionPolicy
	// ElicitationPolicy answers elicitation/create; "" means ElicitationAsk.
	// An asked question reaches the human as plain text; form and URL modes
	// are not rendered, so the elicitation capability is not advertised.
	//
	// AGO-DEVIATION: Python advertises the form and url elicitation
	// capabilities and renders per-property prompts; the Bridge answers
	// message-only questions (D-052), so a conforming agent never asks in the
	// structured modes.
	ElicitationPolicy ElicitationPolicy
	// FSRoot is the root of the mediated fs/* access; "" means CWD.
	FSRoot string
	// DisableTerminal withholds the terminal capability.
	DisableTerminal bool
	// AdditionalDirectories are extra ACP workspace roots.
	AdditionalDirectories []string
	// StartupTimeout bounds the agent's initialize handshake and session/new;
	// 0 means 30s. A failed or hung start never leaves a subprocess behind.
	//
	// AGO-DEVIATION: Python's startup_timeout bounds only its tool-gateway
	// start; here the tool host starts synchronously, so the same knob bounds
	// the agent's handshake instead, where a hang actually happens.
	StartupTimeout time.Duration
	// TurnTimeout bounds one prompt turn; 0 means no limit. On expiry the
	// agent is asked to cancel; the turn then answers with finish reason
	// "timeout" and whatever text streamed.
	TurnTimeout time.Duration
	// CancelTimeout is the grace the agent gets after a cancel to return the
	// in-flight prompt; 0 means 5s. Past it the subprocess is killed.
	CancelTimeout time.Duration
	// DisableTools stops the run's tools being exposed to the agent: function
	// tools through the in-process MCP tool host, MCPServerTool entries as
	// declared MCP servers.
	DisableTools bool

	mu       sync.Mutex
	sessions map[string]*providerSession
}

var _ config.ModelConfig = (*Config)(nil)

// Provider implements config.ModelConfig.
func (c *Config) Provider() config.Provider { return ProviderACP }

// Model implements config.ModelConfig: the requested model id, "" when the
// agent's default applies.
func (c *Config) Model() string { return c.ModelName }

// Create implements config.ModelConfig. A Config without a Command wraps
// config.ErrNotProvided. The client starts nothing: the subprocess is
// launched lazily, on the first turn of a conversation.
func (c *Config) Create() (config.LLMClient, error) {
	if len(c.Command) == 0 || c.Command[0] == "" {
		return nil, fmt.Errorf("%w: acp.Config.Command", config.ErrNotProvided)
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &llmClient{cfg: c}, nil
}

func (c *Config) validate() error {
	switch c.PermissionPolicy {
	case "", PermissionAsk, PermissionAuto, PermissionDeny:
	default:
		return fmt.Errorf("acp: unknown permission policy %q", c.PermissionPolicy)
	}
	switch c.ElicitationPolicy {
	case "", ElicitationAsk, ElicitationDecline:
	default:
		return fmt.Errorf("acp: unknown elicitation policy %q", c.ElicitationPolicy)
	}
	return nil
}

// Clone implements config.ModelConfig: a copy with overrides applied and no
// sessions of its own. Keys are the field names, or Python's snake_case
// spelling: command, cwd, env, model, permission_policy, elicitation_policy,
// fs_root, allow_terminal, additional_directories, startup_timeout,
// turn_timeout, cancel_timeout, expose_tools (timeouts in seconds as
// float64/int, or a time.Duration).
func (c *Config) Clone(overrides map[string]any) (config.ModelConfig, error) {
	cp := &Config{
		Command: append([]string(nil), c.Command...), CWD: c.CWD, ModelName: c.ModelName,
		PermissionPolicy: c.PermissionPolicy, ElicitationPolicy: c.ElicitationPolicy,
		FSRoot: c.FSRoot, DisableTerminal: c.DisableTerminal,
		AdditionalDirectories: append([]string(nil), c.AdditionalDirectories...),
		StartupTimeout:        c.StartupTimeout, TurnTimeout: c.TurnTimeout, CancelTimeout: c.CancelTimeout,
		DisableTools: c.DisableTools,
	}
	if c.Env != nil {
		cp.Env = make(map[string]string, len(c.Env))
		for k, v := range c.Env {
			cp.Env[k] = v
		}
	}
	for k, v := range overrides {
		if err := cp.set(k, v); err != nil {
			return nil, err
		}
	}
	return cp, cp.validate()
}

func (c *Config) set(key string, v any) error {
	str := func(dst *string) error {
		if v == nil {
			*dst = ""
			return nil
		}
		s, ok := v.(string)
		if !ok {
			return fmt.Errorf("acp: override %q wants a string, got %T", key, v)
		}
		*dst = s
		return nil
	}
	strs := func(dst *[]string) error {
		if v == nil {
			*dst = nil
			return nil
		}
		s, ok := v.([]string)
		if !ok {
			return fmt.Errorf("acp: override %q wants []string, got %T", key, v)
		}
		*dst = append([]string(nil), s...)
		return nil
	}
	flag := func(dst *bool, invert bool) error {
		b, ok := v.(bool)
		if !ok {
			return fmt.Errorf("acp: override %q wants a bool, got %T", key, v)
		}
		*dst = b != invert
		return nil
	}
	dur := func(dst *time.Duration) error {
		switch d := v.(type) {
		case nil:
			*dst = 0
		case time.Duration:
			*dst = d
		case float64:
			*dst = time.Duration(d * float64(time.Second))
		case int:
			*dst = time.Duration(d) * time.Second
		default:
			return fmt.Errorf("acp: override %q wants seconds or a time.Duration, got %T", key, v)
		}
		return nil
	}
	switch key {
	case "Command", "command":
		return strs(&c.Command)
	case "CWD", "cwd":
		return str(&c.CWD)
	case "Env", "env":
		if v == nil {
			c.Env = nil
			return nil
		}
		m, ok := v.(map[string]string)
		if !ok {
			return fmt.Errorf("acp: override %q wants map[string]string, got %T", key, v)
		}
		c.Env = make(map[string]string, len(m))
		for k, val := range m {
			c.Env[k] = val
		}
	case "ModelName", "model":
		return str(&c.ModelName)
	case "PermissionPolicy", "permission_policy":
		var s string
		if err := str(&s); err != nil {
			return err
		}
		c.PermissionPolicy = PermissionPolicy(s)
	case "ElicitationPolicy", "elicitation_policy":
		var s string
		if err := str(&s); err != nil {
			return err
		}
		c.ElicitationPolicy = ElicitationPolicy(s)
	case "FSRoot", "fs_root":
		return str(&c.FSRoot)
	case "DisableTerminal":
		return flag(&c.DisableTerminal, false)
	case "allow_terminal":
		return flag(&c.DisableTerminal, true)
	case "AdditionalDirectories", "additional_directories":
		return strs(&c.AdditionalDirectories)
	case "StartupTimeout", "startup_timeout":
		return dur(&c.StartupTimeout)
	case "TurnTimeout", "turn_timeout":
		return dur(&c.TurnTimeout)
	case "CancelTimeout", "cancel_timeout":
		return dur(&c.CancelTimeout)
	case "DisableTools":
		return flag(&c.DisableTools, false)
	case "expose_tools":
		return flag(&c.DisableTools, true)
	default:
		return fmt.Errorf("acp: unknown config override %q", key)
	}
	return nil
}

// Close ends every live ACP session started from this config, stopping each
// subprocess and its tool host. Idempotent; the config can be used again
// afterwards (new sessions start lazily).
func (c *Config) Close() error {
	c.mu.Lock()
	sessions := c.sessions
	c.sessions = nil
	c.mu.Unlock()
	var errs []error
	for _, s := range sessions {
		if err := s.close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (c *Config) cwd() string {
	cwd := c.CWD
	if cwd == "" {
		cwd = "."
	}
	// ACP's session/new takes an absolute path.
	if abs, err := filepath.Abs(cwd); err == nil {
		return abs
	}
	return cwd
}

// launchSpec is the subprocess command line: executable, arguments and the
// complete environment (whitelisted parent variables, then Config.Env on top,
// sorted for a deterministic result).
func (c *Config) launchSpec() (name string, args []string, env []string) {
	name = c.Command[0]
	args = append([]string(nil), c.Command[1:]...)
	merged := map[string]string{}
	for _, k := range envWhitelist {
		if v, ok := os.LookupEnv(k); ok {
			merged[k] = v
		}
	}
	for k, v := range c.Env {
		merged[k] = v
	}
	keys := make([]string, 0, len(merged))
	for k := range merged {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		env = append(env, k+"="+merged[k])
	}
	return name, args, env
}

func (c *Config) agentLabel() string {
	if len(c.Command) > 0 && c.Command[0] != "" {
		return c.Command[0]
	}
	return "acp-agent"
}
