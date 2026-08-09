package provider

// Thin BYOK client calls for the /apikeys picker — same env-derived connection
// story as FetchModels (models.go): the gateway owns the provider roster and
// the key store; the CLI never persists or lists raw keys, it only submits one
// on the user's explicit action.

import (
	"context"
	"os"

	"github.com/memcode-ai/memcode/internal/gateway/client"
)

func byokClient() (*client.Client, error) {
	if os.Getenv(EnvAPIToken) == "" {
		return nil, ErrNotLoggedIn
	}
	return client.New(APIURL(), os.Getenv(EnvAPIToken)), nil
}

// ByokList fetches the provider roster + the user's masked key rows.
func ByokList(ctx context.Context) (client.ByokKeys, error) {
	c, err := byokClient()
	if err != nil {
		return client.ByokKeys{}, err
	}
	return c.ByokList(ctx)
}

// ByokPut stores/replaces the user's key for a provider (gateway live-probes
// it first). The caller is responsible for redacting the key from any UI/log
// surfaces BEFORE calling.
func ByokPut(ctx context.Context, providerID, key string) (client.ByokPutResult, error) {
	c, err := byokClient()
	if err != nil {
		return client.ByokPutResult{}, err
	}
	return c.ByokPut(ctx, providerID, key)
}

// ByokDelete removes the user's key for a provider.
func ByokDelete(ctx context.Context, providerID string) error {
	c, err := byokClient()
	if err != nil {
		return err
	}
	return c.ByokDelete(ctx, providerID)
}

// ByokValidate live-probes the stored key.
func ByokValidate(ctx context.Context, providerID string) (bool, string, error) {
	c, err := byokClient()
	if err != nil {
		return false, "", err
	}
	return c.ByokValidate(ctx, providerID)
}
