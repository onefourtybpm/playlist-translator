package appleid

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// FakeApple stands in for appleid.apple.com: it hands out identity tokens for codes registered
// with it, signed with its own RSA key, and publishes that key.
type fakeApple struct {
	key    *rsa.PrivateKey
	tokens map[string]map[string]any // code → identity token claims
	// clientSecrets records the client_secret JWTs the client sent.
	clientSecrets []string
}

func newFake(t *testing.T) (*fakeApple, *httptest.Server) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeApple{key: key, tokens: map[string]map[string]any{}}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/auth/keys":
			json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
				"kty": "RSA", "kid": "k1", "alg": "RS256", "use": "sig",
				"n": b64(key.N.Bytes()), "e": b64(big.NewInt(int64(key.E)).Bytes()),
			}}})
		case "/auth/token":
			f.clientSecrets = append(f.clientSecrets, r.FormValue("client_secret"))
			claims, ok := f.tokens[r.FormValue("code")]
			if !ok || r.FormValue("grant_type") != "authorization_code" {
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprint(w, `{"error":"invalid_grant"}`)
				return
			}
			fmt.Fprintf(w, `{"access_token":"a","token_type":"Bearer","expires_in":3600,"id_token":%q}`, f.sign("k1", claims))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(ts.Close)
	return f, ts
}

func (f *fakeApple) sign(kid string, claims map[string]any) string {
	h, _ := json.Marshal(map[string]string{"alg": "RS256", "kid": kid})
	c, _ := json.Marshal(claims)
	input := b64(h) + "." + b64(c)
	digest := sha256.Sum256([]byte(input))
	sig, _ := rsa.SignPKCS1v15(rand.Reader, f.key, crypto.SHA256, digest[:])
	return input + "." + b64(sig)
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func newTestClient(t *testing.T, fake *httptest.Server) (*Client, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	c := New("TEAM", "com.example.web", "SIWAKEY", key, "https://app.example/auth/apple/callback")
	c.TokenURL = fake.URL + "/auth/token"
	c.KeysURL = fake.URL + "/auth/keys"
	return c, key
}

func claims(overrides map[string]any) map[string]any {
	c := map[string]any{
		"iss": "https://appleid.apple.com", "aud": "com.example.web", "sub": "001.abc",
		"exp": time.Now().Add(10 * time.Minute).Unix(), "iat": time.Now().Unix(), "nonce": "n1",
		"email": "Friend@iCloud.com", "email_verified": "true", "is_private_email": false,
	}
	for k, v := range overrides {
		if v == nil {
			delete(c, k)
		} else {
			c[k] = v
		}
	}
	return c
}

func TestExchange(t *testing.T) {
	fake, ts := newFake(t)
	client, key := newTestClient(t, ts)
	fake.tokens["code1"] = claims(nil)

	id, err := client.Exchange(context.Background(), "code1", "n1")
	if err != nil {
		t.Fatal(err)
	}
	if id.Subject != "001.abc" || id.Email != "Friend@iCloud.com" || !id.EmailVerified || id.PrivateEmail {
		t.Errorf("identity = %+v", id)
	}

	// The client secret is an ES256 JWT for our Services ID, signed with our key.
	parts := strings.Split(fake.clientSecrets[0], ".")
	var header, body map[string]any
	decodePart(parts[0], &header)
	decodePart(parts[1], &body)
	if header["alg"] != "ES256" || header["kid"] != "SIWAKEY" || body["iss"] != "TEAM" ||
		body["sub"] != "com.example.web" || body["aud"] != "https://appleid.apple.com" {
		t.Errorf("client secret header %v, claims %v", header, body)
	}
	sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if !ecdsa.Verify(&key.PublicKey, digest[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
		t.Error("client secret signature doesn't verify")
	}

	if _, err := client.Exchange(context.Background(), "unknown", "n1"); err == nil {
		t.Error("unknown code: want error")
	}
}

func TestVerifyRejectsBadTokens(t *testing.T) {
	fake, ts := newFake(t)
	client, _ := newTestClient(t, ts)
	ctx := context.Background()

	cases := map[string]string{
		"wrong issuer":   fake.sign("k1", claims(map[string]any{"iss": "https://evil.example"})),
		"wrong audience": fake.sign("k1", claims(map[string]any{"aud": "com.other.web"})),
		"expired":        fake.sign("k1", claims(map[string]any{"exp": time.Now().Add(-time.Minute).Unix()})),
		"wrong nonce":    fake.sign("k1", claims(map[string]any{"nonce": "other"})),
		"missing nonce where supported": fake.sign("k1",
			claims(map[string]any{"nonce": nil, "nonce_supported": true})),
		"unknown key": fake.sign("k2", claims(nil)),
		"no subject":  fake.sign("k1", claims(map[string]any{"sub": ""})),
	}
	tampered := strings.Split(fake.sign("k1", claims(nil)), ".")
	other, _ := json.Marshal(claims(map[string]any{"sub": "someone-else"}))
	cases["tampered"] = tampered[0] + "." + b64(other) + "." + tampered[2]

	for name, token := range cases {
		if _, err := client.Verify(ctx, token, "n1"); err == nil {
			t.Errorf("%s: want error", name)
		}
	}

	// Apple's booleans may be strings or booleans, and a missing nonce is fine where the platform
	// says it doesn't support them.
	ok := fake.sign("k1", claims(map[string]any{
		"nonce": nil, "nonce_supported": "false", "email_verified": true, "is_private_email": "true",
		"aud": []string{"com.example.web"},
	}))
	id, err := client.Verify(ctx, ok, "n1")
	if err != nil || !id.EmailVerified || !id.PrivateEmail {
		t.Errorf("Verify() = %+v, %v", id, err)
	}
}

func TestAuthorizeURL(t *testing.T) {
	c := New("TEAM", "com.example.web", "K", nil, "https://app.example/auth/apple/callback")
	u := c.AuthorizeURL("s1", "n1")
	for _, want := range []string{
		"client_id=com.example.web", "response_mode=form_post", "scope=name+email", "state=s1", "nonce=n1",
		"redirect_uri=https%3A%2F%2Fapp.example%2Fauth%2Fapple%2Fcallback",
	} {
		if !strings.Contains(u, want) {
			t.Errorf("%s: missing %s", u, want)
		}
	}
}

func TestParseFirstSignInName(t *testing.T) {
	if got := ParseFirstSignInName(`{"name":{"firstName":"Maria","lastName":"Ruiz"},"email":"x@y.z"}`); got != "Maria Ruiz" {
		t.Errorf("got %q", got)
	}
	if got := ParseFirstSignInName(`not json`); got != "" {
		t.Errorf("got %q", got)
	}
}
