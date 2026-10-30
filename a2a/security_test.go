package a2a

import (
	"encoding/json"
	"testing"
)

// TestSecuritySchemeRoundTrip marshals every factory's scheme to the a2a-sdk
// nested-oneof wire shape (a2aproject/a2a-go a2a/auth.go:135-139) and decodes
// it back.
func TestSecuritySchemeRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		s    NamedScheme
		want string // the JSON the wire must carry
	}{
		{
			name: "bearer",
			s:    NewBearerScheme("bearer", "", "for the API"),
			want: `{"httpAuthSecurityScheme":{"description":"for the API","scheme":"bearer","bearerFormat":"JWT"}}`,
		},
		{
			name: "bearer custom format",
			s:    NewBearerScheme("bearer", "Opaque", ""),
			want: `{"httpAuthSecurityScheme":{"scheme":"bearer","bearerFormat":"Opaque"}}`,
		},
		{
			name: "basic",
			s:    NewHTTPAuthScheme("basic", "basic", "", ""),
			want: `{"httpAuthSecurityScheme":{"scheme":"basic"}}`,
		},
		{
			name: "api key",
			s:    NewAPIKeyScheme("key", "X-API-Key", "header", ""),
			want: `{"apiKeySecurityScheme":{"location":"header","name":"X-API-Key"}}`,
		},
		{
			name: "oidc",
			s:    NewOpenIDConnectScheme("oidc", "https://idp/.well-known/openid-configuration", ""),
			want: `{"openIdConnectSecurityScheme":{"openIdConnectUrl":"https://idp/.well-known/openid-configuration"}}`,
		},
		{
			name: "mtls",
			s:    NewMutualTLSScheme("mtls", ""),
			want: `{"mtlsSecurityScheme":{}}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(tc.s.Scheme)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if got := string(raw); got != tc.want {
				t.Fatalf("wire = %s, want %s", got, tc.want)
			}
			var back SecurityScheme
			if err := json.Unmarshal(raw, &back); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			switch tc.name {
			case "bearer", "bearer custom format", "basic":
				if back.HTTPAuth == nil {
					t.Fatalf("decoded scheme lost the http variant: %+v", back)
				}
			case "api key":
				if back.APIKey == nil || back.APIKey.Name != "X-API-Key" {
					t.Fatalf("decoded scheme lost the apiKey variant: %+v", back)
				}
			case "oidc":
				if back.OpenIDConnect == nil {
					t.Fatalf("decoded scheme lost the openIdConnect variant: %+v", back)
				}
			case "mtls":
				if back.MutualTLS == nil {
					t.Fatalf("decoded scheme lost the mutualTLS variant: %+v", back)
				}
			}
		})
	}
}

// TestSecuritySchemeRejectsTwoVariants refuses a wire object carrying more
// than one variant.
func TestSecuritySchemeRejectsTwoVariants(t *testing.T) {
	raw := `{"apiKeySecurityScheme":{"name":"k"},"httpAuthSecurityScheme":{"scheme":"bearer"}}`
	var s SecurityScheme
	if err := json.Unmarshal([]byte(raw), &s); err == nil {
		t.Fatalf("two variants decoded, want an error")
	}
}

// TestOAuth2Scheme carries the flows object through the wire.
func TestOAuth2Scheme(t *testing.T) {
	s := NewOAuth2Scheme("oauth", OAuthFlows{
		ClientCredentials: &ClientCredentialsOAuthFlow{
			TokenURL: "https://idp/token",
			Scopes:   map[string]string{"read": "read access"},
		},
	}, "https://idp/.well-known/oauth-authorization-server", "")
	raw, err := json.Marshal(s.Scheme)
	if err != nil {
		t.Fatal(err)
	}
	var back SecurityScheme
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("decode: %v (%s)", err, raw)
	}
	if back.OAuth2 == nil || back.OAuth2.Flows.ClientCredentials == nil {
		t.Fatalf("flows lost in transit: %s", raw)
	}
	if back.OAuth2.Flows.ClientCredentials.TokenURL != "https://idp/token" {
		t.Fatalf("token url = %q", back.OAuth2.Flows.ClientCredentials.TokenURL)
	}
}

// TestSecurityScopesWire checks the ProtoJSON StringList rendering and its
// accepted input forms (a2aproject/a2a-go a2a/auth.go securityScopeList).
func TestSecurityScopesWire(t *testing.T) {
	empty, err := json.Marshal(SecurityScopes(nil))
	if err != nil {
		t.Fatal(err)
	}
	if string(empty) != `{}` {
		t.Fatalf("empty scopes = %s, want {}", empty)
	}
	some, err := json.Marshal(SecurityScopes{"read", "write"})
	if err != nil {
		t.Fatal(err)
	}
	if string(some) != `{"list":["read","write"]}` {
		t.Fatalf("scopes = %s", some)
	}
	for _, input := range []string{`{"list":["a"]}`, `["a"]`, `{}`} {
		var s SecurityScopes
		if err := json.Unmarshal([]byte(input), &s); err != nil {
			t.Fatalf("accept %s: %v", input, err)
		}
	}
}

// TestSecurityRequirementWire checks the AND-set rendering and scope copies
// (security.py require / with_scopes → SecurityRequirement proto).
func TestSecurityRequirementWire(t *testing.T) {
	bearer := NewBearerScheme("bearer", "", "")
	oauth := NewOAuth2Scheme("oauth", OAuthFlows{}, "", "").WithScopes("read", "write")
	req := NewSecurityRequirement(bearer, oauth)
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"schemes":{"bearer":{},"oauth":{"list":["read","write"]}}}`
	if string(raw) != want {
		t.Fatalf("requirement = %s, want %s", raw, want)
	}
	var back SecurityRequirement
	if err := json.Unmarshal([]byte(want), &back); err != nil {
		t.Fatalf("decode requirement: %v", err)
	}
	if len(back.Schemes) != 2 || len(back.Schemes["oauth"]) != 2 {
		t.Fatalf("decoded requirement = %+v", back)
	}
	// WithScopes returns a copy — the source scheme stays unscoped.
	oauth2 := NewOAuth2Scheme("oauth", OAuthFlows{}, "", "")
	if len(oauth2.Scopes) != 0 {
		t.Fatalf("WithScopes mutated the receiver's scheme")
	}
}

// TestCardCarriesSecurity checks the AgentCard serializes the two security
// fields under their a2a-sdk names (a2aproject/a2a-go a2a/agent.go).
func TestCardCarriesSecurity(t *testing.T) {
	bearer := NewBearerScheme("bearer", "", "")
	card := testCard(false)
	card.SecuritySchemes = map[string]*SecurityScheme{"bearer": &bearer.Scheme}
	card.Security = []SecurityRequirement{NewSecurityRequirement(bearer)}
	raw, err := json.Marshal(card)
	if err != nil {
		t.Fatal(err)
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		t.Fatal(err)
	}
	if _, ok := probe["securitySchemes"]; !ok {
		t.Fatalf("card JSON has no securitySchemes: %s", raw)
	}
	if _, ok := probe["securityRequirements"]; !ok {
		t.Fatalf("card JSON has no securityRequirements: %s", raw)
	}
	var back AgentCard
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("decode card: %v", err)
	}
	s := back.SecuritySchemes["bearer"]
	if s == nil || s.HTTPAuth == nil || s.HTTPAuth.Scheme != "bearer" {
		t.Fatalf("decoded scheme = %+v", s)
	}
}
