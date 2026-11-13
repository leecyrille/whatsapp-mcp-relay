package access

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Attack tests, matching C:\mcp\gateway\test_access_proxy.py. Uses throwaway keys.

var key, _ = rsa.GenerateKey(rand.Reader, 2048)
var otherKey, _ = rsa.GenerateKey(rand.Reader, 2048)

const issuer = "https://myteam.cloudflareaccess.com"

func verifier(t *testing.T) *Verifier {
	t.Helper()
	v, err := New("myteam", "aud-1", []string{"User@Example.com"}, func(string) (*rsa.PublicKey, error) { return &key.PublicKey, nil })
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func claims(over map[string]any) jwt.MapClaims {
	now := time.Now().Unix()
	c := jwt.MapClaims{"iss": issuer, "aud": []string{"aud-1"}, "email": "user@example.com", "iat": now, "exp": now + 300}
	for k, v := range over {
		if v == nil {
			delete(c, k)
		} else {
			c[k] = v
		}
	}
	return c
}

func sign(t *testing.T, k *rsa.PrivateKey, over map[string]any) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims(over))
	s, err := tok.SignedString(k)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func check(v *Verifier, token string) (string, int) {
	r := httptest.NewRequest("GET", "/", nil)
	if token != "" {
		r.Header.Set(HeaderName, token)
	}
	email, status, _ := v.Check(r)
	return email, status
}

func TestValidToken(t *testing.T) {
	email, status := check(verifier(t), sign(t, key, nil))
	if status != 200 || email != "user@example.com" {
		t.Fatalf("%d %q", status, email)
	}
}

func TestNoToken(t *testing.T) {
	if _, s := check(verifier(t), ""); s != 401 {
		t.Fatal(s)
	}
}

func TestMalformed(t *testing.T) {
	for _, bad := range []string{"garbage", "a.b.c", "..", "eyJhbGciOiJSUzI1NiJ9.e30."} {
		if _, s := check(verifier(t), bad); s != 401 {
			t.Errorf("%q: %d", bad, s)
		}
	}
}

func TestWrongSigningKey(t *testing.T) {
	if _, s := check(verifier(t), sign(t, otherKey, nil)); s != 401 {
		t.Fatal(s)
	}
}

func TestWrongAudience(t *testing.T) {
	if _, s := check(verifier(t), sign(t, key, map[string]any{"aud": []string{"another-access-app"}})); s != 401 {
		t.Fatal(s)
	}
}

func TestWrongIssuer(t *testing.T) {
	if _, s := check(verifier(t), sign(t, key, map[string]any{"iss": "https://evil.cloudflareaccess.com"})); s != 401 {
		t.Fatal(s)
	}
}

func TestExpired(t *testing.T) {
	now := time.Now().Unix()
	if _, s := check(verifier(t), sign(t, key, map[string]any{"iat": now - 900, "exp": now - 600})); s != 401 {
		t.Fatal(s)
	}
}

func TestMissingRequiredClaims(t *testing.T) {
	for _, missing := range []string{"exp", "iat", "iss", "aud"} {
		if _, s := check(verifier(t), sign(t, key, map[string]any{missing: nil})); s != 401 {
			t.Errorf("without %s: %d", missing, s)
		}
	}
}

func TestIssuedInFuture(t *testing.T) {
	now := time.Now().Unix()
	if _, s := check(verifier(t), sign(t, key, map[string]any{"iat": now + 3600, "exp": now + 7200})); s != 401 {
		t.Fatal(s)
	}
}

func enc(v any) string {
	b, _ := json.Marshal(v)
	return base64.RawURLEncoding.EncodeToString(b)
}

func TestAlgNone(t *testing.T) {
	forged := enc(map[string]string{"alg": "none", "typ": "JWT"}) + "." + enc(claims(nil)) + "."
	if _, s := check(verifier(t), forged); s != 401 {
		t.Fatal(s)
	}
}

func TestHS256KeyConfusion(t *testing.T) {
	der, _ := x509.MarshalPKIXPublicKey(&key.PublicKey)
	pub := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
	signing := enc(map[string]string{"alg": "HS256", "typ": "JWT"}) + "." + enc(claims(nil))
	mac := hmac.New(sha256.New, pub)
	mac.Write([]byte(signing))
	forged := signing + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if _, s := check(verifier(t), forged); s != 401 {
		t.Fatal(s)
	}
}

func TestEmailNotOnAllowlist(t *testing.T) {
	if _, s := check(verifier(t), sign(t, key, map[string]any{"email": "stranger@example.com"})); s != 403 {
		t.Fatal(s)
	}
}

func TestTokenWithoutEmail(t *testing.T) {
	if _, s := check(verifier(t), sign(t, key, map[string]any{"email": nil})); s != 403 {
		t.Fatal(s)
	}
}

func TestEmailCaseInsensitive(t *testing.T) {
	if _, s := check(verifier(t), sign(t, key, map[string]any{"email": "USER@example.COM"})); s != 200 {
		t.Fatal(s)
	}
}

func TestKeyLookupFailureRejects(t *testing.T) {
	v, _ := New("myteam", "aud-1", []string{"user@example.com"}, func(string) (*rsa.PublicKey, error) { return nil, fmt.Errorf("network down") })
	if _, s := check(v, sign(t, key, nil)); s != 401 {
		t.Fatal(s)
	}
}

func TestBadConfigFailsClosed(t *testing.T) {
	keys := func(string) (*rsa.PublicKey, error) { return nil, nil }
	good := []string{"user@example.com"}
	cases := []struct {
		team, aud string
		emails    []string
	}{
		{"", "a", good}, {"TODO", "a", good}, {"evil.com/x", "a", good}, {"a.b", "a", good},
		{"t", "", good}, {"t", "TODO", good},
		{"t", "a", nil}, {"t", "a", []string{"*"}}, {"t", "a", []string{"*@mail.example"}}, {"t", "a", []string{"notanemail"}},
	}
	for _, c := range cases {
		if _, err := New(c.team, c.aud, c.emails, keys); err == nil {
			t.Errorf("accepted %+v", c)
		}
	}
}

func jwksServer(t *testing.T, kid string, pub *rsa.PublicKey, hits *atomic.Int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kid": kid, "kty": "RSA",
			"n": base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
		}}})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestJWKSVerifiesARealToken(t *testing.T) {
	var hits atomic.Int32
	srv := jwksServer(t, "k1", &key.PublicKey, &hits)
	source := NewJWKS(srv.URL)
	v, _ := New("myteam", "aud-1", []string{"user@example.com"}, source.Key)
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims(nil))
	tok.Header["kid"] = "k1"
	s, _ := tok.SignedString(key)
	if _, status := check(v, s); status != 200 {
		t.Fatal(status)
	}
	check(v, s)
	if hits.Load() != 1 {
		t.Fatalf("keys fetched %d times", hits.Load())
	}
}

func TestJWKSUnknownKidCantFloodCloudflare(t *testing.T) {
	var hits atomic.Int32
	srv := jwksServer(t, "k1", &key.PublicKey, &hits)
	source := NewJWKS(srv.URL)
	for i := 0; i < 50; i++ {
		if _, err := source.Key("unknown"); err == nil {
			t.Fatal("unknown key accepted")
		}
	}
	if hits.Load() != 1 {
		t.Fatalf("keys fetched %d times", hits.Load())
	}
}
