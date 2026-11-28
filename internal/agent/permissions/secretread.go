package permissions

import (
	"strings"

	"mvdan.cc/sh/v3/syntax"

	"github.com/memcode-ai/memcode/internal/agent/secrets"
)

// ReadsSecretPath reports whether any simple command in the shell command names
// a credential file as an argument or an input redirect (`cat .env`,
// `base64 < ~/.ssh/id_rsa`). The command still runs; its output is masked the
// way read_file masks such files, so values reach neither the model nor the
// logs. Parsed with the shell AST, never by splitting the string.
func ReadsSecretPath(command string) bool {
	f, err := parse(command)
	if err != nil {
		return false
	}
	found := false
	check := func(w *syntax.Word) {
		if w == nil || found {
			return
		}
		// Only words shaped like paths: a bare `env` is a search term, not .env.
		t := wordText(w)
		if t == "" || strings.HasPrefix(t, "-") || !strings.ContainsAny(t, "/.~") {
			return
		}
		if secrets.IsSecretPath(expandHome(t)) {
			found = true
		}
	}
	syntax.Walk(f, func(n syntax.Node) bool {
		switch x := n.(type) {
		case *syntax.CallExpr:
			for i, w := range x.Args {
				if i > 0 {
					check(w)
				}
			}
		case *syntax.Redirect:
			check(x.Word)
		}
		return !found
	})
	return found
}

// expandHome turns a leading ~ into a path the secret-path patterns recognize.
func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		return "/home" + strings.TrimPrefix(p, "~")
	}
	return p
}
