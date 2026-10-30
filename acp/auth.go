package acp

import (
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
// AGO-DEVIATION: the Python seam's terminal/OAuth method kinds
// (EnvVarAuthMethod, TerminalAuthMethod) are deferred — only the shared-token
// admission of StaticTokenAuth is ported, which is the env-key/secret case
// (docs/DECISIONS.md D-066).
//
// The seam authenticates a connection, it does not identify a tenant:
// Authenticate returns no principal, and nothing it resolves reaches the
// turn. Every authenticated client drives the same agent and the same tools.
// Serve one tenant per served agent.

// ErrAuthenticationFailed reports a credential the provider rejected.
var ErrAuthenticationFailed = errors.New("acp: authentication failed")

// AuthMethod is one way to authenticate, advertised in the initialize
// response (Python parity: the acp schema AuthMethodAgent — id, name, an
// optional description; the env-var/terminal kinds are deferred).
type AuthMethod struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

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
	return []AuthMethod{{ID: a.methodID, Name: "Shared token", Description: a.description}}
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
	if supplied != a.token {
		return fmt.Errorf("%w: invalid token", ErrAuthenticationFailed)
	}
	return nil
}
