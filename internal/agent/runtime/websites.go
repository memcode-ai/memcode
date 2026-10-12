package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/memcode-ai/memcode/internal/authflow"
	"os"
	"path/filepath"
	"strings"

	"github.com/memcode-ai/memcode/internal/agent/permissions"
	"github.com/memcode-ai/memcode/internal/agent/secrets"
	"github.com/memcode-ai/memcode/internal/agent/tools"
	"github.com/memcode-ai/memcode/internal/provider"
	"github.com/memcode-ai/memcode/internal/websites"
)

// websitesTool is the remote/API-backed surface over memcode.ai's Websites
// feature — list/create/build/edit/publish/assets, all addressed by slug.
// Local workflows that need real files (pull/dev/push, or editing inside a
// pulled checkout) stay on the `memcode websites` CLI.
//
// Gating is per action (websiteActionRisk): drafts and metadata are Medium,
// anything that changes what the PUBLIC sees or destroys data is Dangerous —
// so auto mode still prompts for publish/unpublish/delete/asset changes, and a
// remembered "don't ask again" applies only to the one Medium action it was
// given for, never to a destructive one.
func (s *Session) websitesTool(ctx context.Context, input json.RawMessage) toolResult {
	var in tools.WebsitesInput
	if err := json.Unmarshal(input, &in); err != nil {
		return errResult("websites: bad input: " + err.Error())
	}
	risk, known := websiteActionRisk(in.Action)
	if !known {
		return errResult("websites: unknown action " + in.Action + " (list | get | create | develop | push | zip | share | build | edit | builds | preview | publish | unpublish | delete | update | assets_list | assets_upload | assets_delete)")
	}
	// Plan mode and read-only sub-agents research; they don't mutate a live site — check
	// this BEFORE the account check so the Read Only denial always wins, with or without a
	// login token (the read-only actions are exempt, same split as artifactTool).
	if risk > permissions.Safe && (s.readOnly || s.planCtl.Planning() || s.chatReadOnly) {
		return errResult("denied: websites " + in.Action + " isn't available while planning/read-only — the read-only actions are, changes wait for execution")
	}
	// Argument problems are reported before any network call or approval prompt.
	uploadPath, err := s.validateWebsitesInput(in)
	if err != nil {
		return errResult(err.Error())
	}
	if strings.TrimSpace(os.Getenv(provider.EnvAPIToken)) == "" {
		return errResult("websites need a memcode.ai account — run `memcode login` first")
	}
	client, err := websites.New()
	if err != nil {
		return errResult(err.Error())
	}

	if in.Action == "list" {
		sites, err := client.List(ctx)
		if err != nil {
			return errResult("websites list failed: " + err.Error())
		}
		var localOnly []string
		if all, err := websites.ListLocal(); err == nil {
			for _, l := range all {
				if !l.Pushed() {
					localOnly = append(localOnly, l.Meta.Name)
				}
			}
		}
		s.toolLine(true, "Websites", "list", fmt.Sprintf("%d sites", len(sites)+len(localOnly)), false)
		if len(sites) == 0 && len(localOnly) == 0 {
			return textResult("no websites yet — create one with websites{action:\"create\", name:..., prompt:...}")
		}
		var b strings.Builder
		if len(sites) > 0 {
			b.WriteString(websites.RenderList(sites))
		}
		if len(localOnly) > 0 {
			fmt.Fprintf(&b, "\nOnly on this machine (not pushed yet): %s", strings.Join(localOnly, ", "))
		}
		return textResult(strings.TrimSpace(b.String()))
	}

	// Local-first: in an interactive session a new site starts on this
	// machine, and memcode.ai changes only when the user pushes or publishes.
	// A one-shot run has no one to look at a local preview, so it builds on
	// memcode.ai as before.
	switch in.Action {
	case "create":
		if !in.Remote && s.conversational {
			return s.createLocal(ctx, client, in.Name, in.Prompt)
		}
	case "develop":
		if !s.conversational {
			return errResult("websites develop runs a local preview and needs an interactive session; use build to change the site on memcode.ai")
		}
		return s.developSite(ctx, client, in.Slug, in.Prompt)
	case "push":
		return s.pushSite(ctx, client, in.Slug)
	case "zip":
		return s.zipSite(in.Slug)
	case "publish":
		if l, ok := websites.FindLocal(in.Slug); ok && l.NeedsPush() {
			detail := "pushes your local changes, then replaces what the public sees at the live URL"
			if !l.Pushed() {
				detail = "creates it on memcode.ai, uploads it, then makes it live"
			}
			if denied := s.gateWebsites(ctx, in.Action, risk, "Publish website", l.Label(), detail); denied != nil {
				return *denied
			}
			site, err := s.pushBeforePublish(ctx, client, l)
			if err != nil {
				return errResult("websites publish stopped before going live: " + err.Error())
			}
			return s.publish(ctx, client, site)
		}
	}

	if in.Action == "create" {
		verb, detail := "Create website", in.Name
		if in.Prompt != "" {
			// One approval, two effects: say so — the build spends credits.
			verb, detail = "Create website and queue a build", clip(in.Prompt, 200)
		}
		if denied := s.gateWebsites(ctx, in.Action, risk, verb, in.Name, detail); denied != nil {
			return *denied
		}
		site, err := client.Create(ctx, in.Name, in.Prompt, in.Model)
		if err != nil {
			return errResult("websites create failed: " + err.Error())
		}
		s.toolLine(true, "Websites", "create", site.Slug, false)
		if in.Prompt == "" {
			return textResult(fmt.Sprintf("created %q (%s) — queue a build with websites{action:\"build\", slug:%q, prompt:...}", site.Name, site.Slug, site.Slug))
		}
		build, buildErr := client.Build(ctx, site.ID, in.Prompt, in.Model)
		switch {
		case errors.Is(buildErr, websites.ErrBuildsGated):
			return textResult(fmt.Sprintf("created %q (%s), but no build was queued: %s. The site exists but is empty; tell the user, don't retry.", site.Name, site.Slug, buildErr.Error()))
		case buildErr != nil:
			return textResult(fmt.Sprintf("created %q (%s), but the build failed to queue: %s", site.Name, site.Slug, buildErr.Error()))
		}
		s.watchSiteBuild(client, site, build)
		return textResult(fmt.Sprintf("created %q (%s) and queued a build (%s, status %s). %s", site.Name, site.Slug, build.ID, build.Status, s.buildFollowUp(site.Slug)))
	}

	// Every remaining action targets one existing site on memcode.ai. A local
	// site is named by its local name until pushed.
	if l, ok := websites.FindLocal(in.Slug); ok {
		if !l.Pushed() {
			return errResult(fmt.Sprintf("%q exists only on this machine (%s); websites %s needs it on memcode.ai — push it first if the user wants that", l.Meta.Name, l.Source, in.Action))
		}
		in.Slug = l.Meta.Slug
	}
	if in.Action == "get" {
		site, err := client.GetBySlug(ctx, in.Slug)
		if err != nil {
			return errResult("websites get failed: " + err.Error())
		}
		s.toolLine(true, "Websites", "get", site.Slug, false)
		return textResult(describeSite(site))
	}
	site, err := client.Resolve(ctx, in.Slug)
	if err != nil {
		return errResult(err.Error())
	}

	switch in.Action {
	case "build", "edit":
		verb, detail := "Build website", in.Prompt
		if in.Action == "edit" {
			verb, detail = "Edit website", fmt.Sprintf("%d find/replace edit(s)", len(in.Edits))
		}
		if denied := s.gateWebsites(ctx, in.Action, risk, verb, site.Slug, detail); denied != nil {
			return *denied
		}
		var build websites.Build
		if in.Action == "build" {
			build, err = client.Build(ctx, site.ID, in.Prompt, in.Model)
		} else {
			edits := make([]websites.Edit, len(in.Edits))
			for i, e := range in.Edits {
				edits[i] = websites.Edit{Find: e.Find, Replace: e.Replace}
			}
			build, err = client.Edit(ctx, site.ID, edits, in.Model)
		}
		if errors.Is(err, websites.ErrBuildsGated) {
			return errResult(err.Error() + ". This is an account restriction, not a transient failure — tell the user, don't retry.")
		}
		if err != nil {
			return errResult("websites " + in.Action + " failed: " + err.Error())
		}
		s.toolLine(true, "Websites", in.Action, site.Slug, false)
		s.watchSiteBuild(client, site, build)
		return textResult(fmt.Sprintf("%s queued (%s), status %s. %s", in.Action, build.ID, build.Status, s.buildFollowUp(site.Slug)))

	case "builds":
		builds, err := client.ListBuilds(ctx, site.ID)
		if err != nil {
			return errResult("websites builds failed: " + err.Error())
		}
		s.toolLine(true, "Websites", "builds", site.Slug, false)
		if len(builds) == 0 {
			return textResult("no builds yet")
		}
		var b strings.Builder
		for _, build := range builds {
			fmt.Fprintf(&b, "%s  %-8s %-8s", build.ID, build.Mode, build.Status)
			if build.Error != "" {
				fmt.Fprintf(&b, "  error: %s", build.Error)
			} else if build.Summary != "" {
				fmt.Fprintf(&b, "  %s", build.Summary)
			}
			b.WriteByte('\n')
		}
		return textResult(strings.TrimRight(b.String(), "\n"))

	case "share":
		if denied := s.gateWebsites(ctx, in.Action, risk, "Share website preview", site.Slug, "a link anyone can use to view the preview for 7 days"); denied != nil {
			return *denied
		}
		link, err := client.Share(ctx, site.ID, 0)
		if err != nil {
			return errResult("websites share failed: " + err.Error())
		}
		s.toolLine(true, "Websites", "share", site.Slug, false)
		return textResult(fmt.Sprintf("Share link: %s (anyone with it can view the preview, not change it, until %s; `memcode websites share %s --off <link>` turns it off).",
			link.URL, link.ExpiresAt.Local().Format("Jan 2 15:04"), site.Slug))

	case "preview":
		url, err := client.Preview(ctx, site.ID)
		if err != nil {
			return errResult("websites preview failed: " + err.Error())
		}
		s.toolLine(true, "Websites", "preview", site.Slug, false)
		return s.offerPreview(ctx, site.Slug, url, in.Reveal)

	case "publish":
		if denied := s.gateWebsites(ctx, in.Action, risk, "Publish website", site.Slug, "replaces what the public sees at the live URL"); denied != nil {
			return *denied
		}
		return s.publish(ctx, client, site)

	case "unpublish":
		if denied := s.gateWebsites(ctx, in.Action, risk, "Unpublish website", site.Slug, "takes the live site offline"); denied != nil {
			return *denied
		}
		if err := client.Unpublish(ctx, site.ID); err != nil {
			return errResult("websites unpublish failed: " + err.Error())
		}
		s.toolLine(true, "Websites", "unpublish", site.Slug, false)
		return textResult("unpublished " + site.Slug)

	case "update":
		var namePtr, modelPtr *string
		if in.Name != "" {
			namePtr = &in.Name
		}
		if in.Model != "" {
			modelPtr = &in.Model
		}
		if denied := s.gateWebsites(ctx, in.Action, risk, "Update website", site.Slug, ""); denied != nil {
			return *denied
		}
		updated, err := client.Update(ctx, site.ID, namePtr, modelPtr)
		if err != nil {
			return errResult("websites update failed: " + err.Error())
		}
		s.toolLine(true, "Websites", "update", updated.Slug, false)
		return textResult("updated " + updated.Slug)

	case "delete":
		if denied := s.gateWebsites(ctx, in.Action, risk, "Delete website", site.Slug, "destructive — it stops serving immediately"); denied != nil {
			return *denied
		}
		if err := client.Delete(ctx, site.ID); err != nil {
			return errResult("websites delete failed: " + err.Error())
		}
		s.toolLine(true, "Websites", "delete", site.Slug, false)
		return textResult("deleted " + site.Slug)

	case "assets_list":
		assets, err := client.ListAssets(ctx, site.ID)
		if err != nil {
			return errResult("websites assets_list failed: " + err.Error())
		}
		s.toolLine(true, "Websites", "assets_list", site.Slug, false)
		if len(assets) == 0 {
			return textResult("no assets uploaded yet")
		}
		var b strings.Builder
		for _, a := range assets {
			fmt.Fprintf(&b, "%-30s %10d bytes  %s\n", a.Name, a.Size, a.URL)
		}
		return textResult(strings.TrimRight(b.String(), "\n"))

	case "assets_upload":
		if denied := s.gateWebsites(ctx, in.Action, risk, "Upload website asset", site.Slug, in.AssetPath+" → public /assets/ URL"); denied != nil {
			return *denied
		}
		asset, err := client.UploadAsset(ctx, site.ID, uploadPath, in.AssetName)
		if err != nil {
			return errResult("websites assets_upload failed: " + err.Error())
		}
		s.toolLine(true, "Websites", "assets_upload", asset.Name, false)
		return textResult(fmt.Sprintf("uploaded %s (%d bytes) — %s", asset.Name, asset.Size, asset.URL))

	case "assets_delete":
		if denied := s.gateWebsites(ctx, in.Action, risk, "Delete website asset", site.Slug, in.AssetName); denied != nil {
			return *denied
		}
		if err := client.DeleteAsset(ctx, site.ID, in.AssetName); err != nil {
			return errResult("websites assets_delete failed: " + err.Error())
		}
		s.toolLine(true, "Websites", "assets_delete", in.AssetName, false)
		return textResult("deleted asset " + in.AssetName)
	}
	return errResult("websites: unhandled action " + in.Action) // unreachable: websiteActionRisk vetted it
}

// websiteActionRisk classifies each action. Safe = read-only. Medium = changes
// a draft or metadata only. Dangerous = changes what the PUBLIC sees or
// destroys data (publish, unpublish, delete, uploading a repo file to a public
// URL, removing an asset live pages may link to).
func websiteActionRisk(action string) (permissions.Risk, bool) {
	switch action {
	case "list", "get", "builds", "preview", "assets_list", "develop", "zip":
		// develop changes only the local copy, through a worker whose own
		// file tools are gated as usual; zip writes one file locally.
		return permissions.Safe, true
	case "push":
		return websitesPushRisk, true
	case "create", "build", "edit", "update", "share":
		// share: a link that lets anyone holding it view the draft for a week.
		return permissions.Medium, true
	case "publish", "unpublish", "delete", "assets_upload", "assets_delete":
		return permissions.Dangerous, true
	}
	return permissions.Safe, false
}

func (s *Session) publish(ctx context.Context, client *websites.Client, site websites.Site) toolResult {
	url, err := client.Publish(ctx, site.ID)
	if err != nil {
		return errResult("websites publish failed: " + err.Error())
	}
	s.toolLine(true, "Websites", "publish", site.Slug, false)
	if url != "" {
		return textResult("published — live at " + url)
	}
	return textResult("published " + site.Slug)
}

// validateWebsitesInput checks arguments up front, before any network call or
// approval prompt. For assets_upload it returns the resolved absolute path.
func (s *Session) validateWebsitesInput(in tools.WebsitesInput) (string, error) {
	switch in.Action {
	case "list":
		return "", nil
	case "create":
		if in.Name == "" && in.Prompt == "" {
			return "", fmt.Errorf("websites create needs `name`, `prompt`, or both")
		}
		return "", nil
	}
	if strings.TrimSpace(in.Slug) == "" {
		return "", fmt.Errorf("websites %s needs `slug`", in.Action)
	}
	switch in.Action {
	case "edit":
		if len(in.Edits) == 0 {
			return "", fmt.Errorf("websites edit needs at least one entry in `edits`")
		}
	case "update":
		if in.Name == "" && in.Model == "" {
			return "", fmt.Errorf("websites update needs `name`, `model`, or both")
		}
	case "assets_delete":
		if in.AssetName == "" {
			return "", fmt.Errorf("websites assets_delete needs `asset_name`")
		}
	case "assets_upload":
		return s.uploadableAsset(in.AssetPath)
	}
	return "", nil
}

// uploadableAsset resolves an asset_path and refuses anything that must never
// land on a public URL: paths outside the workspace (symlinks included — safeJoin
// resolves them), hidden files/dirs (.env, .git/…, .memcode/…), and anything the
// secrets package recognizes as credential-bearing (keys, certs, credentials).
func (s *Session) uploadableAsset(rel string) (string, error) {
	if strings.TrimSpace(rel) == "" {
		return "", fmt.Errorf("websites assets_upload needs `asset_path`")
	}
	abs, err := safeJoin(s.root, rel)
	if err != nil {
		return "", fmt.Errorf("websites: asset_path escapes the workspace: %s", rel)
	}
	root, err := filepath.EvalSymlinks(s.root)
	if err != nil {
		root = s.root
	}
	// Resolve the FILE itself, not just its ancestors: an innocently named
	// symlink (public/logo.png → ../.ssh/id_rsa) must be judged by its target.
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("websites: can't read %s: %v", rel, err)
	}
	inRepo, err := filepath.Rel(root, real)
	if err != nil || inRepo == ".." || strings.HasPrefix(inRepo, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("websites: asset_path escapes the workspace: %s", rel)
	}
	abs = real
	for _, p := range []string{rel, inRepo} {
		for _, seg := range strings.Split(filepath.ToSlash(p), "/") {
			if strings.HasPrefix(seg, ".") && seg != "." && seg != ".." {
				return "", fmt.Errorf("websites: refusing to upload %s — hidden files and directories are never published", rel)
			}
		}
		if secrets.IsSecretPath(p) {
			return "", fmt.Errorf("websites: refusing to upload %s — it looks like a credentials file, and assets are served publicly", rel)
		}
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("websites: can't read %s: %v", rel, err)
	}
	if fi.IsDir() {
		return "", fmt.Errorf("websites: %s is a directory — upload one file at a time", rel)
	}
	return abs, nil
}

// describeSite renders one site's detail for the tool result (mirrors `memcode websites get`).
func describeSite(site websites.Site) string {
	status := site.Status
	if status == "" {
		status = "draft"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s (%s)\n  status:  %s\n", site.Name, site.Slug, status)
	if site.Model != "" {
		fmt.Fprintf(&b, "  model:   %s\n", site.Model)
	}
	if site.LiveURL != "" {
		fmt.Fprintf(&b, "  url:     %s\n", site.LiveURL)
	}
	if site.PublishedAt != "" {
		fmt.Fprintf(&b, "  published: %s\n", site.PublishedAt)
	}
	fmt.Fprintf(&b, "  preview: %v", site.HasPreview)
	return b.String()
}

// gateWebsites runs the approval gate for one mutating websites action.
// Returns nil when allowed; a pointer to the denial result otherwise.
//   - Medium actions: auto/allow-all run without a prompt; ask mode prompts and
//     offers "don't ask again" scoped to THAT action only.
//   - Dangerous actions: prompt in ask AND auto mode, and never offer or honor
//     a remembered approval — approving `create` once must not pre-approve
//     `delete`.
func (s *Session) gateWebsites(ctx context.Context, action string, risk permissions.Risk, verb, slug, detail string) *toolResult {
	rememberable := risk < permissions.Dangerous
	if rememberable && s.approvedWebsites[action] {
		return nil
	}
	if permissions.Decide(s.effectiveMode(), risk, false) == permissions.Allow {
		return nil
	}
	d := s.askApproval(ctx, ApprovalRequest{
		Title:    slug,
		Label:    verb,
		Detail:   detail,
		Editable: rememberable,
	})
	if d.Interrupt {
		r := errResult("websites action interrupted — stopped at your request.")
		return &r
	}
	if !d.Allow {
		r := errResult("websites action declined by the user — leave the site as is or adjust and ask again.")
		return &r
	}
	if d.Remember && rememberable {
		s.rememberWebsitesApproval(action)
	}
	return nil
}

// websitesApprovalsPath remembers the repo-scoped "don't ask again" approvals
// for websites actions: one action name per line (Medium actions only).
func websitesApprovalsPath(root string) string {
	return filepath.Join(root, ".memcode", "websites-approvals")
}

// loadWebsitesApproval reads the remembered per-action approvals. Lines naming
// a Dangerous (or unknown) action are ignored even if hand-edited in.
func loadWebsitesApproval(root string) map[string]bool {
	m := map[string]bool{}
	b, err := os.ReadFile(websitesApprovalsPath(root))
	if err != nil {
		return m
	}
	for _, ln := range strings.Split(string(b), "\n") {
		a := strings.TrimSpace(ln)
		if risk, ok := websiteActionRisk(a); ok && risk == permissions.Medium {
			m[a] = true
		}
	}
	return m
}

// rememberWebsitesApproval records a per-action don't-ask-again in memory AND
// on disk. Best-effort persistence — an unwritable file just means re-asking.
func (s *Session) rememberWebsitesApproval(action string) {
	if s.approvedWebsites == nil {
		s.approvedWebsites = map[string]bool{}
	}
	if s.approvedWebsites[action] {
		return
	}
	s.approvedWebsites[action] = true
	path := websitesApprovalsPath(s.root)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.WriteString(action + "\n")
}

// buildFollowUp tells the model what happens next with a queued build. In an
// interactive session the session watches it: the user sees it in the status
// bar and is told when it finishes, with an offer to open the preview, so
// the model must not ask them to check back or poll for it.
func (s *Session) buildFollowUp(slug string) string {
	if s.conversational {
		return "memcode is watching this build: the user sees it in the status bar and is told when it finishes, with an offer to open the preview. Don't ask them to check back, and don't poll for it."
	}
	return fmt.Sprintf("Check progress with websites{action:\"builds\", slug:%q}.", slug)
}

// openBrowser opens a URL in the user's browser; a var so tests never launch one.
var openBrowser = authflow.OpenBrowser

// Preview choices offered to the user.
const (
	previewOpen = "Open it in my browser"
	previewLink = "Show me the link"
)

// offerPreview hands a preview to the user without putting its link into
// the conversation: the link carries a private access token. Interactive
// sessions offer to open it in the browser (or show the link to the user);
// the model only learns what happened. With reveal, or with no one to ask (a
// one-shot run), the link itself is returned.
func (s *Session) offerPreview(ctx context.Context, slug, url string, reveal bool) toolResult {
	if reveal || !s.conversational || s.ask == nil {
		return textResult(url + "\n(The link carries a private access token: anyone with it can view the draft for 6 hours.)")
	}
	resp := s.ask(ctx, AskRequest{
		Question: "The preview of " + slug + " is ready. Open it?",
		Options: []AskOption{
			{Label: previewOpen},
			{Label: previewLink, Description: "for you only; it's a private link that works for 6 hours"},
			{Label: "Not now"},
		},
	})
	switch resp.Answer {
	case previewOpen:
		if err := openBrowser(url); err != nil {
			s.printf("  preview: %s\n", url)
			return textResult("couldn't open a browser here, so the link was shown to the user. Don't repeat it.")
		}
		return textResult("opened the preview of " + slug + " in the user's browser.")
	case previewLink:
		s.printf("  preview: %s\n", url)
		return textResult("showed the user the preview link. It carries a private access token: don't repeat it.")
	}
	return textResult("the user chose not to open the preview now.")
}
