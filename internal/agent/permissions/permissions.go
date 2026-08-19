// Package permissions classifies the risk of an action and decides — given the
// active mode — whether it may run, needs approval, or is blocked. v0 has no
// remembered approvals; every medium/dangerous action is decided live.
package permissions

import (
	"path/filepath"
	"strconv"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// Risk is how dangerous an action is.
type Risk int

const (
	Safe      Risk = iota // read-only / inspection
	Medium                // writes, installs, builds, tests
	Dangerous             // destructive / irreversible / external side effects
)

func (r Risk) String() string {
	switch r {
	case Safe:
		return "safe"
	case Medium:
		return "medium"
	default:
		return "dangerous"
	}
}

// Mode is the permission policy for a run.
type Mode string

const (
	ModeAsk      Mode = "ask"       // prompt before medium/dangerous (default)
	ModeAuto     Mode = "auto"      // run safe+medium; prompt for dangerous
	ModeAllowAll Mode = "allow-all" // run everything except catastrophic (still prompts those)
)

// Decision is the gate's verdict for an action.
type Decision int

const (
	Allow      Decision = iota // run without prompting
	NeedPrompt                 // ask the human first
	Block                      // never (catastrophic in a non-interactive context)
)

// Decide returns whether an action of the given risk may proceed under mode.
// catastrophic marks irreversible commands that always require confirmation,
// even in allow-all.
func Decide(mode Mode, risk Risk, catastrophic bool) Decision {
	if catastrophic {
		return NeedPrompt
	}
	switch mode {
	case ModeAllowAll:
		return Allow
	case ModeAuto:
		if risk >= Dangerous {
			return NeedPrompt
		}
		return Allow
	default: // ModeAsk
		if risk >= Medium {
			return NeedPrompt
		}
		return Allow
	}
}

// The classifier's guiding rule: when in doubt, classify HIGHER. A command we
// can't confidently prove read-only must land at Medium/Dangerous so it PROMPTS
// (and is therefore never auto-run in a read-only/plan context). Safe is reserved
// for operations we positively recognize as read-only.

// readLocal: local tools that only read/inspect.
// NOTE: find/fd are NOT here — they can delete (-delete) or run commands (-exec/-x), so they're
// classified explicitly (see classifyFindExec) rather than blanket-Safe.
var readLocal = set(
	"cat", "ls", "ll", "head", "tail", "grep", "egrep", "fgrep", "rg", "ag",
	"wc", "tree", "file", "stat", "pwd", "echo", "printf", "env", "printenv", "du", "df",
	"ps", "top", "htop", "which", "type", "whereis", "date", "uname", "hostname", "whoami",
	"id", "uptime", "jq", "yq", "xq", "sort", "uniq", "cut", "tr", "column", "fold", "nl",
	"basename", "dirname", "realpath", "readlink", "less", "more", "diff", "cmp", "comm",
	// hash/encode inspectors — BOTH spellings: GNU (md5sum/sha256sum) and the BSD/macOS
	// tools (md5/shasum), which were falling through to unknown→Medium and prompting.
	"md5sum", "sha1sum", "sha256sum", "sha384sum", "sha512sum", "b2sum",
	"md5", "shasum", "cksum", "sum",
	"base64", "xxd", "od", "hexdump", "strings",
)

// readNet: read-only NETWORK diagnostics — they look things up (DNS records, whois
// registries) and mutate nothing. Same papercut class as the BSD hash tools: `dig`
// fell through to unknown→Medium and prompted on every DNS check. Query-name exfil
// is not the threat model here — any permitted GET has the same channel; the gate
// classifies by impact, and these have none. Deliberately EXCLUDES raw-socket /
// transfer tools (nc, socat, ssh, scp) and curl/wget (direction-classified in
// classifyNet).
var readNet = set("dig", "nslookup", "host", "whois")

// safeBuiltins: shell builtins that neither read sensitive data nor mutate the
// filesystem/remote state — directory navigation and no-ops. They appear constantly
// inside otherwise read-only compounds (`cd x && grep …`); leaving them unrecognized
// made the WHOLE compound classify Medium and prompt — the "why is it asking about a
// pile of reads" papercut. cd only moves the shell's cwd; it can't write or destroy.
var safeBuiltins = set("cd", "pushd", "popd", "dirs", "true", "false", ":")

// safeUtils are pure, side-effect-free LEAF utilities — they wait, compute, test a condition,
// or poke the terminal, and never read secrets, write files, hit the network, or mutate state.
// They were falling through to the unknown→Medium default and prompting needlessly (e.g. a
// `sleep 5 && curl …` health-check kept asking about "sleep"). A `>` redirect is still caught on
// the Stmt, so `seq 1 5 > f` stays a write. NOT for wrappers (timeout/xargs/nice/…) — those run
// another command and are unwrapped by innerCommand so their INNER risk is what counts.
var safeUtils = set(
	"sleep", "seq", "test", "[", "expr", "bc", "factor", "yes",
	"clear", "tput", "tty", "cal", "ncal", "nproc", "arch", "getconf", "locale",
)

// wrapperValueFlags lists, per run-the-rest wrapper, the flags that take a SEPARATE value arg
// — so innerCommand skips both the flag and its value and lands on the real command. Missing a
// value-flag would leave its value masquerading as the command head (hiding e.g. an `rm -rf`);
// these cover the forms agents actually use. Unlisted/exotic flags fall back to Medium (prompt),
// the safe direction. Boolean flags need no entry; `--flag=v` / `-fv` carry the value in-token.
var wrapperValueFlags = map[string]map[string]bool{
	"sudo":    set("-u", "--user", "-g", "--group", "-p", "--prompt", "-C", "--close-from", "-r", "--role", "-t", "--type"),
	"timeout": set("-s", "--signal", "-k", "--kill-after"),
	"nice":    set("-n", "--adjustment"),
	"ionice":  set("-c", "--class", "-n", "--classdata", "-p", "--pid"),
	"time":    set("-o", "--output", "-f", "--format"),
	"watch":   set("-n", "--interval"),
	"xargs":   set("-I", "--replace", "-i", "-n", "--max-args", "-P", "--max-procs", "-d", "--delimiter", "-s", "--max-chars", "-a", "--arg-file", "-E", "-e", "--eof", "-L", "--max-lines"),
	// setsid takes only boolean flags (-c/-f/-w); doas/stdbuf take a few separate values.
	// These are "run-the-rest" wrappers just like sudo — see innerCommand. Without unwrapping,
	// `setsid rm -rf /` / `doas rm -rf /` / `stdbuf -o0 rm -rf x` hid the inner op behind an
	// unrecognized head and classified Medium (auto-runs in yolo).
	"setsid": set(),
	"doas":   set("-u", "-C", "-a"),
	"stdbuf": set("-i", "--input", "-o", "--output", "-e", "--error"),
}

// parallelValueFlags / flockValueFlags: the leading options of GNU parallel and util-linux
// flock that take a SEPARATE value, so innerCommand skips past them to the real command.
var parallelValueFlags = set("-j", "--jobs", "-N", "--colsep", "-d", "--delimiter", "-I",
	"--results", "-S", "--sshlogin", "--slf", "-a", "--arg-file", "-L", "--max-lines",
	"--tmpdir", "--joblog", "--retries", "-P")
var flockValueFlags = set("-w", "--timeout", "-E", "--conflict-exit-code")

// sshValueFlags are ssh options that take a separate value, so the host (and then the remote
// command) is found correctly — otherwise `ssh -p 22 host rm -rf x` would mistake the value or
// host for the command and miss the destructive remote op.
var sshValueFlags = set("-p", "-i", "-o", "-l", "-F", "-L", "-R", "-D", "-E", "-b", "-c",
	"-m", "-O", "-Q", "-S", "-W", "-J", "-w")

// isBareShellStmt reports whether a statement runs a shell interpreter that reads its program
// from STDIN — `bash`/`sh`/`zsh`/… with no `-c` and no script-file operand. Such a stage on the
// receiving end of a pipe executes whatever was piped in.
func isBareShellStmt(st *syntax.Stmt) bool {
	if st == nil {
		return false
	}
	ce, ok := st.Cmd.(*syntax.CallExpr)
	if !ok || len(ce.Args) == 0 {
		return false
	}
	argv := wordsToArgv(ce.Args)
	head := argv[0]
	if i := strings.LastIndexByte(head, '/'); i >= 0 {
		head = head[i+1:]
	}
	switch head {
	case "bash", "sh", "zsh", "dash", "ksh":
	default:
		return false
	}
	// First pass: -c means an explicit inline program (classified on its own, not stdin);
	// -s (incl. bundled like -es) forces reading the program from STDIN, so trailing
	// operands are positional params ($0…), NOT a script file.
	sFlag := false
	for _, a := range argv[1:] {
		if a == "-c" {
			return false
		}
		if strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && strings.ContainsRune(a, 's') {
			sFlag = true
		}
	}
	for _, a := range argv[1:] {
		if a == "--" {
			continue
		}
		if !strings.HasPrefix(a, "-") {
			// A non-flag operand: with -s it's a positional param (shell still reads
			// stdin — so `curl … | sh -s -- foo` IS a pipe-into-shell); without -s it's
			// a script-file operand (not reading stdin).
			if sFlag {
				return true
			}
			return false
		}
	}
	return true
}

// classifyFindExec classifies find/fd, which are reads UNLESS they delete (-delete) or run a
// command (-exec/-execdir/-ok[dir] for find; -x/--exec/-X/--exec-batch for fd). The shell AST
// can't see the spawned command (it's argv to find), so we extract and classify it ourselves;
// the placeholder {} is harmless to the inner classifier (`rm -rf {}` → catastrophic).
func classifyFindExec(args []string) (Risk, bool) {
	if hasArg(args, "-delete") {
		return Dangerous, true // irreversible bulk delete
	}
	for i, a := range args {
		switch a {
		case "-exec", "-execdir", "-ok", "-okdir", "-x", "--exec", "-X", "--exec-batch":
			var cmd []string
			for _, t := range args[i+1:] {
				if t == ";" || t == "+" || t == "\\;" {
					break
				}
				cmd = append(cmd, t)
			}
			if len(cmd) == 0 {
				return Dangerous, false // can't see the command → conservative
			}
			return classifyCommand(joinArgv(cmd), 1)
		}
	}
	return Safe, false // plain find/fd is a read
}

// sedInPlace reports whether a sed invocation edits files in place (a write). Covers the long
// form (--in-place[=suffix]) and short bundles containing 'i' (-i, -i.bak, -i”, -ni) — 'i' in a
// sed short flag is always in-place, so matching it is precise; erring toward Medium is safe.
func sedInPlace(args []string) bool {
	for _, a := range args {
		if a == "--in-place" || strings.HasPrefix(a, "--in-place=") {
			return true
		}
		if strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && strings.ContainsRune(a[1:], 'i') {
			return true
		}
	}
	return false
}

// isDurationToken reports a bare number or a number with a time-unit suffix (30, 30s, 1.5m, 2h)
// — the leading positional argument `timeout` takes before the command to run. An all-letters
// token like "dd" trims to empty and is NOT a duration (so a real command isn't skipped).
func isDurationToken(s string) bool {
	n := strings.TrimRight(s, "smhd")
	if n == "" {
		return false
	}
	_, err := strconv.ParseFloat(n, 64)
	return err == nil
}

// netHeads fetch over the network — risk depends on DIRECTION: a plain GET reads,
// but uploading/POSTing data is exfiltration (sending data out is a trust-boundary
// crossing, per the impact-based model — not a syntactically "read" tool).
var netHeads = set("curl", "wget")

// cloudCLIs whose risk depends on the subcommand verb, not the head.
var cloudCLIs = set(
	"gcloud", "gsutil", "bq", "aws", "az", "kubectl", "k", "oc", "gh", "glab", "vercel",
	"supabase", "flyctl", "fly", "doctl", "heroku", "helm", "terraform", "tofu", "pulumi",
	"docker", "podman", "nomad", "consul", "stripe",
)

// cloud read verbs (exact tokens). AWS-style hyphenated verbs (describe-*, list-*,
// get-*) are matched by prefix in cloudVerb.
var cloudReadVerbs = set(
	"list", "describe", "get", "ls", "show", "read", "info", "status", "logs", "log",
	"version", "versions", "cat", "top", "history", "current-context", "whoami",
	"get-caller-identity", "inspect", "api-resources", "cluster-info", "plan", "preview",
	"explain", "view", "search", "ps", "images", "stats", "diff", "port", "config",
	"contexts", "projects", "accounts", "whatif", "validate", "test", "check", "ping",
)

// cloud write/destructive verbs.
var cloudWriteVerbs = set(
	"create", "delete", "destroy", "update", "set", "deploy", "apply", "patch", "scale",
	"add", "remove", "rm", "rmi", "rb", "restart", "stop", "start", "put", "run", "push", "pull",
	"exec", "ssh", "login", "logout", "auth", "sync", "init", "import", "edit", "rollout",
	"drain", "cordon", "uncordon", "label", "annotate", "cp", "mv", "attach", "port-forward",
	"taint", "enable", "disable", "build", "commit", "tag", "kill", "terminate", "reboot",
	"set-iam-policy", "add-iam-policy-binding", "remove-iam-policy-binding",
)

// cloud verbs that destroy shared/remote state — irreversible, so they confirm even in
// allow-all/yolo (catastrophic): delete a db/bucket/app/namespace, terminate an instance,
// uninstall a release (helm), destroy a stack (terraform/pulumi), reset a db (supabase).
// "rb" = remove-bucket (aws s3 rb, gsutil rb) — deletes a cloud bucket, as irreversible
// as any delete, so it's catastrophic (confirms even in allow-all/yolo).
var cloudCatastrophicVerbs = set("delete", "destroy", "terminate", "rm", "rmi", "rb", "uninstall", "reset")

// localRuntimes manage DISPOSABLE local resources — their rm/rmi is Dangerous, not catastrophic
// (deleting a local container/image is recoverable; deleting cloud infra is not).
var localRuntimes = set("docker", "podman", "nerdctl")

// sqlClients run SQL; risk comes from the SQL verb, not the client.
var sqlClients = set("psql", "mysql", "mariadb", "sqlite3", "mongo", "mongosh",
	"redis-cli", "cockroach", "clickhouse-client", "duckdb", "pgcli", "mycli")

// destructive single-purpose heads.
var dangerHeads = set("rm", "rmdir", "mv", "dd", "mkfs", "shutdown", "reboot", "halt",
	"poweroff", "kill", "pkill", "killall", "chown", "chmod", "chgrp", "ln", "truncate",
	"shred", "wipefs", "fdisk", "parted", "systemctl", "service", "launchctl", "crontab",
	"iptables", "ufw", "mount", "umount", "scp", "rsync", "ssh", "tee", "patch")

// catastrophicHeads are destructive heads that must always confirm.
var catastrophicHeads = set("mkfs", "dd", "shred", "wipefs", "fdisk", "shutdown",
	"reboot", "halt", "poweroff")

// routine write/build heads (Medium).
var writeHeads = set("npm", "pnpm", "yarn", "bun", "deno", "go", "cargo", "pip", "pip3",
	"make", "cmake", "gradle", "mvn", "python", "python3", "node", "ruby", "rake", "bundle",
	"composer", "dotnet", "sbt", "git")

// pkgReadSubs are UNAMBIGUOUS package-manager read subcommands. The read-or-write-depending ones
// (env, config, audit, version, fmt) are NOT here — classifyPkg decides those by their operand/
// flags, so `go env GOPATH` stays Safe while `go env -w …` is a write.
var pkgReadSubs = set("ls", "list", "view", "show", "outdated", "why", "info", "search",
	"freeze", "--version", "doc", "vet")

// gitRead subcommands → Safe (writes fall through to Medium; destructive ops are caught
// structurally in classifyGit).
var gitReadSubs = set("log", "show", "diff", "status", "branch", "remote", "rev-parse",
	"describe", "blame", "ls-files", "ls-tree", "cat-file", "reflog", "shortlog",
	"whatchanged", "for-each-ref", "merge-base", "name-rev", "tag", "config", "grep",
	"rev-list", "show-ref", "symbolic-ref", "var", "count-objects")

func set(items ...string) map[string]bool {
	m := make(map[string]bool, len(items))
	for _, i := range items {
		m[i] = true
	}
	return m
}

// ClassifyBash returns the risk of a shell command and whether it is catastrophic
// (irreversible / never auto-approved). It classifies by INTENT — subcommand verbs
// for cloud CLIs, the SQL verb for DB clients, and recursively for wrappers
// (sudo/ssh/bash -c/docker exec) — so `gcloud … list` and `psql -c "SELECT"` read as
// Safe while `gcloud … delete` and `psql -c "DELETE"` read as Dangerous. Anything not
// positively recognized as read-only lands at Medium or higher so it prompts.
//
// The command is parsed with a real shell parser (mvdan.cc/sh) and classified by
// walking the AST, so quoting, pipelines, redirects, command substitutions, and
// subshells are understood structurally rather than guessed at with string surgery.
func ClassifyBash(command string) (Risk, bool) {
	return classifyCommand(command, 0)
}

// RiskHead returns the basename of the simple command that DROVE the command's
// overall risk — the sub-command that actually triggered the approval prompt. For a
// compound like `echo … && supabase status`, every safe head (echo/cd/cat/ls) is
// noise; the prompt exists because of `supabase`, so the approval card and any
// remembered "don't ask again" rule must key on "supabase", not the leading "echo".
//
// It mirrors classifyFile's walk but tracks the head of the highest-risk CallExpr
// (first one wins on ties, in source order). Falls back to CommandHead — the leading
// binary — when nothing escalates above Safe or the command can't be parsed, so an
// all-safe command still keys on its own head.
func RiskHead(command string) string {
	f, err := parse(command)
	if err != nil {
		return CommandHead(command)
	}
	if head, _, ok := riskAnchor(f); ok {
		return head
	}
	return CommandHead(command)
}

// riskAnchor finds the highest-risk SIMPLE COMMAND in the parsed command and returns its head
// basename plus the source offset of its first argument. The risk of each command combines the
// SAME two sources classifyFile sums: the command's argv risk AND a file-write redirect on its
// own statement (`cat > f`, `… >> f`). Walking CallExprs alone (as this used to) misses the
// redirect — which lives on the *Stmt* — so a `cd && cat > f` clobber keyed on the harmless
// leading `cd`. Anchoring on the same inputs as the gate keeps head/segment from ever drifting
// from the gate's risk level again. first-on-ties, in source order; found=false when empty.
func riskAnchor(f *syntax.File) (head string, off int, found bool) {
	best := Safe
	bestCat := false
	off = -1
	syntax.Walk(f, func(n syntax.Node) bool {
		st, ok := n.(*syntax.Stmt)
		if !ok {
			return true
		}
		ce, ok := st.Cmd.(*syntax.CallExpr)
		if !ok || len(ce.Args) == 0 {
			return true
		}
		argv := wordsToArgv(ce.Args)
		r, cat := classifyArgv(argv, 0)
		for _, rd := range st.Redirs {
			if isFileWriteRedir(rd) && r < Medium {
				r = Medium // a `>`/`>>` to a real file is a write, however safe the command itself is
			}
		}
		// Rank by (risk, catastrophic): a catastrophic command outranks a same-risk
		// non-catastrophic one, so `curl -X POST … && rm -rf x` anchors on `rm`, not `curl`.
		if !found || r > best || (r == best && cat && !bestCat) {
			best, bestCat, found = r, cat, true
			h := argv[0]
			if i := strings.LastIndexByte(h, '/'); i >= 0 {
				h = h[i+1:]
			}
			head, off = h, int(ce.Args[0].Pos().Offset())
		}
		return true
	})
	return head, off, found
}

// RiskSegment returns the command text anchored at its highest-risk simple command
// — the SAME call RiskHead keys a remembered rule on. It mirrors RiskHead's walk
// (first-on-ties, highest-risk wins) but returns the source from that call's first
// arg onward, so a rule saved from a compound (`echo … && supabase …` → "supabase *")
// can match the command that produced it. Anchoring at the *highest*-risk call is the
// safety invariant: the pattern must cover the riskiest sub-command (anything earlier
// is lower-risk; anything later is swallowed by the rule's trailing '*'), and Match's
// catastrophic guard still forces a re-prompt regardless. Falls back to the whole
// command when it can't be parsed or has no simple command.
func RiskSegment(command string) string {
	command = strings.TrimSpace(command)
	f, err := parse(command)
	if err != nil {
		return command
	}
	if _, off, ok := riskAnchor(f); ok && off >= 0 && off <= len(command) {
		return strings.TrimSpace(command[off:])
	}
	return command
}

// RecoverableInRepo reports whether command's ENTIRE destructive footprint is file deletions
// (rm/rmdir) whose every target resolves to a path strictly UNDER root — i.e. the blast radius is
// inside the repo, where git can restore it. When true, the gate may treat the command like an
// edit (Medium) instead of an always-confirm catastrophe. It is deliberately CONSERVATIVE: ANY
// destructive call that isn't an in-repo rm — a disk-level destroyer (dd/mkfs/shred), a remote op
// (git push --force), a cloud delete, an mv, or an rm whose target is out-of-repo, the repo root
// itself, its .git, or unresolvable (a glob/variable) — returns false, keeping the catastrophic
// floor. Pure: it reasons about parsed argv and path containment only — no filesystem or git IO
// (the caller decides separately whether root is actually a git repo). cwd is where relative
// targets resolve; pass root when it's unset.
func RecoverableInRepo(command, cwd, root string) bool {
	if strings.TrimSpace(cwd) == "" {
		cwd = root
	}
	f, err := parse(command)
	if err != nil {
		return false // can't prove it — keep the floor
	}
	sawDestructive, recoverable := false, true
	syntax.Walk(f, func(n syntax.Node) bool {
		if !recoverable {
			return false
		}
		ce, ok := n.(*syntax.CallExpr)
		if !ok || len(ce.Args) == 0 {
			return true
		}
		argv := wordsToArgv(ce.Args)
		if r, cat := classifyArgv(argv, 0); r < Dangerous && !cat {
			return true // not a destructive call — irrelevant to recoverability
		}
		sawDestructive = true
		head := argv[0]
		if i := strings.LastIndexByte(head, '/'); i >= 0 {
			head = head[i+1:]
		}
		if head != "rm" && head != "rmdir" {
			recoverable = false // a non-rm destroyer (dd / git push --force / cloud delete / …)
			return false
		}
		for _, a := range argv[1:] {
			if strings.HasPrefix(a, "-") {
				continue // a flag, not a target
			}
			if !targetUnderRoot(cwd, root, a) {
				recoverable = false
				return false
			}
		}
		return true
	})
	return sawDestructive && recoverable
}

// targetUnderRoot reports whether an rm target resolves strictly inside root. Statically
// unresolvable targets (globs, variables, command substitutions, ~) → false (can't prove safe).
// The repo root itself and its .git → false (deleting them isn't git-recoverable).
func targetUnderRoot(cwd, root, target string) bool {
	if target == "" || strings.HasPrefix(target, "~") ||
		strings.ContainsAny(target, "*?[") || strings.Contains(target, "_VAR_") || strings.Contains(target, "_SUB_") {
		return false
	}
	p := target
	if !filepath.IsAbs(p) {
		p = filepath.Join(cwd, p)
	}
	p = filepath.Clean(p)
	root = filepath.Clean(root)
	// Resolve symlinks so a symlinked PATH COMPONENT (repo/link/x where link → outside)
	// can't lexically masquerade as in-repo — the under-root check was purely textual, so
	// a target that physically escapes the repo was wrongly judged git-recoverable. Resolve
	// the parent (deleting a leaf symlink itself is safe, so don't resolve the final
	// component) — and only then resolve root, so BOTH sides are real or BOTH stay lexical.
	// Resolving only one side would mismatch when the target's dir doesn't exist yet (e.g.
	// on macOS where the temp root's /var is itself a symlink to /private/var).
	if realDir, err := filepath.EvalSymlinks(filepath.Dir(p)); err == nil {
		p = filepath.Clean(filepath.Join(realDir, filepath.Base(p)))
		if realRoot, err := filepath.EvalSymlinks(root); err == nil {
			root = realRoot
		}
	}
	if p == root || p == filepath.Join(root, ".git") {
		return false
	}
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// parse turns a command line into a shell AST (Bash dialect).
func parse(command string) (*syntax.File, error) {
	return syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(command), "")
}

// IsSequence reports whether command is a multi-statement SEQUENCE — statements separated by
// ;, &, or newlines — rather than a single command or pipeline (a | b and a && b are each ONE
// statement). Parsed with the real shell AST, never by splitting on ";". Used to tell a chained
// command whose LAST step returned non-zero (a partial success — e.g. a trailing grep that found
// nothing) apart from a single command that outright failed.
func IsSequence(command string) bool {
	f, err := parse(command)
	if err != nil {
		return false
	}
	return len(f.Stmts) > 1
}

func classifyCommand(command string, depth int) (Risk, bool) {
	// The fork bomb `:(){ :|:& };:` is a recursive shell FUNCTION definition — it has no
	// simple-command head to classify structurally, so it's the ONE pattern matched on raw
	// text. Everything else (git history rewrites, publishes, rm -rf, …) is classified
	// structurally off the parsed AST below.
	if strings.Contains(strings.ToLower(command), ":(){") {
		return Dangerous, true
	}
	f, err := parse(command)
	if err != nil {
		// Unparseable → we can't prove it read-only, so PROMPT (never auto-run). Not
		// catastrophic: in allow-all the user has opted into running unknown commands.
		return Dangerous, false
	}
	return classifyFile(f, depth)
}

// scriptInterpreters run a program read from STDIN when piped into with no inline-eval flag:
// `curl … | python`, `… | node`, `… | ruby|perl|php`. Same run-arbitrary hazard as piping into
// a bare shell (isBareShellStmt), which only covered sh/bash/zsh/dash/ksh.
var scriptInterpreters = set("python", "python2", "python3", "pypy", "pypy3",
	"node", "nodejs", "ruby", "perl", "php")

// isPipedInterpreterStmt reports whether st is a scripting interpreter that would execute
// whatever is piped into it. It errs toward true: only an explicit inline-eval flag
// (-c/-e/-E/-r/-p/--eval/--print) proves the program comes from the flag rather than stdin.
// Over-flagging `… | python script.py` merely prompts; under-flagging silently runs downloaded
// code — so in the pipe context we accept the occasional false prompt to close the hole.
func isPipedInterpreterStmt(st *syntax.Stmt) bool {
	if st == nil {
		return false
	}
	ce, ok := st.Cmd.(*syntax.CallExpr)
	if !ok || len(ce.Args) == 0 {
		return false
	}
	argv := wordsToArgv(ce.Args)
	head := argv[0]
	if i := strings.LastIndexByte(head, '/'); i >= 0 {
		head = head[i+1:]
	}
	if !scriptInterpreters[head] {
		return false
	}
	for _, a := range argv[1:] {
		switch a {
		case "-c", "-e", "-E", "-r", "-p", "--eval", "--print":
			return false // program is inline, stdin is data
		}
	}
	return true
}

// classifyFile walks every simple command in the AST and takes the highest risk.
// The walk visits commands at every nesting level — inside pipelines, lists,
// subshells, command substitutions, and process substitutions — so nothing hides.
// Redirects are inspected on each statement (a `>` to a real file is a write).
func classifyFile(f *syntax.File, depth int) (Risk, bool) {
	risk, cat := Safe, false
	bump := func(r Risk, c bool) {
		if r > risk {
			risk = r
		}
		cat = cat || c
	}
	syntax.Walk(f, func(n syntax.Node) bool {
		switch x := n.(type) {
		case *syntax.BinaryCmd:
			// Piping INTO a bare shell interpreter executes whatever the upstream produced —
			// `curl … | bash`, `cat script | sh` — i.e. run-arbitrary. Medium would auto-run in
			// auto mode, so flag it Dangerous (not catastrophic: not inherently irreversible).
			if x.Op == syntax.Pipe && (isBareShellStmt(x.Y) || isPipedInterpreterStmt(x.Y)) {
				bump(Dangerous, false)
			}
		case *syntax.Stmt:
			for _, rd := range x.Redirs {
				if isFileWriteRedir(rd) {
					bump(Medium, false)
				}
			}
		case *syntax.CallExpr:
			if len(x.Args) == 0 {
				return true // pure assignment; its substitutions are walked as children
			}
			r, c := classifyArgv(wordsToArgv(x.Args), depth)
			bump(r, c)
		}
		return true
	})
	return risk, cat
}

// classifyArgv classifies one simple command given its already-split, unquoted argv.
func classifyArgv(argv []string, depth int) (Risk, bool) {
	if len(argv) == 0 {
		return Safe, false
	}
	head := argv[0]
	if i := strings.LastIndexByte(head, '/'); i >= 0 {
		head = head[i+1:]
	}
	// De-obfuscate the command name. A backslash-escaped name (`\rm`) runs the real
	// command while dodging an alias AND our head match; strip backslashes so `\rm`
	// classifies as `rm`. A command NAME that contains a variable/command expansion
	// (`rm$IFS-rf` → head "rm_VAR_-rf", `$(echo rm)` → "_SUB_") can't be statically
	// resolved and is a known evasion — refuse to auto-run it (Dangerous, prompts).
	head = strings.ReplaceAll(head, "\\", "")
	if strings.Contains(head, "_VAR_") || strings.Contains(head, "_SUB_") {
		return Dangerous, false
	}
	args := argv[1:]

	// Wrappers that run another command (sudo/bash -c/docker exec/…): classify the
	// INNER command instead of the wrapper.
	if depth < 4 {
		if inner, ok := innerCommand(head, args, argv); ok {
			if strings.TrimSpace(inner) == "" {
				return Medium, false // can't see the real command → prompt
			}
			return classifyCommand(inner, depth+1)
		}
	}

	switch {
	case safeBuiltins[head]:
		return Safe, false // navigation / no-op builtin — no read of secrets, no mutation
	case safeUtils[head]:
		return Safe, false // pure utility (sleep/seq/test/expr/…) — no side effects; a `>` is still caught on the Stmt
	case head == "find" || head == "fd":
		return classifyFindExec(args)
	case readLocal[head]:
		return Safe, false // a `>` redirect is a write, but that's caught on the Stmt
	case readNet[head]:
		return Safe, false // read-only network lookup (dig/nslookup/host/whois) — inspects, never mutates
	case netHeads[head]:
		return classifyNet(args)
	case head == "git":
		return classifyGit(args)
	case head == "terraform" || head == "tofu":
		// `apply -destroy` tears down infra just like `destroy` — the verb scan sees `apply`
		// (Dangerous) and skips the `-destroy` flag, so catch it before falling through.
		if hasArg(args, "apply") && hasAnyFlag(args, "-destroy", "--destroy") {
			return Dangerous, true
		}
		return classifyCloud(head, args)
	case cloudCLIs[head]:
		return classifyCloud(head, args)
	case sqlClients[head]:
		return classifySQL(argv)
	case writeHeads[head]:
		return classifyPkg(head, args)
	case dangerHeads[head]:
		if head == "rm" && rmRecursiveForce(args) {
			return Dangerous, true // `rm -rf` is irreversible → always confirm
		}
		return Dangerous, catastrophicHeads[head]
	default:
		// `sed -i` rewrites in place; plain sed reads. In-place has many spellings —
		// `-i`, `-i.bak`, `-i''`, `--in-place`, `--in-place=.bak`, and combined bundles
		// like `-ni` — all caught by sedInPlace; anything else reads.
		if head == "sed" {
			if sedInPlace(args) {
				return Medium, false
			}
			return Safe, false
		}
		// Unknown command → Medium. NOTE: Medium prompts in ask mode and in research/plan
		// contexts, but AUTO mode auto-runs Medium (see Decide) — so an unknown binary auto-runs
		// under auto. That's a deliberate policy choice (auto = "run routine writes"), not an
		// oversight; raising unknown→Dangerous would make auto prompt for every unrecognized tool.
		return Medium, false
	}
}

// gitGlobalValueFlags are git GLOBAL options that take a SEPARATE value arg (git -C <path> …,
// git -c <name>=<value> …, --git-dir <path>). The subcommand scan must skip BOTH the flag and
// its value — else the value (a repo path, a config pair) is mistaken for the subcommand and a
// `push --force` hidden behind `-C repo` slips past the catastrophic check.
var gitGlobalValueFlags = set("-C", "-c", "--git-dir", "--work-tree", "--namespace")

// gitSubcommand returns the git subcommand (push/reset/clean/checkout/…), skipping leading
// global flags AND their values. One-token `--git-dir=…` forms start with '-' and are skipped
// generically; the separate-value forms above also consume the following arg. (firstNonFlag
// can't be used here — it would return the value of `-C`/`-c` as the "subcommand".)
func gitSubcommand(args []string) string {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if gitGlobalValueFlags[a] {
			i++ // skip the flag's value too
			continue
		}
		if strings.HasPrefix(a, "-") {
			continue // boolean global (-p / --bare / --no-pager) or a `--flag=value` one-token form
		}
		return strings.ToLower(a)
	}
	return ""
}

// classifyGit: read subcommands are Safe; config/tag only when reading; the rest
// (commit/add/merge/…) are Medium. The catastrophic ops (force-push, reset --hard,
// clean -f, checkout --) are detected structurally from the parsed subcommand + flags.
func classifyGit(args []string) (Risk, bool) {
	sub := gitSubcommand(args)
	if sub == "" {
		return Safe, false // bare `git` / `git --version`
	}
	// Catastrophic ops — irreversible history/worktree destruction, local or remote — confirm
	// even in allow-all/yolo. All decided from the parsed subcommand + flags, never raw text.
	switch sub {
	case "push":
		if gitPushIsForce(args) {
			return Dangerous, true
		}
		return Medium, false
	case "reset":
		if hasFlag(args, "--hard") {
			return Dangerous, true // discards committed + working changes irreversibly
		}
		return Medium, false
	case "clean":
		if hasForceFlag(args) {
			return Dangerous, true // -f/-fd/-xf/--force deletes untracked files irreversibly
		}
		return Medium, false
	case "checkout":
		if hasArg(args, "--") || hasAnyFlag(args, "-f", "--force") {
			return Dangerous, true // discards working-tree changes irreversibly
		}
		return Medium, false // branch switch / -b: a worktree write
	case "restore":
		// `git restore <path>` overwrites the worktree (irreversible local loss); `--staged`
		// WITHOUT `--worktree` only unstages → Medium.
		if hasAnyFlag(args, "--staged", "--cached") && !hasAnyFlag(args, "--worktree", "-W") {
			return Medium, false
		}
		return Dangerous, true
	case "branch":
		// `-d/-D/--delete/-m/-M/--move` mutate; bare/list flags read; a NAME operand creates.
		if hasAnyFlag(args, "-d", "-D", "--delete", "-m", "-M", "--move") {
			return Medium, false
		}
		if gitListsOnly(args, "branch") {
			return Safe, false
		}
		return Medium, false // create a branch
	case "tag":
		if hasAnyFlag(args, "-d", "--delete") {
			return Medium, false
		}
		if gitListsOnly(args, "tag") {
			return Safe, false
		}
		return Medium, false // create a tag
	case "remote":
		switch subVerbAfter(args, "remote") {
		case "", "show", "get-url":
			return Safe, false
		}
		return Medium, false // add / remove / rm / set-url / rename / prune / update / set-head
	case "stash":
		switch subVerbAfter(args, "stash") {
		case "", "list", "show":
			return Safe, false
		case "drop", "clear":
			return Dangerous, false // discards stashed work
		}
		return Medium, false // push / pop / apply / save / branch
	case "config":
		// Reads: --get*/--list, or a single bare key with no value. Writes: a value, or any
		// --unset/--add/--replace/--rename/--remove (so `git config --unset k` stays Medium).
		if hasAnyFlag(args, "--get", "--list", "-l", "--get-all", "--get-regexp") {
			return Safe, false
		}
		return Medium, false
	}
	if !gitReadSubs[sub] {
		return Medium, false
	}
	return Safe, false
}

// gitPushIsForce reports a push that rewrites or deletes REMOTE shared state — by flag
// (--force/-f, --force-with-lease[=…], --force-if-includes, --mirror, --delete/-d) OR by
// refspec (`+src:dst` forces, `:dst` deletes the remote ref). Both spellings are catastrophic.
func gitPushIsForce(args []string) bool {
	if hasAnyFlag(args, "--force", "-f", "--mirror", "--delete", "-d") {
		return true
	}
	for _, a := range args {
		if strings.HasPrefix(a, "--force-with-lease") || strings.HasPrefix(a, "--force-if-includes") {
			return true
		}
		if !strings.HasPrefix(a, "-") && (strings.HasPrefix(a, "+") || strings.HasPrefix(a, ":")) {
			return true // forced (+) or deletion (:) refspec
		}
	}
	return false
}

// gitListsOnly reports a branch/tag invocation that only lists (no NAME operand to create),
// treating an explicit list flag (-l/--list) as a read even when it carries a pattern.
func gitListsOnly(args []string, sub string) bool {
	if hasAnyFlag(args, "-l", "--list") {
		return true
	}
	for _, a := range args {
		if a == sub || strings.HasPrefix(a, "-") {
			continue
		}
		if a == "-l" || a == "--list" {
			continue
		}
		return false // a bare operand → create
	}
	return true
}

// classifyPkg classifies a package-manager/build head (go/npm/pip/cargo/…). Most subcommands are
// routine writes (Medium); `publish` ships an irreversible release (catastrophic); and the
// read-or-write-depending subcommands are decided by their operand/flags so a genuine read
// (`go env GOPATH`, `npm config get`, `npm audit`) stays Safe while its writing sibling
// (`go env -w`, `npm config set`, `npm audit fix`, `go fmt`, `npm version patch`) is Medium.
func classifyPkg(head string, args []string) (Risk, bool) {
	switch firstNonFlag(args) {
	case "env":
		if hasAnyFlag(args, "-w", "-u") {
			return Medium, false // go env -w/-u writes the env config
		}
		return Safe, false
	case "config":
		switch subVerbAfter(args, "config") {
		case "", "get", "list", "ls", "edit-noop": // edit-noop never matches; get/list read
			return Safe, false
		}
		return Medium, false // set / delete / unset / rm / edit
	case "audit":
		if subVerbAfter(args, "audit") == "fix" {
			return Medium, false
		}
		return Safe, false
	case "version":
		if subVerbAfter(args, "version") == "" {
			return Safe, false // `npm version` / `go version` just prints
		}
		return Medium, false // `npm version patch|major|<v>` bumps + tags
	case "fmt":
		return Medium, false // `go fmt` rewrites files
	case "publish":
		return Dangerous, true // irreversible public release
	}
	if hasReadSub(args, pkgReadSubs) {
		return Safe, false
	}
	return Medium, false
}

// subVerbAfter returns the first non-flag token after the given subcommand (the secondary verb
// for `git remote <verb>`, `npm config <verb>`, …), or "" when there is none.
func subVerbAfter(args []string, sub string) string {
	seen := false
	for _, a := range args {
		if !seen {
			if a == sub {
				seen = true
			}
			continue
		}
		if strings.HasPrefix(a, "-") {
			continue
		}
		return strings.ToLower(a)
	}
	return ""
}

// classifyNet classifies curl/wget by DIRECTION: uploading/POSTing data out is
// exfiltration → Dangerous; a plain fetch (GET) → Safe. (Download-and-execute,
// `curl … | bash`, is handled by classifying every command in the pipeline — the
// bash segment isn't Safe on its own.)
func classifyNet(args []string) (Risk, bool) {
	// An explicit mutating method (parsed from -X/--request/--method in ALL their forms:
	// `-X POST`, `-XPOST`, `--request=POST`, `--method POST`) → write. GET/HEAD stay Safe, and
	// a URL that merely contains "post" no longer false-trips (we read the method, not any token).
	switch httpMethod(args) {
	case "POST", "PUT", "PATCH", "DELETE":
		return Dangerous, false
	}
	longSends := []string{
		"--data", "--data-binary", "--data-raw", "--data-ascii", "--data-urlencode", "--json",
		"--form", "--form-string", "--upload-file", // curl
		"--post-data", "--post-file", "--body-data", "--body-file", // wget
	}
	for _, a := range args {
		// Short send flags are CASE-SENSITIVE and may carry their value attached: -d/-F/-T and
		// forms like -dKEY=VAL, -Ffile, -Tfile. (-D/-f/-t are different curl options — don't
		// lowercase, or -D dump-header would be misread as -d data.)
		if len(a) >= 2 && a[0] == '-' && a[1] != '-' {
			switch a[1] {
			case 'd', 'F', 'T':
				return Dangerous, false
			}
		}
		la := strings.ToLower(a)
		for _, f := range longSends {
			if la == f || strings.HasPrefix(la, f+"=") {
				return Dangerous, false // sending a body out is exfiltration, regardless of method
			}
		}
	}
	return Safe, false
}

// httpMethod extracts the request method from curl/wget args across every spelling:
// `-X POST`, `-XPOST`, `--request POST`, `--request=POST`, `--method POST`, `--method=POST`.
// Returns the upper-cased method, or "" when none is set. It reads only the method FLAG's value,
// so a URL or header containing "post" can't masquerade as the method.
func httpMethod(args []string) string {
	for i, a := range args {
		switch {
		case a == "-X" || a == "--request" || a == "--method":
			if i+1 < len(args) {
				return strings.ToUpper(args[i+1])
			}
		case strings.HasPrefix(a, "-X") && len(a) > 2:
			return strings.ToUpper(a[2:])
		case strings.HasPrefix(a, "--request="):
			return strings.ToUpper(a[len("--request="):])
		case strings.HasPrefix(a, "--method="):
			return strings.ToUpper(a[len("--method="):])
		}
	}
	return ""
}

// classifyCloud scans the args for a read/write verb (handles both verb-first like
// `kubectl get` and noun-first like `gcloud compute instances list`, plus AWS-style
// `describe-instances`). A write verb anywhere wins; otherwise a recognized read
// verb → Safe; nothing recognized → Dangerous (don't auto-run an unknown cloud op).
func classifyCloud(head string, args []string) (Risk, bool) {
	sawRead := false
	for _, a := range args {
		if strings.HasPrefix(a, "-") {
			continue
		}
		v := strings.ToLower(a)
		// Destructive op on shared/remote state → catastrophic (confirm even in allow-all/yolo).
		// Match the bare verb, AWS hyphenated forms (terminate-instances, delete-bucket), and
		// colon forms (heroku apps:destroy / pg:reset). EXCEPT a local container runtime's rm/rmi,
		// which only disposes a local resource → Dangerous. Err catastrophic when unsure: a confirm
		// is far cheaper than deleting prod infra.
		if isDestructiveCloudVerb(v) {
			return Dangerous, !localRuntimes[head]
		}
		// A local runtime's `pull` just fills the local image cache — routine, not Dangerous
		// (unlike a registry `push`, which uploads). Cloud `pull` stays Dangerous below.
		if localRuntimes[head] && v == "pull" {
			return Medium, false
		}
		if cloudWriteVerbs[v] || hasAnyPrefix(v, "create-", "put-", "update-", "modify-", "remove-", "start-", "stop-", "reboot-", "run-", "deploy-", "attach-", "detach-", "associate-", "disassociate-", "authorize-", "revoke-", "register-", "deregister-") {
			return Dangerous, false // routine mutation: prompts in ask/auto, auto-runs in allow-all
		}
		if cloudReadVerbs[v] || hasAnyPrefix(v, "describe-", "list-", "get-", "batch-get-", "lookup-", "search-") {
			sawRead = true
		}
	}
	if sawRead {
		return Safe, false
	}
	return Dangerous, false // unrecognized cloud operation → conservative
}

// isDestructiveCloudVerb reports whether a cloud subcommand token destroys shared state —
// the bare verb (delete/destroy/terminate/uninstall/reset/rm/rmi), an AWS hyphenated form
// (delete-*, terminate-*, destroy-*), or a heroku colon form (apps:destroy, pg:reset).
func isDestructiveCloudVerb(v string) bool {
	for _, part := range strings.Split(v, ":") { // heroku apps:destroy / pg:reset
		if cloudCatastrophicVerbs[part] {
			return true
		}
	}
	return hasAnyPrefix(v, "delete-", "destroy-", "terminate-") // aws delete-bucket / terminate-instances
}

// classifySQL inspects the inline SQL (-c/-e/--command/--eval/--query). A write/DDL
// verb → Dangerous; read-only → Safe; no inline SQL (interactive session) → Medium.
func classifySQL(argv []string) (Risk, bool) {
	var sql strings.Builder
	for _, flag := range []string{"--command", "--eval", "-c", "-e", "--query"} {
		if v, ok := flagValue(argv, flag); ok && v != "" {
			sql.WriteByte(' ')
			sql.WriteString(v)
		}
	}
	s := strings.TrimSpace(sql.String())
	if s == "" {
		return Medium, false // interactive session — can't see what they'll run
	}
	// SQL is a DIFFERENT language and we do NOT parse it. Two rules keep this honest without a
	// real SQL parser: (1) a write/DDL verb token only RAISES risk (Dangerous; drop/truncate
	// catastrophic) — it fires even when the verb hides inside a SELECT/CTE/string literal, which
	// is the SAFE direction; (2) we clear to Safe ONLY when a read verb is positively recognized
	// and NO write verb appears — an unrecognized query floors at Medium, never auto-Safe. (A
	// bare SELECT calling a side-effecting function, e.g. pg_read_file, would need a real dialect
	// parser — pg_query_go / Vitess — to catch; the shell AST already isolates the query here.)
	risk, hasRead := Medium, false
	for _, tok := range sqlTokens(s) {
		switch {
		case sqlCatastrophicVerbs[tok]:
			return Dangerous, true
		case sqlWriteVerbs[tok]:
			risk = Dangerous
		case sqlReadVerbs[tok]:
			hasRead = true
		}
	}
	if risk == Dangerous {
		return Dangerous, false // a write verb appeared — raise wins over any read token
	}
	if hasRead {
		return Safe, false // positively recognized read, no write verb seen
	}
	return Medium, false // unrecognized → floor; never auto-clear unknown SQL to Safe
}

// sqlTokens splits a SQL string into lowercased word tokens ([a-z0-9_] runs) — so "insert"
// matches but the column "insert_count" doesn't (mirrors a \bword\b match, no regexp).
func sqlTokens(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_')
	})
}

// SQL verb sets (token match). Write = any DDL/DML mutation; catastrophic = irreversible;
// read = the leading verbs that positively mark a query as read-only (→ Safe when no write
// verb is also present).
var (
	sqlWriteVerbs = set("insert", "update", "delete", "drop", "alter", "create", "truncate",
		"grant", "revoke", "merge", "replace", "vacuum", "reindex", "call", "do", "comment", "copy")
	sqlCatastrophicVerbs = set("drop", "truncate")
	sqlReadVerbs         = set("select", "show", "explain", "describe", "desc", "with", "values", "table", "pragma")
)

// innerCommand extracts the nested command a wrapper will run, so we classify the
// real operation rather than the wrapper. Returns ("", false) if head isn't a known
// wrapper. The returned string is a shell command (re-parsed by the caller); argv
// fragments are re-quoted on join so quoted SQL/commands survive the round-trip.
func innerCommand(head string, args []string, argv []string) (string, bool) {
	switch head {
	case "sudo", "nice", "ionice", "nohup", "time", "timeout", "watch", "xargs", "command", "setsid", "doas", "stdbuf":
		// run-the-rest wrappers: skip the wrapper's OWN flags (and any separate values they
		// take) so the INNER command is what we classify — otherwise a flag's value, or a
		// unit-suffixed duration, is mistaken for the command and a `timeout -s TERM 30 rm -rf x`
		// slips through as Medium. Skipping a known value-flag's argument is the safety-critical
		// part: a value left in place becomes a bogus command head and hides the real one.
		vf := wrapperValueFlags[head]
		rest := args
		for len(rest) > 0 && strings.HasPrefix(rest[0], "-") {
			flag := rest[0]
			rest = rest[1:]
			// `--flag=v` / `-fv` carry the value in-token; only a separate value needs skipping.
			if vf[flag] && len(rest) > 0 && !strings.Contains(flag, "=") {
				rest = rest[1:]
			}
		}
		// `timeout 30` / `timeout 30s`: skip the leading positional duration (timeout only —
		// the other wrappers take their timing via flags, handled above).
		if head == "timeout" && len(rest) > 0 && isDurationToken(rest[0]) {
			rest = rest[1:]
		}
		return joinArgv(rest), true
	case "eval":
		// eval concatenates its arguments and runs the RESULT as a command — classify
		// that. Join RAW (not joinArgv, which would re-quote the already-unquoted arg
		// back into one opaque token): `eval "rm -rf x"` → arg "rm -rf x" → the command
		// "rm -rf x"; `eval rm -rf x` → "rm -rf x". Without this the nested rm -rf hid
		// behind eval as Medium.
		inner := strings.TrimSpace(strings.Join(args, " "))
		if inner == "" {
			return "", false
		}
		return inner, true
	case "bash", "sh", "zsh", "dash", "ksh":
		if v, ok := flagValue(argv, "-c"); ok {
			return v, true
		}
		return "", false // `bash script.sh` → not inline; let it be unknown (Medium)
	case "env":
		rest := args
		for len(rest) > 0 {
			a := rest[0]
			switch {
			case a == "-u" || a == "--unset" || a == "-C" || a == "--chdir":
				rest = rest[1:]
				if len(rest) > 0 {
					rest = rest[1:] // skip the value (var name / dir) — else it masks the command
				}
			case a == "-S" || a == "--split-string":
				if len(rest) > 1 {
					return rest[1], true // the split-string IS the command to run
				}
				return "", false
			case strings.HasPrefix(a, "--split-string="):
				return a[len("--split-string="):], true
			case strings.HasPrefix(a, "-S") && len(a) > 2:
				return a[2:], true
			case strings.Contains(a, "=") || strings.HasPrefix(a, "-"):
				rest = rest[1:] // a NAME=VALUE assignment or a boolean flag
			default:
				return joinArgv(rest), true // the remainder is the command
			}
		}
		return "", false // `env` (with only assignments/flags) runs nothing → read (falls to readLocal)
	case "ssh":
		// Skip ssh's own options (several take a value), then the host, then classify the remote
		// command — both `ssh host "rm -rf x"` (one quoted token) and `ssh host rm -rf x`
		// (separate tokens). No remote command (interactive shell) → not unwrapped (ssh stays
		// Dangerous via dangerHeads).
		rest := args
		for len(rest) > 0 && strings.HasPrefix(rest[0], "-") {
			f := rest[0]
			rest = rest[1:]
			if sshValueFlags[f] && len(rest) > 0 {
				rest = rest[1:]
			}
		}
		if len(rest) == 0 {
			return "", false // no host
		}
		rest = rest[1:] // skip the host
		if len(rest) == 0 {
			return "", false // interactive shell — no remote command
		}
		if len(rest) == 1 && strings.ContainsAny(rest[0], " \t") {
			return rest[0], true // a single quoted remote command
		}
		return joinArgv(rest), true
	case "flock":
		// `flock [opts] LOCKFILE command args…` or `flock [opts] LOCKFILE -c 'command'`.
		// Skip flags (and -w/-E values), then the lockfile operand, then the rest is the
		// command. Without this `flock /tmp/l rm -rf x` hid the rm -rf behind flock.
		rest := args
		for len(rest) > 0 && strings.HasPrefix(rest[0], "-") {
			f := rest[0]
			if f == "-c" || f == "--command" {
				if len(rest) > 1 {
					return rest[1], true
				}
				return "", false
			}
			rest = rest[1:]
			if flockValueFlags[f] && len(rest) > 0 && !strings.Contains(f, "=") {
				rest = rest[1:]
			}
		}
		if len(rest) == 0 {
			return "", false // just a lockfile / fd, no command
		}
		rest = rest[1:] // skip the lockfile / dir / fd operand
		if len(rest) == 0 {
			return "", false
		}
		if rest[0] == "-c" || rest[0] == "--command" {
			if len(rest) > 1 {
				return rest[1], true
			}
			return "", false
		}
		return joinArgv(rest), true
	case "parallel":
		// GNU parallel: `parallel [opts] command-template ::: args`. The command is the
		// tokens after parallel's own options up to the first ::: / :::: separator (its
		// OWN flags like `rm -rf` belong to the command once the template has started).
		// `parallel rm -rf ::: a b` → classify `rm -rf` → catastrophic, not Medium.
		rest := args
		for len(rest) > 0 && strings.HasPrefix(rest[0], "-") {
			f := rest[0]
			rest = rest[1:]
			if parallelValueFlags[f] && len(rest) > 0 && !strings.Contains(f, "=") {
				rest = rest[1:]
			}
		}
		var cmd []string
		for _, a := range rest {
			if a == ":::" || a == "::::" || a == ":::+" || a == "::::+" {
				break
			}
			cmd = append(cmd, a)
		}
		return joinArgv(cmd), true
	}
	// `gcloud compute ssh … --command="X"` and `docker/kubectl/oc … exec … -- X`.
	if (head == "gcloud" || head == "az") && hasArg(args, "ssh") {
		if v, ok := flagValue(argv, "--command"); ok {
			return v, true
		}
		return "", false
	}
	if (head == "docker" || head == "podman" || head == "kubectl" || head == "k" || head == "oc") && hasArg(args, "exec") {
		// after a `--` it's the command (kubectl form); else skip flags + the
		// container/pod name and take the rest (docker form).
		if v, ok := afterDoubleDash(args); ok {
			return v, true
		}
		if v, ok := commandAfterExec(args); ok {
			return v, true
		}
		return "", false
	}
	return "", false
}

// commandAfterExec extracts the command an `exec` runs: skip the wrapper's flags
// (and the value of flags that take one), skip the container/pod name, and return
// everything from the command on.
func commandAfterExec(args []string) (string, bool) {
	idx := -1
	for i, a := range args {
		if a == "exec" {
			idx = i
			break
		}
	}
	if idx < 0 {
		return "", false
	}
	rest := args[idx+1:]
	flagsWithValue := set("-u", "--user", "-e", "--env", "-w", "--workdir", "-l",
		"--label", "-n", "--namespace", "-c", "--container")
	skippedContainer := false
	for i := 0; i < len(rest); i++ {
		tok := rest[i]
		if strings.HasPrefix(tok, "-") {
			if flagsWithValue[tok] {
				i++ // skip its value token
			}
			continue
		}
		if !skippedContainer {
			skippedContainer = true
			continue
		}
		return joinArgv(rest[i:]), true
	}
	return "", false
}

// firstCall parses command and returns its first simple command (in source order)
// that has a binary — i.e. skipping a pure `FOO=bar` assignment-only statement. The
// walk is pre-order, so the OUTER command is found before any nested substitution.
func firstCall(command string) (*syntax.CallExpr, bool) {
	f, err := parse(command)
	if err != nil {
		return nil, false
	}
	var found *syntax.CallExpr
	syntax.Walk(f, func(n syntax.Node) bool {
		if found != nil {
			return false
		}
		if c, ok := n.(*syntax.CallExpr); ok && len(c.Args) > 0 {
			found = c
			return false
		}
		return true
	})
	return found, found != nil
}

// --- AST helpers ---

// wordsToArgv resolves each word to the literal string the shell would pass to the
// program: quotes are removed, parameter expansions ($VAR) become a neutral _VAR_
// placeholder, and command substitutions $(…) become _SUB_ (their contents are
// classified separately, since the AST walk visits them as their own commands).
func wordsToArgv(words []*syntax.Word) []string {
	argv := make([]string, 0, len(words))
	for _, w := range words {
		argv = append(argv, wordText(w))
	}
	return argv
}

func wordText(w *syntax.Word) string {
	var b strings.Builder
	writeParts(&b, w.Parts)
	return b.String()
}

func writeParts(b *strings.Builder, parts []syntax.WordPart) {
	for _, part := range parts {
		switch p := part.(type) {
		case *syntax.Lit:
			b.WriteString(p.Value)
		case *syntax.SglQuoted:
			b.WriteString(p.Value)
		case *syntax.DblQuoted:
			writeParts(b, p.Parts)
		case *syntax.CmdSubst:
			b.WriteString("_SUB_")
		default: // ParamExp, ArithmExp, ProcSubst, ExtGlob, …
			b.WriteString("_VAR_")
		}
	}
}

// isFileWriteRedir reports a redirect that writes to a real file. fd duplications
// (2>&1), here-docs, and the /dev/null|stdout|stderr sinks write nothing real.
func isFileWriteRedir(r *syntax.Redirect) bool {
	switch r.Op {
	case syntax.RdrOut, syntax.AppOut, syntax.RdrAll, syntax.AppAll, syntax.RdrClob, syntax.RdrInOut:
		if r.Word == nil {
			return false
		}
		switch wordText(r.Word) {
		case "/dev/null", "/dev/stdout", "/dev/stderr":
			return false
		}
		return true
	}
	return false
}

// shellQuote renders a token so it survives re-parsing as a single word.
func shellQuote(tok string) string {
	if tok == "" {
		return "''"
	}
	if !strings.ContainsAny(tok, " \t\n'\"|&;<>()$`\\*?[]{}~#!") {
		return tok
	}
	return "'" + strings.ReplaceAll(tok, "'", `'\''`) + "'"
}

// joinArgv rebuilds a command string from argv tokens, re-quoting so a token like
// `SELECT 1` stays one argument when the result is re-parsed.
func joinArgv(argv []string) string {
	parts := make([]string, len(argv))
	for i, a := range argv {
		parts[i] = shellQuote(a)
	}
	return strings.Join(parts, " ")
}

// flagValue returns the value of `--flag=V`, `--flag V`, or `-c V` from argv.
func flagValue(argv []string, flag string) (string, bool) {
	for i, a := range argv {
		if a == flag {
			if i+1 < len(argv) {
				return argv[i+1], true
			}
			return "", true
		}
		if strings.HasPrefix(a, flag+"=") {
			return a[len(flag)+1:], true
		}
	}
	return "", false
}

// rmRecursiveForce reports whether an rm has BOTH recursive and force set (any of
// -r/-R/--recursive together with -f/--force, including bundled forms like -rf).
func rmRecursiveForce(args []string) bool {
	rec, force := false, false
	for _, a := range args {
		switch {
		case a == "--recursive":
			rec = true
		case a == "--force":
			force = true
		case strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--"):
			for _, c := range a[1:] {
				if c == 'r' || c == 'R' {
					rec = true
				}
				if c == 'f' {
					force = true
				}
			}
		}
	}
	return rec && force
}

// --- small helpers ---

func firstNonFlag(args []string) string {
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			return strings.ToLower(a)
		}
	}
	return ""
}

func hasReadSub(args []string, readSubs map[string]bool) bool {
	return readSubs[firstNonFlag(args)]
}

func hasFlag(args []string, flag string) bool {
	for _, a := range args {
		if a == flag {
			return true
		}
	}
	return false
}

func hasAnyFlag(args []string, flags ...string) bool {
	for _, f := range flags {
		if hasFlag(args, f) {
			return true
		}
	}
	return false
}

// hasForceFlag reports a force flag (--force, or a combined short flag containing 'f' like
// -f / -fd / -xf) in argv — for `git clean`, which only deletes with force.
func hasForceFlag(args []string) bool {
	for _, a := range args {
		if a == "--force" {
			return true
		}
		if len(a) >= 2 && a[0] == '-' && a[1] != '-' && strings.ContainsRune(a, 'f') {
			return true
		}
	}
	return false
}

func hasArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

func hasAnyPrefix(s string, prefixes ...string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

func afterDoubleDash(args []string) (string, bool) {
	for i, a := range args {
		if a == "--" && i+1 < len(args) {
			return joinArgv(args[i+1:]), true
		}
	}
	return "", false
}
