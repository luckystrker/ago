package a2a

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"hash"
	"math"
	"math/big"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"
)

// AgentCard JWS signatures (parity: a2a-sdk `signing.py` —
// create_agent_card_signer / create_signature_verifier — which AG2 exposes as
// CardSigner/card_signature_verifier in ag2/ag2/a2a/transports/_common.py and
// client.py).
//
// The signature is a detached JWS in the A2A card shape: an array of
// {protected, signature} entries, where "protected" is the base64url-encoded
// JOSE header and "signature" is the base64url-encoded signature over
// ASCII(base64url(header) + "." + base64url(payload)). The payload is the
// card's JSON with "signatures" removed and every empty string/list/object
// dropped, canonicalized per RFC 8785 (JCS) — the same rules the SDK applies.
//
// AGO-DEVIATION: the SDK canonicalizes the protobuf-JSON form of the card
// (camelCase, absent proto fields omitted); this package canonicalizes the
// JSON it serves and reads, which is the same camelCase card with its own
// occam-omitted fields (AGo cards always spell out capability booleans).
// Signatures therefore verify against the card as this package renders it.
// Nothing here is provider-specific: stdlib crypto covers every algorithm the
// SDK offers.

// ErrCardSignature reports a card signature that does not verify, an
// unsigned card where a verifier was configured, or a signature the server
// cannot re-create over the card it must serve. Match with errors.Is.
var ErrCardSignature = errors.New("a2a: agent card signature verification failed")

// ErrStaleCardSignature reports a card that carries signatures while the
// server has to change it (flipping the extended-card or push-notification
// capability) without a CardSigner to re-sign the result — serving it would
// put a signature on the wire that does not match its payload (parity:
// errors.py A2AStaleCardSignatureError).
var ErrStaleCardSignature = errors.New("a2a: agent card signatures do not cover the card the server must serve")

// CardSignature is one signature on an AgentCard (parity: the a2a-sdk
// AgentCardSignature proto — protected header, signature, optional unprotected
// header).
type CardSignature struct {
	Protected string         `json:"protected"`
	Signature string         `json:"signature"`
	Header    map[string]any `json:"header,omitempty"`
}

// SigningHeader is the JOSE protected header a CardSigner stamps into every
// signature (parity: the SDK's ProtectedHeader TypedDict). Algorithm names a
// JWS algorithm (ES256/ES384/ES512, RS256/RS384/RS512, PS256/PS384/PS512,
// EdDSA, HS256/HS384/HS512); KeyID is the "kid" a verifier looks the key up
// by; JKU is the key-set URL a verifier may fetch a key from; Type is the
// "typ" marker, conventionally "JOSE".
type SigningHeader struct {
	Algorithm string `json:"alg"`
	KeyID     string `json:"kid,omitempty"`
	JKU       string `json:"jku,omitempty"`
	Type      string `json:"typ,omitempty"`
}

// CardSigner signs an agent card in place (parity: _common.py CardSigner).
// The server hands it a copy: a signer must not assume the caller's card is
// disposable.
//
// AGO-DEVIATION: Python's CardSigner returns the signed card (the SDK signer
// mutates and returns its argument); the Go shape mutates and returns only the
// error, so a signer cannot swap the card the server serves.
type CardSigner func(card *AgentCard) error

// CardVerifier checks the signatures on an agent card, returning an error
// (wrapping ErrCardSignature) when none verifies (parity: client.py
// CardVerifier — a callable that raises).
type CardVerifier func(card *AgentCard) error

// KeyProvider supplies the verification key for one signature (parity: the
// SDK's key_provider — it receives the signature's "kid" and "jku" and returns
// a key: *rsa.PublicKey, *ecdsa.PublicKey, ed25519.PublicKey, or []byte — a
// PEM/DER public key, or the shared secret for the HMAC algorithms).
type KeyProvider func(keyID, jku string) (any, error)

// NewCardSigner builds the JWS signer for key and header (parity:
// create_agent_card_signer). The key may be *ecdsa.PrivateKey,
// *rsa.PrivateKey, ed25519.PrivateKey, or []byte holding a PEM/DER private key
// (PKCS#8, PKCS#1 or SEC1) — or, for the HMAC algorithms, the shared secret
// itself.
func NewCardSigner(key any, header SigningHeader) (CardSigner, error) {
	signer, err := newJWSSigner(key, header)
	if err != nil {
		return nil, err
	}
	return func(card *AgentCard) error {
		if card == nil {
			return fmt.Errorf("%w: nil agent card", ErrWire)
		}
		sig, err := signer.sign(card)
		if err != nil {
			return err
		}
		card.Signatures = append(card.Signatures, sig)
		return nil
	}, nil
}

// SignCardJWS signs card in place with key and header, appending one
// signature. Signatures already on the card are kept: a composed signer (key
// rotation) lands every key's signature (parity: sign_card over a
// signature-stripped copy once per signer).
func SignCardJWS(card *AgentCard, key any, header SigningHeader) error {
	signer, err := NewCardSigner(key, header)
	if err != nil {
		return err
	}
	return signer(card)
}

// NewCardSignatureVerifier builds the verifier for the given algorithms and
// key source (parity: create_signature_verifier). The card is accepted when
// at least one signature verifies; an algorithm outside the list is never
// accepted, which is what stops an algorithm-confusion attack (a signature
// re-labelled HS256 over the public key). A key provider failure aborts
// verification with that error wrapped, as in AG2.
func NewCardSignatureVerifier(keys KeyProvider, algorithms []string) CardVerifier {
	allowed := make(map[string]bool, len(algorithms))
	for _, alg := range algorithms {
		allowed[alg] = true
	}
	return func(card *AgentCard) error {
		if card == nil {
			return fmt.Errorf("%w: nil agent card", ErrCardSignature)
		}
		if len(card.Signatures) == 0 {
			return fmt.Errorf("%w: the card carries no signatures", ErrCardSignature)
		}
		payload, err := cardSigningPayload(card)
		if err != nil {
			return fmt.Errorf("%w: canonical payload: %v", ErrCardSignature, err)
		}
		for _, sig := range card.Signatures {
			header, err := decodeProtectedHeader(sig.Protected)
			if err != nil {
				continue
			}
			if !allowed[header.Algorithm] {
				continue
			}
			key, err := keys(header.KeyID, header.JKU)
			if err != nil {
				return fmt.Errorf("%w: key for kid %q: %v", ErrCardSignature, header.KeyID, err)
			}
			resolved, err := resolveVerificationKey(key, header.Algorithm)
			if err != nil {
				return fmt.Errorf("%w: key for kid %q: %v", ErrCardSignature, header.KeyID, err)
			}
			if err := verifyJWS(sig, payload, header.Algorithm, resolved); err != nil {
				continue
			}
			return nil
		}
		return fmt.Errorf("%w: no signature on the card verifies", ErrCardSignature)
	}
}

// signingInput builds the JWS signing input: the ASCII of
// base64url(protected) + "." + base64url(payload).
func signingInput(protected, payload []byte) []byte {
	out := make([]byte, 0, len(protected)+1+len(payload))
	out = append(out, base64.RawURLEncoding.EncodeToString(protected)...)
	out = append(out, '.')
	out = append(out, base64.RawURLEncoding.EncodeToString(payload)...)
	return out
}

// encodeBase64URL is the unpadded base64url alphabet JWS uses.
func encodeBase64URL(data []byte) string { return base64.RawURLEncoding.EncodeToString(data) }

// decodeProtectedHeader parses a signature's JOSE header.
func decodeProtectedHeader(protected string) (SigningHeader, error) {
	var header SigningHeader
	raw, err := base64.RawURLEncoding.DecodeString(protected)
	if err != nil {
		return header, fmt.Errorf("%w: protected header is not base64url: %v", ErrCardSignature, err)
	}
	if err := json.Unmarshal(raw, &header); err != nil {
		return header, fmt.Errorf("%w: protected header is not a JSON object: %v", ErrCardSignature, err)
	}
	if header.Algorithm == "" {
		return header, fmt.Errorf("%w: protected header has no alg", ErrCardSignature)
	}
	return header, nil
}

// jwsSigner is one key/algorithm pair that signs canonical card payloads.
type jwsSigner struct {
	header SigningHeader
	key    any
}

// newJWSSigner validates the algorithm/key pairing at construction, so a
// misconfigured server fails where it is configured rather than per request.
func newJWSSigner(key any, header SigningHeader) (*jwsSigner, error) {
	if header.Algorithm == "" {
		return nil, fmt.Errorf("%w: JWS algorithm is required", ErrWire)
	}
	if !supportedJWSAlgorithm(header.Algorithm) {
		return nil, fmt.Errorf("%w: unsupported JWS algorithm %q", ErrWire, header.Algorithm)
	}
	resolved, err := resolveSigningKey(key, header.Algorithm)
	if err != nil {
		return nil, err
	}
	return &jwsSigner{header: header, key: resolved}, nil
}

// sign returns the signature of one card.
func (s *jwsSigner) sign(card *AgentCard) (CardSignature, error) {
	payload, err := cardSigningPayload(card)
	if err != nil {
		return CardSignature{}, err
	}
	protected, err := json.Marshal(s.header)
	if err != nil {
		return CardSignature{}, fmt.Errorf("%w: encode JOSE header: %v", ErrWire, err)
	}
	sig, err := signJWS(signingInput(protected, payload), s.header.Algorithm, s.key)
	if err != nil {
		return CardSignature{}, err
	}
	return CardSignature{Protected: encodeBase64URL(protected), Signature: encodeBase64URL(sig)}, nil
}

// signJWS signs input with the named algorithm.
func signJWS(input []byte, alg string, key any) ([]byte, error) {
	switch alg {
	case "HS256", "HS384", "HS512":
		secret, ok := key.([]byte)
		if !ok {
			return nil, fmt.Errorf("%w: %s needs a []byte secret", ErrWire, alg)
		}
		mac := hmac.New(hashFuncFor(alg), secret)
		mac.Write(input)
		return mac.Sum(nil), nil
	case "RS256", "RS384", "RS512":
		priv, ok := key.(*rsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("%w: %s needs an RSA private key", ErrWire, alg)
		}
		digest := hashFor(alg).New()
		digest.Write(input)
		return rsa.SignPKCS1v15(rand.Reader, priv, hashIDFor(alg), digest.Sum(nil))
	case "PS256", "PS384", "PS512":
		priv, ok := key.(*rsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("%w: %s needs an RSA private key", ErrWire, alg)
		}
		digest := hashFor(alg).New()
		digest.Write(input)
		return rsa.SignPSS(rand.Reader, priv, hashIDFor(alg), digest.Sum(nil), &rsa.PSSOptions{
			SaltLength: rsa.PSSSaltLengthEqualsHash,
		})
	case "ES256", "ES384", "ES512":
		priv, ok := key.(*ecdsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("%w: %s needs an ECDSA private key", ErrWire, alg)
		}
		digest := hashFor(alg).New()
		digest.Write(input)
		r, s, err := ecdsa.Sign(rand.Reader, priv, digest.Sum(nil))
		if err != nil {
			return nil, fmt.Errorf("%w: sign %s: %v", ErrWire, alg, err)
		}
		size := (priv.Curve.Params().BitSize + 7) / 8
		out := make([]byte, 2*size)
		r.FillBytes(out[:size])
		s.FillBytes(out[size:])
		return out, nil
	case "EdDSA":
		priv, ok := key.(ed25519.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("%w: EdDSA needs an ed25519 private key", ErrWire)
		}
		return ed25519.Sign(priv, input), nil
	}
	return nil, fmt.Errorf("%w: unsupported JWS algorithm %q", ErrWire, alg)
}

// verifyJWS checks one signature over input. A signature that does not verify
// returns a non-nil error; the caller moves on to the next one.
func verifyJWS(sig CardSignature, payload []byte, alg string, key any) error {
	protected, err := base64.RawURLEncoding.DecodeString(sig.Protected)
	if err != nil {
		return fmt.Errorf("%w: protected header is not base64url", ErrCardSignature)
	}
	raw, err := base64.RawURLEncoding.DecodeString(sig.Signature)
	if err != nil {
		return fmt.Errorf("%w: signature is not base64url", ErrCardSignature)
	}
	input := signingInput(protected, payload)
	switch alg {
	case "HS256", "HS384", "HS512":
		secret, ok := key.([]byte)
		if !ok {
			return fmt.Errorf("%w: %s needs a []byte secret", ErrCardSignature, alg)
		}
		mac := hmac.New(hashFuncFor(alg), secret)
		mac.Write(input)
		if !hmac.Equal(raw, mac.Sum(nil)) {
			return fmt.Errorf("%w: %s signature mismatch", ErrCardSignature, alg)
		}
		return nil
	case "RS256", "RS384", "RS512":
		pub, ok := key.(*rsa.PublicKey)
		if !ok {
			return fmt.Errorf("%w: %s needs an RSA public key", ErrCardSignature, alg)
		}
		digest := hashFor(alg).New()
		digest.Write(input)
		return rsa.VerifyPKCS1v15(pub, hashIDFor(alg), digest.Sum(nil), raw)
	case "PS256", "PS384", "PS512":
		pub, ok := key.(*rsa.PublicKey)
		if !ok {
			return fmt.Errorf("%w: %s needs an RSA public key", ErrCardSignature, alg)
		}
		digest := hashFor(alg).New()
		digest.Write(input)
		return rsa.VerifyPSS(pub, hashIDFor(alg), digest.Sum(nil), raw, &rsa.PSSOptions{
			SaltLength: rsa.PSSSaltLengthEqualsHash,
		})
	case "ES256", "ES384", "ES512":
		pub, ok := key.(*ecdsa.PublicKey)
		if !ok {
			return fmt.Errorf("%w: %s needs an ECDSA public key", ErrCardSignature, alg)
		}
		size := (pub.Curve.Params().BitSize + 7) / 8
		if len(raw) != 2*size {
			return fmt.Errorf("%w: %s signature has %d bytes, want %d", ErrCardSignature, alg, len(raw), 2*size)
		}
		r := new(big.Int).SetBytes(raw[:size])
		s := new(big.Int).SetBytes(raw[size:])
		digest := hashFor(alg).New()
		digest.Write(input)
		if !ecdsa.Verify(pub, digest.Sum(nil), r, s) {
			return fmt.Errorf("%w: %s signature mismatch", ErrCardSignature, alg)
		}
		return nil
	case "EdDSA":
		pub, ok := key.(ed25519.PublicKey)
		if !ok {
			return fmt.Errorf("%w: EdDSA needs an ed25519 public key", ErrCardSignature)
		}
		if !ed25519.Verify(pub, input, raw) {
			return fmt.Errorf("%w: EdDSA signature mismatch", ErrCardSignature)
		}
		return nil
	}
	return fmt.Errorf("%w: unsupported JWS algorithm %q", ErrCardSignature, alg)
}

// supportedJWSAlgorithm reports whether this package signs and verifies the
// algorithm: every family the a2a-sdk offers (HMAC, RSA PKCS#1, RSA-PSS, ECDSA
// and Ed25519).
func supportedJWSAlgorithm(alg string) bool {
	switch alg {
	case "HS256", "HS384", "HS512",
		"RS256", "RS384", "RS512",
		"PS256", "PS384", "PS512",
		"ES256", "ES384", "ES512",
		"EdDSA":
		return true
	}
	return false
}

// hashFor maps an algorithm to its digest.
func hashFor(alg string) crypto.Hash {
	switch alg {
	case "HS384", "RS384", "PS384", "ES384":
		return crypto.SHA384
	case "HS512", "RS512", "PS512", "ES512":
		return crypto.SHA512
	}
	return crypto.SHA256
}

// hashIDFor maps an algorithm to the crypto.Hash the RSA helpers take.
func hashIDFor(alg string) crypto.Hash { return hashFor(alg) }

// hashFuncFor maps an algorithm to its hash constructor, the form hmac.New
// takes.
func hashFuncFor(alg string) func() hash.Hash {
	switch hashFor(alg) {
	case crypto.SHA384:
		return sha512.New384
	case crypto.SHA512:
		return sha512.New
	}
	return sha256.New
}

// resolveSigningKey accepts a typed private key or its PEM/DER encoding. An
// unparsable []byte is a shared secret, which only the HMAC algorithms accept.
func resolveSigningKey(key any, alg string) (any, error) {
	switch k := key.(type) {
	case *ecdsa.PrivateKey, *rsa.PrivateKey, ed25519.PrivateKey:
		return k, nil
	case []byte:
		parsed, err := parsePrivateKeyPEM(k)
		if err == nil {
			return parsed, nil
		}
		if strings.HasPrefix(alg, "HS") {
			return k, nil
		}
		return nil, fmt.Errorf("%w: cannot parse the signing key: %v", ErrWire, err)
	case string:
		return resolveSigningKey([]byte(k), alg)
	}
	return nil, fmt.Errorf("%w: unsupported signing key type %T", ErrWire, key)
}

// parsePrivateKeyPEM parses a PEM-encoded PKCS#8, PKCS#1 or SEC1 private key,
// or a DER blob.
func parsePrivateKeyPEM(data []byte) (any, error) {
	der := data
	if block, _ := pem.Decode(data); block != nil {
		der = block.Bytes
	}
	if key, err := x509.ParsePKCS8PrivateKey(der); err == nil {
		return key, nil
	}
	if key, err := x509.ParseECPrivateKey(der); err == nil {
		return key, nil
	}
	if key, err := x509.ParsePKCS1PrivateKey(der); err == nil {
		return key, nil
	}
	return nil, fmt.Errorf("%w: not a PKCS#8, SEC1 or PKCS#1 private key", ErrWire)
}

// resolveVerificationKey accepts a typed public key, a PEM/DER public key, or
// (for HMAC) the shared secret.
func resolveVerificationKey(key any, alg string) (any, error) {
	switch k := key.(type) {
	case *ecdsa.PublicKey, *rsa.PublicKey, ed25519.PublicKey:
		return k, nil
	case []byte:
		parsed, err := parsePublicKeyPEM(k)
		if err == nil {
			return parsed, nil
		}
		if strings.HasPrefix(alg, "HS") {
			return k, nil
		}
		return nil, fmt.Errorf("%w: cannot parse the verification key: %v", ErrCardSignature, err)
	case string:
		return resolveVerificationKey([]byte(k), alg)
	}
	return nil, fmt.Errorf("%w: unsupported verification key type %T", ErrCardSignature, key)
}

// parsePublicKeyPEM parses a PEM-encoded PKIX or PKCS#1 public key, or a DER
// blob.
func parsePublicKeyPEM(data []byte) (any, error) {
	der := data
	if block, _ := pem.Decode(data); block != nil {
		der = block.Bytes
	}
	if key, err := x509.ParsePKIXPublicKey(der); err == nil {
		return key, nil
	}
	if key, err := x509.ParsePKCS1PublicKey(der); err == nil {
		return key, nil
	}
	return nil, fmt.Errorf("%w: not a PKIX or PKCS#1 public key", ErrCardSignature)
}

// cardSigningPayload is the canonical JWS payload of a card, built exactly
// the way the a2a-sdk builds it (parity: signing.py _canonicalize_agent_card):
//
//  1. render the card the way protobuf JSON does (MessageToDict) — camelCase
//     keys, and *no* field holding its proto3 default (false, 0, "", an empty
//     list or map);
//  2. drop "signatures" (the payload never covers them);
//  3. drop every member that is still empty/absent (_clean_empty);
//  4. canonicalize per RFC 8785 (utils/_jcs.py).
//
// Step 1 is what makes a Go signature verify in Python and vice versa: the SDK
// never puts `"pushNotifications":false` on the wire it signs, and a payload
// that carries it hashes differently. The card AGo *serves* keeps its explicit
// booleans (its wire shape is unchanged); only the signed form drops them.
func cardSigningPayload(card *AgentCard) ([]byte, error) {
	data, err := json.Marshal(card)
	if err != nil {
		return nil, fmt.Errorf("%w: encode agent card: %v", ErrWire, err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var generic any
	if err := decoder.Decode(&generic); err != nil {
		return nil, fmt.Errorf("%w: decode agent card: %v", ErrWire, err)
	}
	object, ok := generic.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%w: agent card is not a JSON object", ErrWire)
	}
	delete(object, "signatures")
	cleaned, err := normalizeProtoValue(object, 1, true, false)
	if err != nil {
		return nil, err
	}
	return canonicalJSON(cleaned)
}

// maxExactJSONInteger is the largest integer a JSON number (an IEEE-754
// double) represents exactly; the SDK rejects integers beyond it
// (_jcs.py _SAFE_INT_MAX).
const maxExactJSONInteger = 1<<53 - 1

// maxCanonicalDepth bounds container nesting, exactly as the SDK's
// _jcs.MAX_DEPTH does: AgentExtension.params is a Struct whose nesting an
// attacker controls, and an unbounded walk turns a hostile card into a stack
// exhaustion in whichever process verifies it. The SDK's bound is 128 — deep
// enough that no plausible card reaches it, shallow enough to be safe — and a
// peer's canonicalizer rejects anything deeper, so accepting more here would
// only produce signatures the SDK refuses to verify.
const maxCanonicalDepth = 128

// protoOptionalFields are the card's proto3 `optional` fields — explicit
// presence, from the synthetic oneofs in the a2a-sdk descriptor
// (AgentCapabilities _streaming / _push_notifications / _extended_agent_card,
// AgentCard _documentation_url / _icon_url). MessageToDict keeps an explicitly
// set field even at its default value, so a present `false` must stay in the
// payload while an unset one must not appear; an optional string that is empty
// is still dropped by the _clean_empty rule.
var protoOptionalFields = map[string]bool{
	"streaming":         true,
	"pushNotifications": true,
	"extendedAgentCard": true,
	"documentationUrl":  true,
	"iconUrl":           true,
}

// normalizeProtoValue renders a decoded card value the way
// google.protobuf.json_format.MessageToDict renders a proto3 message, then
// applies the SDK's _clean_empty. message says whether the value is a proto
// message member (proto3 implicit-presence defaults are dropped) or a value
// inside a google.protobuf.Struct (kept: MessageToDict copies Struct contents
// verbatim, so `false`/`0` inside AgentExtension.params survive, and only the
// empty string/container pruning applies). present says the scalar is a proto3
// `optional` field that was set, which keeps a false. A nil result means "drop
// this member".
func normalizeProtoValue(value any, depth int, message, present bool) (any, error) {
	switch v := value.(type) {
	case map[string]any:
		if depth > maxCanonicalDepth {
			return nil, fmt.Errorf("%w: agent card nests deeper than %d", ErrWire, maxCanonicalDepth)
		}
		out := make(map[string]any, len(v))
		for key, member := range v {
			// The card's only Struct members are AgentExtension.params and
			// AgentCardSignature.header (removed with the signatures). A value
			// inside a Struct is not a proto field, and neither is anything
			// nested below it, so the Struct mode propagates unchanged.
			memberIsMessage := message && key != "params"
			cleaned, err := normalizeProtoValue(member, depth+1, memberIsMessage,
				memberIsMessage && protoOptionalFields[key])
			if err != nil {
				return nil, err
			}
			if cleaned != nil {
				out[key] = cleaned
			}
		}
		if len(out) == 0 {
			return nil, nil
		}
		return out, nil
	case []any:
		if depth > maxCanonicalDepth {
			return nil, fmt.Errorf("%w: agent card nests deeper than %d", ErrWire, maxCanonicalDepth)
		}
		out := make([]any, 0, len(v))
		for _, member := range v {
			cleaned, err := normalizeProtoValue(member, depth+1, message, false)
			if err != nil {
				return nil, err
			}
			if cleaned != nil {
				out = append(out, cleaned)
			}
		}
		if len(out) == 0 {
			return nil, nil
		}
		return out, nil
	case string:
		if v == "" {
			return nil, nil
		}
		return v, nil
	case bool:
		if message && !present && !v {
			return nil, nil
		}
		return v, nil
	case json.Number:
		if message && isZeroNumber(v) {
			return nil, nil
		}
		return v, nil
	}
	return value, nil
}

// isZeroNumber reports whether a JSON number is the proto3 default (zero), in
// any spelling: 0, -0, 0.0, 0e10.
func isZeroNumber(n json.Number) bool {
	f, err := strconv.ParseFloat(n.String(), 64)
	return err == nil && f == 0
}

// canonicalJSON renders a decoded JSON value in RFC 8785 (JCS) form: object
// members sorted by their UTF-16 code units, no insignificant whitespace,
// minimal string escaping, and ES6 number formatting.
func canonicalJSON(value any) ([]byte, error) {
	return appendCanonical(nil, value)
}

// appendCanonical appends value's canonical form to dst.
func appendCanonical(dst []byte, value any) ([]byte, error) {
	switch v := value.(type) {
	case nil:
		return append(dst, "null"...), nil
	case bool:
		if v {
			return append(dst, "true"...), nil
		}
		return append(dst, "false"...), nil
	case string:
		return appendCanonicalString(dst, v), nil
	case json.Number:
		if text := v.String(); !strings.ContainsAny(text, ".eE") {
			// An integer literal: _jcs.py writes it verbatim, but only while it
			// is exactly representable as a JSON (IEEE-754) number.
			integer, err := strconv.ParseInt(text, 10, 64)
			if err != nil || integer > maxExactJSONInteger || integer < -maxExactJSONInteger {
				return nil, fmt.Errorf("%w: integer %s is outside the range a JSON number represents exactly", ErrWire, text)
			}
			return strconv.AppendInt(dst, integer, 10), nil
		}
		f, err := strconv.ParseFloat(v.String(), 64)
		if err != nil {
			return nil, fmt.Errorf("%w: number %q is not finite: %v", ErrWire, v.String(), err)
		}
		return appendCanonicalNumber(dst, f)
	case float64:
		return appendCanonicalNumber(dst, v)
	case []any:
		dst = append(dst, '[')
		for i, member := range v {
			if i > 0 {
				dst = append(dst, ',')
			}
			var err error
			dst, err = appendCanonical(dst, member)
			if err != nil {
				return nil, err
			}
		}
		return append(dst, ']'), nil
	case map[string]any:
		keys := make([]string, 0, len(v))
		for key := range v {
			keys = append(keys, key)
		}
		sort.Slice(keys, func(i, j int) bool { return lessUTF16(keys[i], keys[j]) })
		dst = append(dst, '{')
		for i, key := range keys {
			if i > 0 {
				dst = append(dst, ',')
			}
			dst = appendCanonicalString(dst, key)
			dst = append(dst, ':')
			var err error
			dst, err = appendCanonical(dst, v[key])
			if err != nil {
				return nil, err
			}
		}
		return append(dst, '}'), nil
	}
	return nil, fmt.Errorf("%w: %T is not a JSON value", ErrWire, value)
}

// lessUTF16 orders strings by their UTF-16 code units, the JCS collation.
func lessUTF16(a, b string) bool {
	ua, ub := utf16.Encode([]rune(a)), utf16.Encode([]rune(b))
	for i := 0; i < len(ua) && i < len(ub); i++ {
		if ua[i] != ub[i] {
			return ua[i] < ub[i]
		}
	}
	return len(ua) < len(ub)
}

// appendCanonicalString appends a JCS string literal: only the mandatory
// escapes (quote, backslash, the control characters) are written, everything
// else stays literal UTF-8 (RFC 8785 §3.2.2.2).
func appendCanonicalString(dst []byte, s string) []byte {
	dst = append(dst, '"')
	for _, r := range s {
		switch r {
		case '"':
			dst = append(dst, '\\', '"')
		case '\\':
			dst = append(dst, '\\', '\\')
		case '\b':
			dst = append(dst, '\\', 'b')
		case '\t':
			dst = append(dst, '\\', 't')
		case '\n':
			dst = append(dst, '\\', 'n')
		case '\f':
			dst = append(dst, '\\', 'f')
		case '\r':
			dst = append(dst, '\\', 'r')
		default:
			if r < 0x20 {
				dst = append(dst, fmt.Sprintf("\\u%04x", r)...)
				continue
			}
			dst = append(dst, string(r)...)
		}
	}
	return append(dst, '"')
}

// appendCanonicalNumber appends a number in the ECMAScript Number::toString
// form JCS requires (RFC 8785 §3.2.2.3): decimal notation between 1e-6 and
// 1e21, exponential outside it, shortest round-trip digits, and no trailing
// ".0".
func appendCanonicalNumber(dst []byte, f float64) ([]byte, error) {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return nil, fmt.Errorf("%w: number is not finite", ErrWire)
	}
	if f == 0 {
		return append(dst, '0'), nil
	}
	// The shortest round-trip digits, in the form "d[.ddd]e±XX": digit count k
	// and decimal exponent e (the value is 0.digits × 10^(e+1)).
	scientific := strconv.FormatFloat(f, 'e', -1, 64)
	mantissa, exponentText, _ := strings.Cut(scientific, "e")
	exp, err := strconv.Atoi(exponentText)
	if err != nil {
		return nil, fmt.Errorf("%w: format number %v", ErrWire, f)
	}
	negative := strings.HasPrefix(mantissa, "-")
	mantissa = strings.TrimPrefix(mantissa, "-")
	digits := strings.Replace(mantissa, ".", "", 1)
	k := len(digits)
	n := exp + 1
	if negative {
		dst = append(dst, '-')
	}
	switch {
	case k <= n && n <= 21:
		dst = append(dst, digits...)
		for i := 0; i < n-k; i++ {
			dst = append(dst, '0')
		}
	case 0 < n && n <= 21:
		dst = append(dst, digits[:n]...)
		dst = append(dst, '.')
		dst = append(dst, digits[n:]...)
	case -6 < n && n <= 0:
		dst = append(dst, '0', '.')
		for i := 0; i < -n; i++ {
			dst = append(dst, '0')
		}
		dst = append(dst, digits...)
	default:
		dst = append(dst, digits[0])
		if k > 1 {
			dst = append(dst, '.')
			dst = append(dst, digits[1:]...)
		}
		dst = append(dst, 'e')
		if n-1 >= 0 {
			dst = append(dst, '+')
		}
		dst = append(dst, strconv.Itoa(n-1)...)
	}
	return dst, nil
}
