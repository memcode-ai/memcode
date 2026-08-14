// Package grok turns a SuperGrok / X Premium+ subscription into a memcode
// backend via xAI's OAuth device-code flow. Unlike the claude/codex/copilot
// sources, there is no other tool's login to reuse: memcode runs the login
// itself (accounts.x.ai device code, the flow the grok CLI client is
// registered for) and owns the token file. The resulting bearer token drives
// api.x.ai/v1 through the shared Grok Responses adapter unchanged.
//
// Consent-gated upstream like every subscription source: it activates only
// when the user explicitly selects the grok source, and the login itself is
// an interactive approval in the user's browser.
package grok

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	// clientID is the OAuth client the grok CLI uses; the device-code flow and
	// refreshes are registered under it.
	clientID = "b1a00492-073a-47ea-816f-4c329264a828"
	scope    = "openid profile email offline_access grok-cli:access api:access"

	// BaseURL is the xAI inference host the subscription token is valid for.
	BaseURL = "https://api.x.ai/v1"
)

// Vars for tests.
var (
	issuer          = "https://auth.x.ai"
	deviceCodeURL   = issuer + "/oauth2/device/code"
	discoveryURL    = issuer + "/.well-known/openid-configuration"
	httpTimeout     = 20 * time.Second
	sleep           = time.Sleep
	tokenPathEnvDir = "XDG_CONFIG_HOME" // same root as the global env file
)

// refreshSkew refreshes well before expiry: xAI subscription access tokens are
// short-lived (~6h), and a long-lived gateway may only touch the provider every
// so often — a wide skew keeps the token warm instead of surfacing brief
// credential-expiry gaps.
const refreshSkew = time.Hour

// ErrTierGated marks a 403 from the token endpoint: the OAuth grant is valid
// but xAI's backend does not allow API access for this subscription tier.
// Re-logging in won't change it; the fix is an XAI_API_KEY or a higher tier.
var ErrTierGated = errors.New("this xAI account's subscription tier is not allowed API access over OAuth — set XAI_API_KEY instead, or upgrade at x.ai/grok")

// creds is the on-disk token state, owned by memcode.
type creds struct {
	AccessToken   string `json:"access_token"`
	RefreshToken  string `json:"refresh_token"`
	TokenEndpoint string `json:"token_endpoint"`
	// ExpiresAt is epoch seconds; 0 = unknown (treated as expiring).
	ExpiresAt   int64  `json:"expires_at"`
	LastRefresh string `json:"last_refresh,omitempty"`
}

// tokenPath is memcode's own token file, next to the global env file.
func tokenPath() string {
	dir := os.Getenv(tokenPathEnvDir)
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "memcode", "grok.json")
}

// Available reports whether a Grok login is stored, for the wizard's menu (no
// parse, no network).
func Available() bool {
	p := tokenPath()
	if p == "" {
		return false
	}
	_, err := os.Stat(p)
	return err == nil
}

// Resolve returns a live access token, refreshing it first when it is close to
// expiry. An error explains the fix (run `memcode auth use grok`).
func Resolve() (string, error) {
	c, err := read()
	if err != nil {
		return "", err
	}
	if time.Now().Unix() < c.ExpiresAt-int64(refreshSkew/time.Second) {
		return c.AccessToken, nil
	}
	refreshed, err := refresh(c)
	if err != nil {
		// A failed refresh with a still-unexpired access token is survivable:
		// serve the current token rather than failing the turn.
		if !errors.Is(err, ErrTierGated) && time.Now().Unix() < c.ExpiresAt {
			return c.AccessToken, nil
		}
		return "", err
	}
	write(refreshed)
	return refreshed.AccessToken, nil
}

// Login runs the device-code flow: it requests a code, hands the verification
// URL and user code to prompt (which shows them and may open a browser), polls
// until the user approves, and stores the tokens. Blocking; ctx cancels it.
func Login(ctx context.Context, prompt func(verificationURL, userCode string)) error {
	tokenEndpoint, err := discoverTokenEndpoint(ctx)
	if err != nil {
		return err
	}
	d, err := requestDeviceCode(ctx)
	if err != nil {
		return err
	}
	verification := d.VerificationURIComplete
	if verification == "" {
		verification = d.VerificationURI
	}
	prompt(verification, d.UserCode)
	tok, err := pollDeviceToken(ctx, tokenEndpoint, d)
	if err != nil {
		return err
	}
	c := credsFromToken(tok, tokenEndpoint, creds{})
	if c.AccessToken == "" || c.RefreshToken == "" {
		return fmt.Errorf("the xAI login response was missing tokens — try again")
	}
	write(c)
	return nil
}

// Logout removes the stored login.
func Logout() error {
	p := tokenPath()
	if p == "" {
		return nil
	}
	err := os.Remove(p)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func read() (creds, error) {
	p := tokenPath()
	if p == "" {
		return creds{}, fmt.Errorf("no Grok login found — run `memcode auth use grok`")
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return creds{}, fmt.Errorf("no Grok login at %s — run `memcode auth use grok`", p)
	}
	var c creds
	if err := json.Unmarshal(b, &c); err != nil {
		return creds{}, fmt.Errorf("the Grok login file is unreadable — run `memcode auth use grok` to sign in again: %w", err)
	}
	if c.AccessToken == "" {
		return creds{}, fmt.Errorf("the Grok login has no access token — run `memcode auth use grok`")
	}
	return c, nil
}

// write persists tokens atomically, 0600. Best-effort: a failed write costs a
// refresh next run, not the session.
func write(c creds) {
	p := tokenPath()
	if p == "" {
		return
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return
	}
	tmp := p + ".tmp"
	if os.WriteFile(tmp, b, 0o600) != nil {
		return
	}
	_ = os.Rename(tmp, p)
}

// discoverTokenEndpoint reads the OIDC discovery document. The token endpoint
// is never hardcoded and always validated to stay on x.ai — a poisoned stored
// endpoint would otherwise receive every future refresh token.
func discoverTokenEndpoint(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, discoveryURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := (&http.Client{Timeout: httpTimeout}).Do(req)
	if err != nil {
		return "", fmt.Errorf("xAI OAuth discovery: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("xAI OAuth discovery returned %s", resp.Status)
	}
	var doc struct {
		TokenEndpoint string `json:"token_endpoint"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return "", fmt.Errorf("xAI OAuth discovery decode: %w", err)
	}
	if err := validateEndpoint(doc.TokenEndpoint); err != nil {
		return "", err
	}
	return doc.TokenEndpoint, nil
}

// validateEndpoint accepts only an https URL on x.ai (or a test override host
// matching the issuer). Refresh tokens are posted to this URL in plaintext
// form fields, so it must never point off-vendor.
func validateEndpoint(endpoint string) error {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("xAI OAuth discovery returned an invalid token endpoint %q", endpoint)
	}
	iss, _ := url.Parse(issuer)
	sameAsIssuer := iss != nil && u.Host == iss.Host && u.Scheme == iss.Scheme
	onXAI := u.Scheme == "https" && (u.Host == "x.ai" || strings.HasSuffix(u.Host, ".x.ai"))
	if !sameAsIssuer && !onXAI {
		return fmt.Errorf("refusing non-xAI token endpoint %q", endpoint)
	}
	return nil
}

type deviceCode struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
}

func requestDeviceCode(ctx context.Context) (deviceCode, error) {
	form := url.Values{"client_id": {clientID}, "scope": {scope}}
	body, status, err := postForm(ctx, deviceCodeURL, form)
	if err != nil {
		return deviceCode{}, fmt.Errorf("xAI device-code request: %w", err)
	}
	if status != http.StatusOK {
		return deviceCode{}, fmt.Errorf("xAI device-code request returned HTTP %d: %s", status, strings.TrimSpace(string(body)))
	}
	var d deviceCode
	if err := json.Unmarshal(body, &d); err != nil {
		return deviceCode{}, fmt.Errorf("xAI device-code decode: %w", err)
	}
	if d.DeviceCode == "" || d.VerificationURI == "" && d.VerificationURIComplete == "" {
		return deviceCode{}, fmt.Errorf("xAI device-code response was missing fields")
	}
	return d, nil
}

// tokenResponse is the token endpoint's success payload (device grant and
// refresh share it).
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	Error        string `json:"error"`
	ErrorDesc    string `json:"error_description"`
}

// pollDeviceToken polls until approval, honoring authorization_pending and
// slow_down per RFC 8628, bounded by the device code's expiry window.
func pollDeviceToken(ctx context.Context, tokenEndpoint string, d deviceCode) (tokenResponse, error) {
	interval := d.Interval
	if interval < 1 {
		interval = 5
	}
	expires := d.ExpiresIn
	if expires < 1 {
		expires = 600
	}
	deadline := time.Now().Add(time.Duration(expires) * time.Second)
	form := url.Values{
		"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
		"client_id":   {clientID},
		"device_code": {d.DeviceCode},
	}
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return tokenResponse{}, err
		}
		body, status, err := postForm(ctx, tokenEndpoint, form)
		if err != nil {
			return tokenResponse{}, fmt.Errorf("xAI login polling: %w", err)
		}
		var tok tokenResponse
		if err := json.Unmarshal(body, &tok); err != nil {
			return tokenResponse{}, fmt.Errorf("xAI login polling returned HTTP %d: %s", status, strings.TrimSpace(string(body)))
		}
		if status == http.StatusOK && tok.AccessToken != "" {
			return tok, nil
		}
		switch tok.Error {
		case "authorization_pending":
			sleep(time.Duration(interval) * time.Second)
		case "slow_down":
			if interval < 30 {
				interval++
			}
			sleep(time.Duration(interval) * time.Second)
		default:
			msg := tok.ErrorDesc
			if msg == "" {
				msg = tok.Error
			}
			if msg == "" {
				msg = strings.TrimSpace(string(body))
			}
			return tokenResponse{}, fmt.Errorf("xAI login failed: %s", msg)
		}
	}
	return tokenResponse{}, fmt.Errorf("timed out waiting for the xAI login approval — run `memcode auth use grok` to try again")
}

// refresh exchanges the refresh token. Tokens rotate; the caller persists the
// result. A 403 is the tier gate (ErrTierGated), 400/401 mean the grant is
// dead and a fresh login is needed.
func refresh(c creds) (creds, error) {
	if c.RefreshToken == "" {
		return creds{}, fmt.Errorf("the Grok login can't be refreshed — run `memcode auth use grok` to sign in again")
	}
	endpoint := c.TokenEndpoint
	if endpoint == "" {
		var err error
		if endpoint, err = discoverTokenEndpoint(context.Background()); err != nil {
			return creds{}, err
		}
	}
	if err := validateEndpoint(endpoint); err != nil {
		return creds{}, err
	}
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {clientID},
		"refresh_token": {c.RefreshToken},
	}
	body, status, err := postForm(context.Background(), endpoint, form)
	if err != nil {
		return creds{}, fmt.Errorf("xAI token refresh: %w", err)
	}
	switch status {
	case http.StatusOK:
	case http.StatusForbidden:
		return creds{}, ErrTierGated
	case http.StatusBadRequest, http.StatusUnauthorized:
		return creds{}, fmt.Errorf("the Grok login was revoked — run `memcode auth use grok` to sign in again (HTTP %d: %s)", status, strings.TrimSpace(string(body)))
	default:
		return creds{}, fmt.Errorf("xAI token refresh returned HTTP %d: %s", status, strings.TrimSpace(string(body)))
	}
	var tok tokenResponse
	if err := json.Unmarshal(body, &tok); err != nil {
		return creds{}, fmt.Errorf("xAI token refresh decode: %w", err)
	}
	if tok.AccessToken == "" {
		return creds{}, fmt.Errorf("xAI token refresh returned an empty token")
	}
	return credsFromToken(tok, endpoint, c), nil
}

// credsFromToken folds a token response into stored creds, keeping the old
// refresh token when the response omitted a replacement.
func credsFromToken(tok tokenResponse, endpoint string, prev creds) creds {
	c := creds{
		AccessToken:   tok.AccessToken,
		RefreshToken:  tok.RefreshToken,
		TokenEndpoint: endpoint,
		LastRefresh:   time.Now().UTC().Format(time.RFC3339),
	}
	if c.RefreshToken == "" {
		c.RefreshToken = prev.RefreshToken
	}
	if tok.ExpiresIn > 0 {
		c.ExpiresAt = time.Now().Unix() + tok.ExpiresIn
	}
	return c
}

func postForm(ctx context.Context, endpoint string, form url.Values) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := (&http.Client{Timeout: httpTimeout}).Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return body, resp.StatusCode, nil
}
