//go:build !faucet_e2e

package main

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/coder/faucet/faucet"
)

func TestProductionBuildRefusesLoopbackCallbacks(t *testing.T) {
	t.Parallel()

	var hits atomic.Int32
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	})
	plain, secure := httptest.NewServer(handler), httptest.NewTLSServer(handler)
	t.Cleanup(plain.Close)
	t.Cleanup(secure.Close)

	for _, url := range []string{plain.URL, secure.URL} {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url, nil)
		require.NoError(t, err)
		resp, err := newCallbackClient().Do(req)
		if err == nil {
			_ = resp.Body.Close()
		}
		require.ErrorIs(t, err, faucet.ErrDestinationRefused, url)
	}
	require.Zero(t, hits.Load(), "no request reached a server on loopback")

	cfg, err := loadHandshakeConfig(t, map[string]string{envWebhookKey: "oqKjpKWmp6ipqqusra6vsLGys7S1tre4ubq7vL2+v8A=", envWebhookVerify: verifyHandshake})
	require.NoError(t, err)
	require.False(t, cfg.webhook.AllowInsecureLoopback, "subscribe refuses a loopback callback URL")
}
