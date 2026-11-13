package main

import (
	"encoding/base64"
	"net/http"
	"strings"

	"golang.org/x/xerrors"

	"github.com/coder/faucet/faucet"
)

const (
	envWebhookKey     = "FAUCET_WEBHOOK_KEY" //nolint:gosec // The name of an environment variable, not a credential.
	envWebhookOrigins = "FAUCET_WEBHOOK_ALLOWED_ORIGINS"
	envWebhookVerify  = "FAUCET_WEBHOOK_CALLBACK_VERIFICATION"

	verifyAllowlist = "allowlist"
	verifyHandshake = "handshake"
)

// loadWebhookConfig stops the start when the key and the way to verify callback
// URLs do not go together, so a typo does not turn webhook mode off. In
// allowlist mode, the default, the key and the allowlist are both required. In
// handshake mode the key is required and the allowlist must be unset, because
// an operator who set both could think that the allowlist still limits the
// destinations. No error names a value, because one of them is a key.
func loadWebhookConfig(getenv func(string) string, cfg *config) error {
	rawKey, rawOrigins, mode := getenv(envWebhookKey), getenv(envWebhookOrigins), getenv(envWebhookVerify)
	switch {
	case mode == "" && rawKey == "" && rawOrigins == "":
		return nil
	case mode == "":
		mode = verifyAllowlist
	case mode != verifyAllowlist && mode != verifyHandshake:
		return xerrors.Errorf("%s must be %s or %s", envWebhookVerify, verifyAllowlist, verifyHandshake)
	}
	switch {
	case rawKey == "":
		return xerrors.Errorf("webhook delivery needs %s", envWebhookKey)
	case mode == verifyAllowlist && rawOrigins == "":
		return xerrors.Errorf("webhook delivery needs %s", envWebhookOrigins)
	case mode == verifyHandshake && rawOrigins != "":
		return xerrors.Errorf("%s must not be set when %s is %s", envWebhookOrigins, envWebhookVerify, verifyHandshake)
	}

	decoded, err := base64.StdEncoding.DecodeString(rawKey)
	if err != nil {
		return xerrors.Errorf("%s must be the standard base64 of exactly 32 bytes", envWebhookKey)
	}
	key, err := faucet.NewWebhookKey(decoded)
	if err != nil {
		return xerrors.Errorf("%s must be the standard base64 of exactly 32 bytes that are not all zero", envWebhookKey)
	}
	deps := &faucet.WebhookDeps{Key: key, Client: newCallbackClient()}
	if mode == verifyHandshake {
		deps.Callbacks, err = faucet.NewCallbackHandshake(faucet.HandshakeOptions{Client: deps.Client})
		if err != nil {
			return xerrors.Errorf("%s: %w", envWebhookVerify, err)
		}
		deps.AllowInsecureLoopback = allowInsecureLoopback
	} else if deps.Callbacks, err = newAllowlist(rawOrigins); err != nil {
		return err
	}
	cfg.webhook = deps
	return nil
}

func newAllowlist(rawOrigins string) (faucet.CallbackVerifier, error) {
	origins := strings.Split(rawOrigins, ",")
	for i := range origins {
		origins[i] = strings.TrimSpace(origins[i])
	}
	callbacks, err := faucet.NewCallbackAllowlist(origins, false)
	if err != nil {
		return nil, xerrors.Errorf("%s must be comma-separated https origins written scheme://host[:port]: %w", envWebhookOrigins, err)
	}
	return callbacks, nil
}

// newCallbackClient is the client that sends the verification requests and the
// deliveries. It reaches loopback over http only in a binary built with the faucet_e2e tag.
func newCallbackClient() *http.Client {
	return faucet.NewHTTPClient(faucet.HTTPClientOptions{AllowInsecureLoopback: allowInsecureLoopback})
}
