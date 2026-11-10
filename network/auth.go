package network

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
)

// ErrAuth means a passport's auth claim failed validation (Python
// AuthError). Every adapter and registry failure wraps it.
var ErrAuth = errors.New("network: authentication failed")

// AuthSchemeNone is the scheme of a passport with no auth block (NoAuth).
const AuthSchemeNone = "none"

// AuthAdapter validates a passport's auth claim at registration (and at a
// future connection handshake). Implementations must be safe for concurrent
// use.
type AuthAdapter interface {
	// Scheme is the registry key matched against Passport.Auth["scheme"].
	Scheme() string
	// Validate returns nil when claim proves the passport's identity and an
	// error wrapping ErrAuth otherwise. The hub keeps no claims, so an
	// adapter must check claim against its own configured secrets, never
	// against the passport.
	Validate(ctx context.Context, p Passport, claim map[string]any) error
}

// NoAuth accepts every claim; it is the default registry's only entry.
type NoAuth struct{}

// Scheme implements AuthAdapter.
func (NoAuth) Scheme() string { return AuthSchemeNone }

// Validate implements AuthAdapter.
func (NoAuth) Validate(context.Context, Passport, map[string]any) error { return nil }

// AuthSchemeAPIKey is the scheme of ApiKeyAuth.
const AuthSchemeAPIKey = "api_key"

// KeyResolver looks a passport name up in a dynamic key store (database,
// secret manager). ok=false means the name is unknown.
type KeyResolver func(ctx context.Context, name string) (token string, ok bool)

// ApiKeyAuth compares a claim's "token" against the key registered for the
// passport name, in constant time. Keys come from the static map first, then
// the optional resolver. With no keys and no resolver every validation fails.
type ApiKeyAuth struct {
	keys     map[string]string
	resolver KeyResolver
}

// ApiKeyOption configures NewApiKeyAuth.
type ApiKeyOption func(*ApiKeyAuth)

// WithKeyResolver adds a dynamic key lookup consulted when the static map has
// no entry for the name.
func WithKeyResolver(fn KeyResolver) ApiKeyOption {
	return func(a *ApiKeyAuth) { a.resolver = fn }
}

// NewApiKeyAuth builds an api-key adapter over a name → token map (copied).
func NewApiKeyAuth(keys map[string]string, opts ...ApiKeyOption) *ApiKeyAuth {
	a := &ApiKeyAuth{keys: make(map[string]string, len(keys))}
	for k, v := range keys {
		a.keys[k] = v
	}
	for _, opt := range opts {
		opt(a)
	}
	return a
}

// Scheme implements AuthAdapter.
func (*ApiKeyAuth) Scheme() string { return AuthSchemeAPIKey }

// Validate implements AuthAdapter.
func (a *ApiKeyAuth) Validate(ctx context.Context, p Passport, claim map[string]any) error {
	token, _ := claim["token"].(string)
	if token == "" {
		return fmt.Errorf("%w: api_key claim missing required string field 'token'", ErrAuth)
	}
	expected, ok := a.keys[p.Name]
	if !ok && a.resolver != nil {
		expected, ok = a.resolver(ctx, p.Name)
	}
	if !ok {
		return fmt.Errorf("%w: no api_key registered for %q", ErrAuth, p.Name)
	}
	// Constant-time compare so rejection latency does not reveal how much of
	// the token matched.
	if subtle.ConstantTimeCompare([]byte(expected), []byte(token)) != 1 {
		return fmt.Errorf("%w: api_key mismatch for %q", ErrAuth, p.Name)
	}
	return nil
}

// AuthRegistry maps scheme strings to adapters. It is immutable after
// construction and therefore safe for concurrent use.
type AuthRegistry struct {
	adapters map[string]AuthAdapter
	order    []string
}

// NewAuthRegistry builds a registry; a later adapter with the same scheme
// replaces an earlier one. Pass NoAuth{} explicitly when passports without
// credentials must stay registerable.
func NewAuthRegistry(adapters ...AuthAdapter) *AuthRegistry {
	r := &AuthRegistry{adapters: make(map[string]AuthAdapter, len(adapters))}
	for _, a := range adapters {
		if _, dup := r.adapters[a.Scheme()]; !dup {
			r.order = append(r.order, a.Scheme())
		}
		r.adapters[a.Scheme()] = a
	}
	return r
}

// DefaultAuthRegistry is the NoAuth-only registry a hub uses unless
// WithHubAuth says otherwise.
func DefaultAuthRegistry() *AuthRegistry { return NewAuthRegistry(NoAuth{}) }

// Get resolves a scheme; unknown schemes fail with ErrAuth.
func (r *AuthRegistry) Get(scheme string) (AuthAdapter, error) {
	a, ok := r.adapters[scheme]
	if !ok {
		return nil, fmt.Errorf("%w: unknown auth scheme %q", ErrAuth, scheme)
	}
	return a, nil
}

// Schemes lists the registered schemes in registration order.
func (r *AuthRegistry) Schemes() []string { return append([]string(nil), r.order...) }

var (
	_ AuthAdapter = NoAuth{}
	_ AuthAdapter = (*ApiKeyAuth)(nil)
)

// authSchemeAndClaim reads the scheme and claim of a passport's Auth block;
// no block means NoAuth.
func authSchemeAndClaim(p Passport) (string, map[string]any) {
	scheme, _ := p.Auth["scheme"].(string)
	if scheme == "" {
		scheme = AuthSchemeNone
	}
	claim, _ := p.Auth["claim"].(map[string]any)
	return scheme, claim
}

// withoutClaim returns the passport with its auth claim removed: the claim
// proved the identity at registration and the hub keeps only the scheme.
func withoutClaim(p Passport) Passport {
	if _, has := p.Auth["claim"]; !has {
		return p
	}
	auth := make(map[string]any, len(p.Auth))
	for k, v := range p.Auth {
		if k != "claim" {
			auth[k] = v
		}
	}
	p.Auth = auth
	return p
}
