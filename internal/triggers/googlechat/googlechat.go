// Package googlechat is the gateway's Google Chat adapter. A Chat app is
// configured in the Google Cloud console with an HTTP endpoint URL; inbound
// events arrive as POSTs carrying a Google-signed bearer JWT (issuer
// chat@system.gserviceaccount.com) whose audience is the app's project number,
// and outbound replies go to the Chat REST API (spaces.messages.create)
// authenticated as a service account. Credentials: GOOGLE_CHAT_SA_KEY holds the
// path to the service-account JSON key and googlechat.audience (the project
// number) lives in gateway.yaml; the caller reads the key file and hands the
// bytes to New — this package never reads the environment.
package googlechat

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"

	"github.com/memcode-ai/memcode/internal/channels"
)

const defaultAPIBase = "https://chat.googleapis.com"

// defaultJWKSURL serves the public certs Google signs inbound event JWTs with.
const defaultJWKSURL = "https://www.googleapis.com/oauth2/v3/certs"

// chatIssuer is the only issuer Google Chat signs event tokens as.
const chatIssuer = "chat@system.gserviceaccount.com"

// chatScope is the two-legged OAuth scope for a Chat app acting as itself.
const chatScope = "https://www.googleapis.com/auth/chat.bot"

const maxBody = 2 << 20 // 2 MiB

// chatMaxMessage is our outbound chunk size; Chat rejects text past 4000
// characters, so we stay under with headroom for the JSON envelope.
const chatMaxMessage = 3900

// Channel is a Google Chat app connection: webhook in, REST out.
type Channel struct {
	saKeyJSON []byte
	audience  string // the app's project number; inbound JWT aud must match
	mediaDir  string // media spool; "" disables inbound attachment downloads
	client    *http.Client

	apiBase string // Chat REST base; overridable in tests
	jwksURL string // Google cert endpoint; overridable in tests

	// tokenSource authenticates outbound REST calls (and attachment downloads).
	// Defaulted lazily from the service-account key; tests inject a
	// oauth2.StaticTokenSource instead.
	tokenSource oauth2.TokenSource
	tsMu        sync.Mutex

	// keys caches Google's JWKS by kid; refreshed at most once per request when
	// an unknown kid appears (Google rotates keys).
	keys   map[string]*rsa.PublicKey
	keysMu sync.Mutex
}

// New builds a Google Chat channel from the service-account key JSON, the
// expected JWT audience (the app's project number), and the gateway media
// spool directory ("" disables attachment downloads).
func New(saKeyJSON []byte, audience, mediaDir string) *Channel {
	return &Channel{
		saKeyJSON: saKeyJSON,
		audience:  audience,
		mediaDir:  mediaDir,
		client:    &http.Client{Timeout: 30 * time.Second},
		apiBase:   defaultAPIBase,
		jwksURL:   defaultJWKSURL,
	}
}

// Name returns the adapter identifier.
func (c *Channel) Name() string { return "googlechat" }

// Handler returns the webhook HTTP handler. Chat expects a synchronous JSON
// response; we always reply asynchronously through the REST API, so a handled
// event returns 200 with body {} (an empty object means "no synchronous
// message"). A Deliver failure returns 503 so Google redelivers.
func (c *Channel) Handler(sink channels.Sink) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		// Every inbound POST must carry Google's signed bearer JWT; without a
		// valid one we cannot tell Google from an internet stranger, so nothing
		// is parsed, let alone delivered.
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || !c.verifyJWT(r.Context(), token) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
		if err != nil {
			http.Error(w, "read error", http.StatusBadRequest)
			return
		}
		inb, deliver := c.toInbound(r.Context(), body)
		if !deliver {
			w.WriteHeader(http.StatusOK) // non-MESSAGE event (or bot echo): ack, nothing to run
			return
		}
		if err := sink.Deliver(r.Context(), inb); err != nil {
			http.Error(w, "not recorded", http.StatusServiceUnavailable) // Google retries
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "{}")
	})
}

// event is the subset of a Google Chat event payload we read.
type event struct {
	Type    string `json:"type"`
	Message struct {
		Name         string `json:"name"`
		Text         string `json:"text"`
		ArgumentText string `json:"argumentText"`
		Sender       struct {
			Name string `json:"name"` // users/<id> — stable principal
			Type string `json:"type"` // "HUMAN" | "BOT"
		} `json:"sender"`
		Annotations []struct {
			Type string `json:"type"`
		} `json:"annotations"`
		Attachment []chatAttachment `json:"attachment"`
	} `json:"message"`
	Space struct {
		Name      string `json:"name"`      // spaces/<id>
		SpaceType string `json:"spaceType"` // "DIRECT_MESSAGE" | "SPACE" (newer field)
		Type      string `json:"type"`      // "DM" | "ROOM" (legacy field)
	} `json:"space"`
}

type chatAttachment struct {
	ContentName string `json:"contentName"`
	ContentType string `json:"contentType"`
	DownloadURI string `json:"downloadUri"`
}

// toInbound maps a MESSAGE event to a normalized Inbound. Every other event
// type (ADDED_TO_SPACE, CARD_CLICKED, …) and messages from other bots are
// acknowledged without delivery.
func (c *Channel) toInbound(ctx context.Context, body []byte) (channels.Inbound, bool) {
	var ev event
	if err := json.Unmarshal(body, &ev); err != nil || ev.Type != "MESSAGE" {
		return channels.Inbound{}, false
	}
	if ev.Message.Sender.Type == "BOT" || ev.Message.Sender.Name == "" {
		return channels.Inbound{}, false // never let bots (ourselves included) trigger turns
	}
	// argumentText is the message with the app's @mention stripped — the actual
	// task — so prefer it when present.
	text := ev.Message.ArgumentText
	if text == "" {
		text = ev.Message.Text
	}
	isDirect := ev.Space.SpaceType == "DIRECT_MESSAGE" || ev.Space.Type == "DM" ||
		ev.Space.SpaceType == "DM" || ev.Space.Type == "DIRECT_MESSAGE"
	// In a space, a Chat app only receives messages it was @mentioned in, so
	// Mentioned is effectively always true there; we still detect it
	// structurally from the USER_MENTION annotation rather than assuming.
	mentioned := false
	for _, a := range ev.Message.Annotations {
		if a.Type == "USER_MENTION" {
			mentioned = true
			break
		}
	}
	return channels.Inbound{
		Channel:      "googlechat",
		Conversation: ev.Space.Name,
		Principal:    ev.Message.Sender.Name,
		Text:         text,
		MessageID:    ev.Message.Name,
		IsDirect:     isDirect,
		Mentioned:    mentioned,
		Attachments:  c.download(ctx, ev.Message.Attachment),
	}, true
}

// download fetches inbound attachments into the spool. Chat attachment media
// requires authentication, so the OUTBOUND service-account bearer is reused.
// Best-effort: a failed download drops that attachment, the message still flows.
func (c *Channel) download(ctx context.Context, atts []chatAttachment) []channels.Attachment {
	if c.mediaDir == "" || len(atts) == 0 {
		return nil
	}
	ts, err := c.source(ctx)
	if err != nil {
		return nil
	}
	tok, err := ts.Token()
	if err != nil {
		return nil
	}
	var out []channels.Attachment
	for _, a := range atts {
		if a.DownloadURI == "" {
			continue
		}
		att, err := c.downloadOne(ctx, tok.AccessToken, a)
		if err != nil {
			continue
		}
		out = append(out, att)
	}
	return out
}

func (c *Channel) downloadOne(ctx context.Context, bearer string, a chatAttachment) (channels.Attachment, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.DownloadURI, nil)
	if err != nil {
		return channels.Attachment{}, err
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := c.client.Do(req)
	if err != nil {
		return channels.Attachment{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return channels.Attachment{}, fmt.Errorf("googlechat attachment download: status %d", resp.StatusCode)
	}
	name := a.ContentName
	if name == "" {
		name = "attachment"
	}
	return channels.SaveToSpool(c.mediaDir, resp.Body, a.ContentType, name)
}

// Send posts a text reply to a conversation (a spaces/<id> name) through the
// Chat REST API, split with the shared chunker.
func (c *Channel) Send(ctx context.Context, conversation string, msg channels.Outbound) error {
	ts, err := c.source(ctx)
	if err != nil {
		return err
	}
	tok, err := ts.Token()
	if err != nil {
		return fmt.Errorf("googlechat token: %w", err)
	}
	for _, part := range channels.Chunk(msg.Text, chatMaxMessage) {
		if err := c.sendOne(ctx, tok.AccessToken, conversation, part); err != nil {
			return err
		}
	}
	return nil
}

func (c *Channel) sendOne(ctx context.Context, bearer, conversation, text string) error {
	body, err := json.Marshal(map[string]string{"text": text})
	if err != nil {
		return err
	}
	endpoint := fmt.Sprintf("%s/v1/%s/messages", c.apiBase, conversation)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("googlechat send: status %d", resp.StatusCode)
	}
	return nil
}

// source returns the cached outbound token source, building it lazily from the
// service-account key via the two-legged JWT flow. Cached with a background
// context so one cancelled request can't poison later token refreshes.
func (c *Channel) source(ctx context.Context) (oauth2.TokenSource, error) {
	c.tsMu.Lock()
	defer c.tsMu.Unlock()
	if c.tokenSource != nil {
		return c.tokenSource, nil
	}
	cfg, err := google.JWTConfigFromJSON(c.saKeyJSON, chatScope)
	if err != nil {
		return nil, fmt.Errorf("googlechat service-account key: %w", err)
	}
	c.tokenSource = cfg.TokenSource(context.Background())
	return c.tokenSource, nil
}

// --- inbound JWT verification (stdlib JWKS, no new module deps) ---

// verifyJWT checks a Google-signed RS256 bearer: signature against Google's
// published JWKS, issuer chat@system.gserviceaccount.com, audience equal to
// the app's project number, and unexpired.
func (c *Channel) verifyJWT(ctx context.Context, token string) bool {
	if c.audience == "" {
		return false // no configured audience can never verify — reject, don't trust
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return false
	}
	headerRaw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return false
	}
	var header struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if err := json.Unmarshal(headerRaw, &header); err != nil || header.Alg != "RS256" {
		return false
	}
	claimsRaw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return false
	}
	var claims struct {
		Iss string `json:"iss"`
		Aud string `json:"aud"`
		Exp int64  `json:"exp"`
	}
	if err := json.Unmarshal(claimsRaw, &claims); err != nil {
		return false
	}
	if claims.Iss != chatIssuer || claims.Aud != c.audience || time.Now().Unix() >= claims.Exp {
		return false
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return false
	}
	pub, err := c.publicKey(ctx, header.Kid)
	if err != nil {
		return false
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	return rsa.VerifyPKCS1v15(pub, crypto.SHA256, sum[:], sig) == nil
}

// publicKey resolves a kid from the JWKS cache, refreshing from Google at most
// once per lookup when the kid is unknown (key rotation).
func (c *Channel) publicKey(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	c.keysMu.Lock()
	defer c.keysMu.Unlock()
	if pub, ok := c.keys[kid]; ok {
		return pub, nil
	}
	keys, err := c.fetchJWKS(ctx)
	if err != nil {
		return nil, err
	}
	c.keys = keys
	if pub, ok := c.keys[kid]; ok {
		return pub, nil
	}
	return nil, fmt.Errorf("googlechat jwt: unknown key id %q", kid)
}

// fetchJWKS pulls Google's cert set and parses the RSA keys (n/e per RFC 7517).
func (c *Channel) fetchJWKS(ctx context.Context) (map[string]*rsa.PublicKey, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.jwksURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("googlechat jwks: status %d", resp.StatusCode)
	}
	var doc struct {
		Keys []struct {
			Kty string `json:"kty"`
			Kid string `json:"kid"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(&doc); err != nil {
		return nil, err
	}
	out := make(map[string]*rsa.PublicKey, len(doc.Keys))
	for _, k := range doc.Keys {
		if k.Kty != "RSA" || k.Kid == "" {
			continue
		}
		nb, err := base64.RawURLEncoding.DecodeString(k.N)
		if err != nil {
			continue
		}
		eb, err := base64.RawURLEncoding.DecodeString(k.E)
		if err != nil || len(eb) == 0 || len(eb) > 8 {
			continue
		}
		e := 0
		for _, b := range eb {
			e = e<<8 | int(b)
		}
		if e <= 1 {
			continue
		}
		out[k.Kid] = &rsa.PublicKey{N: new(big.Int).SetBytes(nb), E: e}
	}
	return out, nil
}
