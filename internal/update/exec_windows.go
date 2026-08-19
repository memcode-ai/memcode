//go:build windows

package update

// Windows has no exec(2); ReexecStaged returns before calling this, but the
// symbol must exist to compile.
var syscallExec = func(argv0 string, argv, envv []string) error { return nil }
