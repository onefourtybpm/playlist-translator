package apple

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"strings"
	"testing"
	"time"
)

func newTestSigner(t *testing.T) (*TokenSigner, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	s, err := NewTokenSigner("TEAM123456", "KEY1234567", pemBytes)
	if err != nil {
		t.Fatal(err)
	}
	return s, key
}

func TestTokenIsValidES256JWT(t *testing.T) {
	s, key := newTestSigner(t)
	now := time.Unix(1_800_000_000, 0)
	s.now = func() time.Time { return now }

	token, err := s.Token()
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("token has %d parts, want 3", len(parts))
	}

	var header map[string]string
	decodePart(t, parts[0], &header)
	if header["alg"] != "ES256" || header["kid"] != "KEY1234567" || header["typ"] != "JWT" {
		t.Errorf("header = %v", header)
	}
	var claims map[string]any
	decodePart(t, parts[1], &claims)
	if claims["iss"] != "TEAM123456" || claims["iat"] != float64(now.Add(-time.Minute).Unix()) || claims["exp"] != float64(now.Add(tokenTTL).Unix()) {
		t.Errorf("claims = %v", claims)
	}

	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(sig) != 64 {
		t.Fatalf("signature: len %d, err %v", len(sig), err)
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	r, ss := new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])
	if !ecdsa.Verify(&key.PublicKey, digest[:], r, ss) {
		t.Error("signature doesn't verify")
	}
}

func TestTokenIsCachedUntilNearExpiry(t *testing.T) {
	s, _ := newTestSigner(t)
	now := time.Unix(1_800_000_000, 0)
	s.now = func() time.Time { return now }

	first, _ := s.Token()
	now = now.Add(tokenTTL - tokenRenewBefore - time.Minute)
	if again, _ := s.Token(); again != first {
		t.Error("token was replaced before it needed renewing")
	}
	now = now.Add(2 * time.Minute)
	if renewed, _ := s.Token(); renewed == first {
		t.Error("token wasn't renewed near expiry")
	}
}

func TestNewTokenSignerRejectsBadKeys(t *testing.T) {
	if _, err := NewTokenSigner("T", "K", []byte("not a key")); err == nil {
		t.Error("accepted non-PEM key")
	}
	if _, err := NewTokenSigner("", "K", nil); err == nil {
		t.Error("accepted empty team ID")
	}
}

func decodePart(t *testing.T, part string, out any) {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(part)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		t.Fatal(err)
	}
}
