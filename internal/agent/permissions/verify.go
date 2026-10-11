package permissions

import (
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// LocalVerification reports whether a command is ROUTINE LOCAL VERIFICATION: it
// runs at least one test/build/vet/lint/typecheck/format tool, and every other
// simple command in it is Safe (cd, tail, grep, echo …). It never matches a
// command with a file-write redirect, an unparseable string, or any step that
// classifies above Safe on its own (a push, an install, a deploy).
//
// Why it exists: these commands sit at Medium (they run project code), so in
// auto mode they reached the AUTHORIZATION judge, which asks "did the user's
// latest message explicitly authorize THIS?". A user saying "continue" or "fix
// everything" never names `go test -race ./...`, so the judge's conservative
// "unclear → ask" turned the agent's own verification step into a permission
// prompt. Verifying work is part of doing the work, not an action to authorize;
// the gate skips the judge for exactly these (the risk ladder still applies).
//
// Matching is on the parsed argv, never on substrings ("git push && go test"
// does not match: the push is not Safe).
func LocalVerification(command string) bool {
	f, err := parse(command)
	if err != nil {
		return false
	}
	sawVerify, ok := false, true
	syntax.Walk(f, func(n syntax.Node) bool {
		if !ok {
			return false
		}
		switch x := n.(type) {
		case *syntax.Stmt:
			for _, rd := range x.Redirs {
				if isFileWriteRedir(rd) {
					ok = false
					return false
				}
			}
			switch x.Cmd.(type) {
			case nil, *syntax.CallExpr, *syntax.BinaryCmd, *syntax.Block, *syntax.Subshell:
			default:
				ok = false // loops, functions, conditionals: not a routine verification line
				return false
			}
		case *syntax.CmdSubst, *syntax.ProcSubst:
			ok = false // hidden computation; let the judge see it
			return false
		case *syntax.CallExpr:
			argv := wordsToArgv(x.Args)
			if len(argv) == 0 {
				return true
			}
			if isVerifyArgv(argv) {
				sawVerify = true
				return true
			}
			if r, cat := classifyArgv(argv, 0); r != Safe || cat {
				ok = false
				return false
			}
		}
		return true
	})
	return ok && sawVerify
}

// isVerifyArgv recognizes one test/build/lint/typecheck/format invocation.
func isVerifyArgv(argv []string) bool {
	head := argv[0]
	if i := strings.LastIndexByte(head, '/'); i >= 0 {
		head = head[i+1:]
	}
	sub := ""
	if len(argv) > 1 {
		sub = argv[1]
	}
	in := func(s string, set ...string) bool {
		for _, v := range set {
			if s == v {
				return true
			}
		}
		return false
	}
	switch head {
	case "go":
		return in(sub, "test", "vet", "build", "fmt")
	case "gofmt", "jest", "vitest", "tsc", "eslint", "pytest", "mypy":
		return true
	case "cargo":
		return in(sub, "test", "build", "check", "clippy", "fmt")
	case "ruff":
		return in(sub, "check", "format")
	case "npm", "pnpm", "yarn", "bun":
		if in(sub, "test", "t") {
			return true
		}
		if sub == "run" && len(argv) > 2 {
			return in(argv[2], "test", "build", "lint", "typecheck", "type-check", "check", "format")
		}
		return head != "npm" && in(sub, "build", "lint", "typecheck")
	case "npx":
		if sub == "next" {
			return len(argv) > 2 && in(argv[2], "build", "lint")
		}
		return sub != "" && in(sub, "jest", "vitest", "tsc", "eslint", "prettier")
	case "make":
		return in(sub, "test", "build", "check", "lint")
	case "python", "python3":
		return sub == "-m" && len(argv) > 2 && in(argv[2], "pytest", "mypy")
	}
	return false
}
