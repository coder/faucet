package oauth_test

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"cdr.dev/slog/v3"
	"cdr.dev/slog/v3/sloggers/sloghuman"
	"github.com/coder/faucet/faucet"
	"github.com/coder/faucet/faucet/oauth"
	"github.com/coder/quartz"
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

func TestATokenExchangeWaitsWhileTheKeysOfItsIssuerFail(t *testing.T) {
	t.Parallel()
	h, failKeys := newWorkloadHarness(t)
	failKeys()

	reply := h.post("", exchange(h.vercelToken()))
	require.Equal(t, http.StatusServiceUnavailable, reply.status, reply.rawBody)
	require.Equal(t, "60", reply.header.Get("Retry-After"))
	require.Equal(t, map[string]any{
		"error": "temporarily_unavailable", "error_description": "faucet could not fetch the keys of the subject token's issuer: try again in a minute",
	}, reply.body)
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
	for name, form := range map[string]url.Values{
		"a JWT":                     exchange(token),
		"an ID token":               exchange(token, "subject_token_type", "urn:ietf:params:oauth:token-type:id_token"),
		"an access token asked for": exchange(token, "requested_token_type", accessTokenType),
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
		form       url.Values
		code, want string
	}{
		"another audience":         {exchange(token, "audience", issuer+"/mcp"), "invalid_target", "the tokens of this client are for " + resource},
		"faucet as the audience":   {exchange(token, "audience", issuer), "invalid_target", "the tokens of this client are for " + resource},
		"another resource":         {exchange(token, "resource", issuer+"/mcp"), "invalid_target", "the tokens of this client are for " + resource},
		"a scope the client lacks": {exchange(token, "scope", "repo"), "invalid_scope", "the client holds none of the scopes asked for"},
	} {
		reply := h.post("", c.form)
		require.Equal(t, http.StatusBadRequest, reply.status, "%s: %s", name, reply.rawBody)
		require.Equal(t, map[string]any{"error": c.code, "error_description": c.want}, reply.body, name)
	}
	reply := h.post("", exchange(token, "audience", resource, "resource", resource))
	require.Equal(t, http.StatusOK, reply.status, reply.rawBody)
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

	for id, want := range map[string]struct {
		status      int
		code, about string
	}{
		"nobody":     {http.StatusUnauthorized, "invalid_client", "client authentication failed"},
		"Not_An_ID":  {http.StatusUnauthorized, "invalid_client", "client authentication failed"},
		"retired":    {http.StatusUnauthorized, "invalid_client", "client authentication failed"},
		"ci-watcher": {http.StatusUnauthorized, "invalid_client", "the client trusts no workload issuer: add one with faucet clients trust"},
		"elsewhere":  {http.StatusUnauthorized, "invalid_client", "which this server does not serve"},
		"other-team": {http.StatusBadRequest, "invalid_request", "the subject token is refused: it matches no trust rule of the client"},
	} {
		reply := h.post("", exchange(token, "client_id", id))
		require.Equal(t, want.status, reply.status, "%s: %s", id, reply.rawBody)
		require.Equal(t, want.code, reply.body["error"], id)
		require.Contains(t, reply.body["error_description"], want.about, id)
		require.NotContains(t, reply.rawBody, "team_other", "%s: no answer names a value that a rule pins", id)
	}
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
