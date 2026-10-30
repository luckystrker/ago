package a2a

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// Declarative security schemes for the agent card (parity:
// ag2/ag2/a2a/security.py — Scheme/Requirement plus the *_scheme factories).
// These describe HOW callers must authenticate; they are pure card metadata
// with no crypto of their own.
//
// Wire shapes follow the a2a-sdk union (src/a2a/types/a2a_pb2.pyi:324-380) as
// the official Go SDK renders it in JSON (a2aproject/a2a-go a2a/auth.go):
// the scheme is a nested oneof — "apiKeySecurityScheme" /
// "httpAuthSecurityScheme" / "oauth2SecurityScheme" /
// "openIdConnectSecurityScheme" / "mtlsSecurityScheme" — and a security
// requirement maps scheme names to scope lists wrapped as
// {"schemes": {name: {} | {"list": [...]}}}. The card carries
// securitySchemes + securityRequirements (a2a/agent.go).
//
// NOT ported (D-056): JWS card signing/verification (sign_card, CardSigner,
// A2ACardSignatureError in errors.py) — that is real cryptography over the
// card payload and the stdlib has no JWS/JWT.

// SecurityScheme is one authentication scheme definition on the card.
// Exactly one variant is populated; the wire selects it by field name.
type SecurityScheme struct {
	APIKey        *APIKeySecurityScheme        `json:"apiKeySecurityScheme,omitempty"`
	HTTPAuth      *HTTPAuthSecurityScheme      `json:"httpAuthSecurityScheme,omitempty"`
	MutualTLS     *MutualTLSSecurityScheme     `json:"mtlsSecurityScheme,omitempty"`
	OAuth2        *OAuth2SecurityScheme        `json:"oauth2SecurityScheme,omitempty"`
	OpenIDConnect *OpenIDConnectSecurityScheme `json:"openIdConnectSecurityScheme,omitempty"`
}

// APIKeySecurityScheme declares API-key auth (parity:
// security.py api_key_scheme — key name + header/query/cookie location).
type APIKeySecurityScheme struct {
	Description string `json:"description,omitempty"`
	Location    string `json:"location,omitempty"`
	Name        string `json:"name,omitempty"`
}

// HTTPAuthSecurityScheme declares HTTP auth — bearer (with an optional
// bearerFormat hint, security.py bearer_scheme), basic, digest, ...
type HTTPAuthSecurityScheme struct {
	Description  string `json:"description,omitempty"`
	Scheme       string `json:"scheme,omitempty"`
	BearerFormat string `json:"bearerFormat,omitempty"`
}

// OAuth2SecurityScheme wraps the flow definitions plus the optional OAuth2
// metadata URL (parity: security.py oauth2_scheme).
type OAuth2SecurityScheme struct {
	Description       string     `json:"description,omitempty"`
	Oauth2MetadataURL string     `json:"oauth2MetadataUrl,omitempty"`
	Flows             OAuthFlows `json:"flows"`
}

// OpenIDConnectSecurityScheme points at an OIDC discovery document
// (parity: security.py open_id_connect_scheme).
type OpenIDConnectSecurityScheme struct {
	Description      string `json:"description,omitempty"`
	OpenIDConnectURL string `json:"openIdConnectUrl,omitempty"`
}

// MutualTLSSecurityScheme declares client-certificate auth
// (parity: security.py mtls_scheme).
type MutualTLSSecurityScheme struct {
	Description string `json:"description,omitempty"`
}

// OAuthFlows is the five-flow object; zero-value flows are omitted from the
// wire (parity: a2a-sdk OAuthFlows).
type OAuthFlows struct {
	AuthorizationCode *AuthorizationCodeOAuthFlow `json:"authorizationCode,omitempty"`
	ClientCredentials *ClientCredentialsOAuthFlow `json:"clientCredentials,omitempty"`
	Implicit          *ImplicitOAuthFlow          `json:"implicit,omitempty"`
	Password          *PasswordOAuthFlow          `json:"password,omitempty"`
	DeviceCode        *DeviceCodeOAuthFlow        `json:"deviceCode,omitempty"`
}

// AuthorizationCodeOAuthFlow is the classic web-app redirect flow.
type AuthorizationCodeOAuthFlow struct {
	AuthorizationURL string            `json:"authorizationUrl,omitempty"`
	RefreshURL       string            `json:"refreshUrl,omitempty"`
	Scopes           map[string]string `json:"scopes,omitempty"`
	TokenURL         string            `json:"tokenUrl,omitempty"`
}

// ClientCredentialsOAuthFlow is the machine-to-machine flow.
type ClientCredentialsOAuthFlow struct {
	RefreshURL string            `json:"refreshUrl,omitempty"`
	Scopes     map[string]string `json:"scopes,omitempty"`
	TokenURL   string            `json:"tokenUrl,omitempty"`
}

// ImplicitOAuthFlow is the legacy redirect flow without the code step.
type ImplicitOAuthFlow struct {
	AuthorizationURL string            `json:"authorizationUrl,omitempty"`
	RefreshURL       string            `json:"refreshUrl,omitempty"`
	Scopes           map[string]string `json:"scopes,omitempty"`
}

// PasswordOAuthFlow is the direct resource-owner flow.
type PasswordOAuthFlow struct {
	RefreshURL string            `json:"refreshUrl,omitempty"`
	Scopes     map[string]string `json:"scopes,omitempty"`
	TokenURL   string            `json:"tokenUrl,omitempty"`
}

// DeviceCodeOAuthFlow is the limited-input device flow.
type DeviceCodeOAuthFlow struct {
	DeviceAuthorizationURL string            `json:"deviceAuthorizationUrl,omitempty"`
	RefreshURL             string            `json:"refreshUrl,omitempty"`
	Scopes                 map[string]string `json:"scopes,omitempty"`
	TokenURL               string            `json:"tokenUrl,omitempty"`
}

// UnmarshalJSON decodes the oneof by its field name; an empty object is a
// valid (mutual-TLS-shaped) scheme, more than one populated variant is a wire
// error.
func (s *SecurityScheme) UnmarshalJSON(data []byte) error {
	type alias SecurityScheme
	var wire alias
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&wire); err != nil {
		return err
	}
	populated := 0
	for _, v := range []bool{wire.APIKey != nil, wire.HTTPAuth != nil, wire.OAuth2 != nil, wire.OpenIDConnect != nil, wire.MutualTLS != nil} {
		if v {
			populated++
		}
	}
	if populated > 1 {
		return fmt.Errorf("%w: security scheme with %d populated variants", ErrWire, populated)
	}
	*s = SecurityScheme(wire)
	return nil
}

// NamedScheme is a scheme bound to its card-level name and the OAuth scopes
// required with it (parity: security.py Scheme). WithScopes returns a copy.
type NamedScheme struct {
	Name   string
	Scheme SecurityScheme
	Scopes []string
}

// WithScopes returns a copy of the scheme with the given OAuth2/OIDC scopes
// (parity: Scheme.with_scopes).
func (s NamedScheme) WithScopes(scopes ...string) NamedScheme {
	s.Scopes = append([]string(nil), scopes...)
	return s
}

// SecurityScopes is the ProtoJSON StringList of one requirement entry: an
// empty set renders {} and a populated one {"list": [...]}; the legacy bare
// array form is accepted on input (a2aproject/a2a-go a2a/auth.go).
type SecurityScopes []string

// MarshalJSON implements json.Marshaler.
func (s SecurityScopes) MarshalJSON() ([]byte, error) {
	if len(s) == 0 {
		return []byte(`{}`), nil
	}
	return json.Marshal(struct {
		List []string `json:"list"`
	}{List: s})
}

// UnmarshalJSON implements json.Unmarshaler.
func (s *SecurityScopes) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) > 0 && trimmed[0] == '[' {
		var list []string
		if err := json.Unmarshal(trimmed, &list); err != nil {
			return err
		}
		*s = list
		return nil
	}
	var wire struct {
		List []string `json:"list"`
	}
	if err := json.Unmarshal(trimmed, &wire); err != nil {
		return err
	}
	*s = wire.List
	return nil
}

// SecurityRequirement is one AND-set of named schemes that must all be
// presented together; multiple entries in AgentCard.Security are OR-ed
// (parity: security.py Requirement.to_proto over the a2a-sdk
// SecurityRequirement proto).
type SecurityRequirement struct {
	Schemes map[string]SecurityScopes `json:"schemes"`
}

// NewSecurityRequirement builds a requirement from named schemes.
func NewSecurityRequirement(schemes ...NamedScheme) SecurityRequirement {
	req := SecurityRequirement{Schemes: make(map[string]SecurityScopes, len(schemes))}
	for _, s := range schemes {
		req.Schemes[s.Name] = SecurityScopes(s.Scopes)
	}
	return req
}

// Scheme factories (parity: security.py bearer_scheme / http_auth_scheme /
// api_key_scheme / oauth2_scheme / open_id_connect_scheme / mtls_scheme).

// NewBearerScheme declares HTTP bearer auth ("Authorization: Bearer <token>");
// bearerFormat defaults to "JWT" as security.py does.
func NewBearerScheme(name, bearerFormat, description string) NamedScheme {
	if bearerFormat == "" {
		bearerFormat = "JWT"
	}
	return NamedScheme{Name: name, Scheme: SecurityScheme{
		HTTPAuth: &HTTPAuthSecurityScheme{Scheme: "bearer", BearerFormat: bearerFormat, Description: description},
	}}
}

// NewHTTPAuthScheme declares any HTTP authentication scheme (basic, digest,
// bearer with a custom format, ...).
func NewHTTPAuthScheme(name, scheme, bearerFormat, description string) NamedScheme {
	return NamedScheme{Name: name, Scheme: SecurityScheme{
		HTTPAuth: &HTTPAuthSecurityScheme{Scheme: scheme, BearerFormat: bearerFormat, Description: description},
	}}
}

// NewAPIKeyScheme declares API-key auth; keyName is the header/query/cookie
// key the client sends, location is "header", "query" or "cookie".
func NewAPIKeyScheme(name, keyName, location, description string) NamedScheme {
	return NamedScheme{Name: name, Scheme: SecurityScheme{
		APIKey: &APIKeySecurityScheme{Name: keyName, Location: location, Description: description},
	}}
}

// NewOAuth2Scheme declares OAuth2 auth wrapping pre-built flows.
func NewOAuth2Scheme(name string, flows OAuthFlows, metadataURL, description string) NamedScheme {
	return NamedScheme{Name: name, Scheme: SecurityScheme{
		OAuth2: &OAuth2SecurityScheme{Flows: flows, Oauth2MetadataURL: metadataURL, Description: description},
	}}
}

// NewOpenIDConnectScheme declares an OIDC discovery URL.
func NewOpenIDConnectScheme(name, url, description string) NamedScheme {
	return NamedScheme{Name: name, Scheme: SecurityScheme{
		OpenIDConnect: &OpenIDConnectSecurityScheme{OpenIDConnectURL: url, Description: description},
	}}
}

// NewMutualTLSScheme declares mutual TLS (client-certificate auth).
func NewMutualTLSScheme(name, description string) NamedScheme {
	return NamedScheme{Name: name, Scheme: SecurityScheme{
		MutualTLS: &MutualTLSSecurityScheme{Description: description},
	}}
}
