package oauth_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/coder/faucet/faucet/oauth"
)

// token sends form to the token endpoint, as a client does.
func (h *loginHarness) token(form url.Values) tokenReply {
	h.t.Helper()
	req, err := http.NewRequestWithContext(h.t.Context(), http.MethodPost, h.issuer+oauth.TokenPath, strings.NewReader(form.Encode()))
	require.NoError(h.t, err)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := (&http.Client{Transport: h.transport}).Do(req)
	require.NoError(h.t, err)
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(h.t, err)
	reply := tokenReply{status: resp.StatusCode, header: resp.Header, rawBody: string(raw)}
	require.NoError(h.t, json.Unmarshal(raw, &reply.body), string(raw))
	return reply
}

// loginSession logs the user in through c in a new browser, and redeems the
// code as c does.
func (h *loginHarness) loginSession(c loginClient) grantSession {
	h.t.Helper()
	b := h.newBrowser(h.t)
	consent, back := b.loginBack(c)
	code := h.loginRequireCode(h.t, b.get(back), c)
	return grantTokens(h.t, consent.id, h.token(url.Values{
		"grant_type": {"authorization_code"}, "code": {code}, "code_verifier": {c.verifier()},
		"redirect_uri": {c.redirectURI}, "client_id": {c.id},
	}))
}

// refreshSession presents the refresh token of s as c does.
func (h *loginHarness) refreshSession(c loginClient, s grantSession) tokenReply {
	h.t.Helper()
	return h.token(url.Values{"grant_type": {"refresh_token"}, "refresh_token": {s.refresh}, "client_id": {c.id}})
}

// faucet/6: the user revokes faucet's GitHub App at GitHub, which faucet does
// not see, and logs in again through another client. The login gives the
// user's connection new GitHub tokens, which must not reach the sessions of
// the authorization that the user revoked.
func TestALoginAfterTheUserRevokedTheAppEndsTheSessionsOfTheRevokedAuthorization(t *testing.T) {
	t.Parallel()
	h := newLoginHarness(t)
	oldClient, newClient := h.newClient("old", nil), h.newClient("new", nil)
	old := h.loginSession(oldClient)
	h.github.revoke()
	fresh := h.loginSession(newClient)

	grantRequireRefused(t, h.refreshSession(oldClient, old), http.StatusBadRequest, "invalid_grant", "the session of the revoked authorization")
	grantTokens(t, fresh.id, h.refreshSession(newClient, fresh))
}

// A user logs in through a second client while the first one holds a
// session. The login keeps that session only when GitHub still takes the
// tokens that the login replaces: with the access token when it lasts, with
// the refresh token when it expired.
func TestALoginKeepsTheSessionsOfTheUserOnlyWhileGitHubTakesTheirAuthorization(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		between func(h *loginHarness)
		kept    bool
		// refreshes is how many refresh tokens GitHub got.
		refreshes int
	}{
		"the user kept the App": {between: func(*loginHarness) {}, kept: true},
		"the user kept the App and the GitHub token expired": {
			between: func(h *loginHarness) { h.clock.Advance(8 * time.Hour) }, kept: true, refreshes: 1,
		},
		"the user revoked the App and the GitHub token expired": {
			between: func(h *loginHarness) { h.github.revoke(); h.clock.Advance(8 * time.Hour) }, refreshes: 1,
		},
		"GitHub does not answer for the GitHub token": {
			between: func(h *loginHarness) {
				h.github.set(func(g *loginGitHub) { g.userStatus[g.issued[0].access] = http.StatusBadGateway })
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newLoginHarness(t)
			oldClient, newClient := h.newClient("old", nil), h.newClient("new", nil)
			old := h.loginSession(oldClient)
			c.between(h)
			fresh := h.loginSession(newClient)

			reply := h.refreshSession(oldClient, old)
			if c.kept {
				grantTokens(t, old.id, reply)
			} else {
				grantRequireRefused(t, reply, http.StatusBadRequest, "invalid_grant", "the session of the replaced tokens")
			}
			grantTokens(t, fresh.id, h.refreshSession(newClient, fresh))
			require.Equal(t, c.refreshes, h.github.refreshCount())
		})
	}
}

// A client that the user let in before revoking the App holds its code
// until the user logs in again. The code then makes no session.
func TestACodeOfTheRevokedAuthorizationMakesNoSessionAfterTheNextLogin(t *testing.T) {
	t.Parallel()
	h := newLoginHarness(t)
	oldClient, newClient := h.newClient("old", nil), h.newClient("new", nil)
	b := h.newBrowser(t)
	_, back := b.loginBack(oldClient)
	code := h.loginRequireCode(t, b.get(back), oldClient)
	h.github.revoke()
	h.loginSession(newClient)

	grantRequireRefused(t, h.token(url.Values{
		"grant_type": {"authorization_code"}, "code": {code}, "code_verifier": {oldClient.verifier()},
		"redirect_uri": {oldClient.redirectURI}, "client_id": {oldClient.id},
	}), http.StatusBadRequest, "invalid_grant", "the code of the revoked authorization")
}
