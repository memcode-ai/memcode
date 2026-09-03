package webjwt

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// This package is the authentication boundary for inbound platform webhooks
// (Bot Framework, Google Chat). "Fails closed" is a claim worth proving rather
// than asserting, so these tests exercise each way a token can be wrong.

const (
	testIssuer = "https://issuer.example/v1"
	testAud    = "aud-under-test"
	testKID    = "key-1"
)

// jwksServer serves a JWKS containing key, counting how many times it is hit so
// the refresh throttle can be observed.
type jwksServer struct {
	*httptest.Server
	hits atomic.Int64
}

func newJWKS(t *testing.T, kid string, pub *rsa.PublicKey) *jwksServer {
	t.Helper()
	js := &jwksServer{}
	js.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		js.hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kty": "RSA",
			"kid": kid,
			"n":   base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
			"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
		}}})
	}))
	t.Cleanup(js.Close)
	return js
}

// signed mints a compact RS256 token with the given claims and kid.
func signed(t *testing.T, key *rsa.PrivateKey, kid string, claims jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = kid
	raw, err := tok.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func goodClaims() jwt.MapClaims {
	return jwt.MapClaims{
		"iss": testIssuer,
		"aud": testAud,
		"exp": time.Now().Add(10 * time.Minute).Unix(),
		"iat": time.Now().Add(-time.Minute).Unix(),
	}
}

func newKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// TestVerifyAcceptsAWellFormedToken is the baseline: without it, every negative
// test below could pass for the wrong reason.
func TestVerifyAcceptsAWellFormedToken(t *testing.T) {
	key := newKey(t)
	js := newJWKS(t, testKID, &key.PublicKey)
	v := &Verifier{JWKSURL: js.URL, Issuer: testIssuer, Audience: testAud}

	if err := v.Verify(context.Background(), signed(t, key, testKID, goodClaims())); err != nil {
		t.Fatalf("a valid token must verify: %v", err)
	}
}

// TestVerifyFailsClosed walks every way a token can be wrong. Each case must be
// an error — a webhook verifier that accepts any of these is an open door.
func TestVerifyFailsClosed(t *testing.T) {
	key := newKey(t)
	other := newKey(t)
	js := newJWKS(t, testKID, &key.PublicKey)

	expired := goodClaims()
	expired["exp"] = time.Now().Add(-time.Minute).Unix()

	unexpiring := goodClaims()
	delete(unexpiring, "exp")

	wrongIss := goodClaims()
	wrongIss["iss"] = "https://attacker.example"

	wrongAud := goodClaims()
	wrongAud["aud"] = "someone-elses-app"

	for _, tc := range []struct {
		name string
		tok  string
		why  string
	}{
		{"expired", signed(t, key, testKID, expired), "an expired token is replayable forever"},
		{"no expiry at all", signed(t, key, testKID, unexpiring), "a token that never expires is a permanent credential"},
		{"wrong issuer", signed(t, key, testKID, wrongIss), "another platform could mint tokens for this endpoint"},
		{"wrong audience", signed(t, key, testKID, wrongAud), "a token for a different app would be accepted"},
		{"signed by an unknown key", signed(t, other, testKID, goodClaims()), "anyone could sign their own tokens"},
		{"unknown kid", signed(t, key, "not-a-real-kid", goodClaims()), "the key must actually be published"},
		{"garbage", "not.a.token", "malformed input must not panic or pass"},
		{"empty", "", "an absent token must not verify"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := &Verifier{JWKSURL: js.URL, Issuer: testIssuer, Audience: testAud}
			if err := v.Verify(context.Background(), tc.tok); err == nil {
				t.Errorf("must be rejected — %s", tc.why)
			}
		})
	}
}

// TestUnsignedTokenIsRejected covers the alg-confusion classic: a token with
// "alg":"none", or one signed with a symmetric algorithm, must never verify
// against an RSA key set.
func TestUnsignedTokenIsRejected(t *testing.T) {
	key := newKey(t)
	js := newJWKS(t, testKID, &key.PublicKey)
	v := &Verifier{JWKSURL: js.URL, Issuer: testIssuer, Audience: testAud}

	none := jwt.NewWithClaims(jwt.SigningMethodNone, goodClaims())
	none.Header["kid"] = testKID
	raw, err := none.SignedString(jwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatal(err)
	}
	if err := v.Verify(context.Background(), raw); err == nil {
		t.Error(`an "alg":"none" token must never verify`)
	}

	hs := jwt.NewWithClaims(jwt.SigningMethodHS256, goodClaims())
	hs.Header["kid"] = testKID
	if rawHS, err := hs.SignedString([]byte("guessable")); err == nil {
		if err := v.Verify(context.Background(), rawHS); err == nil {
			t.Error("an HMAC-signed token must not verify against an RSA key set")
		}
	}
}

// TestMissingConfigurationNeverVerifies: an unconfigured verifier must reject
// everything rather than defaulting to permissive.
func TestMissingConfigurationNeverVerifies(t *testing.T) {
	key := newKey(t)
	js := newJWKS(t, testKID, &key.PublicKey)
	tok := signed(t, key, testKID, goodClaims())

	for _, tc := range []struct {
		name string
		v    *Verifier
	}{
		{"no issuer", &Verifier{JWKSURL: js.URL, Audience: testAud}},
		{"no audience", &Verifier{JWKSURL: js.URL, Issuer: testIssuer}},
		{"no key source", &Verifier{Issuer: testIssuer, Audience: testAud}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.v.Verify(context.Background(), tok); err == nil {
				t.Error("an unconfigured verifier must fail closed")
			}
		})
	}
}

// TestUnknownKidFloodIsThrottled pins the stated defense: an unknown kid may
// trigger at most one refetch per interval, so forged random kids cannot be
// amplified into unbounded traffic against the platform's JWKS host.
func TestUnknownKidFloodIsThrottled(t *testing.T) {
	key := newKey(t)
	js := newJWKS(t, testKID, &key.PublicKey)
	v := &Verifier{JWKSURL: js.URL, Issuer: testIssuer, Audience: testAud}

	for i := 0; i < 25; i++ {
		_ = v.Verify(context.Background(), signed(t, key, "forged-kid", goodClaims()))
	}
	if hits := js.hits.Load(); hits > 2 {
		t.Errorf("25 forged kids caused %d JWKS fetches; the throttle should bound this to ~1", hits)
	}

	// The throttle must not lock out a legitimate token already in the key set.
	if err := v.Verify(context.Background(), signed(t, key, testKID, goodClaims())); err != nil {
		t.Errorf("a known kid must still verify during a flood: %v", err)
	}
}

// TestMetadataURLIsFollowed covers the Bot Framework path: the verifier reads
// jwks_uri out of an OpenID configuration document rather than being handed the
// JWKS endpoint directly.
func TestMetadataURLIsFollowed(t *testing.T) {
	key := newKey(t)
	js := newJWKS(t, testKID, &key.PublicKey)

	meta := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"jwks_uri": js.URL})
	}))
	defer meta.Close()

	v := &Verifier{MetadataURL: meta.URL, Issuer: testIssuer, Audience: testAud}
	if err := v.Verify(context.Background(), signed(t, key, testKID, goodClaims())); err != nil {
		t.Fatalf("metadata discovery must reach the JWKS: %v", err)
	}
}

// TestUnreachableJWKSFailsClosed: if the key source is down, tokens are rejected
// rather than admitted.
func TestUnreachableJWKSFailsClosed(t *testing.T) {
	key := newKey(t)
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusInternalServerError)
	}))
	defer dead.Close()

	v := &Verifier{JWKSURL: dead.URL, Issuer: testIssuer, Audience: testAud}
	err := v.Verify(context.Background(), signed(t, key, testKID, goodClaims()))
	if err == nil {
		t.Fatal("an unreachable key source must reject, never admit")
	}
	if strings.Contains(strings.ToLower(err.Error()), "panic") {
		t.Errorf("unexpected failure shape: %v", err)
	}
}
