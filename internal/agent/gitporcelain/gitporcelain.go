// Package gitporcelain holds small shared helpers for parsing git's porcelain
// output. Both the runtime's working-tree scans and the acceptance reconciler
// read `git status --porcelain` lines; with core.quotePath=true (git's default)
// any path with a non-ASCII or special character is emitted C-quoted
// ("caf\303\251.txt"), which a naive parser would treat as a literal path.
package gitporcelain

import "strconv"

// Unquote decodes a path as git porcelain output prints it when core.quotePath
// is active: wrapped in double quotes with C-style escapes (\t, \", \\ and
// \nnn octal bytes). Git's escapes are a subset of Go's string-literal escapes,
// so strconv.Unquote decodes them directly. A path git did not quote — or a
// malformed quote — is returned unchanged.
func Unquote(p string) string {
	if len(p) < 2 || p[0] != '"' || p[len(p)-1] != '"' {
		return p
	}
	if u, err := strconv.Unquote(p); err == nil {
		return u
	}
	return p
}
