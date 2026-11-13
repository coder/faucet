package faucet_test

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/coder/faucet/faucet"
)

func TestCallbackAllowlistAcceptsOnlyListedOrigins(t *testing.T) {
	t.Parallel()

	allowlist, err := faucet.NewCallbackAllowlist([]string{"https://coder.example.test", "https://hooks.example.test:8443/"}, false)
	require.NoError(t, err)
	h := newWebhookHarness(t, func(d *faucet.Deps) { d.Webhook.Callbacks = allowlist })
	secret := newSecret(t, 32)

	for _, url := range []string{
		"https://coder.example.test/api/v2/hook",
		"https://CODER.example.test:443/x?y=1",
		"https://coder.example.test./",
		"https://hooks.example.test:8443/deep/path",
	} {
		h.subscribe(subscribeTo(repoArgs(), url, secret))
	}
	require.Len(t, h.rows(), 4)

	for _, url := range []string{
		"https://evil.example.test/hook",
		"https://coder.example.test:8443/hook",
		"https://hooks.example.test/hook",
		"https://hooks.example.test:8444/hook",
		"https://sub.coder.example.test/hook",
		"https://coder.example.test.evil.test/hook",
		"https://8.8.8.8/hook",
	} {
		rpcErr := h.subscribeError(subscribeTo(repoArgs(), url, secret))
		require.EqualValues(t, codeCallbackEndpoint, rpcErr.Code, url)
		require.JSONEq(t, `{"reason":"challenge_failed"}`, string(rpcErr.Data), url)
	}
	require.Len(t, h.rows(), 4, "an origin that is not listed stores nothing")
}

func TestNewCallbackAllowlistRefusesUnsafeEntries(t *testing.T) {
	t.Parallel()

	for name, origins := range map[string][]string{
		"none":          nil,
		"http":          {"http://coder.example.test"},
		"a path":        {"https://coder.example.test/api"},
		"a query":       {"https://coder.example.test/?a=1"},
		"user info":     {"https://u:p@coder.example.test"},
		"loopback":      {"https://127.0.0.1"},
		"private":       {"https://10.0.0.1"},
		"empty":         {""},
		"one bad entry": {"https://coder.example.test", "ftp://x.example.test"},
	} {
		_, err := faucet.NewCallbackAllowlist(origins, false)
		require.Error(t, err, name)
	}

	allowlist, err := faucet.NewCallbackAllowlist([]string{"http://127.0.0.1:8080"}, true)
	require.NoError(t, err, "the loopback option applies to entries too")
	h := newWebhookHarness(t, func(d *faucet.Deps) {
		d.Webhook.Callbacks = allowlist
		d.Webhook.AllowInsecureLoopback = true
	})
	h.subscribe(subscribeTo(repoArgs(), "http://127.0.0.1:8080/hook", newSecret(t, 32)))
}

func TestNewWebhookKeyNeedsExactly32NonZeroBytes(t *testing.T) {
	t.Parallel()

	for name, b := range map[string][]byte{
		"nil": nil, "31 bytes": make([]byte, 31), "33 bytes": bytes.Repeat([]byte{7}, 33), "all zero": make([]byte, 32),
	} {
		_, err := faucet.NewWebhookKey(b)
		require.Error(t, err, name)
		require.NotContains(t, err.Error(), "7777", name)
	}
	_, err := faucet.NewWebhookKey(testWebhookKey)
	require.NoError(t, err)
}

func TestWebhookKeyIsNeverPrinted(t *testing.T) {
	t.Parallel()

	key, err := faucet.NewWebhookKey(testWebhookKey)
	require.NoError(t, err)
	deps := faucet.WebhookDeps{Key: key, Callbacks: &fakeVerifier{}}
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%x", "%d", "%q"} {
		for _, value := range []any{key, &key, deps, &deps, []faucet.WebhookKey{key}, map[string]faucet.WebhookKey{"k": key}} {
			out := fmt.Sprintf(verb, value)
			require.NotContains(t, out, "a0a1a2", "%s of %T", verb, value)
			require.NotContains(t, out, "160 161 162", "%s of %T", verb, value)
			require.NotContains(t, out, "160,161,162", "%s of %T", verb, value)
			require.NotContains(t, out, string(testWebhookKey[:8]), "%s of %T", verb, value)
		}
	}
}

func TestNewRejectsAnIncompleteWebhookConfig(t *testing.T) {
	t.Parallel()

	key, err := faucet.NewWebhookKey(testWebhookKey)
	require.NoError(t, err)
	for name, webhook := range map[string]*faucet.WebhookDeps{
		"no key":      {Callbacks: &fakeVerifier{}},
		"no verifier": {Key: key},
	} {
		deps := testDeps(&fakeStore{})
		deps.Webhook = webhook
		_, err := faucet.New(deps)
		require.Error(t, err, name)
		require.NotContains(t, err.Error(), string(testWebhookKey[:8]))
	}

	deps := testDeps(&fakeStore{})
	svc, err := faucet.New(deps)
	require.NoError(t, err, "webhook delivery is optional")
	require.NotNil(t, svc)
}
