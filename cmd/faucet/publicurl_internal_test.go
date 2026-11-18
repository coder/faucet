package main

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// On Vercel production, faucet's public URL defaults to the production URL
// that Vercel gives. A preview or a development deployment never takes it,
// because Vercel sets the variable there too, nor a process without VERCEL,
// and an explicit FAUCET_PUBLIC_URL always wins.
func TestOnVercelProductionThePublicURLDefaultsToTheProductionURL(t *testing.T) {
	t.Parallel()
	const production = "faucet.example.com"
	for name, c := range map[string]struct {
		env     map[string]string
		want    string
		derived bool
	}{
		"production":        {env: map[string]string{envVercel: "1", envVercelEnv: "production", envVercelProductionURL: production}, want: "https://faucet.example.com", derived: true},
		"preview":           {env: map[string]string{envVercel: "1", envVercelEnv: "preview", envVercelProductionURL: production}},
		"development":       {env: map[string]string{envVercel: "1", envVercelEnv: "development", envVercelProductionURL: production}},
		"no VERCEL_ENV":     {env: map[string]string{envVercel: "1", envVercelProductionURL: production}},
		"no VERCEL":         {env: map[string]string{envVercelEnv: "production", envVercelProductionURL: production}},
		"no production URL": {env: map[string]string{envVercel: "1", envVercelEnv: "production"}},
		"an explicit FAUCET_PUBLIC_URL": {
			env:  map[string]string{envVercel: "1", envVercelEnv: "production", envVercelProductionURL: production, envPublicURL: "https://events.example.com"},
			want: "https://events.example.com",
		},
		"an explicit FAUCET_PUBLIC_URL in a preview": {
			env:  map[string]string{envVercel: "1", envVercelEnv: "preview", envVercelProductionURL: production, envPublicURL: "https://events.example.com"},
			want: "https://events.example.com",
		},
	} {
		cfg, err := loadWith(c.env)
		require.NoError(t, err, name)
		if c.want == "" {
			require.Nil(t, cfg.publicURL, name)
		} else {
			require.NotNil(t, cfg.publicURL, name)
			require.Equal(t, c.want, cfg.publicURL.String(), name)
		}
		require.Equal(t, c.derived, cfg.publicURLDerived, name)
	}

	env := browserLoginEnv()
	delete(env, envPublicURL)
	env[envVercel], env[envVercelEnv], env[envVercelProductionURL] = "1", "production", production
	cfg, err := loadWith(env)
	require.NoError(t, err)
	require.NotNil(t, cfg.browserLogin, "the browser login takes the derived URL")
}

// A production URL that is no public URL stops the server, which names the
// variable and not the value.
func TestABadVercelProductionURLStopsTheServer(t *testing.T) {
	t.Parallel()
	for _, bad := range []string{"Faucet.Example.com", "faucet.example.com/path", "faucet.example.com:443", "user@faucet.example.com"} {
		_, err := loadWith(map[string]string{envVercel: "1", envVercelEnv: "production", envVercelProductionURL: bad})
		require.ErrorContains(t, err, envVercelProductionURL, bad)
		require.NotContains(t, err.Error(), bad)
	}
}
