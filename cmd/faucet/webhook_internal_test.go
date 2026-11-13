package main

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLoadConfigWebhook(t *testing.T) {
	t.Parallel()

	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0xA5}, 32))
	const origins = "https://coder.example.test, https://hooks.example.test:8443"
	load := func(extra map[string]string) (config, error) {
		env := map[string]string{envDatabaseURL: "postgres://localhost/faucet", envSecret: testSecret}
		for k, v := range extra {
			env[k] = v
		}
		return loadConfig(func(name string) string { return env[name] })
	}

	cfg, err := load(nil)
	require.NoError(t, err)
	require.Nil(t, cfg.webhook, "webhook delivery is off without its variables")

	cfg, err = load(map[string]string{envWebhookKey: key, envWebhookOrigins: origins})
	require.NoError(t, err)
	require.NotNil(t, cfg.webhook)
	require.NotNil(t, cfg.webhook.Callbacks)
	printed := fmt.Sprintf("%v %+v %#v", cfg, cfg.webhook, cfg.webhook.Key)
	require.NotContains(t, printed, key, "the printed config does not show the key")
	require.NotContains(t, printed, "a5a5a5", "or its bytes")

	for name, tc := range map[string]struct {
		env  map[string]string
		want string
	}{
		"a key without origins": {map[string]string{envWebhookKey: key}, envWebhookOrigins},
		"origins without a key": {map[string]string{envWebhookOrigins: origins}, envWebhookKey},
		"a key that is not base64": {
			map[string]string{envWebhookKey: "not base64 at all!", envWebhookOrigins: origins}, envWebhookKey,
		},
		"a 31 byte key": {
			map[string]string{envWebhookKey: base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 31)), envWebhookOrigins: origins}, envWebhookKey,
		},
		"a 33 byte key": {
			map[string]string{envWebhookKey: base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 33)), envWebhookOrigins: origins}, envWebhookKey,
		},
		"an all-zero key": {
			map[string]string{envWebhookKey: base64.StdEncoding.EncodeToString(make([]byte, 32)), envWebhookOrigins: origins}, envWebhookKey,
		},
		"a URL-safe key": {
			map[string]string{envWebhookKey: strings.NewReplacer("+", "-", "/", "_").Replace(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0xfb}, 32))), envWebhookOrigins: origins}, envWebhookKey,
		},
		"an http origin":         {map[string]string{envWebhookKey: key, envWebhookOrigins: "http://coder.example.test"}, envWebhookOrigins},
		"an origin with a path":  {map[string]string{envWebhookKey: key, envWebhookOrigins: "https://coder.example.test/api"}, envWebhookOrigins},
		"a loopback origin":      {map[string]string{envWebhookKey: key, envWebhookOrigins: "https://127.0.0.1"}, envWebhookOrigins},
		"a private origin":       {map[string]string{envWebhookKey: key, envWebhookOrigins: "https://10.0.0.1"}, envWebhookOrigins},
		"an empty entry":         {map[string]string{envWebhookKey: key, envWebhookOrigins: "https://a.example.test,,https://b.example.test"}, envWebhookOrigins},
		"a trailing comma":       {map[string]string{envWebhookKey: key, envWebhookOrigins: "https://a.example.test,"}, envWebhookOrigins},
		"origins with user info": {map[string]string{envWebhookKey: key, envWebhookOrigins: "https://u:p@a.example.test"}, envWebhookOrigins},
	} {
		_, err := load(tc.env)
		require.ErrorContains(t, err, tc.want, name)
		for _, value := range tc.env {
			if len(value) >= 8 {
				require.NotContains(t, err.Error(), value, "%s: the error names variables, never values", name)
			}
		}
	}
}

func loadHandshakeConfig(t *testing.T, extra map[string]string) (config, error) {
	t.Helper()
	env := map[string]string{envDatabaseURL: "postgres://localhost/faucet", envSecret: testSecret}
	for k, v := range extra {
		env[k] = v
	}
	return loadConfig(func(name string) string { return env[name] })
}

func TestLoadConfigWebhookCallbackVerification(t *testing.T) {
	t.Parallel()

	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0xA5}, 32))
	const origins = "https://coder.example.test"

	for name, env := range map[string]map[string]string{
		"the handshake with a key":                    {envWebhookKey: key, envWebhookVerify: verifyHandshake},
		"the allowlist named, with a key and origins": {envWebhookKey: key, envWebhookOrigins: origins, envWebhookVerify: verifyAllowlist},
	} {
		cfg, err := loadHandshakeConfig(t, env)
		require.NoError(t, err, name)
		require.NotNil(t, cfg.webhook, name)
		require.NotNil(t, cfg.webhook.Callbacks, name)
	}

	for name, tc := range map[string]struct {
		env  map[string]string
		want string
	}{
		"the handshake without a key": {map[string]string{envWebhookVerify: verifyHandshake}, "webhook delivery needs " + envWebhookKey},
		"the handshake with origins": {
			map[string]string{envWebhookKey: key, envWebhookOrigins: origins, envWebhookVerify: verifyHandshake},
			envWebhookOrigins + " must not be set when " + envWebhookVerify + " is handshake",
		},
		"the allowlist named, without origins": {
			map[string]string{envWebhookKey: key, envWebhookVerify: verifyAllowlist}, "webhook delivery needs " + envWebhookOrigins,
		},
		"a mode that does not exist": {
			map[string]string{envWebhookKey: key, envWebhookOrigins: origins, envWebhookVerify: "handshake-7c1f4e"},
			envWebhookVerify + " must be allowlist or handshake",
		},
		"a mode in capitals": {map[string]string{envWebhookKey: key, envWebhookVerify: "HANDSHAKE"}, envWebhookVerify + " must be allowlist or handshake"},
		"the handshake with a bad key": {
			map[string]string{envWebhookKey: "not base64 at all!", envWebhookVerify: verifyHandshake}, envWebhookKey + " must be the standard base64",
		},
	} {
		_, err := loadHandshakeConfig(t, tc.env)
		require.ErrorContains(t, err, tc.want, name)
		for _, value := range tc.env {
			if len(value) >= 8 && value != verifyHandshake && value != verifyAllowlist {
				require.NotContains(t, err.Error(), value, "%s: the error names variables, never values", name)
			}
		}
	}

	cfg, err := loadHandshakeConfig(t, map[string]string{envWebhookVerify: verifyAllowlist + "x"})
	require.Error(t, err)
	require.Nil(t, cfg.webhook)
}
