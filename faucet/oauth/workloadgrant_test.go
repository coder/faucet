package oauth_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/stretchr/testify/require"

	"cdr.dev/slog/v3"
	"cdr.dev/slog/v3/sloggers/sloghuman"
	"github.com/coder/quartz"
	"github.com/coder/superblink-events/faucet"
	"github.com/coder/superblink-events/faucet/oauth"
)

const (
	tokenExchange   = "urn:ietf:params:oauth:grant-type:token-exchange" //nolint:gosec // A URN, not a credential.
	jwtTokenType    = "urn:ietf:params:oauth:token-type:jwt"            //nolint:gosec // A URN, not a credential.
	accessTokenType = "urn:ietf:params:oauth:token-type:access_token"   //nolint:gosec // A URN, not a credential.
)

func newWorkloadHarness(t *testing.T) (h *tokenHarness, failKeys func()) {
	t.Helper()
	h = newTokenHarness(t)
	client, failKeys := oauth.ServeVercel(t)
	oauth.WorkloadKeysThrough(h.server, client)
	h.addWorkloadClient("digest-bot", resource, oauth.VercelRule())
	return h, failKeys
}

func (h *tokenHarness) addWorkloadClient(id, endpoint string, rules ...oauth.TrustRule) {
	h.t.Helper()
	c, err := oauth.NewSecretlessClient(id, endpoint, bothScopes, []string{"repo:coder/faucet"}, h.clock.Now())
	require.NoError(h.t, err)
	require.NoError(h.t, h.store.InsertClient(context.Background(), c))
	for _, r := range rules {
		_, err := h.store.AddTrustRule(context.Background(), id, r, h.clock.Now())
		require.NoError(h.t, err)
	}
}

func (h *tokenHarness) vercelToken() string {
	h.t.Helper()
	return oauth.SignAsVercel(h.t, oauth.VercelFunctionClaims(h.clock.Now()))
}

func exchange(subjectToken string, extra ...string) url.Values {
	form := url.Values{"grant_type": {tokenExchange}, "client_id": {"digest-bot"}, "subject_token": {subjectToken}, "subject_token_type": {jwtTokenType}}
	for i := 0; i < len(extra); i += 2 {
		form.Set(extra[i], extra[i+1])
	}
	return form
}

func TestAVercelFunctionTradesItsOIDCTokenForATokenOfItsClient(t *testing.T) {
	t.Parallel()
	h, _ := newWorkloadHarness(t)
	now := h.clock.Now()

	reply := h.post("", exchange(h.vercelToken(), "scope", "events:read"))
	require.Equal(t, http.StatusOK, reply.status, reply.rawBody)
	require.Equal(t, "no-store", reply.header.Get("Cache-Control"))
	token, _ := reply.body["access_token"].(string)
	require.Regexp(t, `^fct_[A-Za-z0-9_-]{43}$`, token)
	require.Equal(t, map[string]any{
		"access_token": token, "token_type": "Bearer", "expires_in": float64(900), "scope": "events:read", "issued_token_type": accessTokenType,
	}, reply.body, "no refresh token: the workload trades a fresh token from its platform")
	stored, err := h.store.AccessToken(context.Background(), oauth.HashToken(token))
	require.NoError(t, err)
	require.Equal(t, oauth.AccessToken{
		Hash: oauth.HashToken(token), ExpiresAt: now.Add(15 * time.Minute), Scopes: []faucet.Scope{faucet.ScopeEventsRead},
		Resource: resource, SubjectKind: oauth.SubjectService, Subject: "digest-bot",
	}, stored)
	p, err := h.verify(token, resource)
	require.NoError(t, err)
	require.Equal(t, "svc:digest-bot", p.Login)

	claims := oauth.VercelFunctionClaims(now)
	claims["exp"] = now.Add(90 * time.Second).Unix()
	reply = h.post("", exchange(oauth.SignAsVercel(t, claims)))
	require.Equal(t, http.StatusOK, reply.status, reply.rawBody)
	require.Equal(t, float64(90), reply.body["expires_in"])
	stored, err = h.store.AccessToken(context.Background(), oauth.HashToken(reply.body["access_token"].(string)))
	require.NoError(t, err)
	require.Equal(t, now.Add(90*time.Second), stored.ExpiresAt, "the token ends with its subject token")
}

// Faucet answers expires_in in whole seconds, and golang.org/x/oauth2 takes 0
// for a token that never expires, so a subject token with less than a second
// left gets no token.
func TestAWorkloadTokenLastsAtLeastASecond(t *testing.T) {
	t.Parallel()
	h, _ := newWorkloadHarness(t)
	claims := oauth.VercelFunctionClaims(h.clock.Now())
	claims["exp"] = h.clock.Now().Add(time.Second).Unix()
	form := exchange(oauth.SignAsVercel(t, claims))

	reply := h.post("", form)
	require.Equal(t, http.StatusOK, reply.status, reply.rawBody)
	require.Equal(t, float64(1), reply.body["expires_in"])

	h.clock.Advance(time.Millisecond)
	reply = h.post("", form)
	require.Equal(t, http.StatusBadRequest, reply.status, reply.rawBody)
	require.Equal(t, map[string]any{
		"error": "invalid_request", "error_description": "the subject token is refused: it has expired, or expires in less than a second",
	}, reply.body)
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestAWorkloadTokenLastsFromItsMint(t *testing.T) {
	t.Parallel()
	for left, want := range map[time.Duration]int{10 * time.Second: 7, 3 * time.Second: 0} {
		h := newTokenHarness(t)
		keys, _ := oauth.ServeVercel(t)
		oauth.WorkloadKeysThrough(h.server, &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
			h.clock.Advance(1500 * time.Millisecond)
			return keys.Transport.RoundTrip(r)
		})})
		h.addWorkloadClient("digest-bot", resource, oauth.VercelRule())
		start := h.clock.Now()
		claims := oauth.VercelFunctionClaims(start)
		claims["exp"] = start.Add(left).Unix()

		reply := h.post("", exchange(oauth.SignAsVercel(t, claims)))
		require.Equal(t, start.Add(3*time.Second), h.clock.Now(), "the discovery document and the key set took 3 seconds")
		if want == 0 {
			require.Equal(t, http.StatusBadRequest, reply.status, reply.rawBody)
			require.Equal(t, map[string]any{
				"error": "invalid_request", "error_description": "the subject token is refused: it has expired, or expires in less than a second",
			}, reply.body, "%s left at the check", left)
			continue
		}
		require.Equal(t, http.StatusOK, reply.status, reply.rawBody)
		require.Equal(t, float64(want), reply.body["expires_in"], "%s left at the check", left)
		stored, err := h.store.AccessToken(context.Background(), oauth.HashToken(reply.body["access_token"].(string)))
		require.NoError(t, err)
		require.Equal(t, start.Add(left), stored.ExpiresAt)
	}
}

var waitForKeys = map[string]any{
	"error": "temporarily_unavailable", "error_description": "faucet cannot get the key of the subject token from its issuer yet: try again after Retry-After seconds",
}

func TestATokenExchangeWaitsWhileTheKeysOfItsIssuerFail(t *testing.T) {
	t.Parallel()
	h, failKeys := newWorkloadHarness(t)
	failKeys()

	reply := h.post("", exchange(h.vercelToken()))
	require.Equal(t, http.StatusServiceUnavailable, reply.status, reply.rawBody)
	require.Equal(t, "60", reply.header.Get("Retry-After"))
	require.Equal(t, waitForKeys, reply.body)
	require.True(t, logged(h.logs.String(), "[warn]", "a workload's token waits for the keys of its issuer", "client_id=digest-bot",
		"faucet could not fetch the keys of the subject token's issuer https://oidc.vercel.com/codercom: the key set: it must be served with status 200 and no redirect, not 404"), h.logs.String())

	h.clock.Advance(15*time.Second + time.Millisecond)
	reply = h.post("", exchange(h.vercelToken()))
	require.Equal(t, http.StatusServiceUnavailable, reply.status, reply.rawBody)
	require.Equal(t, "45", reply.header.Get("Retry-After"), "the failure is kept for a minute")
}

// A kid that keys fetched a moment ago lack gets the answer of a token that
// no rule verifies, at every retry, so that only an issuer that fails tells
// a caller without a trusted token that the client trusts it.
func TestAKidThatFreshKeysLackIsRefusedAtEveryRetry(t *testing.T) {
	t.Parallel()
	h, _ := newWorkloadHarness(t)
	reply := h.post("", exchange(h.vercelToken()))
	require.Equal(t, http.StatusOK, reply.status, reply.rawBody)

	for range 8 {
		h.clock.Advance(10 * time.Second)
		reply = h.post("", exchange(oauth.SignAsVercelUnder(t, "made-up", oauth.VercelFunctionClaims(h.clock.Now()))))
		require.Equal(t, http.StatusBadRequest, reply.status, reply.rawBody)
		require.Equal(t, refusedExchange, reply.body)
		require.Empty(t, reply.header.Get("Retry-After"))
	}
	require.True(t, logged(h.logs.String(), "[info]", "a token exchange was refused", "client_id=digest-bot", "its kid and alg name no key of its issuer"), h.logs.String())
}

func TestATokenExchangeCarriesNoSecretAndNoActor(t *testing.T) {
	t.Parallel()
	h, _ := newWorkloadHarness(t)
	token := h.vercelToken()

	for name, c := range map[string]struct {
		authorization string
		form          url.Values
		want          string
	}{
		"a secret in HTTP Basic":         {basic("digest-bot", "fcs_guess"), exchange(token), "a token exchange carries no client secret"},
		"HTTP Basic with no secret":      {basic("digest-bot", ""), exchange(token), "a token exchange carries no client secret"},
		"a client_secret":                {"", exchange(token, "client_secret", "fcs_guess"), "a token exchange carries no client secret"},
		"a client assertion":             {"", exchange(token, "client_assertion", token, "client_assertion_type", "urn:ietf:params:oauth:client-assertion-type:jwt-bearer"), "a token exchange carries no client secret and no client assertion"},
		"a client_assertion_type":        {"", exchange(token, "client_assertion_type", "urn:ietf:params:oauth:client-assertion-type:jwt-bearer"), "no client assertion"},
		"no client_id":                   {"", exchange(token, "client_id", ""), "client_id is required"},
		"no subject_token":               {"", exchange("", "subject_token", ""), "subject_token is required"},
		"no subject_token_type":          {"", exchange(token, "subject_token_type", ""), "subject_token_type must be " + jwtTokenType},
		"an access token as the subject": {"", exchange(token, "subject_token_type", accessTokenType), "subject_token_type must be " + jwtTokenType},
		"an actor_token":                 {"", exchange(token, "actor_token", token, "actor_token_type", jwtTokenType), "faucet takes no actor_token"},
		"an actor_token_type":            {"", exchange(token, "actor_token_type", jwtTokenType), "faucet takes no actor_token"},
		"a refresh token asked for":      {"", exchange(token, "requested_token_type", "urn:ietf:params:oauth:token-type:refresh_token"), "requested_token_type can only be " + accessTokenType},
	} {
		reply := h.post(c.authorization, c.form)
		require.Equal(t, http.StatusBadRequest, reply.status, "%s: %s", name, reply.rawBody)
		require.Equal(t, "invalid_request", reply.body["error"], name)
		require.Contains(t, reply.body["error_description"], c.want, name)
	}
	for name, header := range map[string][]string{"an empty Authorization header": {""}, "an empty one before HTTP Basic": {"", basic("digest-bot", "")}} {
		reply := h.postWith(header, exchange(token))
		require.Equal(t, http.StatusBadRequest, reply.status, "%s: %s", name, reply.rawBody)
		require.Contains(t, reply.body["error_description"], "a token exchange carries no client secret", name)
	}
	for name, form := range map[string]url.Values{
		"a JWT":                         exchange(token),
		"an ID token":                   exchange(token, "subject_token_type", "urn:ietf:params:oauth:token-type:id_token"),
		"an access token asked for":     exchange(token, "requested_token_type", accessTokenType),
		"an empty requested_token_type": exchange(token, "requested_token_type", ""),
	} {
		reply := h.post("", form)
		require.Equal(t, http.StatusOK, reply.status, "%s: %s", name, reply.rawBody)
	}
}

func TestATokenExchangeIsForTheClientsEndpoint(t *testing.T) {
	t.Parallel()
	h, _ := newWorkloadHarness(t)
	token := h.vercelToken()

	for name, c := range map[string]struct {
		extra      []string
		code, want string
	}{
		"another audience":         {[]string{"audience", issuer + "/mcp"}, "invalid_target", "the tokens of this client are for " + resource},
		"faucet as the audience":   {[]string{"audience", issuer}, "invalid_target", "the tokens of this client are for " + resource},
		"another resource":         {[]string{"resource", issuer + "/mcp"}, "invalid_target", "the tokens of this client are for " + resource},
		"a scope the client lacks": {[]string{"scope", "repo"}, "invalid_scope", "the client holds none of the scopes asked for"},
	} {
		reply := h.post("", exchange(token, c.extra...))
		require.Equal(t, http.StatusBadRequest, reply.status, "%s: %s", name, reply.rawBody)
		require.Equal(t, map[string]any{"error": c.code, "error_description": c.want}, reply.body, name)
		reply = h.post("", exchange("not-a-jwt", c.extra...))
		require.Equal(t, refusedExchange, reply.body, "%s with a token that does not verify", name)
	}
	for name, audiences := range map[string][]string{
		"the client's endpoint":     {resource},
		"an empty audience":         {""},
		"the endpoint twice":        {resource, resource},
		"the endpoint and an empty": {resource, ""},
	} {
		form := exchange(token, "resource", resource)
		form["audience"] = audiences
		reply := h.post("", form)
		require.Equal(t, http.StatusOK, reply.status, "%s: %s", name, reply.rawBody)
	}
	form := exchange(token)
	form["audience"] = []string{resource, issuer + "/mcp"}
	reply := h.post("", form)
	require.Equal(t, map[string]any{"error": "invalid_target", "error_description": "the tokens of this client are for " + resource}, reply.body, "each audience must be the client's endpoint")
}

var refusedExchange = map[string]any{
	"error": "invalid_client", "error_description": "the client takes no such subject token: check client_id and the token, and faucet's log for the reason",
}

func TestATokenExchangeNeedsALiveClientThatTrustsTheToken(t *testing.T) {
	t.Parallel()
	h, _ := newWorkloadHarness(t)
	token := h.vercelToken()
	otherTeam := oauth.VercelRule()
	otherTeam.Claims["owner_id"] = "team_other"
	h.addWorkloadClient("other-team", resource, otherTeam)
	h.addWorkloadClient("elsewhere", "https://faucet.other.example.com/github/mcp", oauth.VercelRule())
	h.addWorkloadClient("retired", resource, oauth.VercelRule())
	_, err := h.store.DisableClient(context.Background(), "retired", h.clock.Now())
	require.NoError(t, err)
	h.addWorkloadClient("cluster-bot", resource, oauth.TrustRule{Issuer: "https://kubernetes.example.com", Claims: map[string]string{"sub": "system:serviceaccount:apps:cluster-bot"}})
	otherKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	otherKeys := oauth.VercelRule()
	otherKeys.JWKS = trustKeySet(t, jose.JSONWebKey{Key: otherKey.Public(), KeyID: oauth.VercelKeyID, Use: "sig"})
	h.addWorkloadClient("other-keys", resource, otherKeys)

	for name, c := range map[string]struct {
		client, subject, reason string
	}{
		"no client ID":                       {"Not_An_ID", token, "client_id is not a client ID"},
		"an unknown client":                  {"nobody", token, "no client has this ID"},
		"a disabled client":                  {"retired", token, "the client is disabled"},
		"a client of another server":         {"elsewhere", token, "the client is for https://faucet.other.example.com/github/mcp, which this server does not serve"},
		"a client with no rule":              {"ci-watcher", token, "the client trusts no workload issuer: add one with faucet clients trust"},
		"a client that trusts other issuers": {"cluster-bot", token, "the subject token is refused: it matches no trust rule of the client"},
		"a client that trusts other keys":    {"other-keys", token, "the subject token is refused: its signature does not verify with its issuer's key"},
		"a token that is no JWT":             {"digest-bot", "not-a-jwt", "the subject token is refused: it must be a JWT in compact form, signed with RS256 or ES256"},
	} {
		reply := h.post("", exchange(c.subject, "client_id", c.client))
		require.Equal(t, http.StatusBadRequest, reply.status, "%s: %s", name, reply.rawBody)
		require.Empty(t, reply.header.Values("WWW-Authenticate"), "%s: the client sends no Authorization header", name)
		require.Equal(t, refusedExchange, reply.body, name)
		require.True(t, logged(h.logs.String(), "[info]", "a token exchange was refused", "client_id="+c.client, c.reason), "%s: %s", name, h.logs.String())
	}

	reply := h.post("", exchange(token, "client_id", "other-team"))
	require.Equal(t, http.StatusBadRequest, reply.status, reply.rawBody)
	require.Equal(t, map[string]any{
		"error": "invalid_request", "error_description": "the subject token is refused: it matches no trust rule of the client",
	}, reply.body, "the token's signature verified under the client's rule")
	require.NotContains(t, reply.rawBody, "team_other", "no answer names a value that a rule pins")
	require.True(t, logged(h.logs.String(), "[info]", "a token exchange was refused", "client_id=other-team", "it matches no trust rule of the client"), h.logs.String())
	require.NotContains(t, h.logs.String(), token)
}

func logged(logs string, parts ...string) bool {
	for line := range strings.Lines(logs) {
		if !slices.ContainsFunc(parts, func(p string) bool { return !strings.Contains(line, p) }) {
			return true
		}
	}
	return false
}

func TestNewServerWarnsOfAWorkloadKeyClientThatReachesLoopback(t *testing.T) {
	t.Parallel()
	const warning = "the client that fetches the keys of workload issuers reaches loopback over http, which is for tests only"
	for loopback, want := range map[bool]bool{true: true, false: false} {
		logs := &loginLogs{}
		_, err := oauth.NewServer(oauth.ServerDeps{
			Store: newFakeStore(), Clock: quartz.NewMock(t), Logger: slog.Make(sloghuman.Sink(logs)), Issuer: issuer,
			Resources: []string{resource}, WorkloadKeys: faucet.HTTPClientOptions{AllowInsecureLoopback: loopback},
		})
		require.NoError(t, err)
		require.Equal(t, want, strings.Contains(logs.String(), "[warn]") && strings.Contains(logs.String(), warning), "AllowInsecureLoopback %v: %s", loopback, logs.String())
	}
}
