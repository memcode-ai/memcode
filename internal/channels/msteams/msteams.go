// Package msteams is the gateway's Microsoft Teams adapter over the Bot
// Framework: an Azure Bot registration POSTs activities to our webhook, and
// replies go back to the activity's serviceUrl as REST calls authenticated with
// an Azure AD client-credentials token. Inbound requests carry a Bot Framework
// JWT we verify against the published JWKS — Teams has no shared-secret HMAC,
// the JWT IS the sender authentication. Credentials (TEAMS_APP_ID,
// TEAMS_APP_PASSWORD, TEAMS_TENANT_ID) are read by the caller and passed to
// New; this package never reads the environment.
package msteams

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/memcode-ai/memcode/internal/channels"
	"github.com/memcode-ai/memcode/internal/webjwt"
)

// botFrameworkIssuer is the issuer every Bot Framework connector token carries.
const botFrameworkIssuer = "https://api.botframework.com"

// msServiceSuffixes are the host suffixes a legitimate Bot Framework serviceUrl
// or attachment lives under. The connector bearer is sent ONLY to these, and a
// reply is refused to any other host — so a manipulated serviceUrl or
// contentUrl can't redirect the agent's output (and its Authorization header)
// to an attacker.
var msServiceSuffixes = []string{
	".botframework.com", ".skype.com", "smba.trafficmanager.net",
	".sharepoint.com", ".office.com", ".microsoft.com", ".microsoftonline.com",
	".core.windows.net", ".azureedge.net",
}

// defaultMetadataURL is the Bot Framework OpenID metadata document; it points
// at the JWKS the connector signs inbound tokens with.
const defaultMetadataURL = "https://login.botframework.com/v1/.well-known/openidconfiguration"

// defaultTokenBase is the Azure AD endpoint the outbound client-credentials
// token comes from ("{base}/{tenant}/oauth2/v2.0/token").
const defaultTokenBase = "https://login.microsoftonline.com"

// botFrameworkScope is the scope for the outbound connector token.
const botFrameworkScope = "https://api.botframework.com/.default"

const maxBody = 2 << 20 // 2 MiB

// teamsMaxMessage caps one outbound text activity. Teams rejects activities
// past ~28 KB of serialized payload; 25000 leaves headroom for the JSON frame.
const teamsMaxMessage = 25000

// Channel is a Microsoft Teams Bot Framework connection.
type Channel struct {
	appID       string
	appPassword string
	tenantID    string
	mediaDir    string // media spool; "" disables inbound media downloads
	tokenBase   string // Azure AD token endpoint base; overridable in tests
	client      *http.Client
	dl          *http.Client           // SSRF-guarded client for attachment downloads
	trustHost   func(host string) bool // serviceUrl/content host trust check; nil = the msServiceSuffixes allowlist (test seam)
	verify      *webjwt.Verifier       // the shared inbound-JWT verifier; tests point its MetadataURL at a fake

	// tokMu guards the cached outbound bearer; refreshed ~60s before expiry so
	// an in-flight Send never races the token's edge.
	tokMu  sync.Mutex
	tok    string
	tokExp time.Time
}

// New builds a Teams channel from the Azure Bot app id, its client secret, and
// the AAD tenant the bot is registered in. mediaDir is the gateway media spool
// inbound attachments are downloaded into; "" disables media handling.
func New(appID, appPassword, tenantID, mediaDir string) *Channel {
	client := &http.Client{Timeout: 30 * time.Second}
	return &Channel{
		dl:          channels.SafeHTTPClient(30 * time.Second),
		appID:       appID,
		appPassword: appPassword,
		tenantID:    tenantID,
		mediaDir:    mediaDir,
		tokenBase:   defaultTokenBase,
		client:      client,
		verify: &webjwt.Verifier{
			MetadataURL: defaultMetadataURL,
			Issuer:      botFrameworkIssuer,
			Audience:    appID,
			Client:      client,
		},
	}
}

// Name returns the adapter identifier.
func (c *Channel) Name() string { return "msteams" }

// activity is the subset of a Bot Framework activity we read.
type activity struct {
	Type string `json:"type"`
	ID   string `json:"id"`
	Text string `json:"text"`
	From struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"from"`
	Recipient struct {
		ID string `json:"id"`
	} `json:"recipient"`
	Conversation struct {
		ID               string `json:"id"`
		ConversationType string `json:"conversationType"`
	} `json:"conversation"`
	ServiceURL string `json:"serviceUrl"`
	Entities   []struct {
		Type      string `json:"type"`
		Mentioned struct {
			ID string `json:"id"`
		} `json:"mentioned"`
	} `json:"entities"`
	Attachments []struct {
		ContentType string `json:"contentType"`
		ContentURL  string `json:"contentUrl"`
		Name        string `json:"name"`
	} `json:"attachments"`
}

// Handler returns the webhook HTTP handler for POST /webhook/teams. It
// verifies the Bot Framework JWT, maps message activities to Inbound, and acks
// 200 only after every Deliver returned nil (503 makes the connector retry).
func (c *Channel) Handler(sink channels.Sink) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		raw, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || c.verify.Verify(r.Context(), raw) != nil {
			// Unauthenticated caller: nothing is delivered. 401, not 503 — a
			// forged request must not be invited to retry.
			http.Error(w, "invalid token", http.StatusUnauthorized)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
		if err != nil {
			http.Error(w, "read error", http.StatusBadRequest)
			return
		}
		var act activity
		if err := json.Unmarshal(body, &act); err != nil {
			http.Error(w, "bad activity", http.StatusBadRequest)
			return
		}
		// Non-message activities (conversationUpdate, typing, invoke, …) are
		// acked and dropped — the gateway only acts on user messages.
		if act.Type != "message" || act.Conversation.ID == "" {
			w.WriteHeader(http.StatusOK)
			return
		}
		inb := c.toInbound(act)
		inb.Attachments = c.download(r.Context(), act)
		if err := sink.Deliver(r.Context(), inb); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable) // not recorded — Bot Framework retries
			return
		}
		w.WriteHeader(http.StatusOK)
	})
}

// toInbound normalizes a message activity. The conversation string encodes
// BOTH the conversation id and the serviceUrl ("id|url") because a Teams reply
// must be posted to the serviceUrl the activity arrived from; serviceUrl never
// contains "|", so the first "|" is an unambiguous split point in Send.
func (c *Channel) toInbound(act activity) channels.Inbound {
	mentioned := false
	for _, e := range act.Entities {
		if e.Type == "mention" && e.Mentioned.ID != "" && e.Mentioned.ID == act.Recipient.ID {
			mentioned = true
			break
		}
	}
	return channels.Inbound{
		Channel:      "msteams",
		Conversation: act.Conversation.ID + "|" + act.ServiceURL,
		Principal:    act.From.ID, // AAD object id — stable across display-name changes
		Text:         stripAtTags(act.Text),
		MessageID:    act.ID,
		IsDirect:     act.Conversation.ConversationType == "personal",
		Mentioned:    mentioned,
	}
}

// stripAtTags removes the "<at>…</at>" spans Teams prepends for bot mentions.
// This cuts only the literal at-tag spans by string search — it is not (and
// must not become) an HTML parser.
func stripAtTags(s string) string {
	const openTag, closeTag = "<at>", "</at>"
	for {
		i := strings.Index(s, openTag)
		if i < 0 {
			break
		}
		j := strings.Index(s[i+len(openTag):], closeTag)
		if j < 0 {
			break
		}
		s = s[:i] + s[i+len(openTag)+j+len(closeTag):]
	}
	return strings.TrimSpace(s)
}

// download fetches attachment content into the spool. Best-effort: a failed
// download drops that attachment, the message still flows. Teams file URLs
// generally require the connector bearer, but some (public blobs) reject
// extraneous auth — so a 401/403 with the bearer is retried without it.
func (c *Channel) download(ctx context.Context, act activity) []channels.Attachment {
	if c.mediaDir == "" {
		return nil
	}
	var out []channels.Attachment
	for _, a := range act.Attachments {
		if !strings.HasPrefix(a.ContentURL, "http://") && !strings.HasPrefix(a.ContentURL, "https://") {
			continue
		}
		// text/html is the message body echoed as an attachment, and card
		// payloads are UI, not media — neither is a file for the agent.
		ct := strings.ToLower(a.ContentType)
		if ct == "text/html" || strings.HasPrefix(ct, "application/vnd.microsoft.card") {
			continue
		}
		att, err := c.downloadOne(ctx, a.ContentURL, a.ContentType, a.Name)
		if err != nil {
			continue
		}
		out = append(out, att)
	}
	return out
}

// hostTrusted reports whether u is a first-party Bot Framework host the
// connector bearer may be sent to. https-only in production; a test seam
// (trustHost) relaxes it for httptest servers.
func (c *Channel) hostTrusted(u *url.URL) bool {
	if u == nil {
		return false
	}
	if c.trustHost != nil {
		return c.trustHost(u.Host)
	}
	return u.Scheme == "https" && channels.HostAllowed(u.Host, msServiceSuffixes...)
}

func (c *Channel) downloadOne(ctx context.Context, contentURL, mimeType, name string) (channels.Attachment, error) {
	// The connector bearer (aud api.botframework.com) is attached ONLY when the
	// content host is a first-party Microsoft host, so a contentUrl pointing
	// anywhere else can't harvest the gateway's token. The SSRF-guarded client
	// independently refuses any internal address.
	u, perr := url.Parse(contentURL)
	if perr != nil {
		return channels.Attachment{}, perr
	}
	bearer := ""
	if c.hostTrusted(u) {
		bearer, _ = c.token(ctx)
	}
	resp, err := c.fetch(ctx, contentURL, bearer)
	if err != nil {
		return channels.Attachment{}, err
	}
	if bearer != "" && (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) {
		resp.Body.Close()
		if resp, err = c.fetch(ctx, contentURL, ""); err != nil {
			return channels.Attachment{}, err
		}
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return channels.Attachment{}, fmt.Errorf("msteams attachment download: status %d", resp.StatusCode)
	}
	if name == "" {
		name = "attachment"
	}
	return channels.SaveToSpool(c.mediaDir, resp.Body, mimeType, name)
}

func (c *Channel) fetch(ctx context.Context, u, bearer string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	return c.dl.Do(req)
}

// token returns a valid outbound connector bearer, minting one via the Azure
// AD client-credentials grant when the cache is empty or within 60s of expiry
// (the margin keeps a token from expiring mid-Send).
func (c *Channel) token(ctx context.Context) (string, error) {
	c.tokMu.Lock()
	defer c.tokMu.Unlock()
	if c.tok != "" && time.Now().Before(c.tokExp.Add(-60*time.Second)) {
		return c.tok, nil
	}
	form := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {c.appID},
		"client_secret": {c.appPassword},
		"scope":         {botFrameworkScope},
	}
	endpoint := fmt.Sprintf("%s/%s/oauth2/v2.0/token", strings.TrimRight(c.tokenBase, "/"), c.tenantID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("msteams token: status %d", resp.StatusCode)
	}
	var tok struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(&tok); err != nil {
		return "", err
	}
	if tok.AccessToken == "" {
		return "", errors.New("msteams token: empty access_token")
	}
	c.tok = tok.AccessToken
	c.tokExp = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second)
	return c.tok, nil
}

// Send posts a reply. The conversation string carries "convID|serviceUrl"
// (see toInbound); the first "|" splits them because a serviceUrl never
// contains one. Text is chunked so an over-long agent reply never bounces.
func (c *Channel) Send(ctx context.Context, conversation string, msg channels.Outbound) error {
	convID, serviceURL, ok := strings.Cut(conversation, "|")
	if !ok || convID == "" || serviceURL == "" {
		return fmt.Errorf("msteams send: malformed conversation %q", conversation)
	}
	// serviceUrl came from the (JWT-authenticated but attacker-shapeable)
	// activity body. Refuse to send the reply — and its connector bearer — to
	// any host that isn't a first-party Bot Framework endpoint, so a forged
	// serviceUrl can't redirect the agent's output to an attacker.
	su, perr := url.Parse(serviceURL)
	if perr != nil || !c.hostTrusted(su) {
		return fmt.Errorf("msteams send: refusing reply to untrusted serviceUrl %q", serviceURL)
	}
	bearer, err := c.token(ctx)
	if err != nil {
		return err
	}
	endpoint := fmt.Sprintf("%s/v3/conversations/%s/activities", strings.TrimRight(serviceURL, "/"), url.PathEscape(convID))
	for _, part := range channels.Chunk(msg.Text, teamsMaxMessage) {
		if err := c.sendOne(ctx, endpoint, bearer, part); err != nil {
			return err
		}
	}
	return nil
}

func (c *Channel) sendOne(ctx context.Context, endpoint, bearer, text string) error {
	body, err := json.Marshal(map[string]string{"type": "message", "text": text})
	if err != nil {
		return err
	}
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
		return fmt.Errorf("msteams send: status %d", resp.StatusCode)
	}
	return nil
}
