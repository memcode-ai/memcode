// Package forks holds vendored forks of third-party Go modules that memcode
// patches locally. Each sub-directory is a self-contained fork pointed to by a
// replace directive in go.mod:
//
//   - forks/vaxis — a fork of github.com/memcode-ai/memcode/internal/forks/vaxis (the TUI renderer used by
//     internal/vxui). Patched so the primary-screen region grows without
//     erasing scrollback.
//
// There are no Go files at this top level; each fork lives in its own
// sub-package with its own upstream package name and docs.
package forks
