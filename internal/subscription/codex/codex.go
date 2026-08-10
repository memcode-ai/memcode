// Package codex turns a ChatGPT (Codex) subscription into a memcode backend by
// reusing the login the Codex CLI already stored. It reads ~/.codex/auth.json
// (or $CODEX_HOME), pulls the account id out of the access-token JWT, and
// returns the ChatGPT/Codex endpoint plus the identity headers Cloudflare and
// the backend require. No vendor SDK — file read + JWT decode — so it returns a
// plain Backend the provider package adapts to an endpoint.
//
// Consent-gated upstream: it activates only when the user explicitly selects the
// codex source. It reuses the Codex CLI's file and never writes it.
package codex

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Backend is the resolved Codex endpoint: the ChatGPT backend base, the access
// token (Bearer), and the identity headers every turn must carry.
type Backend struct {
	BaseURL string
	Token   string
	Headers map[string]string
}

// baseURL is the ChatGPT Codex backend; the Responses adapter targets
// {base}/responses.
const baseURL = "https://chatgpt.com/backend-api/codex"

// Resolve reads the Codex CLI login and shapes it into a backend. An error
// explains the fix (install/sign in with the Codex CLI).
func Resolve() (Backend, error) {
	access, err := readCodexToken()
	if err != nil {
		return Backend{}, err
	}
	account := accountIDFromJWT(access)
	if account == "" {
		return Backend{}, fmt.Errorf("the Codex login has no ChatGPT account id — sign in again with the Codex CLI")
	}
	return Backend{
		BaseURL: baseURL,
		Token:   access,
		Headers: map[string]string{
			// Cloudflare only admits a whitelisted originator; the account id
			// selects the subscription. codex_cli_rs is the accepted originator.
			"User-Agent":         "codex_cli_rs/0.0.0 (memcode)",
			"originator":         "codex_cli_rs",
			"ChatGPT-Account-Id": account,
		},
	}, nil
}

// codexAuth is the subset of ~/.codex/auth.json we read.
type codexAuth struct {
	Tokens struct {
		AccessToken string `json:"access_token"`
	} `json:"tokens"`
}

func authPath() string {
	if h := strings.TrimSpace(os.Getenv("CODEX_HOME")); h != "" {
		return filepath.Join(h, "auth.json")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".codex", "auth.json")
}

func readCodexToken() (string, error) {
	p := authPath()
	if p == "" {
		return "", fmt.Errorf("no Codex login found — install the Codex CLI and run `codex login`")
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return "", fmt.Errorf("no Codex login at %s — run `codex login`", p)
	}
	// utf-8-sig tolerance: strip a leading BOM the CLI may write.
	b = trimBOM(b)
	var a codexAuth
	if err := json.Unmarshal(b, &a); err != nil {
		return "", fmt.Errorf("Codex auth.json is unreadable: %w", err)
	}
	if a.Tokens.AccessToken == "" {
		return "", fmt.Errorf("Codex auth.json has no access token — run `codex login`")
	}
	return a.Tokens.AccessToken, nil
}

func trimBOM(b []byte) []byte {
	if len(b) >= 3 && b[0] == 0xEF && b[1] == 0xBB && b[2] == 0xBF {
		return b[3:]
	}
	return b
}

// accountIDFromJWT extracts the ChatGPT account id from the access-token JWT's
// claims — payload["https://api.openai.com/auth"]["chatgpt_account_id"]. Returns
// "" if the token isn't a JWT or the claim is absent.
func accountIDFromJWT(tok string) string {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return ""
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		Auth struct {
			ChatGPTAccountID string `json:"chatgpt_account_id"`
		} `json:"https://api.openai.com/auth"`
	}
	if json.Unmarshal(raw, &claims) != nil {
		return ""
	}
	return claims.Auth.ChatGPTAccountID
}

// Available reports whether a Codex CLI login file is present, for the wizard's
// menu (no parse, no network).
func Available() bool {
	p := authPath()
	if p == "" {
		return false
	}
	_, err := os.Stat(p)
	return err == nil
}
