package runtime

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/memcode-ai/memcode/internal/agent/permissions"
	"github.com/memcode-ai/memcode/internal/agent/tools"
	"github.com/memcode-ai/memcode/internal/store"
	"github.com/memcode-ai/memcode/internal/websites"
	"github.com/memcode-ai/memcode/internal/wire"
)

func starterTarGz(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range map[string]string{"package.json": "{}", "src/App.jsx": "app", ".gitignore": "node_modules\n"} {
		tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg})
		tw.Write([]byte(body))
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

// localFirstServer is memcode.ai for the local-first flow: it serves the
// starter, records what is created/pushed/published, and serves pushed
// bundles back.
type localFirstServer struct {
	mu                         sync.Mutex
	url                        string
	tpl, hosted                []byte
	created, pushes, publishes int
	promptBuilds               int
}

func (f *localFirstServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.URL.Path == "/api/website/template":
		json.NewEncoder(w).Encode(map[string]string{"url": f.url + "/tpl"})
	case r.URL.Path == "/tpl":
		w.Write(f.tpl)
	case r.URL.Path == "/api/website" && r.Method == http.MethodGet:
		var list []map[string]any
		if f.created > 0 {
			list = append(list, map[string]any{"id": "s1", "name": "Crumb", "slug": "crumb"})
		}
		json.NewEncoder(w).Encode(map[string]any{"websites": list})
	case r.URL.Path == "/api/website" && r.Method == http.MethodPost:
		f.created++
		json.NewEncoder(w).Encode(map[string]any{"website": map[string]any{"id": "s1", "name": "Crumb", "slug": "crumb"}})
	case r.URL.Path == "/api/website/s1":
		json.NewEncoder(w).Encode(map[string]any{"website": map[string]any{"id": "s1", "name": "Crumb", "slug": "crumb"}})
	case r.URL.Path == "/api/website/s1/build":
		f.promptBuilds++
		json.NewEncoder(w).Encode(map[string]any{"build": map[string]any{"id": "b0", "status": "pending"}})
	case r.URL.Path == "/api/website/s1/bundle" && r.Method == http.MethodGet:
		if f.hosted == nil {
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"url": f.url + "/hosted"})
	case r.URL.Path == "/hosted":
		w.Write(f.hosted)
	case r.URL.Path == "/api/website/s1/bundle" && r.Method == http.MethodPost:
		f.hosted, _ = io.ReadAll(r.Body)
		f.pushes++
		json.NewEncoder(w).Encode(map[string]any{"build": map[string]any{"id": "b1", "status": "pending"}})
	case r.URL.Path == "/api/website/s1/builds":
		json.NewEncoder(w).Encode(map[string]any{"builds": []map[string]any{{"id": "b1", "status": "completed"}}})
	case r.URL.Path == "/api/website/s1/publish":
		f.publishes++
		json.NewEncoder(w).Encode(map[string]string{"url": "https://crumb.memcode.app"})
	default:
		http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusTeapot)
	}
}

// localFirstSession: an interactive session against localFirstServer, with
// Node and the dev server stubbed (they are the machine's business).
func localFirstSession(t *testing.T) (*Session, *localFirstServer) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	f := &localFirstServer{tpl: starterTarGz(t)}
	s := websitesSession(t, f)
	f.url = os.Getenv("MEMCODE_WEB_APP_URL")
	s.conversational = true
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	s.bgCtx = ctx

	oldNode, oldDev, oldPoll := nodeReady, startDevServer, siteBuildPoll
	nodeReady = func(*Session, context.Context) (bool, string) { return true, "" }
	startDevServer = func(*Session, context.Context, websites.LocalSite) (string, bool, error) {
		return "http://localhost:5173/", true, nil
	}
	siteBuildPoll = time.Millisecond
	t.Cleanup(func() { nodeReady, startDevServer, siteBuildPoll = oldNode, oldDev, oldPoll })
	return s, f
}

// THE INVARIANT: in an interactive CLI session a new site is made on this
// machine, and memcode.ai changes only when the user pushes or publishes.
func TestCreateIsLocalInTheCLI(t *testing.T) {
	s, f := localFirstSession(t)
	asked := 0
	s.approve = allowAll(&asked)
	r := websitesCall(s, tools.WebsitesInput{Action: "create", Name: "Crumb"})
	if r.isError {
		t.Fatal(r.text())
	}
	if f.created != 0 || f.promptBuilds != 0 || f.pushes != 0 {
		t.Fatalf("create touched memcode.ai: created=%d builds=%d pushes=%d", f.created, f.promptBuilds, f.pushes)
	}
	if asked != 0 {
		t.Fatal("making a site on this machine needs no approval")
	}
	if !strings.Contains(r.text(), "localhost:5173") || !strings.Contains(r.text(), "only") {
		t.Fatalf("result = %s", r.text())
	}
	if _, ok := websites.FindLocal("Crumb"); !ok {
		t.Fatal("no local copy was made")
	}
	// Hosted-only actions say plainly that it isn't on memcode.ai yet.
	if r := websitesCall(s, tools.WebsitesInput{Action: "preview", Slug: "Crumb"}); !r.isError || !strings.Contains(r.text(), "only on this machine") {
		t.Fatalf("preview of a local-only site = %s", r.text())
	}
}

// Asking for it on memcode.ai, or a one-shot run, still builds there.
func TestRemoteCreateStillBuildsThere(t *testing.T) {
	for _, conversational := range []bool{true, false} {
		s, f := localFirstSession(t)
		s.conversational = conversational
		asked := 0
		s.approve = allowAll(&asked)
		r := websitesCall(s, tools.WebsitesInput{Action: "create", Name: "Crumb", Prompt: "a bakery", Remote: conversational})
		if r.isError {
			t.Fatal(r.text())
		}
		if f.created != 1 || f.promptBuilds != 1 {
			t.Fatalf("conversational=%v: created=%d builds=%d", conversational, f.created, f.promptBuilds)
		}
	}
}

// push creates the site on memcode.ai the first time, behind an approval;
// publish pushes local changes first so what goes live is what the user saw.
func TestPushThenPublishCarryLocalWork(t *testing.T) {
	s, f := localFirstSession(t)
	asked := 0
	s.approve = allowAll(&asked)
	if r := websitesCall(s, tools.WebsitesInput{Action: "create", Name: "Crumb"}); r.isError {
		t.Fatal(r.text())
	}
	r := websitesCall(s, tools.WebsitesInput{Action: "push", Slug: "Crumb"})
	if r.isError {
		t.Fatal(r.text())
	}
	if f.created != 1 || f.pushes != 1 || f.promptBuilds != 0 || asked != 1 {
		t.Fatalf("push: created=%d pushes=%d builds=%d asked=%d", f.created, f.pushes, f.promptBuilds, asked)
	}

	// Nothing changed since: publish just publishes.
	if r := websitesCall(s, tools.WebsitesInput{Action: "publish", Slug: "Crumb"}); r.isError {
		t.Fatal(r.text())
	}
	if f.pushes != 1 || f.publishes != 1 {
		t.Fatalf("clean publish: pushes=%d publishes=%d", f.pushes, f.publishes)
	}

	// A local change: publish pushes it first.
	l, _ := websites.FindLocal("Crumb")
	os.WriteFile(filepath.Join(l.Source, "src", "App.jsx"), []byte("new"), 0o644)
	if r := websitesCall(s, tools.WebsitesInput{Action: "publish", Slug: "Crumb"}); r.isError {
		t.Fatal(r.text())
	}
	if f.pushes != 2 || f.publishes != 2 {
		t.Fatalf("publish with local changes: pushes=%d publishes=%d", f.pushes, f.publishes)
	}
}

// A one-shot run can't show a local preview, so develop says to build there.
func TestDevelopNeedsAnInteractiveSession(t *testing.T) {
	s, _ := localFirstSession(t)
	s.conversational = false
	if r := websitesCall(s, tools.WebsitesInput{Action: "develop", Slug: "Crumb"}); !r.isError {
		t.Fatalf("develop in a one-shot run = %s", r.text())
	}
}

// writeScript answers agent-loop calls with edit_file writes, in order.
type writeScript struct {
	paths []string
	n     int
}

func (p *writeScript) Complete(_ context.Context, r wire.Request) (wire.Response, error) {
	agent := false
	for _, d := range r.Tools {
		agent = agent || d.Name == tools.EditFile
	}
	if !agent || p.n >= len(p.paths) {
		return wire.Response{StopReason: "end_turn", Blocks: []wire.Block{wire.TextBlock("done")}}, nil
	}
	in, _ := json.Marshal(tools.EditFileInput{Path: p.paths[p.n], NewString: "x"})
	p.n++
	return wire.Response{StopReason: "tool_use", Blocks: []wire.Block{{Type: "tool_use", ID: "w" + p.paths[p.n-1], Name: tools.EditFile, Input: in}}}, nil
}

func (p *writeScript) Stream(ctx context.Context, r wire.Request, _ wire.StreamHandler) (wire.Response, error) {
	return p.Complete(ctx, r)
}

// A site worker is rooted at the site's folder: it writes there and nowhere
// else, and the session's own boundary is unchanged.
func TestSiteWorkerIsConfinedToTheSite(t *testing.T) {
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	repo, site := t.TempDir(), t.TempDir()
	prov := &writeScript{paths: []string{"src/Note.jsx", "../escaped.txt", filepath.Join(repo, "owned.txt")}}
	s := newSess(st, prov, repo, "sonnet", permissions.ModeAuto, io.Discard)
	if _, err := s.spawnAgent(context.Background(), AgentSpec{Task: "edit the site", Root: site}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(site, "src", "Note.jsx")); err != nil {
		t.Fatal("the worker's write didn't land in the site folder")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(site), "escaped.txt")); err == nil {
		t.Fatal("the worker wrote outside the site folder")
	}
	if _, err := os.Stat(filepath.Join(repo, "owned.txt")); err == nil {
		t.Fatal("the worker wrote into the session's repo")
	}
	if s.root != repo {
		t.Fatal("the session's own root changed")
	}
}
