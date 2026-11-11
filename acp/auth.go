package acp

import (
	"crypto/subtle"
	"errors"
	"fmt"
)

// Authentication seam for the serve role (Python parity: ag2/acp/auth.py).
//
// The serve side owns the protocol hook — advertising methods at initialize
// and gating session operations until authenticate succeeds. The embedding
// application owns everything behind it: credentials, user identity,
// authorization scope. Local stdio needs none of this: the client launched
// the process, so the default is no provider, no advertised methods, and
// authenticate refused.
//
// The three method kinds the Python seam declares (AuthMethodAgent,
// EnvVarAuthMethod, TerminalAuthMethod) are all modelled — see AuthMethod — and
// DeclaredAuth advertises them; only the shared-token admission of
// StaticTokenAuth is validated inside this package, because an env-var or
// terminal method is satisfied outside the process (the client sets the
// variables or runs the login command). Interactive OAuth flows are not part
// of the ACP method union and stay out.
//
// The seam authenticates a connection, it does not identify a tenant:
// Authenticate returns no principal, and nothing it resolves reaches the
// turn. Every authenticated client drives the same agent and the same tools.
// Serve one tenant per served agent.

// ErrAuthenticationFailed reports a credential the provider rejected.
var ErrAuthenticationFailed = errors.New("acp: authentication failed")

// Auth method kinds (Python parity: the acp schema's AuthMethod union —
// AuthMethodAgent, EnvVarAuthMethod, TerminalAuthMethod; the adapter's
// AuthMethod type alias is exactly those three).
//
// The wire carries the kind in "type"; an empty Type is the agent kind, which
// is also the shape every AuthMethod had before the field existed, so a
// provider written against the old struct still advertises what it used to.
const (
	// AuthKindAgent is a method the agent itself validates (a shared token,
	// a caller-supplied credential).
	AuthKindAgent = "agent"
	// AuthKindEnvVar is a method satisfied by environment variables the
	// client launches the agent with: the client must set Vars before
	// starting the process (an editor plugin or a registry launcher).
	AuthKindEnvVar = "env_var"
	// AuthKindTerminal is a method the client satisfies by running an
	// interactive command (a login CLI) with Args.
	AuthKindTerminal = "terminal"
)

// AuthEnvVar is one environment variable an env-var auth method needs
// (parity: the acp schema's AuthEnvVar). Secret marks a value the client must
// not echo or log.
type AuthEnvVar struct {
	Name   string `json:"name"`
	Label  string `json:"label,omitempty"`
	Secret bool   `json:"secret,omitempty"`
}

// AuthMethod is one way to authenticate, advertised in the initialize
// response (Python parity: the acp schema's AuthMethod union). Type selects the
// kind; Vars carries an env-var method's variables and Args a terminal
// method's command arguments, while an agent method uses neither.
type AuthMethod struct {
	ID          string       `json:"id"`
	Name        string       `json:"name"`
	Description string       `json:"description,omitempty"`
	Type        string       `json:"type,omitempty"` // "" ≡ AuthKindAgent
	Vars        []AuthEnvVar `json:"vars,omitempty"`
	Args        []string     `json:"args,omitempty"`
}

// NewAgentAuthMethod builds an agent-validated method (the kind
// StaticTokenAuth advertises).
func NewAgentAuthMethod(id, name, description string) AuthMethod {
	return AuthMethod{ID: id, Name: name, Description: description, Type: AuthKindAgent}
}

// NewEnvVarAuthMethod builds a method the client satisfies by setting
// environment variables before launching the agent.
func NewEnvVarAuthMethod(id, name, description string, vars ...AuthEnvVar) AuthMethod {
	return AuthMethod{ID: id, Name: name, Description: description, Type: AuthKindEnvVar, Vars: vars}
}

// NewTerminalAuthMethod builds a method the client satisfies by running an
// interactive command (args are the command and its arguments).
func NewTerminalAuthMethod(id, name, description string, args ...string) AuthMethod {
	return AuthMethod{ID: id, Name: name, Description: description, Type: AuthKindTerminal, Args: args}
}

// Kind resolves the declared kind ("" ≡ AuthKindAgent).
func (m AuthMethod) Kind() string {
	if m.Type == "" {
		return AuthKindAgent
	}
	return m.Type
}

// DeclaredAuth is an Authenticator that advertises a fixed method list and
// delegates the admission decision: the shape an embedding application uses to
// offer env-var or terminal methods (which AGo cannot validate itself — the
// client's environment or login command is outside the process) alongside
// agent-validated ones. Validate is called for every method, including the
// env-var and terminal kinds: an application that only wants to advertise them
// accepts their ids there and refuses the rest.
type DeclaredAuth struct {
	// Advertised is the method list, in order.
	Advertised []AuthMethod
	// Validate checks one call. Nil refuses every method.
	Validate func(methodID string, meta map[string]any) error
}

// Methods returns the advertised methods.
func (d *DeclaredAuth) Methods() []AuthMethod {
	return append([]AuthMethod(nil), d.Advertised...)
}

// Authenticate validates the call through the configured function.
func (d *DeclaredAuth) Authenticate(methodID string, meta map[string]any) error {
	if d.Validate == nil {
		return fmt.Errorf("%w: no authentication method is accepted", ErrAuthenticationFailed)
	}
	return d.Validate(methodID, meta)
}

var _ Authenticator = (*DeclaredAuth)(nil)

// Authenticator is the auth provider (Python parity: auth.AuthProvider): it
// advertises methods and validates an authenticate call. Returning an error
// rejects the connection; the adapter surfaces it as the ACP
// authentication-required error. An error here is an admission gate, never an
// identity lookup — no value from it is carried into a turn.
type Authenticator interface {
	// Methods are the methods advertised in the initialize response.
	Methods() []AuthMethod
	// Authenticate validates an authenticate request: methodID names the
	// advertised method, meta is the request's _meta map (where the SDK
	// spreads caller-supplied credentials). Return nil to accept.
	Authenticate(methodID string, meta map[string]any) error
}

// StaticTokenAuth is the minimal Authenticator checking a shared secret
// (Python parity: auth.StaticTokenAuth). Intended for tests and single-tenant
// deployments where the client and agent are configured together: one
// credential, it never expires, it identifies no user.
type StaticTokenAuth struct {
	token       string
	methodID    string
	description string
}

// NewStaticTokenAuth builds a provider checking token, offered as method
// "token" (Python parity: the constructor's default method_id). A token is
// required: an empty one would advertise an auth method that accepts
// anything.
func NewStaticTokenAuth(token string) (*StaticTokenAuth, error) {
	if token == "" {
		return nil, fmt.Errorf("%w: token must be a non-empty string", ErrAuthenticationFailed)
	}
	return &StaticTokenAuth{
		token:       token,
		methodID:    "token",
		description: "Shared token configured on both the Client and the Agent.",
	}, nil
}

// Methods advertises the single shared-token method.
func (a *StaticTokenAuth) Methods() []AuthMethod {
	return []AuthMethod{NewAgentAuthMethod(a.methodID, "Shared token", a.description)}
}

// Authenticate validates the method id and the token. The token travels in
// the request's metadata: as a "token" entry, or nested under a "meta" entry
// (Python parity: kwargs.get("token") or kwargs.get("meta", {}).get("token")
// — the SDK spreads the request's _meta into the provider's kwargs).
func (a *StaticTokenAuth) Authenticate(methodID string, meta map[string]any) error {
	if methodID != a.methodID {
		return fmt.Errorf("%w: unknown authentication method: %q", ErrAuthenticationFailed, methodID)
	}
	supplied, _ := meta["token"].(string)
	if supplied == "" {
		if nested, ok := meta["meta"].(map[string]any); ok {
			supplied, _ = nested["token"].(string)
		}
	}
	// Constant-time: a plain == on the supplied token would leak how many
	// leading bytes matched through response timing (same rationale as the
	// agui interrupt token check).
	if subtle.ConstantTimeCompare([]byte(supplied), []byte(a.token)) != 1 {
		return fmt.Errorf("%w: invalid token", ErrAuthenticationFailed)
	}
	return nil
}
