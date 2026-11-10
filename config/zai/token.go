package zai

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/luckystrker/ago/config"
)

// tokenTTLMillis is how long a minted auth token stays valid. Python parity:
// the zai SDK's API_TOKEN_TTL_SECONDS = 210 (a 180 s cache window plus 30 s of
// slack).
const tokenTTLMillis int64 = 210 * 1000

// nowFunc supplies the minting clock; tests replace it to pin the claims.
var nowFunc = time.Now

// tokenHeader and tokenClaims are the JWT body the Z.AI SDK sends when the
// token cache is enabled.
type tokenHeader struct {
	Alg      string `json:"alg"`
	SignType string `json:"sign_type"`
}

type tokenClaims struct {
	APIKey    string `json:"api_key"`
	Exp       int64  `json:"exp"`
	Timestamp int64  `json:"timestamp"`
}

// splitAPIKey parses the "<id>.<secret>" API key form the token path needs:
// the id travels in the claims, the secret signs them.
func splitAPIKey(apiKey string) (id, secret string, err error) {
	id, secret, ok := strings.Cut(apiKey, ".")
	if !ok || id == "" || secret == "" {
		return "", "", fmt.Errorf("%w: zai: an api key must look like \"<id>.<secret>\" to mint a token", config.ErrNotProvided)
	}
	return id, secret, nil
}

// mintAuthToken signs the short-lived bearer token the Z.AI API expects when
// the token cache is enabled: HS256 over the API key's "<id>.<secret>" form,
// with the id in the payload and the secret as the signing key (Python parity:
// zai/core/_jwt_token.generate_token).
//
// AGO-DEVIATION: the Python SDK caches the minted token for 180 s; AGo signs a
// fresh one on every request, so a tool loop longer than the token lifetime
// never sends an expired bearer. The wire value and its 210 s lifetime are the
// same; the cost is one HMAC per call.
func mintAuthToken(apiKey string, now time.Time) (string, error) {
	id, secret, err := splitAPIKey(apiKey)
	if err != nil {
		return "", err
	}
	head, err := json.Marshal(tokenHeader{Alg: "HS256", SignType: "SIGN"})
	if err != nil {
		return "", fmt.Errorf("zai: encoding token header: %w", err)
	}
	ms := now.UnixMilli()
	claims, err := json.Marshal(tokenClaims{APIKey: id, Exp: ms + tokenTTLMillis, Timestamp: ms})
	if err != nil {
		return "", fmt.Errorf("zai: encoding token claims: %w", err)
	}
	signingInput := base64.RawURLEncoding.EncodeToString(head) + "." + base64.RawURLEncoding.EncodeToString(claims)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(signingInput))
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}
