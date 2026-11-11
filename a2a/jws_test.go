package a2a

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

// testHeader is the JOSE header the tests sign with.
func testHeader(alg string) SigningHeader {
	return SigningHeader{Algorithm: alg, KeyID: "test-key", Type: "JOSE"}
}

func newECDSAKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate ECDSA key: %v", err)
	}
	return key
}

func newRSAKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	return key
}

// pemPrivate encodes a private key as PKCS#8 PEM, the shape Python's
// cryptography library hands to the SDK signer.
func pemPrivate(t *testing.T, key any) []byte {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal private key: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}

// pemPublic encodes a public key as PKIX PEM.
func pemPublic(t *testing.T, key any) []byte {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(key)
	if err != nil {
		t.Fatalf("marshal public key: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
}

// TestCanonicalJSONVectors pins the RFC 8785 (JCS) rules the card payload is
// built with: member ordering by UTF-16 code units, ES6 number formatting,
// minimal string escaping.
func TestCanonicalJSONVectors(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "rfc8785 number sorting example",
			in:   `{"numbers":[333333333.33333329,1E30,4.50,2e-3,0.000000000000000000000000001],"string":"\u20ac$.\u000f\u0010\\\"\u0009\b\f\r"}`,
			want: `{"numbers":[333333333.3333333,1e+30,4.5,0.002,1e-27],"string":"€$.\u000f\u0010\\\"\t\b\f\r"}`,
		},
		{
			name: "rfc8785 key sorting example",
			in:   `{"\u20ac":"Euro Sign","\r":"Carriage Return","\ufb33":"Hebrew Letter Dalet With Dagesh","1":"One","\ud83d\ude00":"Emoji: Grinning Face","\u0080":"Control","\u00f6":"Latin Small Letter O With Diaeresis"}`,
			want: "{\"\\r\":\"Carriage Return\",\"1\":\"One\",\"\u0080\":\"Control\",\"\u00f6\":\"Latin Small Letter O With Diaeresis\",\"\u20ac\":\"Euro Sign\",\"\U0001F600\":\"Emoji: Grinning Face\",\"\ufb33\":\"Hebrew Letter Dalet With Dagesh\"}",
		},
		{
			name: "integers stay decimal",
			in:   `{"a":1,"b":-0,"c":1e20,"d":9007199254740991,"e":0.000001}`,
			want: `{"a":1,"b":0,"c":100000000000000000000,"d":9007199254740991,"e":0.000001}`,
		},
		{
			name: "prefix keys sort short first",
			in:   `{"abc":3,"ab":2,"a":1,"b":4}`,
			want: `{"a":1,"ab":2,"abc":3,"b":4}`,
		},
		{
			name: "html characters are not escaped",
			in:   `{"x":"<a href=\"u\">&</a>"}`,
			want: `{"x":"<a href=\"u\">&</a>"}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var value any
			decoder := json.NewDecoder(strings.NewReader(tc.in))
			decoder.UseNumber()
			if err := decoder.Decode(&value); err != nil {
				t.Fatalf("decode %s: %v", tc.in, err)
			}
			got, err := canonicalJSON(value)
			if err != nil {
				t.Fatalf("canonicalize: %v", err)
			}
			if string(got) != tc.want {
				t.Fatalf("canonical form\n got: %s\nwant: %s", got, tc.want)
			}
		})
	}
}

// TestCanonicalJSONIntegerBound mirrors the SDK's integer guard (_jcs.py
// _SAFE_INT_MAX): a JSON integer is written verbatim while an IEEE-754 double
// represents it exactly, and rejected beyond that rather than silently rounded.
func TestCanonicalJSONIntegerBound(t *testing.T) {
	cases := []struct {
		in   string
		want string
		fail bool
	}{
		{"9007199254740991", "9007199254740991", false},
		{"-9007199254740991", "-9007199254740991", false},
		{"9007199254740992", "", true},
		{"-9007199254740992", "", true},
		{"12345678901234567890", "", true},
		// A float literal is an IEEE-754 double: formatting, not range-checking.
		{"1e300", "1e+300", false},
		{"9007199254740992.0", "9007199254740992", false},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			var value any
			decoder := json.NewDecoder(strings.NewReader(tc.in))
			decoder.UseNumber()
			if err := decoder.Decode(&value); err != nil {
				t.Fatalf("decode %s: %v", tc.in, err)
			}
			got, err := canonicalJSON(value)
			if tc.fail {
				if err == nil {
					t.Fatalf("%s canonicalized to %s, want the SDK's out-of-range rejection", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("canonicalize %s: %v", tc.in, err)
			}
			if string(got) != tc.want {
				t.Fatalf("canonical form\n got: %s\nwant: %s", got, tc.want)
			}
		})
	}
}

// TestCardSignatureRoundTrip signs and verifies with every algorithm family
// the SDK offers.
func TestCardSignatureRoundTrip(t *testing.T) {
	rsaKey := newRSAKey(t)
	ecKey := newECDSAKey(t)
	_, edKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate ed25519 key: %v", err)
	}
	secret := []byte("a shared secret long enough for HMAC")

	cases := []struct {
		alg       string
		signKey   any
		verifyKey any
	}{
		{"ES256", ecKey, &ecKey.PublicKey},
		{"RS256", rsaKey, &rsaKey.PublicKey},
		{"PS256", rsaKey, &rsaKey.PublicKey},
		{"EdDSA", edKey, edKey.Public().(ed25519.PublicKey)},
		{"HS256", secret, secret},
	}
	for _, tc := range cases {
		t.Run(tc.alg, func(t *testing.T) {
			card := &AgentCard{Name: "round-trip", Capabilities: AgentCapabilities{Streaming: true}}
			if err := SignCardJWS(card, tc.signKey, testHeader(tc.alg)); err != nil {
				t.Fatalf("sign: %v", err)
			}
			if len(card.Signatures) != 1 {
				t.Fatalf("signatures = %d, want 1", len(card.Signatures))
			}
			verifier := NewCardSignatureVerifier(func(kid, jku string) (any, error) {
				if kid != "test-key" {
					t.Errorf("key provider got kid %q, want test-key", kid)
				}
				return tc.verifyKey, nil
			}, []string{tc.alg})
			if err := verifier(card); err != nil {
				t.Fatalf("verify: %v", err)
			}
		})
	}
}

// TestCardSignatureFromPEMKeys covers the shape a deployment actually has:
// PEM key material, as Python's SDK signer takes.
func TestCardSignatureFromPEMKeys(t *testing.T) {
	ecKey := newECDSAKey(t)
	privPEM := pemPrivate(t, ecKey)
	pubPEM := pemPublic(t, &ecKey.PublicKey)

	card := &AgentCard{Name: "pem"}
	if err := SignCardJWS(card, privPEM, testHeader("ES256")); err != nil {
		t.Fatalf("sign with a PEM key: %v", err)
	}
	verifier := NewCardSignatureVerifier(func(string, string) (any, error) { return pubPEM, nil }, []string{"ES256"})
	if err := verifier(card); err != nil {
		t.Fatalf("verify with a PEM key: %v", err)
	}
}

// A card whose payload changed after signing does not verify: every field is
// covered, not only the ones a signer might have been told about.
func TestCardSignatureTamperedFieldFails(t *testing.T) {
	ecKey := newECDSAKey(t)
	card := &AgentCard{Name: "original", Description: "signed description"}
	if err := SignCardJWS(card, ecKey, testHeader("ES256")); err != nil {
		t.Fatalf("sign: %v", err)
	}
	verifier := NewCardSignatureVerifier(func(string, string) (any, error) { return &ecKey.PublicKey, nil }, []string{"ES256"})
	if err := verifier(card); err != nil {
		t.Fatalf("untampered card rejected: %v", err)
	}

	tampered := *card
	tampered.Description = "evil description"
	if err := verifier(&tampered); !errors.Is(err, ErrCardSignature) {
		t.Fatalf("tampered description verified (%v), want ErrCardSignature", err)
	}

	skill := *card
	skill.Skills = []AgentSkill{{ID: "injected", Name: "injected"}}
	if err := verifier(&skill); !errors.Is(err, ErrCardSignature) {
		t.Fatalf("injected skill verified (%v), want ErrCardSignature", err)
	}
}

func TestCardSignatureWrongKeyFails(t *testing.T) {
	signer, other := newECDSAKey(t), newECDSAKey(t)
	card := &AgentCard{Name: "wrong-key"}
	if err := SignCardJWS(card, signer, testHeader("ES256")); err != nil {
		t.Fatalf("sign: %v", err)
	}
	verifier := NewCardSignatureVerifier(func(string, string) (any, error) { return &other.PublicKey, nil }, []string{"ES256"})
	if err := verifier(card); !errors.Is(err, ErrCardSignature) {
		t.Fatalf("wrong key verified (%v), want ErrCardSignature", err)
	}
}

func TestCardSignatureUnsignedCardRejected(t *testing.T) {
	card := &AgentCard{Name: "unsigned"}
	verifier := NewCardSignatureVerifier(func(string, string) (any, error) { return []byte("k"), nil }, []string{"HS256"})
	if err := verifier(card); !errors.Is(err, ErrCardSignature) {
		t.Fatalf("unsigned card accepted (%v), want ErrCardSignature", err)
	}
}

// A signature re-labelled with an algorithm outside the allowed list is not
// verified even if the key would satisfy it: that is the algorithm-confusion
// guard.
func TestCardSignatureAlgorithmNotAllowed(t *testing.T) {
	ecKey := newECDSAKey(t)
	pubPEM := pemPublic(t, &ecKey.PublicKey)
	// Sign as HMAC over the public key bytes — the classic confusion attack.
	card := &AgentCard{Name: "confused"}
	if err := SignCardJWS(card, pubPEM, testHeader("HS256")); err != nil {
		t.Fatalf("sign: %v", err)
	}
	verifier := NewCardSignatureVerifier(func(string, string) (any, error) { return &ecKey.PublicKey, nil },
		[]string{"ES256", "RS256"})
	if err := verifier(card); !errors.Is(err, ErrCardSignature) {
		t.Fatalf("algorithm confusion accepted (%v), want ErrCardSignature", err)
	}
	// Even when HMAC is allowed, a typed public key is not an HMAC secret.
	hmacVerifier := NewCardSignatureVerifier(func(string, string) (any, error) { return &ecKey.PublicKey, nil }, []string{"HS256"})
	if err := hmacVerifier(card); !errors.Is(err, ErrCardSignature) {
		t.Fatalf("public key used as an HMAC secret (%v), want ErrCardSignature", err)
	}
}

// TestCardSignatureGoldenVector is the cross-language check: the payload and
// the signature come from a2a-sdk 1.3.0's own algorithm (signing.py
// _canonicalize_agent_card + _jcs.canonicalize, PyJWT ES256), so Go has to
// agree with Python on what is signed and on the signature encoding.
//
// The card below does not state push support at all: pushNotifications is a
// proto3 optional field (see TestSigningPresenceGoldenVector), unset here, so
// MessageToDict omits it and so does Go. A card that *states* `false` signs
// differently — that is the presence vector, not this one.
func TestCardSignatureGoldenVector(t *testing.T) {
	const (
		goldenPrivateKey = `-----BEGIN PRIVATE KEY-----
MIGHAgEAMBMGByqGSM49AgEGCCqGSM49AwEHBG0wawIBAQQg0+ek4Zr2OKMWtMDG
ZEivlb73ylKYKVJEOOp1op+/9tKhRANCAATyPfpkLjWuhIC83gEXrzMVLLhTF31P
ukG/WE+qSDP6IqBjXlwwbpTnJ/dcO0TKOXKcG6/+mbVLm2lygwQjP+rg
-----END PRIVATE KEY-----
`
		goldenPublicKey = `-----BEGIN PUBLIC KEY-----
MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAE8j36ZC41roSAvN4BF68zFSy4Uxd9
T7pBv1hPqkgz+iKgY15cMG6U5yf3XDtEyjlynBuv/pm1S5tpcoMEIz/q4A==
-----END PUBLIC KEY-----
`
		goldenProtected = "eyJhbGciOiJFUzI1NiIsImprdSI6bnVsbCwia2lkIjoiZ29sZGVuLWtleSIsInR5cCI6IkpPU0UifQ"
		goldenPayload   = `{"capabilities":{"streaming":true},"name":"golden"}`
		goldenSignature = "7jlaSrj2a6GNgE480JgPo8KKAbI5LOazHINFb9GaZp_8dtrlJlQtJbVtDpgzj0knx-9Wcu8lyVhRTO9CMEOLIA"
	)

	card := &AgentCard{Name: "golden", Capabilities: AgentCapabilities{Streaming: true}}
	payload, err := cardSigningPayload(card)
	if err != nil {
		t.Fatalf("canonical payload: %v", err)
	}
	if string(payload) != goldenPayload {
		t.Fatalf("canonical payload\n got: %s\nwant: %s", payload, goldenPayload)
	}

	// The Python signature verifies here: same payload bytes, same JWS encoding.
	foreign := *card
	foreign.Signatures = []CardSignature{{Protected: goldenProtected, Signature: goldenSignature}}
	verifier := NewCardSignatureVerifier(func(string, string) (any, error) {
		return []byte(goldenPublicKey), nil
	}, []string{"ES256"})
	if err := verifier(&foreign); err != nil {
		t.Fatalf("a2a-sdk-produced signature rejected: %v", err)
	}

	// And a payload changed under the Python signature is refused.
	tampered := foreign
	tampered.Name = "golden-evil"
	if err := verifier(&tampered); !errors.Is(err, ErrCardSignature) {
		t.Fatalf("tampered golden card verified (%v), want ErrCardSignature", err)
	}

	// Signing the same card with the same key yields a signature the Python
	// key verifies (the ECDSA nonce is random, so the bytes differ).
	if err := SignCardJWS(&foreign, []byte(goldenPrivateKey), SigningHeader{
		Algorithm: "ES256", KeyID: "golden-key", Type: "JOSE",
	}); err != nil {
		t.Fatalf("sign with the golden key: %v", err)
	}
	header, err := decodeProtectedHeader(foreign.Signatures[0].Protected)
	if err != nil || header.Algorithm != "ES256" || header.KeyID != "golden-key" {
		t.Fatalf("protected header = %#v (err %v), want alg/kid from the header", header, err)
	}
	if err := verifier(&foreign); err != nil {
		t.Fatalf("Go signature does not verify with the SDK key: %v", err)
	}
}

// TestCardSigningPayloadMatchesSDKForm pins the signed payload to the shape
// protobuf-python's MessageToDict + signing.py _clean_empty produce, byte for
// byte: no proto3 default (false bools, zero numbers, empty strings, empty
// lists), no empty member, while a Struct member (AgentExtension.params) keeps
// its false and 0 — MessageToDict copies Struct contents verbatim and only the
// empty-string pruning applies there.
func TestCardSigningPayloadMatchesSDKForm(t *testing.T) {
	card := &AgentCard{
		Name:        "sdk-shape",
		Description: "d",
		Version:     "1.0.0",
		URL:         "",
		SupportedInterfaces: []AgentInterface{{
			URL: "http://test", ProtocolBinding: ProtocolBindingJSONRPC, ProtocolVersion: "1.0", Tenant: "",
		}},
		Capabilities: AgentCapabilities{
			Streaming:         false,
			PushNotifications: false,
			Extensions: []AgentExtension{{
				URI:         "urn:ag2:client-tools:v1",
				Description: "",
				Required:    false,
				Params:      map[string]any{"flag": false, "zero": 0, "text": ""},
			}},
		},
		DefaultInputModes:  []string{"text/plain"},
		DefaultOutputModes: nil,
		Skills:             []AgentSkill{{ID: "s", Name: "s", Tags: []string{"s"}}},
	}
	// Produced by a2a-sdk 1.3.0's _canonicalize_agent_card path (see
	// TestCardSignatureGoldenVector for how the vectors are derived).
	const want = `{"capabilities":{"extensions":[{"params":{"flag":false,"zero":0},"uri":"urn:ag2:client-tools:v1"}]},` +
		`"defaultInputModes":["text/plain"],"description":"d","name":"sdk-shape",` +
		`"skills":[{"id":"s","name":"s","tags":["s"]}],` +
		`"supportedInterfaces":[{"protocolBinding":"JSONRPC","protocolVersion":"1.0","url":"http://test"}],"version":"1.0.0"}`
	got, err := cardSigningPayload(card)
	if err != nil {
		t.Fatalf("canonical payload: %v", err)
	}
	if string(got) != want {
		t.Fatalf("payload is not the SDK form\n got: %s\nwant: %s", got, want)
	}
}

// TestServerServesSignedCardAndClientVerifies is the served-card path end to
// end: the server signs, the client verifies, and a client with the wrong key
// refuses to connect.
func TestServerServesSignedCardAndClientVerifies(t *testing.T) {
	ecKey := newECDSAKey(t)
	signer, err := NewCardSigner(ecKey, testHeader("ES256"))
	if err != nil {
		t.Fatalf("build signer: %v", err)
	}
	srv := httptest.NewServer(NewServer(ServerConfig{
		Card:       testCard(false),
		Handle:     echoHandler,
		CardSigner: signer,
	}).Handler())
	t.Cleanup(srv.Close)

	trusting := NewCardSignatureVerifier(func(string, string) (any, error) { return &ecKey.PublicKey, nil }, []string{"ES256"})
	card, err := NewClient(srv.URL, WithCardVerifier(trusting)).AgentCard(context.Background())
	if err != nil {
		t.Fatalf("fetch and verify the served card: %v", err)
	}
	if len(card.Signatures) != 1 {
		t.Fatalf("served card carries %d signatures, want 1", len(card.Signatures))
	}

	// The signature covers the served payload, i.e. the capability booleans
	// the server wrote, not only the caller's card.
	if err := trusting(card); err != nil {
		t.Fatalf("re-verify the decoded card: %v", err)
	}

	other := newECDSAKey(t)
	distrusting := NewCardSignatureVerifier(func(string, string) (any, error) { return &other.PublicKey, nil }, []string{"ES256"})
	_, err = NewClient(srv.URL, WithCardVerifier(distrusting)).AgentCard(context.Background())
	if !errors.Is(err, ErrCardSignature) {
		t.Fatalf("wrong-key client accepted the card (%v), want ErrCardSignature", err)
	}
	if !strings.Contains(err.Error(), "fetched agent card") {
		t.Fatalf("error %q does not name the fetched card source", err)
	}

	// A server without a signer serves an unsigned card, which a verifying
	// client must refuse.
	plain := httptest.NewServer(NewServer(ServerConfig{Card: testCard(false), Handle: echoHandler}).Handler())
	t.Cleanup(plain.Close)
	if _, err := NewClient(plain.URL, WithCardVerifier(trusting)).AgentCard(context.Background()); !errors.Is(err, ErrCardSignature) {
		t.Fatalf("unsigned card accepted by a verifying client (%v)", err)
	}
}

// TestServerResignsModifiedCard: the per-request card rewrite must be covered
// by a fresh signature, or a verifying client would reject the served card.
func TestServerResignsModifiedCard(t *testing.T) {
	ecKey := newECDSAKey(t)
	signer, err := NewCardSigner(ecKey, testHeader("ES256"))
	if err != nil {
		t.Fatalf("build signer: %v", err)
	}
	srv := httptest.NewServer(NewServer(ServerConfig{
		Card:       testCard(false),
		Handle:     echoHandler,
		CardSigner: signer,
		CardModifier: func(_ *http.Request, card *AgentCard) (*AgentCard, error) {
			card.Description = "modified per request"
			return card, nil
		},
	}).Handler())
	t.Cleanup(srv.Close)

	verifier := NewCardSignatureVerifier(func(string, string) (any, error) { return &ecKey.PublicKey, nil }, []string{"ES256"})
	client := NewClient(srv.URL, WithCardVerifier(verifier))
	card, err := client.AgentCard(context.Background())
	if err != nil {
		t.Fatalf("fetch the modified card: %v", err)
	}
	if card.Description != "modified per request" {
		t.Fatalf("served description = %q, want the modifier's", card.Description)
	}
	if len(card.Signatures) != 1 {
		t.Fatalf("modified card carries %d signatures, want exactly the fresh one", len(card.Signatures))
	}
}

// TestServerRefusesStaleSignature: flipping a capability on a caller-signed
// card voids the signature, and without a signer there is no key to redo it.
func TestServerRefusesStaleSignature(t *testing.T) {
	ecKey := newECDSAKey(t)
	signed := testCard(false)
	if err := SignCardJWS(&signed, ecKey, testHeader("ES256")); err != nil {
		t.Fatalf("sign: %v", err)
	}

	cfg := ServerConfig{
		Card:         signed,
		Handle:       echoHandler,
		ExtendedCard: &AgentCard{Name: "extended"},
	}
	if _, err := NewServerChecked(cfg); !errors.Is(err, ErrStaleCardSignature) {
		t.Fatalf("NewServerChecked = %v, want ErrStaleCardSignature", err)
	}

	// NewServer keeps serving, but the card routes refuse rather than publish
	// a signature that no longer matches the card.
	srv := httptest.NewServer(NewServer(cfg).Handler())
	t.Cleanup(srv.Close)
	rec := httptest.NewRecorder()
	srv.Config.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, DefaultCardPath, nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("GET card = %d, want 500 (%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), msgStaleSignature) {
		t.Fatalf("card route answered %q, want the stale-signature text", rec.Body.String())
	}

	// A signer re-creates the signature over the flipped card, so the same
	// configuration is served happily.
	signer, err := NewCardSigner(ecKey, testHeader("ES256"))
	if err != nil {
		t.Fatalf("build signer: %v", err)
	}
	cfg.CardSigner = signer
	if _, err := NewServerChecked(cfg); err != nil {
		t.Fatalf("NewServerChecked with a signer: %v", err)
	}
}

// TestClientVerifiesPresetCard: a preset card is verified before it is
// adopted, so a tampered copy cannot slip past the check.
func TestClientVerifiesPresetCard(t *testing.T) {
	ecKey := newECDSAKey(t)
	signed := testCard(false)
	if err := SignCardJWS(&signed, ecKey, testHeader("ES256")); err != nil {
		t.Fatalf("sign: %v", err)
	}
	verifier := NewCardSignatureVerifier(func(string, string) (any, error) { return &ecKey.PublicKey, nil }, []string{"ES256"})

	tampered := signed
	tampered.Description = "evil"
	_, err := NewClient("http://unused.test", WithPresetCard(&tampered), WithCardVerifier(verifier)).AgentCard(context.Background())
	if !errors.Is(err, ErrCardSignature) {
		t.Fatalf("tampered preset card = %v, want ErrCardSignature", err)
	}
	if !strings.Contains(err.Error(), "preset agent card") {
		t.Fatalf("error %q does not name the preset card source", err)
	}
}

// TestClientVerifiesExtendedCard: the second, authenticated card is verified
// too — otherwise an unsigned extended card would silently replace the
// verified public one (parity: test_card_signing.py
// test_unsigned_extended_card_raises).
func TestClientVerifiesExtendedCard(t *testing.T) {
	ecKey := newECDSAKey(t)
	signer, err := NewCardSigner(ecKey, testHeader("ES256"))
	if err != nil {
		t.Fatalf("build signer: %v", err)
	}
	verifier := NewCardSignatureVerifier(func(string, string) (any, error) { return &ecKey.PublicKey, nil }, []string{"ES256"})

	signed := httptest.NewServer(NewServer(ServerConfig{
		Card:         testCard(false),
		Handle:       echoHandler,
		CardSigner:   signer,
		ExtendedCard: &AgentCard{Name: "extended", Description: "more detail"},
	}).Handler())
	t.Cleanup(signed.Close)
	card, err := NewClient(signed.URL, WithCardVerifier(verifier)).ExtendedCard(context.Background())
	if err != nil {
		t.Fatalf("fetch and verify the extended card: %v", err)
	}
	if card.Name != "extended" || len(card.Signatures) != 1 {
		t.Fatalf("extended card = %#v, want the signed extended one", card)
	}

	// A card that does not verify is refused on the extended path too, even
	// though the public card would verify.
	_, err = NewClient(signed.URL, WithCardVerifier(func(card *AgentCard) error {
		if card.Name == "extended" {
			return errors.New("unsigned")
		}
		return nil
	})).ExtendedCard(context.Background())
	if !errors.Is(err, ErrCardSignature) {
		t.Fatalf("unsigned extended card = %v, want ErrCardSignature", err)
	}
	if !strings.Contains(err.Error(), "extended agent card") {
		t.Fatalf("error %q does not name the extended card source", err)
	}
}

// TestCardSignatureRotationLandsBothSignatures: a composed signer (key
// rotation) puts one signature per key on the card, and a verifier that trusts
// only one of the keys still accepts it (parity: sign_card strips the prior
// signatures once, then every signer appends).
func TestCardSignatureRotationLandsBothSignatures(t *testing.T) {
	oldKey, newKey := newECDSAKey(t), newECDSAKey(t)
	oldSigner, err := NewCardSigner(oldKey, SigningHeader{Algorithm: "ES256", KeyID: "old-key"})
	if err != nil {
		t.Fatalf("old signer: %v", err)
	}
	newSigner, err := NewCardSigner(newKey, SigningHeader{Algorithm: "ES256", KeyID: "new-key"})
	if err != nil {
		t.Fatalf("new signer: %v", err)
	}
	rotating := func(card *AgentCard) error {
		if err := oldSigner(card); err != nil {
			return err
		}
		return newSigner(card)
	}
	srv := httptest.NewServer(NewServer(ServerConfig{
		Card:       testCard(false),
		Handle:     echoHandler,
		CardSigner: rotating,
	}).Handler())
	t.Cleanup(srv.Close)

	card, err := NewClient(srv.URL).AgentCard(context.Background())
	if err != nil {
		t.Fatalf("fetch card: %v", err)
	}
	if len(card.Signatures) != 2 {
		t.Fatalf("rotated card carries %d signatures, want 2", len(card.Signatures))
	}
	trustOld := NewCardSignatureVerifier(func(kid, _ string) (any, error) {
		if kid == "new-key" {
			return nil, errors.New("unknown kid")
		}
		return &oldKey.PublicKey, nil
	}, []string{"ES256"})
	if err := trustOld(card); err != nil {
		t.Fatalf("old key must still verify the rotated card: %v", err)
	}
	// The verification order is wire order: the new key fails first and the
	// old one carries the card.
	trustNew := NewCardSignatureVerifier(func(kid, _ string) (any, error) {
		if kid == "old-key" {
			return nil, errors.New("unknown kid")
		}
		return &newKey.PublicKey, nil
	}, []string{"ES256"})
	if err := trustNew(card); !errors.Is(err, ErrCardSignature) {
		t.Fatalf("a verifier whose first signature has an unknown key must fail (%v)", err)
	}
}

// TestNormalizeDepthLimit pins the canonicalizer's nesting bound to the SDK's
// (_jcs.MAX_DEPTH = 128, checked on the way into a container, outermost at 1).
// Both container branches carry the check: a mutant that drops either lets a
// hostile card through that no SDK peer would accept.
func TestNormalizeDepthLimit(t *testing.T) {
	atLimit := map[string]any{"k": "v"}
	if _, err := normalizeProtoValue(atLimit, maxCanonicalDepth, true, false); err != nil {
		t.Fatalf("a map at depth %d rejected: %v", maxCanonicalDepth, err)
	}
	if _, err := normalizeProtoValue(atLimit, maxCanonicalDepth+1, true, false); !errors.Is(err, ErrWire) {
		t.Fatalf("a map at depth %d = %v, want the depth refusal", maxCanonicalDepth+1, err)
	}
	list := []any{"v"}
	if _, err := normalizeProtoValue(list, maxCanonicalDepth, true, false); err != nil {
		t.Fatalf("a list at depth %d rejected: %v", maxCanonicalDepth, err)
	}
	if _, err := normalizeProtoValue(list, maxCanonicalDepth+1, true, false); !errors.Is(err, ErrWire) {
		t.Fatalf("a list at depth %d = %v, want the depth refusal", maxCanonicalDepth+1, err)
	}
	if maxCanonicalDepth != 128 {
		t.Fatalf("depth limit = %d, want the SDK's 128", maxCanonicalDepth)
	}
}

// TestSigningDeeplyNestedParamsRefused: nesting arrives from the wire through
// AgentExtension.params, so a card that is too deep must be refused rather than
// recursed into — by maps and by lists alike.
func TestSigningDeeplyNestedParamsRefused(t *testing.T) {
	nestedMaps := func(levels int) map[string]any {
		var value any = "leaf"
		for i := 0; i < levels; i++ {
			value = map[string]any{"next": value}
		}
		return value.(map[string]any)
	}
	nestedListParams := func(levels int) map[string]any {
		var value any = "leaf"
		for i := 0; i < levels; i++ {
			value = []any{value}
		}
		return map[string]any{"deep": value}
	}
	key := newECDSAKey(t)

	hostile := &AgentCard{
		Name: "deep",
		Capabilities: AgentCapabilities{Extensions: []AgentExtension{{
			URI: "urn:ag2:client-tools:v1", Params: nestedMaps(200),
		}}},
	}
	if err := SignCardJWS(hostile, key, testHeader("ES256")); !errors.Is(err, ErrWire) {
		t.Fatalf("200-level map params signed (%v), want the depth refusal", err)
	}
	hostile.Capabilities.Extensions[0].Params = nestedListParams(200)
	if err := SignCardJWS(hostile, key, testHeader("ES256")); !errors.Is(err, ErrWire) {
		t.Fatalf("200-level list params signed (%v), want the depth refusal", err)
	}

	// A plausible card still signs.
	plausible := &AgentCard{
		Name: "shallow",
		Capabilities: AgentCapabilities{Extensions: []AgentExtension{{
			URI: "urn:ag2:client-tools:v1", Params: nestedMaps(8),
		}}},
	}
	if err := SignCardJWS(plausible, key, testHeader("ES256")); err != nil {
		t.Fatalf("shallow card refused: %v", err)
	}
}

// TestCanonicalJSONNumberBoundaries walks the ECMA-262/ES6 Number::toString
// boundaries the JCS serializer switches on (RFC 8785 §3.2.2.3): the decimal
// window (1e-6 .. 1e21), the exponent sign, and the fractional spellings on
// either side of the window.
func TestCanonicalJSONNumberBoundaries(t *testing.T) {
	cases := map[string]string{
		"0.5":      "0.5",
		"0.05":     "0.05",
		"0.000001": "0.000001", // n = -5: still decimal
		"1e-7":     "1e-7",     // n = -6: the window closes
		"1e20":     "100000000000000000000",
		"1e21":     "1e+21", // the window's far end
		"1.5e-8":   "1.5e-8",
		"-2.5":     "-2.5",
		"12.375":   "12.375",
	}
	for in, want := range cases {
		var value any
		decoder := json.NewDecoder(strings.NewReader(in))
		decoder.UseNumber()
		if err := decoder.Decode(&value); err != nil {
			t.Fatalf("decode %s: %v", in, err)
		}
		got, err := canonicalJSON(value)
		if err != nil {
			t.Fatalf("canonicalize %s: %v", in, err)
		}
		if string(got) != want {
			t.Fatalf("%s canonicalized to %s, want %s", in, got, want)
		}
	}

	for _, notJSON := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		if _, err := appendCanonicalNumber(nil, notJSON); err == nil {
			t.Fatalf("%v canonicalized, want a rejection", notJSON)
		}
	}
}

// TestNormalizeProtoValue pins the two halves of the SDK's rendering:
// MessageToDict drops proto3 defaults (false, 0, "", empty containers) from
// message fields, while a google.protobuf.Struct member (AgentExtension.params)
// keeps its false and zero and only loses empty strings.
func TestNormalizeProtoValue(t *testing.T) {
	zero := json.Number("0")
	cases := []struct {
		name    string
		in      any
		message bool
		present bool
		want    any
		dropped bool
	}{
		{name: "message false", in: false, message: true, dropped: true},
		{name: "optional false set", in: false, message: true, present: true, want: false},
		{name: "message true", in: true, message: true, want: true},
		{name: "message zero", in: zero, message: true, dropped: true},
		{name: "message non-zero", in: json.Number("5"), message: true, want: json.Number("5")},
		{name: "message empty string", in: "", message: true, dropped: true},
		{name: "message non-empty string", in: "x", message: true, want: "x"},
		{name: "message empty list", in: []any{}, message: true, dropped: true},
		{name: "message list of defaults", in: []any{false, ""}, message: true, dropped: true},
		{name: "struct false", in: false, message: false, want: false},
		{name: "struct zero", in: zero, message: false, want: zero},
		{name: "struct empty string", in: "", message: false, dropped: true},
		{
			name:    "struct nested values",
			in:      map[string]any{"flag": false, "zero": zero, "text": "", "deep": map[string]any{"no": false}},
			message: false,
			want:    map[string]any{"flag": false, "zero": zero, "deep": map[string]any{"no": false}},
		},
		{
			name:    "params keep struct semantics inside a message",
			in:      map[string]any{"required": false, "params": map[string]any{"flag": false}},
			message: true,
			want:    map[string]any{"params": map[string]any{"flag": false}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := normalizeProtoValue(tc.in, 1, tc.message, tc.present)
			if err != nil {
				t.Fatalf("normalize: %v", err)
			}
			if tc.dropped {
				if got != nil {
					t.Fatalf("value %#v kept, want it dropped", got)
				}
				return
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("normalized %#v, want %#v", got, tc.want)
			}
		})
	}
}

// TestLessUTF16 pins the RFC 8785 key collation: UTF-16 code units, so astral
// characters sort before U+E000..U+FFFF, and a shorter key sorts before a
// longer one it prefixes.
func TestLessUTF16(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"a", "ab", true},
		{"ab", "a", false},
		{"a", "a", false},
		{"", "a", true},
		{"😀", "\ufb33", true}, // surrogate pair 0xD83D < 0xFB33 in code-unit order
		{"\ufb33", "😀", false},
		{"\u0080", "ö", true},
		{"ö", "€", true},
	}
	for _, tc := range cases {
		if got := lessUTF16(tc.a, tc.b); got != tc.want {
			t.Fatalf("lessUTF16(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

// TestSigningPresenceGoldenVector is the proto3 `optional` case, and the vector
// the first two review rounds got wrong. Derivation, from the a2a-sdk 1.3.0
// descriptor (a2a_pb2.py, read not run):
//
//   - AgentCapabilities.streaming / push_notifications / extended_agent_card are
//     declared with `H\x00`/`H\x01`/`H\x02` + trailing `_streaming` /
//     `_push_notifications` / `_extended_agent_card` oneofs — the synthetic
//     oneofs protoc emits for `optional`, i.e. explicit presence. Every other
//     scalar in the card graph is a plain proto3 field (implicit presence).
//   - MessageToDict writes the *set* fields: streaming=true is set and written;
//     pushNotifications is set to false and written all the same (presence, not
//     truth); extendedAgentCard is unset and omitted; `name` is non-default and
//     written; `capabilities` is a set message.
//   - signing.py _clean_empty keeps booleans untouched, then _jcs.canonicalize
//     sorts the keys: {"capabilities":{"pushNotifications":false,"streaming":true},"name":"golden"}
//   - api_jws.encode(payload=canonical, key=PEM, algorithm="ES256", headers=the
//     JOSE header) produces the protected header and signature below.
//
// So a card that *states* "no push" signs differently from one that is silent
// about push — both are valid, and both must verify.
func TestSigningPresenceGoldenVector(t *testing.T) {
	const (
		goldenPrivateKey = `-----BEGIN PRIVATE KEY-----
MIGHAgEAMBMGByqGSM49AgEGCCqGSM49AwEHBG0wawIBAQQgU8REbudayXNGr5Ti
pfvDeUjdTWZIHN4m7Ogx1LA7HZ6hRANCAARFDHbDKeL2rDchRZv3bMEu4zsZ2Fpb
RxA66zBnRZ+iPrA1dIGMgJjvW91YszJhRPwdEooQYAXlTA3xiqdU8x7x
-----END PRIVATE KEY-----
`
		goldenPublicKey = `-----BEGIN PUBLIC KEY-----
MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAERQx2wyni9qw3IUWb92zBLuM7Gdha
W0cQOuswZ0Wfoj6wNXSBjICY71vdWLMyYUT8HRKKEGAF5UwN8YqnVPMe8Q==
-----END PUBLIC KEY-----
`
		goldenProtected = "eyJhbGciOiJFUzI1NiIsImprdSI6bnVsbCwia2lkIjoicHJlc2VuY2Uta2V5IiwidHlwIjoiSk9TRSJ9"
		goldenPayload   = `{"capabilities":{"pushNotifications":false,"streaming":true},"name":"golden"}`
		goldenSignature = "wiR0WdJstBHj8IV_KTbzomQoLE2RGSqocDyn_i2BL4zjNm7qM5scTYUOtAycruBeybZq8TtVOhMuSBOz1WAHVg"
	)

	card := &AgentCard{Name: "golden"}
	card.Capabilities.SetStreaming(true).SetPushNotifications(false)

	payload, err := cardSigningPayload(card)
	if err != nil {
		t.Fatalf("canonical payload: %v", err)
	}
	if string(payload) != goldenPayload {
		t.Fatalf("canonical payload\n got: %s\nwant: %s", payload, goldenPayload)
	}

	foreign := *card
	foreign.Signatures = []CardSignature{{Protected: goldenProtected, Signature: goldenSignature}}
	verifier := NewCardSignatureVerifier(func(string, string) (any, error) {
		return []byte(goldenPublicKey), nil
	}, []string{"ES256"})
	if err := verifier(&foreign); err != nil {
		t.Fatalf("a2a-sdk-produced signature over the presence payload rejected: %v", err)
	}

	// Signing the same card here produces a signature the Python key verifies.
	if err := SignCardJWS(&foreign, []byte(goldenPrivateKey), SigningHeader{
		Algorithm: "ES256", KeyID: "presence-key", Type: "JOSE",
	}); err != nil {
		t.Fatalf("sign with the golden key: %v", err)
	}
	if err := verifier(&foreign); err != nil {
		t.Fatalf("Go signature does not verify with the SDK key: %v", err)
	}
}

// TestCardSigningPayloadPresence pins the presence rule for the three proto3
// optional capability flags: a set field is signed even when false, an unset
// field is absent, and a plain (implicit-presence) default is dropped.
func TestCardSigningPayloadPresence(t *testing.T) {
	cases := []struct {
		name  string
		build func() *AgentCard
		want  string
	}{
		{
			name:  "nothing set",
			build: func() *AgentCard { return &AgentCard{Name: "p"} },
			want:  `{"name":"p"}`,
		},
		{
			name: "push false set",
			build: func() *AgentCard {
				card := &AgentCard{Name: "p"}
				card.Capabilities.SetPushNotifications(false)
				return card
			},
			want: `{"capabilities":{"pushNotifications":false},"name":"p"}`,
		},
		{
			name: "push true set",
			build: func() *AgentCard {
				card := &AgentCard{Name: "p"}
				card.Capabilities.SetPushNotifications(true)
				return card
			},
			want: `{"capabilities":{"pushNotifications":true},"name":"p"}`,
		},
		{
			name: "streaming false set",
			build: func() *AgentCard {
				card := &AgentCard{Name: "p"}
				card.Capabilities.SetStreaming(false)
				return card
			},
			want: `{"capabilities":{"streaming":false},"name":"p"}`,
		},
		{
			name: "extended card false set",
			build: func() *AgentCard {
				card := &AgentCard{Name: "p"}
				card.Capabilities.SetExtendedAgentCard(false)
				return card
			},
			want: `{"capabilities":{"extendedAgentCard":false},"name":"p"}`,
		},
		{
			name: "a plain true is present by itself",
			build: func() *AgentCard {
				return &AgentCard{Name: "p", Capabilities: AgentCapabilities{Streaming: true}}
			},
			want: `{"capabilities":{"streaming":true},"name":"p"}`,
		},
		{
			name: "a plain false is unset and dropped",
			build: func() *AgentCard {
				return &AgentCard{Name: "p", Capabilities: AgentCapabilities{Streaming: false}}
			},
			want: `{"name":"p"}`,
		},
		{
			name: "a plain extension flag stays implicit-presence",
			build: func() *AgentCard {
				return &AgentCard{Name: "p", Capabilities: AgentCapabilities{
					Extensions: []AgentExtension{{URI: "urn:x", Required: false}},
				}}
			},
			want: `{"capabilities":{"extensions":[{"uri":"urn:x"}]},"name":"p"}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := cardSigningPayload(tc.build())
			if err != nil {
				t.Fatalf("canonical payload: %v", err)
			}
			if string(got) != tc.want {
				t.Fatalf("payload\n got: %s\nwant: %s", got, tc.want)
			}
		})
	}
}

// TestCapabilitiesPresenceSurvivesJSON: presence is a property of the card, not
// of the Go value that made it — a card parsed from the wire keeps what it
// stated, so a client verifies exactly what the server signed.
func TestCapabilitiesPresenceSurvivesJSON(t *testing.T) {
	cases := []struct {
		name    string
		card    *AgentCard
		wire    string // the capability JSON the card must produce
		noFlags bool   // no optional flag may be spelled out at all
	}{
		{
			name: "explicit false survives",
			card: func() *AgentCard {
				card := &AgentCard{Name: "p"}
				card.Capabilities.SetPushNotifications(false)
				return card
			}(),
			wire: `"capabilities":{"pushNotifications":false}`,
		},
		{
			// The card object itself is always spelled out (AGo's wire shape);
			// what matters is that no flag inside it is, and that the signed
			// payload drops the empty object entirely.
			name:    "unset stays absent",
			card:    &AgentCard{Name: "p"},
			wire:    `"capabilities":{}`,
			noFlags: true,
		},
		{
			name: "set true survives",
			card: func() *AgentCard {
				card := &AgentCard{Name: "p"}
				card.Capabilities.SetStreaming(true)
				return card
			}(),
			wire: `"capabilities":{"streaming":true}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payload, err := cardSigningPayload(tc.card)
			if err != nil {
				t.Fatalf("payload: %v", err)
			}
			data, err := json.Marshal(tc.card)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if tc.wire != "" && !strings.Contains(string(data), tc.wire) {
				t.Fatalf("wire %s does not contain %s", data, tc.wire)
			}
			if tc.noFlags && (strings.Contains(string(data), `"streaming"`) ||
				strings.Contains(string(data), `"pushNotifications"`) ||
				strings.Contains(string(data), `"extendedAgentCard"`)) {
				t.Fatalf("an unset optional flag was written: %s", data)
			}

			var decoded AgentCard
			if err := json.Unmarshal(data, &decoded); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			// Re-marshalling is byte-identical, and the payload is unchanged:
			// presence made the round trip.
			again, err := json.Marshal(&decoded)
			if err != nil {
				t.Fatalf("re-marshal: %v", err)
			}
			if string(again) != string(data) {
				t.Fatalf("presence drifted over JSON\n first: %s\nsecond: %s", data, again)
			}
			after, err := cardSigningPayload(&decoded)
			if err != nil {
				t.Fatalf("payload after decode: %v", err)
			}
			if string(after) != string(payload) {
				t.Fatalf("signed payload drifted over JSON\n first: %s\nsecond: %s", payload, after)
			}
		})
	}
}

// TestServerStatesPushCapability: the served card always states push support —
// true with a store, false without — because that is what AG2 does
// (server.py:218 passes push_notifications to build_card) and the statement is
// part of the signature. A client verifies both variants.
func TestServerStatesPushCapability(t *testing.T) {
	key := newECDSAKey(t)
	signer, err := NewCardSigner(key, testHeader("ES256"))
	if err != nil {
		t.Fatalf("build signer: %v", err)
	}
	verifier := NewCardSignatureVerifier(func(string, string) (any, error) { return &key.PublicKey, nil }, []string{"ES256"})

	withStore := httptest.NewServer(NewServer(ServerConfig{
		Card: testCard(false), Handle: echoHandler, CardSigner: signer,
		PushStore: NewMemoryPushConfigStore(),
	}).Handler())
	t.Cleanup(withStore.Close)
	withoutStore := httptest.NewServer(NewServer(ServerConfig{
		Card: testCard(false), Handle: echoHandler, CardSigner: signer,
	}).Handler())
	t.Cleanup(withoutStore.Close)

	for _, tc := range []struct {
		name string
		url  string
		want string
	}{
		{"with a push store", withStore.URL, `"pushNotifications":true`},
		{"without a push store", withoutStore.URL, `"pushNotifications":false`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := NewClient(tc.url, WithCardVerifier(verifier))
			card, err := client.AgentCard(context.Background())
			if err != nil {
				t.Fatalf("fetch and verify: %v", err)
			}
			raw, err := json.Marshal(card)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if !strings.Contains(string(raw), tc.want) {
				t.Fatalf("served card %s, want %s", raw, tc.want)
			}
			payload, err := cardSigningPayload(card)
			if err != nil {
				t.Fatalf("payload: %v", err)
			}
			if !strings.Contains(string(payload), tc.want) {
				t.Fatalf("signed payload %s, want %s", payload, tc.want)
			}
		})
	}
}

// TestStaleSignatureWhenServerStatesPush: a caller-signed card that says
// nothing about push is invalidated by the server stating it, so it must be
// refused — the caller states the flag before signing instead.
func TestStaleSignatureWhenServerStatesPush(t *testing.T) {
	key := newECDSAKey(t)
	// A caller preparing a card the server will serve unchanged signs the
	// discovery-defaulted form; what varies below is only the push statement.
	unstated := withDiscoveryDefaults(testCard(false), "")
	if err := SignCardJWS(&unstated, key, testHeader("ES256")); err != nil {
		t.Fatalf("sign: %v", err)
	}
	if _, err := NewServerChecked(ServerConfig{Card: unstated, Handle: echoHandler}); !errors.Is(err, ErrStaleCardSignature) {
		t.Fatalf("NewServerChecked = %v, want ErrStaleCardSignature", err)
	}

	stated := withDiscoveryDefaults(testCard(false), "")
	stated.Capabilities.SetPushNotifications(false)
	if err := SignCardJWS(&stated, key, testHeader("ES256")); err != nil {
		t.Fatalf("sign: %v", err)
	}
	srv, err := NewServerChecked(ServerConfig{Card: stated, Handle: echoHandler})
	if err != nil {
		t.Fatalf("a card that states the derived flag must be accepted: %v", err)
	}
	verifier := NewCardSignatureVerifier(func(string, string) (any, error) { return &key.PublicKey, nil }, []string{"ES256"})
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, DefaultCardPath, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("card = %d (%s)", rec.Code, rec.Body.String())
	}
	var card AgentCard
	if err := json.Unmarshal(rec.Body.Bytes(), &card); err != nil {
		t.Fatalf("decode card: %v", err)
	}
	if err := verifier(&card); err != nil {
		t.Fatalf("served card does not verify: %v", err)
	}
}
