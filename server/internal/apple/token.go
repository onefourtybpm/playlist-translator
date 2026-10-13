// Package apple is a minimal client for the parts of the Apple Music API this app uses.
package apple

import (
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"
)

const (
	// tokenTTL is how long each developer token lasts. Apple allows up to six months. MusicKit
	// sign-in (activate.apple.com/mkauth) started working once our tokens matched a known-good
	// site's: 100-day lifetime plus a "typ" header. It isn't proven which change mattered.
	tokenTTL = 100 * 24 * time.Hour
	// tokenRenewBefore is how early a cached token is replaced, so one handed to a browser
	// doesn't expire mid-session.
	tokenRenewBefore = 7 * 24 * time.Hour
)

// TokenSigner creates developer tokens: ES256 JWTs signed with a MusicKit private key (.p8).
// Both the server's API calls and MusicKit JS in the browser use them.
type TokenSigner struct {
	TeamID string
	KeyID  string
	key    *ecdsa.PrivateKey

	now func() time.Time // overridable in tests

	mu      sync.Mutex
	token   string
	expires time.Time
}

// LoadTokenSigner reads a MusicKit private key from a .p8 file.
func LoadTokenSigner(teamID, keyID, keyPath string) (*TokenSigner, error) {
	pemBytes, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, fmt.Errorf("read MusicKit key: %w", err)
	}
	return NewTokenSigner(teamID, keyID, pemBytes)
}

// NewTokenSigner parses a PEM-encoded PKCS#8 EC private key, the format Apple's .p8 files use.
func NewTokenSigner(teamID, keyID string, pemBytes []byte) (*TokenSigner, error) {
	if teamID == "" || keyID == "" {
		return nil, errors.New("apple: team ID and key ID are required")
	}
	key, err := ParsePrivateKey(pemBytes)
	if err != nil {
		return nil, err
	}
	return &TokenSigner{TeamID: teamID, KeyID: keyID, key: key, now: time.Now}, nil
}

// ParsePrivateKey parses an Apple .p8 private key: a PEM-encoded PKCS#8 P-256 EC key.
func ParsePrivateKey(pemBytes []byte) (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("apple: private key is not PEM-encoded")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("apple: parse private key: %w", err)
	}
	key, ok := parsed.(*ecdsa.PrivateKey)
	if !ok || key.Curve.Params().BitSize != 256 {
		return nil, errors.New("apple: private key must be a P-256 EC key")
	}
	return key, nil
}

// Token returns a valid developer token, reusing the cached one until it's close to expiring.
func (s *TokenSigner) Token() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	if s.token != "" && now.Before(s.expires.Add(-tokenRenewBefore)) {
		return s.token, nil
	}
	expires := now.Add(tokenTTL)
	// Date the token a minute back: Apple briefly rejects a brand-new token whose "issued at"
	// is ahead of its own clock.
	token, err := s.sign(now.Add(-time.Minute), expires)
	if err != nil {
		return "", err
	}
	s.token, s.expires = token, expires
	return token, nil
}

func (s *TokenSigner) sign(issued, expires time.Time) (string, error) {
	return SignJWT(s.key, map[string]string{"alg": "ES256", "kid": s.KeyID, "typ": "JWT"}, map[string]any{
		"iss": s.TeamID,
		"iat": issued.Unix(),
		"exp": expires.Unix(),
	})
}

// SignJWT signs an ES256 JSON Web Token with an Apple private key.
func SignJWT(key *ecdsa.PrivateKey, header map[string]string, claims map[string]any) (string, error) {
	h, err := json.Marshal(header)
	if err != nil {
		return "", err
	}
	c, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	signingInput := b64(h) + "." + b64(c)

	digest := sha256.Sum256([]byte(signingInput))
	r, sig, err := ecdsa.Sign(rand.Reader, key, digest[:])
	if err != nil {
		return "", err
	}
	// JWS wants the raw 64-byte r||s form, not the ASN.1 encoding Go produces by default.
	raw := make([]byte, 64)
	r.FillBytes(raw[:32])
	sig.FillBytes(raw[32:])
	return signingInput + "." + b64(raw), nil
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
