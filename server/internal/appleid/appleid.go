// Package appleid implements Sign in with Apple for the web: the authorization redirect, the
// server-side code exchange, and verification of the identity token Apple returns.
package appleid

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/onefourtybpm/playlist-translator/server/internal/apple"
)

const issuer = "https://appleid.apple.com"

// Client signs people in with their Apple Account. ServicesID is the Services ID registered in
// the Apple developer portal, which Apple calls the client ID.
type Client struct {
	TeamID      string
	ServicesID  string
	KeyID       string
	RedirectURL string

	// Apple's endpoints; overridable in tests.
	AuthURL  string
	TokenURL string
	KeysURL  string

	HTTP *http.Client
	key  *ecdsa.PrivateKey
	now  func() time.Time

	mu   sync.Mutex
	keys map[string]crypto.PublicKey // Apple's token-signing keys by key ID
}

// Load reads the Sign in with Apple private key (.p8) and returns a client.
func Load(teamID, servicesID, keyID, keyPath, redirectURL string) (*Client, error) {
	if teamID == "" || servicesID == "" || keyID == "" {
		return nil, errors.New("team ID, Services ID and key ID are required")
	}
	pemBytes, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, fmt.Errorf("read Sign in with Apple key: %w", err)
	}
	key, err := apple.ParsePrivateKey(pemBytes)
	if err != nil {
		return nil, err
	}
	return New(teamID, servicesID, keyID, key, redirectURL), nil
}

// New returns a client that signs its requests to Apple with key.
func New(teamID, servicesID, keyID string, key *ecdsa.PrivateKey, redirectURL string) *Client {
	return &Client{
		TeamID: teamID, ServicesID: servicesID, KeyID: keyID, RedirectURL: redirectURL,
		AuthURL:  issuer + "/auth/authorize",
		TokenURL: issuer + "/auth/token",
		KeysURL:  issuer + "/auth/keys",
		HTTP:     &http.Client{Timeout: 15 * time.Second},
		key:      key,
		now:      time.Now,
	}
}

// AuthorizeURL is where the browser goes to sign in. Asking for the name and email means Apple
// POSTs the result back to RedirectURL (response_mode=form_post), not a GET redirect.
func (c *Client) AuthorizeURL(state, nonce string) string {
	q := url.Values{
		"client_id":     {c.ServicesID},
		"redirect_uri":  {c.RedirectURL},
		"response_type": {"code"},
		"response_mode": {"form_post"},
		"scope":         {"name email"},
		"state":         {state},
		"nonce":         {nonce},
	}
	return c.AuthURL + "?" + q.Encode()
}

// Identity is who Apple says signed in.
type Identity struct {
	// Subject is Apple's stable, unique ID for this person within our developer team.
	Subject       string
	Email         string
	EmailVerified bool
	// PrivateEmail is true when Email is a Hide My Email relay address.
	PrivateEmail bool
}

// Exchange trades the authorization code from Apple's callback for an identity token, verifies
// it, and returns who signed in.
func (c *Client) Exchange(ctx context.Context, code, nonce string) (Identity, error) {
	secret, err := c.clientSecret()
	if err != nil {
		return Identity{}, err
	}
	form := url.Values{
		"client_id":     {c.ServicesID},
		"client_secret": {secret},
		"code":          {code},
		"grant_type":    {"authorization_code"},
		"redirect_uri":  {c.RedirectURL},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return Identity{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return Identity{}, fmt.Errorf("appleid: token request: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return Identity{}, fmt.Errorf("appleid: token request: %s: %s", resp.Status, body)
	}
	var tok struct {
		IDToken string `json:"id_token"`
	}
	if err := json.Unmarshal(body, &tok); err != nil || tok.IDToken == "" {
		return Identity{}, fmt.Errorf("appleid: token response has no id_token: %s", body)
	}
	return c.Verify(ctx, tok.IDToken, nonce)
}

// clientSecret is the short-lived JWT that authenticates us to Apple's token endpoint.
func (c *Client) clientSecret() (string, error) {
	now := c.now()
	return apple.SignJWT(c.key, map[string]string{"alg": "ES256", "kid": c.KeyID}, map[string]any{
		"iss": c.TeamID,
		"iat": now.Add(-time.Minute).Unix(),
		"exp": now.Add(5 * time.Minute).Unix(),
		"aud": issuer,
		"sub": c.ServicesID,
	})
}

// Verify checks an identity token's signature and claims, as Apple's "Verifying a user" asks:
// signed by Apple, issued by Apple, for our Services ID, not expired, and carrying our nonce.
func (c *Client) Verify(ctx context.Context, idToken, nonce string) (Identity, error) {
	parts := strings.Split(idToken, ".")
	if len(parts) != 3 {
		return Identity{}, errors.New("appleid: identity token isn't a JWT")
	}
	var header struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if err := decodePart(parts[0], &header); err != nil {
		return Identity{}, err
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return Identity{}, errors.New("appleid: bad token signature encoding")
	}
	key, err := c.publicKey(ctx, header.Kid)
	if err != nil {
		return Identity{}, err
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	switch k := key.(type) {
	case *rsa.PublicKey:
		if header.Alg != "RS256" || rsa.VerifyPKCS1v15(k, crypto.SHA256, digest[:], sig) != nil {
			return Identity{}, errors.New("appleid: identity token signature is invalid")
		}
	case *ecdsa.PublicKey:
		if header.Alg != "ES256" || len(sig) != 64 ||
			!ecdsa.Verify(k, digest[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
			return Identity{}, errors.New("appleid: identity token signature is invalid")
		}
	default:
		return Identity{}, errors.New("appleid: unsupported signing key")
	}

	var claims struct {
		Iss            string   `json:"iss"`
		Aud            audience `json:"aud"`
		Exp            int64    `json:"exp"`
		Sub            string   `json:"sub"`
		Nonce          string   `json:"nonce"`
		NonceSupported flag     `json:"nonce_supported"`
		Email          string   `json:"email"`
		EmailVerified  flag     `json:"email_verified"`
		IsPrivateEmail flag     `json:"is_private_email"`
	}
	if err := decodePart(parts[1], &claims); err != nil {
		return Identity{}, err
	}
	switch {
	case claims.Iss != issuer:
		return Identity{}, fmt.Errorf("appleid: token issued by %q", claims.Iss)
	case !claims.Aud.has(c.ServicesID):
		return Identity{}, errors.New("appleid: token is for a different client")
	case !c.now().Before(time.Unix(claims.Exp, 0)):
		return Identity{}, errors.New("appleid: token has expired")
	case nonce == "" || (claims.Nonce != nonce && (claims.Nonce != "" || bool(claims.NonceSupported))):
		// Apple: a token without our nonce is acceptable only where the platform doesn't
		// support nonces (nonce_supported false). The token came straight from Apple over TLS
		// for a code our state cookie tied to this browser, so that's still safe.
		return Identity{}, errors.New("appleid: token nonce doesn't match")
	case claims.Sub == "":
		return Identity{}, errors.New("appleid: token has no subject")
	}
	return Identity{
		Subject:       claims.Sub,
		Email:         claims.Email,
		EmailVerified: bool(claims.EmailVerified),
		PrivateEmail:  bool(claims.IsPrivateEmail),
	}, nil
}

// publicKey returns Apple's signing key with this ID, refetching Apple's key set when it sees a
// key it doesn't know (Apple rotates them).
func (c *Client) publicKey(ctx context.Context, kid string) (crypto.PublicKey, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if k, ok := c.keys[kid]; ok {
		return k, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.KeysURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("appleid: fetch keys: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("appleid: fetch keys: %s", resp.Status)
	}
	var set struct {
		Keys []struct {
			Kty string `json:"kty"`
			Kid string `json:"kid"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&set); err != nil {
		return nil, fmt.Errorf("appleid: decode keys: %w", err)
	}
	keys := map[string]crypto.PublicKey{}
	for _, k := range set.Keys {
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
	c.keys = keys
	if k, ok := keys[kid]; ok {
		return k, nil
	}
	return nil, fmt.Errorf("appleid: unknown signing key %q", kid)
}

func decodePart(part string, v any) error {
	b, err := base64.RawURLEncoding.DecodeString(part)
	if err != nil {
		return errors.New("appleid: bad token encoding")
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("appleid: bad token: %w", err)
	}
	return nil
}

// flag reads Apple's booleans, which "can either be a string or a Boolean".
type flag bool

func (f *flag) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	*f = flag(s == "true")
	return nil
}

// audience reads the JWT "aud" claim, which may be a string or a list of strings.
type audience []string

func (a *audience) UnmarshalJSON(b []byte) error {
	var one string
	if err := json.Unmarshal(b, &one); err == nil {
		*a = audience{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(b, &many); err != nil {
		return err
	}
	*a = many
	return nil
}

func (a audience) has(s string) bool {
	for _, v := range a {
		if v == s {
			return true
		}
	}
	return false
}

// FirstSignInUser is the "user" form field Apple posts only the first time someone signs in.
type FirstSignInUser struct {
	Name struct {
		FirstName string `json:"firstName"`
		LastName  string `json:"lastName"`
	} `json:"name"`
}

// ParseFirstSignInName returns the name from Apple's "user" form field, or "" if absent. It comes
// straight from the browser, so it's only used as a suggestion for a display name.
func ParseFirstSignInName(userField string) string {
	if userField == "" {
		return ""
	}
	var u FirstSignInUser
	if json.Unmarshal([]byte(userField), &u) != nil {
		return ""
	}
	return strings.TrimSpace(u.Name.FirstName + " " + u.Name.LastName)
}
