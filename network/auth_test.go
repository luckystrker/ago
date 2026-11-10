package network

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestApiKeyAuthValidate(t *testing.T) {
	ctx := context.Background()
	a := NewApiKeyAuth(map[string]string{"alice": "s3cret"})
	alice := Passport{Name: "alice"}
	cases := []struct {
		name  string
		p     Passport
		claim map[string]any
		ok    bool
	}{
		{"correct token", alice, map[string]any{"token": "s3cret"}, true},
		{"wrong token", alice, map[string]any{"token": "nope"}, false},
		{"prefix of the token", alice, map[string]any{"token": "s3cre"}, false},
		{"missing token", alice, map[string]any{}, false},
		{"nil claim", alice, nil, false},
		{"non-string token", alice, map[string]any{"token": 42}, false},
		{"unknown name", Passport{Name: "mallory"}, map[string]any{"token": "s3cret"}, false},
	}
	for _, tc := range cases {
		err := a.Validate(ctx, tc.p, tc.claim)
		if tc.ok && err != nil {
			t.Errorf("%s: %v", tc.name, err)
		}
		if !tc.ok && !errors.Is(err, ErrAuth) {
			t.Errorf("%s: want ErrAuth, got %v", tc.name, err)
		}
	}
	// No entries at all: everything fails.
	if err := NewApiKeyAuth(nil).Validate(ctx, alice, map[string]any{"token": "s3cret"}); !errors.Is(err, ErrAuth) {
		t.Fatalf("empty adapter must reject: %v", err)
	}
}

func TestApiKeyAuthResolverIsFallback(t *testing.T) {
	var asked []string
	a := NewApiKeyAuth(map[string]string{"alice": "static"}, WithKeyResolver(func(_ context.Context, name string) (string, bool) {
		asked = append(asked, name)
		if name == "bob" {
			return "dynamic", true
		}
		return "", false
	}))
	ctx := context.Background()
	if err := a.Validate(ctx, Passport{Name: "alice"}, map[string]any{"token": "static"}); err != nil {
		t.Fatalf("static key: %v", err)
	}
	if len(asked) != 0 {
		t.Fatalf("resolver consulted despite a static key: %v", asked)
	}
	if err := a.Validate(ctx, Passport{Name: "bob"}, map[string]any{"token": "dynamic"}); err != nil {
		t.Fatalf("resolver key: %v", err)
	}
	if err := a.Validate(ctx, Passport{Name: "carol"}, map[string]any{"token": "x"}); !errors.Is(err, ErrAuth) {
		t.Fatalf("resolver miss: %v", err)
	}
	// The adapter owns a copy of the map.
	keys := map[string]string{"dave": "k"}
	b := NewApiKeyAuth(keys)
	keys["dave"] = "changed"
	if err := b.Validate(ctx, Passport{Name: "dave"}, map[string]any{"token": "k"}); err != nil {
		t.Fatalf("constructor aliased the caller's map: %v", err)
	}
}

func TestAuthRegistry(t *testing.T) {
	r := NewAuthRegistry(NoAuth{}, NewApiKeyAuth(nil))
	if !reflect.DeepEqual(r.Schemes(), []string{"none", "api_key"}) {
		t.Fatalf("schemes: %v", r.Schemes())
	}
	if a, err := r.Get("api_key"); err != nil || a.Scheme() != "api_key" {
		t.Fatalf("get: %v %v", a, err)
	}
	if _, err := r.Get("jwt"); !errors.Is(err, ErrAuth) {
		t.Fatalf("unknown scheme: %v", err)
	}
	if !reflect.DeepEqual(DefaultAuthRegistry().Schemes(), []string{"none"}) {
		t.Fatalf("default registry: %v", DefaultAuthRegistry().Schemes())
	}
}

func keyedHub(t *testing.T) *Hub {
	t.Helper()
	return newTestHub(t, WithHubAuth(NewAuthRegistry(NoAuth{}, NewApiKeyAuth(map[string]string{"alice": "s3cret"}))))
}

func TestHubRegisterAuthAcceptsValidClaim(t *testing.T) {
	h := keyedHub(t)
	p, err := h.Register(Passport{Name: "alice", Auth: map[string]any{
		"scheme": "api_key", "claim": map[string]any{"token": "s3cret"},
	}}, nil)
	if err != nil {
		t.Fatalf("valid claim rejected: %v", err)
	}
	// The claim proved the identity once; the hub keeps only the scheme.
	if _, kept := p.Auth["claim"]; kept || p.Auth["scheme"] != "api_key" {
		t.Fatalf("returned passport auth: %v", p.Auth)
	}
	stored, _ := h.Passport("alice")
	if _, kept := stored.Auth["claim"]; kept || stored.Auth["scheme"] != "api_key" {
		t.Fatalf("stored passport auth: %v", stored.Auth)
	}
}

func TestHubRegisterAuthRejects(t *testing.T) {
	h := keyedHub(t)
	cases := map[string]Passport{
		"wrong token": {Name: "alice", Auth: map[string]any{"scheme": "api_key", "claim": map[string]any{"token": "bad"}}},
		"no claim":    {Name: "alice", Auth: map[string]any{"scheme": "api_key"}},
		"unknown":     {Name: "alice", Auth: map[string]any{"scheme": "jwt", "claim": map[string]any{"token": "x"}}},
		"unlisted":    {Name: "eve", Auth: map[string]any{"scheme": "api_key", "claim": map[string]any{"token": "s3cret"}}},
	}
	for name, p := range cases {
		if _, err := h.Register(p, nil); !errors.Is(err, ErrAuth) {
			t.Errorf("%s: want ErrAuth, got %v", name, err)
		}
	}
	if got := h.Registrations(); len(got) != 0 {
		t.Fatalf("rejected registrations must not be stored: %+v", got)
	}
	// An unauthenticated passport still falls under the "none" scheme.
	if _, err := h.Register(Passport{Name: "anon"}, nil); err != nil {
		t.Fatalf("none scheme: %v", err)
	}
}

func TestHubDefaultAuthAndRemoteAgents(t *testing.T) {
	// Default hub: NoAuth only, so any other scheme is refused...
	h := newTestHub(t)
	if _, err := h.Register(Passport{Name: "a", Auth: map[string]any{"scheme": "api_key"}}, nil); !errors.Is(err, ErrAuth) {
		t.Fatalf("default registry must refuse api_key: %v", err)
	}
	// ...except for remote agents, whose scheme is a routing label for the
	// remote proxy and whose claim is kept.
	p, err := h.Register(Passport{Name: "far", Kind: KindRemoteAgent, Auth: map[string]any{
		"scheme": "mtls", "claim": map[string]any{"fp": "abc"},
	}}, nil)
	if err != nil {
		t.Fatalf("remote agent: %v", err)
	}
	if p.Auth["claim"] == nil {
		t.Fatalf("remote agent claim must be kept: %v", p.Auth)
	}
}

func TestHubAuthenticateHandshake(t *testing.T) {
	h := keyedHub(t)
	alice := Passport{Name: "alice", Auth: map[string]any{"scheme": "api_key"}}
	ctx := context.Background()
	if err := h.Authenticate(ctx, alice, map[string]any{"token": "s3cret"}); err != nil {
		t.Fatalf("handshake accept: %v", err)
	}
	if err := h.Authenticate(ctx, alice, map[string]any{"token": "x"}); !errors.Is(err, ErrAuth) {
		t.Fatalf("handshake reject: %v", err)
	}
}
