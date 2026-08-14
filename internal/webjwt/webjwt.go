// Package webjwt is the ONE verifier for platform-signed inbound-webhook
// bearer tokens (Bot Framework, Google Chat): RS256 against a published JWKS,
// with issuer/audience/expiry enforced and the key set cached — an unknown kid
// triggers a refetch at most once per interval, so key rotation works but a
// forged-kid flood is not amplified into JWKS hammering. Two adapters verifying JWTs two
// different ways is how drift bugs happen; both use this.
//
// The golang-jwt SDK lives ONLY here (guarded by
// TestVendorSDKsOnlyInTheirAdapters).
package webjwt

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// maxDoc caps a fetched metadata/JWKS document.
const maxDoc = 1 << 20

// Verifier validates RS256 bearer tokens against one platform's JWKS. Set
// either JWKSURL directly (Google's certs endpoint) or MetadataURL (an OpenID
// configuration document whose jwks_uri is followed — Bot Framework). Fields
// are read on each Verify, so tests may point them at a fake server after
// construction and before first use.
type Verifier struct {
	MetadataURL string // OpenID configuration carrying jwks_uri; used when JWKSURL is empty
	JWKSURL     string // direct JWKS endpoint; takes precedence
	Issuer      string // required — an empty issuer never verifies
	Audience    string // required — an empty audience never verifies
	Client      *http.Client

	mu          sync.Mutex
	keys        map[string]*rsa.PublicKey
	lastRefresh time.Time // throttles refreshes so an unknown-kid flood can't hammer the JWKS host
}

// minRefreshInterval bounds how often an unknown kid may trigger a JWKS
// refetch. A flood of forged random kids is thus rate-limited to one outbound
// fetch per interval, not one per request.
const minRefreshInterval = 30 * time.Second

// Verify checks a raw compact JWT. It fails closed: missing configuration,
// unknown alg, unknown kid after one refresh, wrong issuer/audience, or an
// expired (or unexpiring) token are all errors.
func (v *Verifier) Verify(ctx context.Context, raw string) error {
	if v.Issuer == "" || v.Audience == "" {
		return errors.New("verifier not configured (issuer/audience)")
	}
	refreshed := false
	keyfunc := func(t *jwt.Token) (any, error) {
		kid, _ := t.Header["kid"].(string)
		if kid == "" {
			return nil, errors.New("token missing kid")
		}
		if k := v.cachedKey(kid); k != nil {
			return k, nil
		}
		if !refreshed {
			refreshed = true
			if err := v.refreshKeys(ctx); err != nil {
				return nil, err
			}
			if k := v.cachedKey(kid); k != nil {
				return k, nil
			}
		}
		return nil, fmt.Errorf("unknown signing key %q", kid)
	}
	_, err := jwt.Parse(raw, keyfunc,
		jwt.WithValidMethods([]string{"RS256"}),
		jwt.WithIssuer(v.Issuer),
		jwt.WithAudience(v.Audience),
		jwt.WithExpirationRequired(),
	)
	return err
}

func (v *Verifier) cachedKey(kid string) *rsa.PublicKey {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.keys[kid]
}

// refreshKeys resolves the JWKS location and replaces the key cache. Replacing
// (not merging) means revoked keys actually leave. Throttled: at most one
// outbound fetch per minRefreshInterval, so an unauthenticated flood of forged
// kids can't amplify into JWKS hammering (each caller sees a benign "unknown
// key" once the cache is warm and the interval hasn't elapsed).
func (v *Verifier) refreshKeys(ctx context.Context) error {
	v.mu.Lock()
	if !v.lastRefresh.IsZero() && time.Since(v.lastRefresh) < minRefreshInterval {
		v.mu.Unlock()
		return nil // recently refreshed; the caller's kid is treated as unknown
	}
	v.lastRefresh = time.Now()
	v.mu.Unlock()
	jwksURL := v.JWKSURL
	if jwksURL == "" {
		var meta struct {
			JWKSURI string `json:"jwks_uri"`
		}
		if err := v.getJSON(ctx, v.MetadataURL, &meta); err != nil {
			return fmt.Errorf("openid metadata: %w", err)
		}
		if meta.JWKSURI == "" {
			return errors.New("openid metadata has no jwks_uri")
		}
		jwksURL = meta.JWKSURI
	}
	var set struct {
		Keys []struct {
			Kty string `json:"kty"`
			Kid string `json:"kid"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := v.getJSON(ctx, jwksURL, &set); err != nil {
		return fmt.Errorf("jwks fetch: %w", err)
	}
	keys := make(map[string]*rsa.PublicKey, len(set.Keys))
	for _, k := range set.Keys {
		if k.Kty != "RSA" || k.Kid == "" {
			continue
		}
		pub, err := rsaFromJWK(k.N, k.E)
		if err != nil {
			continue // one malformed key must not poison the whole set
		}
		keys[k.Kid] = pub
	}
	if len(keys) == 0 {
		return errors.New("jwks contained no usable rsa keys")
	}
	v.mu.Lock()
	v.keys = keys
	v.mu.Unlock()
	return nil
}

// rsaFromJWK builds an RSA public key from base64url modulus and exponent.
func rsaFromJWK(n64, e64 string) (*rsa.PublicKey, error) {
	nb, err := base64.RawURLEncoding.DecodeString(n64)
	if err != nil {
		return nil, err
	}
	eb, err := base64.RawURLEncoding.DecodeString(e64)
	if err != nil {
		return nil, err
	}
	e := new(big.Int).SetBytes(eb)
	if !e.IsInt64() || e.Int64() <= 0 {
		return nil, errors.New("bad rsa exponent")
	}
	return &rsa.PublicKey{N: new(big.Int).SetBytes(nb), E: int(e.Int64())}, nil
}

func (v *Verifier) getJSON(ctx context.Context, u string, out any) error {
	if u == "" {
		return errors.New("no jwks location configured")
	}
	client := v.Client
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("get %s: status %d", u, resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, maxDoc)).Decode(out)
}
