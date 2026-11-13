// Package access verifies the Cloudflare Access JWT on every request, the same checks as
// C:\mcp\gateway\access_proxy.py: RS256 signature from the team's keys, issuer, this app's audience,
// expiry, and an email on the allowlist. Anything that can't be verified is rejected.
package access

import (
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const HeaderName = "Cf-Access-Jwt-Assertion"

// KeyFunc returns the public key for a key ID.
type KeyFunc func(kid string) (*rsa.PublicKey, error)

// Verifier checks tokens for one Access app.
type Verifier struct {
	issuer  string
	aud     string
	allowed map[string]bool
	keys    KeyFunc
}

// New builds a verifier. It refuses config that couldn't protect anything.
func New(team, aud string, emails []string, keys KeyFunc) (*Verifier, error) {
	team = strings.ToLower(strings.TrimSpace(team))
	if team == "" || strings.ContainsAny(team, "./") || strings.Contains(strings.ToUpper(team), "TODO") {
		return nil, errors.New("team_domain must be the bare team name")
	}
	if aud == "" || strings.Contains(strings.ToUpper(aud), "TODO") {
		return nil, errors.New("aud is missing or a placeholder")
	}
	allowed := map[string]bool{}
	for _, e := range emails {
		e = strings.ToLower(strings.TrimSpace(e))
		if !strings.Contains(e, "@") || strings.Contains(e, "*") {
			return nil, fmt.Errorf("allowed_emails must be explicit addresses, got %q", e)
		}
		allowed[e] = true
	}
	if len(allowed) == 0 {
		return nil, errors.New("allowed_emails is empty")
	}
	return &Verifier{issuer: "https://" + team + ".cloudflareaccess.com", aud: aud, allowed: allowed, keys: keys}, nil
}

// Issuer is the expected token issuer.
func (v *Verifier) Issuer() string { return v.issuer }

// CertsURL is where the team publishes its signing keys.
func CertsURL(team string) string {
	return "https://" + strings.ToLower(strings.TrimSpace(team)) + ".cloudflareaccess.com/cdn-cgi/access/certs"
}

// Check returns the email and 200, or 401 for a token that doesn't verify, or 403 for a valid token
// whose email isn't allowed. why says what failed, for the log; it never contains the token.
func (v *Verifier) Check(r *http.Request) (email string, status int, why string) {
	token := r.Header.Get(HeaderName)
	if token == "" {
		return "", http.StatusUnauthorized, "no jwt"
	}
	claims := jwt.MapClaims{}
	_, err := jwt.ParseWithClaims(token, claims, func(t *jwt.Token) (any, error) {
		kid, _ := t.Header["kid"].(string)
		return v.keys(kid)
	},
		jwt.WithValidMethods([]string{"RS256"}),
		jwt.WithIssuer(v.issuer),
		jwt.WithAudience(v.aud),
		jwt.WithExpirationRequired(),
		jwt.WithIssuedAt(),
		jwt.WithLeeway(5*time.Second),
	)
	if err != nil {
		return "", http.StatusUnauthorized, classify(err)
	}
	if _, ok := claims["iat"]; !ok {
		return "", http.StatusUnauthorized, "no iat"
	}
	email, _ = claims["email"].(string)
	email = strings.ToLower(strings.TrimSpace(email))
	if !v.allowed[email] {
		return email, http.StatusForbidden, "email not allowed"
	}
	return email, http.StatusOK, ""
}

func classify(err error) string {
	for _, known := range []error{
		jwt.ErrTokenExpired, jwt.ErrTokenMalformed, jwt.ErrTokenSignatureInvalid, jwt.ErrTokenInvalidAudience,
		jwt.ErrTokenInvalidIssuer, jwt.ErrTokenUsedBeforeIssued, jwt.ErrTokenRequiredClaimMissing, jwt.ErrTokenUnverifiable,
	} {
		if errors.Is(err, known) {
			return known.Error()
		}
	}
	return "invalid token"
}

// JWKS fetches and caches the team's signing keys.
type JWKS struct {
	url     string
	client  *http.Client
	mu      sync.Mutex
	keys    map[string]*rsa.PublicKey
	fetched time.Time
	tried   time.Time
}

// NewJWKS makes a key source. Nothing is fetched until the first request.
func NewJWKS(url string) *JWKS {
	return &JWKS{url: url, client: &http.Client{Timeout: 10 * time.Second}, keys: map[string]*rsa.PublicKey{}}
}

const (
	jwksMaxAge   = time.Hour
	jwksRetryGap = 30 * time.Second
)

// Key returns a key by ID, refreshing when the cache is old or the ID is new, as happens when
// Cloudflare rotates keys. Refreshes are rate limited so bad tokens can't flood Cloudflare.
func (j *JWKS) Key(kid string) (*rsa.PublicKey, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	key := j.keys[kid]
	stale := time.Since(j.fetched) > jwksMaxAge
	if (key == nil || stale) && time.Since(j.tried) > jwksRetryGap {
		j.tried = time.Now()
		if keys, err := j.fetch(); err == nil {
			j.keys, j.fetched = keys, time.Now()
			key = keys[kid]
		}
	}
	if key == nil {
		return nil, errors.New("unknown signing key")
	}
	return key, nil
}

func (j *JWKS) fetch() (map[string]*rsa.PublicKey, error) {
	resp, err := j.client.Get(j.url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("certs: %d", resp.StatusCode)
	}
	var doc struct {
		Keys []struct {
			Kid string `json:"kid"`
			Kty string `json:"kty"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(nil, resp.Body, 1<<20)).Decode(&doc); err != nil {
		return nil, err
	}
	keys := map[string]*rsa.PublicKey{}
	for _, k := range doc.Keys {
		if k.Kty != "RSA" {
			continue
		}
		n, err1 := base64.RawURLEncoding.DecodeString(k.N)
		e, err2 := base64.RawURLEncoding.DecodeString(k.E)
		if err1 != nil || err2 != nil {
			continue
		}
		keys[k.Kid] = &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}
	}
	if len(keys) == 0 {
		return nil, errors.New("no usable keys")
	}
	return keys, nil
}
