// Package authflow is the browser login flow shared by `memcode login` (cobra)
// and the TUI's /login slash command. A local HTTP server on 127.0.0.1:19090
// receives the token from the web app's /api/cli/auth callback redirect; the
// caller persists it via WriteGlobalEnvToken so the existing LoadDotEnv →
// provider path picks it up.
package authflow

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/memcode-ai/memcode/internal/provider"
)

// DefaultGatewayURL is the production memcode gateway. The web app's
// /api/cli/auth may override this by returning api_url in the callback, but
// this default lets login work even when the callback doesn't include it.
const DefaultGatewayURL = provider.DefaultAPIURL

// DefaultWebAppURL is where the browser opens to authenticate. Use the www
// host directly: the apex (memcode.ai) 308-redirects to www, and that extra
// cross-host hop on the /api/cli/auth?port=&state= URL is needless churn on a
// load-bearing flow. Point straight at the canonical host.
const DefaultWebAppURL = "https://www.memcode.ai"

// Result is a successful login: the minted org key and the gateway to use it
// against.
type Result struct {
	Token      string
	GatewayURL string
	Email      string // the signed-in account email, when the web app supplies it
}

// loginResult is the outcome of the browser callback.
type loginResult struct {
	token  string
	apiURL string
	email  string
	err    string
}

// makeCallbackHandler returns an http.HandlerFunc that validates the state
// parameter and sends the result (token or error) down resultCh.
func makeCallbackHandler(state string, resultCh chan<- loginResult) http.HandlerFunc {
	// deliver is non-blocking: resultCh has capacity 1 and only the FIRST
	// outcome matters. A duplicate success callback (browser re-GET/prefetch)
	// must not block its handler goroutine forever on a full channel.
	deliver := func(res loginResult) {
		select {
		case resultCh <- res:
		default:
		}
	}
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("state") != state {
			// A stray local request (another process probing the port, a
			// prefetch of a stale URL) must NOT abort the pending login —
			// only the request carrying OUR state speaks for this flow.
			http.NotFound(w, r)
			return
		}
		if errMsg := q.Get("error"); errMsg != "" {
			deliver(loginResult{err: errMsg})
			writeCallbackPage(w, false, errMsg)
			return
		}
		token := q.Get("token")
		if token == "" {
			deliver(loginResult{err: "no token received"})
			writeCallbackPage(w, false, "No token received.")
			return
		}
		// The web app may include api_url (which gateway) and email (who signed in).
		apiURL := q.Get("api_url")
		deliver(loginResult{token: token, apiURL: apiURL, email: q.Get("email")})
		writeCallbackPage(w, true, "")
	}
}

// Run executes the browser login flow: starts the local callback server, opens
// the browser, and waits for the redirect. status (nil-safe) receives progress
// lines for display. Cancel ctx to abort (the TUI's Esc); an internal 2-minute
// timeout applies regardless. Run does NOT persist anything — callers decide
// (WriteGlobalEnvToken).
func Run(ctx context.Context, status func(string)) (Result, error) {
	say := func(s string) {
		if status != nil {
			status(s)
		}
	}

	// Generate CSRF state.
	stateBytes := make([]byte, 16)
	if _, err := rand.Read(stateBytes); err != nil {
		return Result{}, fmt.Errorf("failed to generate state: %w", err)
	}
	state := hex.EncodeToString(stateBytes)

	// Start local callback server. 19090 is the stable first choice, but the
	// callback URL carries the port dynamically, so anything else on 19090
	// (common dev-port territory) just moves us to an OS-assigned free port —
	// a first-time user must never be unable to sign in over a busy port.
	listener, err := net.Listen("tcp", "127.0.0.1:19090")
	if err != nil {
		listener, err = net.Listen("tcp", "127.0.0.1:0")
	}
	if err != nil {
		return Result{}, fmt.Errorf("failed to start local callback server: %w", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port

	resultCh := make(chan loginResult, 1)

	mux := http.NewServeMux()
	mux.HandleFunc("/callback", makeCallbackHandler(state, resultCh))

	server := &http.Server{Handler: mux}
	go server.Serve(listener)

	webAppURL := envOr("MEMCODE_WEB_APP_URL", DefaultWebAppURL)
	authURL := fmt.Sprintf("%s/api/cli/auth?port=%d&state=%s", webAppURL, port, state)

	say("Opening your browser to authenticate...")
	say("If it doesn't open, visit:\n    " + authURL)

	// NEVER from a test binary: a signed-in browser would complete the real
	// flow against prod and mint a live key (it happened — the key was revoked
	// and every `go test ./...` popped browser tabs until this guard).
	if testing.Testing() {
		// no-op: tests drive the callback directly or cancel the context
	} else if err := openBrowser(authURL); err != nil {
		say(fmt.Sprintf("(could not open browser automatically: %v)", err))
	}

	say("Waiting for authentication…")

	select {
	case res := <-resultCh:
		sctx, cancel := context.WithTimeout(context.Background(), time.Second)
		server.Shutdown(sctx)
		cancel()

		if res.err != "" {
			return Result{}, fmt.Errorf("authentication failed: %s", res.err)
		}

		// Determine the gateway URL: callback override > env > default.
		gatewayURL := res.apiURL
		if gatewayURL == "" {
			gatewayURL = envOr(provider.EnvAPIURL, DefaultGatewayURL)
		}
		return Result{Token: res.token, GatewayURL: gatewayURL, Email: res.email}, nil

	case <-ctx.Done():
		server.Close()
		return Result{}, fmt.Errorf("login canceled")

	case <-time.After(2 * time.Minute):
		server.Close()
		return Result{}, fmt.Errorf("timed out waiting for authentication (2 min). Try again.")
	}
}

// WriteGlobalEnvToken writes MEMCODE_API_TOKEN and MEMCODE_API_URL into the
// global env file (~/.config/memcode/.env), creating it if needed and
// replacing any existing values for those keys (no-override is for LoadDotEnv;
// login is the explicit user action that SHOULD overwrite).
func WriteGlobalEnvToken(token, apiURL string) error {
	path := provider.GlobalEnvPath()
	if path == "" {
		return fmt.Errorf("cannot determine global config path (no home directory)")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}

	// Read existing lines, drop any old API token / URL lines.
	var lines []string
	if existing, err := os.ReadFile(path); err == nil {
		for _, line := range strings.Split(string(existing), "\n") {
			trimmed := strings.TrimSpace(line)
			if trimmed == "" || strings.HasPrefix(trimmed, "#") {
				lines = append(lines, line)
				continue
			}
			stripped := strings.TrimPrefix(trimmed, "export ")
			key, _, ok := strings.Cut(stripped, "=")
			if ok && (strings.TrimSpace(key) == provider.EnvAPIToken || strings.TrimSpace(key) == provider.EnvAPIURL) {
				continue // drop old value
			}
			lines = append(lines, line)
		}
	}

	// Append the fresh values.
	lines = append(lines, fmt.Sprintf("%s=%s", provider.EnvAPIToken, token))
	lines = append(lines, fmt.Sprintf("%s=%s", provider.EnvAPIURL, apiURL))

	content := strings.Join(lines, "\n") + "\n"
	return os.WriteFile(path, []byte(content), 0600)
}

// StripGlobalEnvToken removes the token/url lines from the global env file
// (logout). Returns whether anything was removed; a missing file is a no-op.
// The token stays valid server-side until revoked from the web app
// (a /api/cli/revoke endpoint is a follow-up).
func StripGlobalEnvToken() (bool, error) {
	path := provider.GlobalEnvPath()
	if path == "" {
		return false, fmt.Errorf("cannot determine global config path (no home directory)")
	}
	existing, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("failed to read config: %w", err)
	}
	var lines []string
	removed := false
	for _, line := range strings.Split(string(existing), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			lines = append(lines, line)
			continue
		}
		stripped := strings.TrimPrefix(trimmed, "export ")
		key, _, ok := strings.Cut(stripped, "=")
		if ok && (strings.TrimSpace(key) == provider.EnvAPIToken || strings.TrimSpace(key) == provider.EnvAPIURL) {
			removed = true
			continue
		}
		lines = append(lines, line)
	}
	if !removed {
		return false, nil
	}
	content := strings.Join(lines, "\n")
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		return false, fmt.Errorf("failed to write config: %w", err)
	}
	return true, nil
}

// callbackScript is the MEMCODE matrix wordmark, ported verbatim from the
// marketing site's MatrixGlyphWordmark canvas component to dependency-free JS:
// a static lit-glyph base plus katakana rain, both composited then clipped to
// the word with destination-in, animated on requestAnimationFrame.
const callbackScript = `<script>(function(){
var G='ハケモサナアウキニツヲヤカコレミネテヒイオリ0123456789=<>+*|/\\'.split('');
var LIT=['#e0d8ff','#d2c6ff','#c4b6ff','#b9aaff'];
function pg(){return G[(Math.random()*G.length)|0];}
function pl(){return LIT[(Math.random()*LIT.length)|0];}
var canvas=document.getElementById('mark');
if(!canvas||!canvas.getContext){return;}
var ctx=canvas.getContext('2d');
var baseCell=11,baseCells=[],lastW=0,lastH=0,fc=14,columns=[];
function setup(w,h){
  lastW=w;lastH=h;baseCells=[];
  for(var y=baseCell/2;y<h;y+=baseCell){for(var x=baseCell/2;x<w;x+=baseCell){baseCells.push({x:x,y:y,glyph:pg(),color:pl()});}}
  var cell=w<640?10:fc,cols=Math.ceil(w/cell);columns=[];
  for(var i=0;i<cols;i++){columns.push({x:i*cell+cell/2,y:Math.random()*h,speed:0.15+Math.random()*0.3,len:4+((Math.random()*8)|0)});}
}
function draw(){
  var cssW=canvas.clientWidth,cssH=canvas.clientHeight;
  if(cssW===0||cssH===0){return;}
  if(cssW!==lastW||cssH!==lastH){setup(cssW,cssH);}
  var dpr=window.devicePixelRatio||1;
  if(canvas.width!==cssW*dpr||canvas.height!==cssH*dpr){canvas.width=cssW*dpr;canvas.height=cssH*dpr;}
  var lay=document.createElement('canvas');lay.width=cssW*dpr;lay.height=cssH*dpr;
  var lx=lay.getContext('2d');lx.setTransform(dpr,0,0,dpr,0,0);lx.clearRect(0,0,cssW,cssH);
  lx.textAlign='center';lx.textBaseline='middle';
  lx.font='700 '+(baseCell*1.05)+"px 'SF Mono',Menlo,Monaco,monospace";
  for(var k=0;k<baseCells.length;k++){var c=baseCells[k];lx.globalAlpha=0.55;lx.fillStyle=c.color;lx.fillText(c.glyph,c.x,c.y);}
  var cell=cssW<640?10:fc,alpha=cssW<640?0.27:0.19;
  lx.font='400 '+(cell*0.86)+"px 'SF Mono',Menlo,Monaco,monospace";
  for(var i=0;i<columns.length;i++){var col=columns[i];
    for(var j=0;j<col.len;j++){var yy=col.y-j*cell;if(yy<-cell||yy>cssH+cell){continue;}var tail=j/col.len;lx.globalAlpha=(1-tail)*(alpha+Math.random()*0.13);lx.fillStyle='#6a5da6';lx.fillText(pg(),col.x,yy);}
    col.y+=col.speed;if(col.y-col.len*cell>cssH){col.y=-Math.random()*cssH*0.5;col.speed=0.15+Math.random()*0.3;col.len=4+((Math.random()*8)|0);}
  }
  lx.globalAlpha=1;lx.globalCompositeOperation='destination-in';
  var word='MEMCODE',fs=cssH*0.92;lx.font='900 '+fs+"px 'Arial Black',Arial,sans-serif";
  while(lx.measureText(word).width>cssW*0.94&&fs>6){fs-=1;lx.font='900 '+fs+"px 'Arial Black',Arial,sans-serif";}
  lx.fillStyle='#fff';lx.fillText(word,cssW/2,cssH/2);
  ctx.setTransform(1,0,0,1,0,0);ctx.clearRect(0,0,canvas.width,canvas.height);ctx.drawImage(lay,0,0);
}
(function loop(){draw();requestAnimationFrame(loop);})();
if(typeof ResizeObserver!=='undefined'){new ResizeObserver(function(){lastW=0;}).observe(canvas);}
})();</script>`

func writeCallbackPage(w http.ResponseWriter, ok bool, errMsg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if !ok {
		w.WriteHeader(http.StatusBadRequest)
	}

	var inner string
	if ok {
		inner = `<h2>Authenticated</h2><p class="sub">You can close this tab and return to your terminal.</p>`
	} else {
		inner = fmt.Sprintf(
			`<h2>Authentication failed</h2><p class="err">%s</p><p class="sub">Close this tab and try again.</p>`,
			html.EscapeString(errMsg),
		)
	}

	// The MEMCODE wordmark is the SAME live canvas animation the marketing site
	// runs (a static glyph base with matrix rain sweeping through, both clipped
	// to the letterforms) — ported to vanilla JS so the self-contained page
	// needs no assets. Generous vertical rhythm below it: wordmark, heading, and
	// help line each get their own breathing room.
	page := `<!doctype html><html lang="en"><head><meta charset="utf-8">` +
		`<meta name="viewport" content="width=device-width,initial-scale=1"><title>memcode</title><style>` +
		`html,body{height:100%}` +
		`body{margin:0;background:#0a0a0a;color:#fafafa;` +
		`font-family:system-ui,-apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,Helvetica,Arial,sans-serif;` +
		`display:flex;align-items:center;justify-content:center}` +
		`.wrap{text-align:center;padding:40px;animation:fade .6s ease both}` +
		`.mark{width:560px;max-width:82vw;height:150px;display:block;margin:0 auto 44px}` +
		`h2{margin:0 0 14px;font-size:24px;font-weight:600;letter-spacing:-.01em}` +
		`.sub{margin:0;color:#888;font-size:15px;line-height:1.5}` +
		`.err{margin:0 0 10px;color:#f87171;font-size:15px}` +
		`@keyframes fade{from{opacity:0}to{opacity:1}}` +
		`</style></head><body><div class="wrap">` +
		`<canvas id="mark" class="mark"></canvas>` +
		inner +
		`</div>` +
		callbackScript +
		`</body></html>`

	io.WriteString(w, page)
}

// envOr returns os.Getenv(key) or fallback if empty.
func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// OpenBrowser opens url in the user's default browser. Best-effort — callers
// always print the URL too, so a headless session still works.
func OpenBrowser(url string) error { return openBrowser(url) }

// openBrowser opens url in the user's default browser. Best-effort.
func openBrowser(url string) error {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", url).Start()
	case "windows":
		prog, args := windowsBrowserCmd(url)
		return exec.Command(prog, args...).Start()
	default: // linux, *bsd
		for _, cmd := range []string{"xdg-open", "wslview", "gio"} {
			if _, err := exec.LookPath(cmd); err == nil {
				return exec.Command(cmd, url).Start()
			}
		}
		return fmt.Errorf("no browser-opening command found")
	}
}

// windowsBrowserCmd builds the argv that opens url in the default browser on
// Windows. It is split out from openBrowser so the shell quoting — the part
// that actually broke — is unit-testable off a Windows host.
//
// It deliberately does NOT use `rundll32 url.dll,FileProtocolHandler`: that
// strips/percent-encodes the query string (the ? became %3F), so
//
//	/api/cli/auth?port=&state=
//
// reached the browser as /api/cli/auth%3Fport=… and 404'd — the Windows-only
// login failure. `cmd /c start` preserves the URL verbatim, with two caveats:
//   - & is a cmd command separator, so every & is caret-escaped (^&); cmd
//     collapses ^& back to a literal & before the browser sees it. (Go does not
//     quote this arg — no spaces — so the caret survives to cmd unquoted, which
//     is exactly where it must be to take effect.)
//   - the empty "" is start's title argument, so a URL that ever ends up quoted
//     is not mistaken for the window title.
func windowsBrowserCmd(url string) (string, []string) {
	return "cmd", []string{"/c", "start", "", strings.ReplaceAll(url, "&", "^&")}
}

// SetGlobalEnv writes key=value pairs into the global env file
// (~/.config/memcode/.env), replacing any existing lines for those keys and
// leaving everything else intact. The wizard uses it to persist a credential
// choice (a selected subscription source, an own key, an endpoint). Values are
// written verbatim; callers pass already-trimmed values.
func SetGlobalEnv(kv map[string]string) error {
	path := provider.GlobalEnvPath()
	if path == "" {
		return fmt.Errorf("cannot determine global config path (no home directory)")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	var lines []string
	if existing, err := os.ReadFile(path); err == nil {
		for _, line := range strings.Split(string(existing), "\n") {
			trimmed := strings.TrimSpace(line)
			if trimmed == "" || strings.HasPrefix(trimmed, "#") {
				lines = append(lines, line)
				continue
			}
			key, _, ok := strings.Cut(strings.TrimPrefix(trimmed, "export "), "=")
			if ok {
				if _, replace := kv[strings.TrimSpace(key)]; replace {
					continue // drop the old value; re-appended below
				}
			}
			lines = append(lines, line)
		}
	}
	for k, v := range kv {
		lines = append(lines, fmt.Sprintf("%s=%s", k, v))
	}
	return os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0600)
}
