package oauth

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// A grant that uses the authorization endpoint is one more row of the grant
// table, and the metadata names the endpoint and its response type.
func TestTheMetadataFollowsTheGrantTable(t *testing.T) {
	t.Parallel()
	table := map[string]grant{
		"client_credentials": grants["client_credentials"],
		"authorization_code": {authMethods: []string{"none"}, responseType: "code"},
		"refresh_token":      {authMethods: []string{"none"}},
	}
	m := metadataOf("https://faucet.example.com", table)
	require.Equal(t, "https://faucet.example.com/oauth/authorize", m.AuthorizationEndpoint)
	require.Equal(t, []string{"code"}, m.ResponseTypesSupported)
	require.Equal(t, []string{"authorization_code", "client_credentials", "refresh_token"}, m.GrantTypesSupported)
	require.Equal(t, []string{"client_secret_basic", "none"}, m.TokenEndpointAuthMethodsSupported)

	m = metadataOf("https://faucet.example.com", grants)
	require.Equal(t, "https://faucet.example.com/oauth/authorize", m.AuthorizationEndpoint, "listed though no grant in this build uses it")
	require.Equal(t, []string{}, m.ResponseTypesSupported)
}
