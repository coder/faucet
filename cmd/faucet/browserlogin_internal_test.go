package main

import (
	"encoding/base64"
	"maps"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/coder/superblink-events/faucet"
)

// testTokenKey is the value of FAUCET_TOKEN_KEY for one made-up key.
func testTokenKey() string {
	return "k1:" + base64.StdEncoding.EncodeToString([]byte(strings.Repeat("k", 32)))
}

func loadWith(extra map[string]string) (config, error) {
	env := map[string]string{envDatabaseURL: "postgres://localhost/faucet", envSecret: testSecret}
	maps.Copy(env, extra)
	return loadConfig(func(name string) string { return env[name] })
}

func browserLoginEnv() map[string]string {
	return map[string]string{
		envPublicURL:             "https://faucet.example.com",
		envGitHubAppClientID:     "Iv1.faucet",
		envGitHubAppClientSecret: "app-secret-value",
		envTokenKey:              testTokenKey(),
	}
}

func TestLoadConfigTurnsTheBrowserLoginOnOnlyWithEveryPart(t *testing.T) {
	t.Parallel()
	cfg, err := loadWith(map[string]string{envPublicURL: "https://faucet.example.com"})
	require.NoError(t, err)
	require.Nil(t, cfg.browserLogin, "off with nothing of it set: the login server is for services alone")

	cfg, err = loadWith(browserLoginEnv())
	require.NoError(t, err)
	require.NotNil(t, cfg.browserLogin)
	require.Equal(t, "Iv1.faucet", cfg.browserLogin.GitHub.ClientID)
	require.Equal(t, defaultGitHubURL, cfg.browserLogin.GitHub.WebURL)
	require.Equal(t, defaultAPIURL, cfg.browserLogin.GitHub.APIURL)
	require.Equal(t, faucet.HTTPClientOptions{AllowInsecureLoopback: allowInsecureLoopback}, cfg.browserLogin.Documents)

	env := browserLoginEnv()
	env[envGitHubURL], env[envAPIURL] = "https://github.example.com", "https://github.example.com/api/v3"
	cfg, err = loadWith(env)
	require.NoError(t, err)
	require.Equal(t, "https://github.example.com", cfg.browserLogin.GitHub.WebURL)
	require.Equal(t, "https://github.example.com/api/v3", cfg.browserLogin.GitHub.APIURL)
}

// A typo in one name must not turn the login off, and no error may show a
// secret.
func TestLoadConfigRefusesABrowserLoginWithAPartMissingAndNamesNoValue(t *testing.T) {
	t.Parallel()
	for _, name := range []string{envPublicURL, envGitHubAppClientID, envGitHubAppClientSecret, envTokenKey} {
		env := browserLoginEnv()
		delete(env, name)
		_, err := loadWith(env)
		require.ErrorContains(t, err, name)
		for _, secret := range []string{"app-secret-value", testTokenKey()} {
			require.NotContains(t, err.Error(), secret, name)
		}
	}

	_, err := loadWith(map[string]string{envPublicURL: "https://faucet.example.com", envGitHubURL: "https://github.example.com"})
	require.ErrorContains(t, err, envGitHubAppClientID, "the GitHub URL alone asks for the rest")

	for name, key := range map[string]string{
		"not base64":   "k1:not-base64",
		"a short key":  "k1:" + base64.StdEncoding.EncodeToString([]byte("short")),
		"no key ID":    base64.StdEncoding.EncodeToString([]byte(strings.Repeat("k", 32))),
		"the same ID":  testTokenKey() + "," + testTokenKey(),
		"an empty key": "k1:",
	} {
		env := browserLoginEnv()
		env[envTokenKey] = key
		_, err := loadWith(env)
		require.ErrorContains(t, err, envTokenKey, name)
		if encoded := strings.TrimPrefix(key, "k1:"); encoded != "" {
			require.NotContains(t, err.Error(), encoded, name)
		}
	}

	env := browserLoginEnv()
	env[envGitHubURL] = "http://github.example.com"
	_, err = loadWith(env)
	require.ErrorContains(t, err, envGitHubURL)
}
