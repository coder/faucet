package oauth_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/coder/superblink-events/faucet/oauth"
)

// revoke sends form to the revocation endpoint with authorization as the
// Authorization header, none when it is empty.
func (h *grantHarness) revoke(authorization string, form url.Values) tokenReply {
	h.t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, h.srv.URL+oauth.RevokePath, strings.NewReader(form.Encode()))
	require.NoError(h.t, err)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	resp, err := h.srv.Client().Do(req)
	require.NoError(h.t, err)
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(h.t, err)
	reply := tokenReply{status: resp.StatusCode, header: resp.Header, rawBody: string(raw)}
	if len(raw) > 0 {
		require.NoError(h.t, json.Unmarshal(raw, &reply.body), "%q", raw)
	}
	return reply
}

// grantRevokeForm is the revocation request of the public client of the
// harness for token.
func grantRevokeForm(token string) url.Values {
	return url.Values{"token": {token}, "client_id": {grantClientID}}
}

func requireRevoked(t *testing.T, reply tokenReply, name string) {
	t.Helper()
	require.Equal(t, http.StatusOK, reply.status, "%s: %s", name, reply.rawBody)
	require.Equal(t, "no-store", reply.header.Get("Cache-Control"), name)
}

// A client logs out by revoking its refresh token (RFC 7009): the session
// ends, so neither of its tokens works, and the user's other sessions live
// on. Revoking it again is no error.
func TestRevokingARefreshTokenEndsItsSession(t *testing.T) {
	t.Parallel()
	h := newGrantHarness(t, newFakeStore())
	s := h.redeem(h.approve("login-1", 1))
	other := h.redeem(h.approve("login-2", 1))

	requireRevoked(t, h.revoke("", grantRevokeForm(s.refresh)), "the refresh token")
	require.False(t, h.session(s.id).Live())
	grantRequireRefused(t, h.post("", grantRefreshForm(s.refresh)), http.StatusBadRequest, "invalid_grant", "the revoked refresh token")
	_, err := h.verify(s.access)
	require.Error(t, err, "the access token of the ended session")
	requireRevoked(t, h.revoke("", grantRevokeForm(s.refresh)), "the refresh token again")
	h.refresh(other)
}

// A revoked access token stops working at once, and its session lives on.
// The hint of the token's type is only a hint.
func TestRevokingAnAccessTokenEndsThatTokenOnly(t *testing.T) {
	t.Parallel()
	h := newGrantHarness(t, newFakeStore())
	s := h.redeem(h.approve("login-1", 1))

	requireRevoked(t, h.revoke(basic(grantClientID, ""), url.Values{"token": {s.access}, "token_type_hint": {"refresh_token"}}), "the access token")
	_, err := h.verify(s.access)
	require.Error(t, err, "the revoked access token")
	require.True(t, h.session(s.id).Live())
	h.principal(h.refresh(s))
}

// A token that faucet does not hold, of any form, is no error (RFC 7009
// section 2.2), and the revocation changes nothing.
func TestRevokingATokenThatFaucetDoesNotHoldChangesNothing(t *testing.T) {
	t.Parallel()
	h := newGrantHarness(t, newFakeStore())
	s := h.redeem(h.approve("login-1", 1))

	for name, token := range map[string]string{
		"an unknown refresh token":   "fcr_" + grantSecret(t),
		"an unknown access token":    "fct_" + grantSecret(t),
		"a refresh token cut short":  s.refresh[:len(s.refresh)-1],
		"a GitHub token":             "ghp_" + strings.Repeat("a", 36),
		"no token of any form":       "not a token",
		"an upper-case refresh name": "FCR_" + strings.TrimPrefix(s.refresh, "fcr_"),
	} {
		requireRevoked(t, h.revoke("", grantRevokeForm(token)), name)
	}
	require.True(t, h.session(s.id).Live())
	h.principal(s)
	h.refresh(s)
}

// A revocation authenticates its client as the token endpoint does, and a
// token issued to another client stays as it is.
func TestARevocationRefusesAnotherClientAndAMalformedRequest(t *testing.T) {
	t.Parallel()
	h := newGrantHarness(t, newFakeStore())
	s := h.redeem(h.approve("login-1", 1))
	const otherClient = "https://other.example.com/client.json"

	for name, c := range map[string]struct {
		authorization string
		form          url.Values
		status        int
		code          string
	}{
		"another client's refresh token": {"", url.Values{"token": {s.refresh}, "client_id": {otherClient}}, http.StatusBadRequest, "invalid_grant"},
		"another client's access token":  {basic(otherClient, ""), url.Values{"token": {s.access}}, http.StatusBadRequest, "invalid_grant"},
		"no client_id":                   {"", url.Values{"token": {s.refresh}}, http.StatusBadRequest, "invalid_request"},
		"a client_secret":                {"", url.Values{"token": {s.refresh}, "client_id": {grantClientID}, "client_secret": {"secret"}}, http.StatusBadRequest, "invalid_request"},
		"an unknown service client":      {basic("ci-watcher", "fcs_"+grantSecret(t)), url.Values{"token": {s.access}}, http.StatusUnauthorized, "invalid_client"},
		"no token":                       {"", url.Values{"client_id": {grantClientID}}, http.StatusBadRequest, "invalid_request"},
		"two tokens":                     {"", url.Values{"token": {s.refresh, s.access}, "client_id": {grantClientID}}, http.StatusBadRequest, "invalid_request"},
	} {
		grantRequireRefused(t, h.revoke(c.authorization, c.form), c.status, c.code, name)
	}
	require.True(t, h.session(s.id).Live())
	h.principal(s)
	h.refresh(s)
}

// A service client revokes its own access tokens with its secret, and no
// token of a user. A public client revokes no token of a service.
func TestAServiceClientRevokesItsOwnAccessTokensOnly(t *testing.T) {
	t.Parallel()
	h := newGrantHarness(t, newFakeStore())
	s := h.redeem(h.approve("login-1", 1))
	c, secret, err := oauth.NewClient("ci-watcher", resource, bothScopes, []string{"repo:coder/faucet"}, h.clock.Now())
	require.NoError(t, err)
	require.NoError(t, h.store.InsertClient(context.Background(), c))
	service := basic("ci-watcher", secret)
	issued := h.post(service, url.Values{"grant_type": {"client_credentials"}})
	require.Equal(t, http.StatusOK, issued.status, issued.rawBody)
	token, _ := issued.body["access_token"].(string)

	grantRequireRefused(t, h.revoke(service, url.Values{"token": {s.access}}), http.StatusBadRequest, "invalid_grant", "a user's access token")
	grantRequireRefused(t, h.revoke(service, url.Values{"token": {s.refresh}}), http.StatusBadRequest, "invalid_grant", "a user's refresh token")
	grantRequireRefused(t, h.revoke("", grantRevokeForm(token)), http.StatusBadRequest, "invalid_grant", "a service's token by a public client")
	h.principal(s)
	_, err = h.store.AccessToken(context.Background(), oauth.HashToken(token))
	require.NoError(t, err, "the service's token is still there")

	requireRevoked(t, h.revoke(service, url.Values{"token": {token}}), "its own access token")
	_, err = h.store.AccessToken(context.Background(), oauth.HashToken(token))
	require.ErrorIs(t, err, oauth.ErrUnknownToken)
}
