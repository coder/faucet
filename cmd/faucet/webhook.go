package main

import (
	"encoding/base64"
	"strings"

	"golang.org/x/xerrors"

	"github.com/coder/faucet/faucet"
)

const (
	envWebhookKey     = "FAUCET_WEBHOOK_KEY" //nolint:gosec // The name of an environment variable, not a credential.
	envWebhookOrigins = "FAUCET_WEBHOOK_ALLOWED_ORIGINS"
)

// loadWebhookConfig reads the settings of webhook delivery. They go together:
// the key seals subscriber secrets and the allowlist says which callback
// origins may receive events. Neither set leaves webhook mode off. Only one set
// stops the start, so a typo does not turn it off. No error names a value,
// because one of them is a key.
func loadWebhookConfig(getenv func(string) string, cfg *config) error {
	rawKey, rawOrigins := getenv(envWebhookKey), getenv(envWebhookOrigins)
	switch {
	case rawKey == "" && rawOrigins == "":
		return nil
	case rawKey == "":
		return xerrors.Errorf("webhook delivery needs %s", envWebhookKey)
	case rawOrigins == "":
		return xerrors.Errorf("webhook delivery needs %s", envWebhookOrigins)
	}

	decoded, err := base64.StdEncoding.DecodeString(rawKey)
	if err != nil {
		return xerrors.Errorf("%s must be the standard base64 of exactly 32 bytes", envWebhookKey)
	}
	key, err := faucet.NewWebhookKey(decoded)
	if err != nil {
		return xerrors.Errorf("%s must be the standard base64 of exactly 32 bytes that are not all zero", envWebhookKey)
	}
	origins := strings.Split(rawOrigins, ",")
	for i := range origins {
		origins[i] = strings.TrimSpace(origins[i])
	}
	callbacks, err := faucet.NewCallbackAllowlist(origins, false)
	if err != nil {
		return xerrors.Errorf("%s must be comma-separated https origins written scheme://host[:port]: %w", envWebhookOrigins, err)
	}
	cfg.webhook = &faucet.WebhookDeps{Key: key, Callbacks: callbacks}
	return nil
}
