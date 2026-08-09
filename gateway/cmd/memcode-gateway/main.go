// memcode-api is the hosted inference gateway: the DELIBERATELY SEPARATE
// service that owns what must never ship in the public CLI binary — backend
// routing, provider API keys, and metering. The CLI keeps the agent loop,
// tools, and prompt doctrine local and calls up here for every model turn.
//
// The wire is the OpenAI-compat surface (one-wire): POST /v1/chat/completions
// + GET /v1/models behind org-key auth, with the memcode extensions riding as
// ignorable headers/fields (internal/compat). Behind the translation: hybrid
// routing (Fireworks cheap lane + the strong-tier vendors), BYOK, entitlement
// gating, and the server-side ledger.
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/memcode-ai/memcode/gateway/internal/server"
)

func main() {
	// The cheap lane is a hosted, always-warm API now — there's no GPU pool/registry
	// control plane to maintain. Backend selection is pure env (see provider.NewFromEnv).
	cfg, err := server.ConfigFromEnv()
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           server.New(cfg),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		log.Printf("memcode-api listening on :%s (backend=%s)", cfg.Port, cfg.BackendName)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("serve: %v", err)
		}
	}()

	// Cloud Run sends SIGTERM on scale-down — drain in-flight streams briefly.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}
