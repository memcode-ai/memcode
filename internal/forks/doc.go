// Package forks holds vendored forks of third-party Go modules that memcode
// patches locally. Each sub-directory is a self-contained copy whose import
// paths were rewritten to live under this module — the code is imported
// directly as github.com/memcode-ai/memcode/internal/forks/... (there is no
// replace directive in go.mod).
//
//   - forks/vaxis — a fork of go.rockorager.dev/vaxis (the TUI renderer used
//     by internal/vxui). The exact upstream version it was copied from was not
//     recorded at vendoring time. Patched so the primary-screen region grows
//     without erasing scrollback; the PrimaryScreen patch spans ~11 files and
//     carries its own tests.
//
// There are no Go files at this top level; each fork lives in its own
// sub-package with its own upstream package name and docs.
package forks
