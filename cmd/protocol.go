package cmd

import (
	"context"
	"io"
	"os"

	"github.com/memcode-ai/memcode/internal/agent/permissions"
	"github.com/memcode-ai/memcode/internal/agent/protocol"
	"github.com/memcode-ai/memcode/internal/agent/runtime"
	"github.com/memcode-ai/memcode/internal/llm"
	"github.com/memcode-ai/memcode/internal/provider"
)

// runStreamJSON drives an interactive session over the stream-json control protocol
// (newline-delimited JSON on stdin/stdout) instead of the TUI — the machine surface
// the sdk/agent wrapper drives as a subprocess. stdout carries ONLY protocol events,
// so nothing here prints to it; the session's output is redirected into
// assistant_delta events and diagnostics go to stderr.
func runStreamJSON(ctx context.Context, mode permissions.Mode, chrome bool) error {
	st, cfg, err := openProject(ctx)
	if err != nil {
		return err
	}
	defer st.Close()

	provider.LoadDotEnv(cfg.Root)
	prov, err := provider.NewFromEnv()
	if err != nil {
		return err
	}
	runner := llm.NewRunner(prov)
	// Model is the gateway's call (server-owned selection); the session model is a
	// display placeholder. Output is redirected by protocol.Run via SetOutput, so the
	// io.Discard here is never used for protocol bytes.
	sess := runtime.New(st, runner, cfg.Root, provider.EffectiveModel(cfg.Models.Coder), mode, io.Discard)
	if chrome {
		sess.SetBrowserEnabled(true)
		defer sess.CloseBrowser()
	}
	return protocol.Run(ctx, sess, os.Stdin, os.Stdout)
}
