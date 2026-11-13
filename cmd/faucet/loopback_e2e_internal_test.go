//go:build faucet_e2e

package main

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestE2EBuildReachesLoopbackCallbacks(t *testing.T) {
	t.Parallel()

	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL, nil)
	require.NoError(t, err)
	resp, err := newCallbackClient().Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.EqualValues(t, 1, hits.Load())

	cfg, err := loadHandshakeConfig(t, map[string]string{envWebhookKey: "oqKjpKWmp6ipqqusra6vsLGys7S1tre4ubq7vL2+v8A=", envWebhookVerify: verifyHandshake})
	require.NoError(t, err)
	require.True(t, cfg.webhook.AllowInsecureLoopback)
}
