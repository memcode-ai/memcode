// Package serve composes the gateway core into an http.Handler. It exists as
// a separate package so the seam types (package gateway) stay a leaf both
// the serving internals and external compositions can import without cycles.
package serve

import (
	"net/http"

	"github.com/memcode-ai/memcode/gateway"
	"github.com/memcode-ai/memcode/gateway/internal/server"
)

// Config re-exports the server configuration surface compositions may set
// beyond the environment contract.
type Config = server.Config

// ConfigFromEnv builds the config from the environment (the same contract
// the self-host docs describe: PORT, MEMCODE_PROVIDER, provider keys, …).
func ConfigFromEnv() (Config, error) { return server.ConfigFromEnv() }

// New builds the serving handler from a config plus extension options.
// With no options this is the self-host composition.
func New(cfg Config, opts ...gateway.Option) http.Handler {
	e := gateway.Resolve(opts...)
	return server.NewWith(cfg, e)
}
