package oauth_test

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"strings"
	"testing"

	"github.com/go-jose/go-jose/v4"
	"github.com/stretchr/testify/require"

	"github.com/coder/faucet/faucet/oauth"
)

// The IDs of the example token on vercel.com/docs/oidc/reference.
const (
	trustVercelTeam    = "team_7Gw5ZMzpQA8h90F832KGp7nwbuh3"
	trustVercelProject = "prj_7Gw5ZMBpQA8h9GF832KGp7nwbuh3"
)

func trustVercelRule(claims map[string]string) oauth.TrustRule {
	return oauth.TrustRule{Issuer: "https://oidc.vercel.com/codercom", Claims: claims}
}

func trustVercelPins() map[string]string {
	return map[string]string{"owner_id": trustVercelTeam, "project_id": trustVercelProject, "environment": "production"}
}

func trustKeySet(t *testing.T, keys ...jose.JSONWebKey) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(jose.JSONWebKeySet{Keys: keys})
	require.NoError(t, err)
	return b
}

func TestAVercelRuleMustPinTheTeamTheProjectAndProduction(t *testing.T) {
	t.Parallel()
	require.NoError(t, oauth.ValidateTrustRule(trustVercelRule(trustVercelPins())))
	withSub := trustVercelPins()
	withSub["sub"] = "owner:codercom:project:digest-bot:environment:production"
	require.NoError(t, oauth.ValidateTrustRule(trustVercelRule(withSub)), "a name pinned next to the IDs only narrows the rule")

	without := func(claim string) map[string]string {
		pins := trustVercelPins()
		delete(pins, claim)
		return pins
	}
	with := func(claim, value string) map[string]string {
		pins := trustVercelPins()
		pins[claim] = value
		return pins
	}
	for name, c := range map[string]struct {
		claims map[string]string
		want   string
	}{
		"no team ID": {without("owner_id"), "must pin owner_id: Vercel signs the tokens of every team with the same keys"},
		"names only": {
			map[string]string{"owner": "codercom", "project": "digest-bot", "sub": "owner:codercom:project:digest-bot:environment:production", "environment": "production"},
			"must pin owner_id",
		},
		"no project ID":  {without("project_id"), "must pin project_id: every project of the team can get a token for faucet"},
		"no environment": {without("environment"), "must pin environment=production: a preview deployment runs any branch"},
		"preview":        {with("environment", "preview"), "must pin environment=production"},
		"development":    {with("environment", "development"), "must pin environment=production"},
	} {
		err := oauth.ValidateTrustRule(trustVercelRule(c.claims))
		require.ErrorContains(t, err, "a trust rule for https://oidc.vercel.com/codercom "+c.want, name)
	}
}

// Every Vercel team shares the global issuer, and a Vercel issuer that is not
// a team's own names no token.
func TestFaucetTrustsOnlyTheIssuerOfOneVercelTeam(t *testing.T) {
	t.Parallel()
	for _, issuer := range []string{"https://oidc.vercel.com", "https://oidc.vercel.com/"} {
		err := oauth.ValidateTrustRule(oauth.TrustRule{Issuer: issuer, Claims: trustVercelPins()})
		require.ErrorContains(t, err, "faucet never trusts "+issuer+", which every Vercel team shares: trust the team's issuer, https://oidc.vercel.com/<team>")
	}
	for _, issuer := range []string{
		"https://oidc.vercel.com/codercom/", "https://oidc.vercel.com/codercom/x", "https://oidc.vercel.com:443/codercom", "https://OIDC.vercel.com/codercom",
	} {
		err := oauth.ValidateTrustRule(oauth.TrustRule{Issuer: issuer, Claims: trustVercelPins()})
		require.ErrorContains(t, err, "a Vercel issuer is https://oidc.vercel.com/<team>", issuer)
	}
}

// Every GitHub account and every gitlab.com namespace gets its tokens from
// the same issuer, and both let a repository choose the parts of sub, so a
// rule pins the IDs of the owner and of the repository.
func TestARuleForGitHubActionsOrGitLabMustPinTheOwnerAndRepositoryIDs(t *testing.T) {
	t.Parallel()
	for issuer, ids := range map[string][2]string{
		"https://token.actions.githubusercontent.com":               {"repository_owner_id", "repository_id"},
		"https://token.actions.githubusercontent.com/octo-business": {"repository_owner_id", "repository_id"},
		"https://gitlab.com": {"namespace_id", "project_id"},
	} {
		owner, repository := ids[0], ids[1]
		rule := func(claims map[string]string) oauth.TrustRule { return oauth.TrustRule{Issuer: issuer, Claims: claims} }
		require.NoError(t, oauth.ValidateTrustRule(rule(map[string]string{owner: "65", repository: "74"})), issuer)
		for _, c := range []struct {
			claims map[string]string
			want   string
		}{
			{map[string]string{"sub": "environment:production"}, owner},
			{map[string]string{owner: "65"}, repository},
			{map[string]string{repository: "74", "sub": "repo:octo-org/octo-repo:ref:refs/heads/main"}, owner},
		} {
			err := oauth.ValidateTrustRule(rule(c.claims))
			require.ErrorContains(t, err, "a trust rule for "+issuer+" must pin "+c.want+": ", issuer)
		}
	}
}

func TestARuleOfAnyOtherIssuerMustPinSub(t *testing.T) {
	t.Parallel()
	const issuer = "https://container.googleapis.com/v1/projects/p/locations/l/clusters/c"
	require.NoError(t, oauth.ValidateTrustRule(oauth.TrustRule{Issuer: issuer, Claims: map[string]string{"sub": "system:serviceaccount:ns:sa"}}))
	err := oauth.ValidateTrustRule(oauth.TrustRule{Issuer: issuer, Claims: map[string]string{"kubernetes.io/namespace": "ns"}})
	require.ErrorContains(t, err, "a trust rule for "+issuer+" must pin sub: every workload of the issuer can get a token for faucet")
}

func TestARuleNamesAnHTTPSIssuerAndPinsAClaimToAValue(t *testing.T) {
	t.Parallel()
	sub := map[string]string{"sub": "system:serviceaccount:ns:sa"}
	for name, c := range map[string]struct {
		rule oauth.TrustRule
		want string
	}{
		"http":           {oauth.TrustRule{Issuer: "http://issuer.example.com", Claims: sub}, "must be an https URL"},
		"no host":        {oauth.TrustRule{Issuer: "https:///path", Claims: sub}, "must be an https URL"},
		"a query":        {oauth.TrustRule{Issuer: "https://issuer.example.com?a=b", Claims: sub}, "no user info, query or fragment"},
		"a fragment":     {oauth.TrustRule{Issuer: "https://issuer.example.com#a", Claims: sub}, "no user info, query or fragment"},
		"user info":      {oauth.TrustRule{Issuer: "https://u@issuer.example.com", Claims: sub}, "no user info, query or fragment"},
		"too long":       {oauth.TrustRule{Issuer: "https://issuer.example.com/" + strings.Repeat("a", 2048), Claims: sub}, "at most 2048 bytes"},
		"no claim":       {oauth.TrustRule{Issuer: "https://issuer.example.com"}, "must pin at least one claim, or every token of its issuer matches"},
		"an empty value": {oauth.TrustRule{Issuer: "https://issuer.example.com", Claims: map[string]string{"sub": ""}}, "neither the name nor the value can be empty"},
		"an empty name":  {oauth.TrustRule{Issuer: "https://issuer.example.com", Claims: map[string]string{"sub": "a", "": "b"}}, "neither the name nor the value can be empty"},
		"a bare query":   {oauth.TrustRule{Issuer: "https://issuer.example.com?", Claims: sub}, "no user info, query or fragment"},
		"17 claims":      {oauth.TrustRule{Issuer: "https://issuer.example.com", Claims: claimsOfSize(17, 8)}, "a trust rule pins at most 16 claims"},
		"a long name":    {oauth.TrustRule{Issuer: "https://issuer.example.com", Claims: map[string]string{"sub": "a", strings.Repeat("n", 257): "b"}}, "a trust rule's claim names and values must be at most 256 bytes"},
		"a long value":   {oauth.TrustRule{Issuer: "https://issuer.example.com", Claims: map[string]string{"sub": strings.Repeat("v", 257)}}, "must be at most 256 bytes"},
	} {
		require.ErrorContains(t, oauth.ValidateTrustRule(c.rule), c.want, name)
	}
	require.NoError(t, oauth.ValidateTrustRule(oauth.TrustRule{Issuer: "https://issuer.example.com", Claims: claimsOfSize(16, 256)}))
}

// claimsOfSize pins sub and n-1 other claims, each name and value of size
// bytes.
func claimsOfSize(n, size int) map[string]string {
	claims := map[string]string{"sub": strings.Repeat("s", size)}
	for i := range n - 1 {
		claims[fmt.Sprintf("%03d", i)+strings.Repeat("n", size-3)] = strings.Repeat("v", size)
	}
	return claims
}

// Faucet checks the registered claims of a token itself, and a pin of one
// matches no token, or one token at most.
func TestARuleCannotPinARegisteredClaim(t *testing.T) {
	t.Parallel()
	sub := map[string]string{"sub": "system:serviceaccount:ns:sa"}
	for _, claim := range []string{"iss", "aud", "exp", "nbf", "iat", "jti"} {
		claims := maps.Clone(sub)
		claims[claim] = "https://issuer.example.com"
		err := oauth.ValidateTrustRule(oauth.TrustRule{Issuer: "https://issuer.example.com", Claims: claims})
		require.ErrorContains(t, err, "a trust rule cannot pin "+claim+": ", claim)
	}
}

func TestAPastedKeySetHoldsAPublicKeyForSignatures(t *testing.T) {
	t.Parallel()
	vercelKeys, err := os.ReadFile("testdata/vercel-codercom-jwks.json")
	require.NoError(t, err)
	rule := trustVercelRule(trustVercelPins())
	rule.JWKS = vercelKeys
	require.NoError(t, oauth.ValidateTrustRule(rule), "Vercel's real key set")

	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	p384, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	require.NoError(t, err)
	sub := map[string]string{"sub": "system:serviceaccount:ns:sa"}
	public, err := jose.JSONWebKey{Key: &rsaKey.PublicKey, KeyID: "a", Use: "sig"}.MarshalJSON()
	require.NoError(t, err)
	b64 := base64.RawURLEncoding.EncodeToString
	withPrimes := strings.TrimSuffix(string(public), "}") + `,"p":"` + b64(rsaKey.Primes[0].Bytes()) + `","q":"` + b64(rsaKey.Primes[1].Bytes()) + `"}`
	x25519 := `{"kty":"OKP","crv":"X25519","kid":"b","x":"` + b64(bytes.Repeat([]byte{1}, 32)) + `","d":"` + b64(bytes.Repeat([]byte{2}, 32)) + `"}`
	for name, c := range map[string]struct {
		jwks json.RawMessage
		want string
	}{
		"a private key that go-jose cannot read": {json.RawMessage(`{"keys":[` + string(public) + `,` + x25519 + `]}`), "must hold no private key"},
		"an RSA key with its primes":             {json.RawMessage(`{"keys":[` + withPrimes + `]}`), "must hold no private key"},
		"a key without a kid":                    {trustKeySet(t, jose.JSONWebKey{Key: &rsaKey.PublicKey, Use: "sig"}), "for signatures, named by a kid"},
		"not JSON":                               {json.RawMessage(`keys`), "a trust rule's JWKS must be a JSON object with a keys array"},
		"a private key":                          {trustKeySet(t, jose.JSONWebKey{Key: rsaKey, KeyID: "a"}), "a trust rule's JWKS must hold no private key"},
		"a symmetric key":                        {trustKeySet(t, jose.JSONWebKey{Key: []byte("0123456789abcdef0123456789abcdef"), KeyID: "a"}), "must hold no private key"},
		"no key":                                 {json.RawMessage(`{"keys":[]}`), "must hold an RSA key of 2048 bits or more, or a P-256 key, for signatures"},
		"an unknown key":                         {json.RawMessage(`{"keys":[{"kty":"XYZ","kid":"a"}]}`), "must hold an RSA key"},
		"a 1024-bit key": {
			json.RawMessage(`{"keys":[{"kty":"RSA","kid":"a","e":"AQAB","n":"` + base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0xff}, 128)) + `"}]}`),
			"must hold an RSA key",
		},
		"a P-384 key":      {trustKeySet(t, jose.JSONWebKey{Key: &p384.PublicKey, KeyID: "a"}), "must hold an RSA key"},
		"a key to encrypt": {trustKeySet(t, jose.JSONWebKey{Key: &rsaKey.PublicKey, KeyID: "a", Use: "enc"}), "must hold an RSA key"},
		"another alg":      {trustKeySet(t, jose.JSONWebKey{Key: &rsaKey.PublicKey, KeyID: "a", Algorithm: "PS256"}), "must hold an RSA key"},
		"too long":         {json.RawMessage(`{"keys":[],"x":"` + strings.Repeat("a", 32<<10) + `"}`), "a trust rule's JWKS must be at most 32768 bytes"},
	} {
		err := oauth.ValidateTrustRule(oauth.TrustRule{Issuer: "https://kubernetes.default.svc", Claims: sub, JWKS: c.jwks})
		require.ErrorContains(t, err, c.want, name)
	}
}
