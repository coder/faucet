package oauth_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"cdr.dev/slog/v3"
	"github.com/coder/superblink-events/faucet"
	"github.com/coder/superblink-events/faucet/internal/githubapi"
	"github.com/coder/superblink-events/faucet/internal/pgtest"
	"github.com/coder/superblink-events/faucet/oauth"
	"github.com/coder/superblink-events/faucet/pgstore"
)

// usertokensStores are the stores that the tests of a GitHub refresh run
// on: the row lock of Postgres is what keeps two refreshes apart there.
var usertokensStores = map[string]func(t *testing.T) oauth.Store{
	"fake":     func(*testing.T) oauth.Store { return newFakeStore() },
	"postgres": func(t *testing.T) oauth.Store { return pgstore.New(pgtest.NewDB(t, pgstore.Migrate)) },
}

// usertokensSoon is a time at which the GitHub tokens that a harness gave
// out at its start expire within the refresh grant's ten minutes.
const usertokensSoon = 8*time.Hour - 5*time.Minute

// Several agents of one user refresh at once while the user's GitHub token
// is about to expire. GitHub's refresh tokens work once, so a second refresh
// with the old one is refused, which ends every session of the user. With
// one GitHub refresh per connection at a time, which reads the pair again
// under the lock, GitHub sees one refresh and every session stays (R2).
func TestSessionsOfOneUserThatRefreshAtOnceRefreshTheGitHubTokenOnce(t *testing.T) {
	t.Parallel()
	for name, newStore := range usertokensStores {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newGrantHarness(t, newStore(t))
			var sessions []grantSession
			for _, id := range []string{"login-1", "login-2", "login-3"} {
				sessions = append(sessions, h.redeem(h.approve(id, 1)))
			}
			h.clock.Advance(usertokensSoon)

			var forms []url.Values
			for _, s := range sessions {
				forms = append(forms, grantRefreshForm(s.refresh))
			}
			replies := h.raceAll(forms...)
			refreshes, _ := h.github.counts()
			require.Equal(t, 1, refreshes, "GitHub saw one refresh")
			for i, s := range sessions {
				next := grantTokens(t, s.id, replies[i])
				require.NotEqual(t, s.refresh, next.refresh, s.id)
				require.NotEqual(t, s.access, next.access, s.id)
				session := h.session(s.id)
				require.True(t, session.Live(), s.id)
				require.False(t, h.refreshToken(s.id, next.refresh).Used(), s.id)
			}
			c, err := h.store.Connection(context.Background(), oauth.ConnectionID(oauth.ProviderGitHub, 1))
			require.NoError(t, err)
			require.True(t, h.clock.Now().Add(8*time.Hour).Equal(c.AccessExpiresAt), "the connection holds GitHub's new pair")
		})
	}
}

// GitHub refusing the refresh token of a connection ends the sessions of
// that connection, and of no other (R2).
func TestGitHubRefusingARefreshTokenEndsTheSessionsOfThatUserOnly(t *testing.T) {
	t.Parallel()
	for name, newStore := range usertokensStores {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newGrantHarness(t, newStore(t))
			first := h.redeem(h.approve("login-a1", 1))
			h.redeem(h.approve("login-a2", 1))
			other := h.redeem(h.approve("login-b", 2))
			h.github.revoke(1)
			h.clock.Advance(usertokensSoon)

			grantRequireRefused(t, h.post("", grantRefreshForm(first.refresh)), http.StatusBadRequest, "invalid_grant", "a refused GitHub refresh")
			require.False(t, h.session("login-a1").Live())
			require.False(t, h.session("login-a2").Live())
			require.True(t, h.session("login-b").Live())
			h.refresh(other)
		})
	}
}

// verify verifies an access token of the harness for resource, with the
// resolver of users.
func (h *grantHarness) verify(token string) (faucet.Principal, error) {
	h.t.Helper()
	v, err := oauth.NewVerifier(h.store, h.clock, map[oauth.SubjectKind]oauth.SubjectResolver{oauth.SubjectUser: oauth.UserResolver(h.store)})
	require.NoError(h.t, err)
	return v.Verify(context.Background(), token, resource)
}

func (h *grantHarness) principal(s grantSession) faucet.Principal {
	h.t.Helper()
	p, err := h.verify(s.access)
	require.NoError(h.t, err, s.id)
	return p
}

// userAccess is the access check of the harness's users, which asks the
// GitHub stub.
func (h *grantHarness) userAccess() oauth.UserAccess {
	h.t.Helper()
	a, err := oauth.NewUserAccess(oauth.UserAccessDeps{
		Store: h.store, Clock: h.clock, Logger: slog.Make(), Login: *h.login, Repos: h.repos,
	})
	require.NoError(h.t, err)
	return a
}

// The principal of an access token carries the token's own session, never
// another session of the same user, so that the access check of one session
// is never taken for another's (R13).
func TestEachSessionOfAUserResolvesToItsOwnSession(t *testing.T) {
	t.Parallel()
	h := newGrantHarness(t, newFakeStore())
	first := h.redeem(h.approve("login-1", 1))
	second := h.redeem(h.approve("login-2", 1))
	connection := oauth.ConnectionID(oauth.ProviderGitHub, 1)

	for _, s := range []grantSession{first, second, h.refresh(first)} {
		p := h.principal(s)
		session, ok := p.OAuthSession()
		require.True(t, ok, s.id)
		require.Equal(t, faucet.OAuthSession{SessionID: s.id, ConnectionID: connection}, session, s.id)
		require.Equal(t, "user1", p.Login, s.id)
		require.Equal(t, bothScopes, p.Scopes(), s.id)
	}
	p, err := oauth.UserResolver(h.store)(context.Background(), "login-2", []faucet.Scope{faucet.ScopeEventsRead})
	require.NoError(t, err)
	session, _ := p.OAuthSession()
	require.Equal(t, "login-2", session.SessionID)
	require.Equal(t, []faucet.Scope{faucet.ScopeEventsRead}, p.Scopes(), "the principal holds the token's scopes")
}

// An access check of one session does not answer for another session of
// the user that logged in after it: that one asks GitHub again, and GitHub's
// answer stands (R13).
func TestAnAccessCheckOfOneSessionDoesNotAnswerForAnother(t *testing.T) {
	t.Parallel()
	h := newGrantHarness(t, newFakeStore())
	first := h.principal(h.redeem(h.approve("login-1", 1)))
	access := h.userAccess()
	ctx := context.Background()

	id, err := access.RepoID(ctx, first, "coder/faucet")
	require.NoError(t, err)
	require.EqualValues(t, grantRepoID("coder/faucet"), id)
	h.clock.Advance(time.Minute)
	second := h.principal(h.redeem(h.approve("login-2", 1)))
	h.github.set(func(g *grantGitHub) { g.repoStatus = http.StatusNotFound })
	_, err = access.RepoID(ctx, second, "coder/faucet")
	require.ErrorIs(t, err, faucet.ErrNoAccess)
	_, err = access.RepoIDFresh(ctx, second, "coder/faucet")
	require.ErrorIs(t, err, faucet.ErrNoAccess)
	_, checks := h.github.counts()
	require.Equal(t, 3, checks, "each check asked GitHub")
}

// usertokensConnection reads the connection of the GitHub user with this ID.
func usertokensConnection(t *testing.T, h *grantHarness, userID int64) oauth.Connection {
	t.Helper()
	c, err := h.store.Connection(context.Background(), oauth.ConnectionID(oauth.ProviderGitHub, userID))
	require.NoError(t, err)
	return c
}

// usertokensReplace stores c in place of the connection with its ID.
func usertokensReplace(t *testing.T, h *grantHarness, c oauth.Connection) {
	t.Helper()
	_, err := h.store.RefreshConnection(context.Background(), c.ID, h.clock.Now(), func(oauth.Connection) (oauth.ConnectionChange, error) {
		return oauth.ConnectionChange{Kind: oauth.ReplaceConnection, Connection: c}, nil
	})
	require.NoError(t, err)
}

// When GitHub rejects the stored token, the access check refreshes it once
// and asks again with the new one, which the connection then keeps.
func TestTheAccessCheckRefreshesATokenThatGitHubRejects(t *testing.T) {
	t.Parallel()
	h := newGrantHarness(t, newFakeStore())
	p := h.principal(h.redeem(h.approve("login-1", 1)))
	stored, _, err := oauth.GrantOpen(h.login.Keys, usertokensConnection(t, h, 1))
	require.NoError(t, err)
	h.github.rejectAccess(stored)
	access := h.userAccess()

	id, err := access.RepoID(context.Background(), p, "coder/faucet")
	require.NoError(t, err)
	require.EqualValues(t, grantRepoID("coder/faucet"), id)
	refreshes, checks := h.github.counts()
	require.Equal(t, []int{1, 2}, []int{refreshes, checks})
	_, err = access.RepoIDFresh(context.Background(), p, "coder/faucet")
	require.NoError(t, err)
	refreshes, checks = h.github.counts()
	require.Equal(t, []int{1, 3}, []int{refreshes, checks}, "the connection kept the new token")
}

// A second rejection after the forced refresh is the answer: the check
// refreshes once and asks twice, and never loops.
func TestTheAccessCheckRefreshesOnceWhenGitHubKeepsRejecting(t *testing.T) {
	t.Parallel()
	h := newGrantHarness(t, newFakeStore())
	s := h.redeem(h.approve("login-1", 1))
	p := h.principal(s)
	h.github.set(func(g *grantGitHub) { g.repoStatus = http.StatusUnauthorized })

	_, err := h.userAccess().RepoID(context.Background(), p, "coder/faucet")
	require.ErrorIs(t, err, faucet.ErrInvalidToken)
	refreshes, checks := h.github.counts()
	require.Equal(t, []int{1, 2}, []int{refreshes, checks})
	require.True(t, h.session(s.id).Live())
}

// A rate limit is no denial, and no reason to refresh: the check returns it
// as GitHub gave it.
func TestTheAccessCheckReturnsARateLimitAsItIs(t *testing.T) {
	t.Parallel()
	h := newGrantHarness(t, newFakeStore())
	p := h.principal(h.redeem(h.approve("login-1", 1)))
	h.github.set(func(g *grantGitHub) { g.repoStatus = http.StatusTooManyRequests })

	_, err := h.userAccess().RepoID(context.Background(), p, "coder/faucet")
	require.ErrorIs(t, err, githubapi.ErrRateLimited)
	require.NotErrorIs(t, err, faucet.ErrNoAccess)
	refreshes, checks := h.github.counts()
	require.Equal(t, []int{0, 1}, []int{refreshes, checks})
}

// The check refreshes a GitHub token that expires within a minute before it
// sends it, and leaves it alone before.
func TestTheAccessCheckRefreshesATokenThatExpiresWithinAMinute(t *testing.T) {
	t.Parallel()
	h := newGrantHarness(t, newFakeStore())
	p := h.principal(h.redeem(h.approve("login-1", 1)))
	access := h.userAccess()

	h.clock.Advance(8*time.Hour - time.Minute - time.Second)
	_, err := access.RepoID(context.Background(), p, "coder/faucet")
	require.NoError(t, err)
	refreshes, _ := h.github.counts()
	require.Zero(t, refreshes)
	h.clock.Advance(time.Second)
	_, err = access.RepoID(context.Background(), p, "coder/other")
	require.NoError(t, err)
	refreshes, _ = h.github.counts()
	require.Equal(t, 1, refreshes)
}

// GitHub refusing the refresh token during a check ends the user's
// sessions, and the check fails with faucet.ErrInvalidToken.
func TestTheAccessCheckEndsTheSessionsWhenGitHubRefusesTheRefreshToken(t *testing.T) {
	t.Parallel()
	h := newGrantHarness(t, newFakeStore())
	s := h.redeem(h.approve("login-1", 1))
	sibling := h.redeem(h.approve("login-2", 1))
	p := h.principal(s)
	h.github.revoke(1)

	_, err := h.userAccess().RepoID(context.Background(), p, "coder/faucet")
	require.ErrorIs(t, err, faucet.ErrInvalidToken)
	require.False(t, h.session(s.id).Live())
	require.False(t, h.session(sibling.id).Live())
}

// Only a user who logged in through a browser is checked with the token
// that faucet keeps.
func TestTheAccessCheckRefusesAPrincipalThatIsNotABrowserUser(t *testing.T) {
	t.Parallel()
	h := newGrantHarness(t, newFakeStore())
	access := h.userAccess()
	pasted, err := faucet.NewPrincipal("user1", "ghp_pasted")
	require.NoError(t, err)
	service, err := faucet.NewService(faucet.ServiceClient{ClientID: "ci-watcher"}, bothScopes)
	require.NoError(t, err)

	for _, p := range []faucet.Principal{pasted, service, {}} {
		_, err := access.RepoID(context.Background(), p, "coder/faucet")
		require.ErrorIs(t, err, faucet.ErrNoAccess)
		_, err = access.RepoIDFresh(context.Background(), p, "coder/faucet")
		require.ErrorIs(t, err, faucet.ErrNoAccess)
	}
	_, checks := h.github.counts()
	require.Zero(t, checks)
}

// A GitHub token that does not open, as under a wrong key, is an error and
// changes nothing. It never ends a session, since a deployment with a wrong
// key would then log every user out.
func TestAGitHubTokenThatDoesNotOpenChangesNothing(t *testing.T) {
	t.Parallel()
	h := newGrantHarness(t, newFakeStore())
	s := h.redeem(h.approve("login-1", 1))
	p := h.principal(s)
	good := usertokensConnection(t, h, 1)
	access, refresh, err := oauth.GrantOpen(h.login.Keys, good)
	require.NoError(t, err)
	otherKeys, err := oauth.ParseTokenKeys("k1:" + base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32)))
	require.NoError(t, err)
	wrong := good
	require.NoError(t, oauth.GrantSeal(otherKeys, &wrong, access, refresh))
	usertokensReplace(t, h, wrong)
	h.github.rejectAccess(access)
	h.clock.Advance(usertokensSoon)

	reply := h.post("", grantRefreshForm(s.refresh))
	require.Equal(t, http.StatusInternalServerError, reply.status, reply.rawBody)
	require.Equal(t, "server_error", reply.body["error"])
	_, err = h.userAccess().RepoID(context.Background(), p, "coder/faucet")
	require.ErrorIs(t, err, oauth.ErrUnreadableToken)

	refreshes, checks := h.github.counts()
	require.Equal(t, []int{0, 0}, []int{refreshes, checks}, "GitHub was not asked")
	session := h.session(s.id)
	require.True(t, session.Live())
	require.False(t, h.refreshToken(s.id, s.refresh).Used(), "the refresh token is unused")
	require.Equal(t, wrong, usertokensConnection(t, h, 1))

	usertokensReplace(t, h, good)
	h.refresh(s)
}

// A caller that gives up while GitHub refreshes its token does not lose the
// new pair. GitHub's refresh token works once, so a lost pair would end the
// user's sessions at the next refresh.
func TestAGitHubRefreshReachesTheStoreWhenTheCallerGivesUp(t *testing.T) {
	t.Parallel()
	for name, newStore := range usertokensStores {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newGrantHarness(t, newStore(t))
			s := h.redeem(h.approve("login-1", 1))
			p := h.principal(s)
			access := h.userAccess()
			h.clock.Advance(8*time.Hour - 30*time.Second)
			ctx, cancel := context.WithCancel(context.Background())
			h.github.set(func(g *grantGitHub) { g.onRefresh = cancel })

			_, err := access.RepoID(ctx, p, "coder/faucet")
			require.ErrorIs(t, err, context.Canceled)
			h.github.set(func(g *grantGitHub) { g.onRefresh = nil })
			_, err = access.RepoID(context.Background(), p, "coder/faucet")
			require.NoError(t, err)
			refreshes, _ := h.github.counts()
			require.Equal(t, 1, refreshes)
			require.True(t, h.session(s.id).Live())
		})
	}
}

// A GitHub refresh takes the login that GitHub gives for the new token, as
// after the user renamed their account, and the user's principal has it.
func TestAGitHubRefreshTakesTheUsersNewLogin(t *testing.T) {
	t.Parallel()
	h := newGrantHarness(t, newFakeStore())
	s := h.redeem(h.approve("login-1", 1))
	h.github.set(func(g *grantGitHub) { g.logins[1] = "renamed" })
	h.clock.Advance(usertokensSoon)

	s = h.refresh(s)
	require.Equal(t, "renamed", usertokensConnection(t, h, 1).Login)
	require.Equal(t, "renamed", h.principal(s).Login)
}

// The resolver refuses an unknown session and an ended one, so their access
// tokens fail.
func TestTheUserResolverRefusesAnUnknownOrEndedSession(t *testing.T) {
	t.Parallel()
	h := newGrantHarness(t, newFakeStore())
	s := h.redeem(h.approve("login-1", 1))
	resolve := oauth.UserResolver(h.store)

	_, err := resolve(context.Background(), "no-such-session", bothScopes)
	require.ErrorIs(t, err, faucet.ErrInvalidToken)
	require.NoError(t, h.store.EndSession(context.Background(), s.id, h.clock.Now()))
	_, err = resolve(context.Background(), s.id, bothScopes)
	require.ErrorIs(t, err, faucet.ErrInvalidToken)
	_, err = h.verify(s.access)
	require.ErrorIs(t, err, faucet.ErrInvalidToken)
}

func TestNewUserAccessRefusesMissingParts(t *testing.T) {
	t.Parallel()
	h := newGrantHarness(t, newFakeStore())
	repos := faucet.TokenAuthorizer{BaseURL: h.github.URL}
	for name, d := range map[string]oauth.UserAccessDeps{
		"no store":         {Clock: h.clock, Login: *h.login, Repos: repos},
		"no clock":         {Store: h.store, Login: *h.login, Repos: repos},
		"no checker":       {Store: h.store, Clock: h.clock, Login: *h.login},
		"no browser login": {Store: h.store, Clock: h.clock, Repos: repos},
	} {
		_, err := oauth.NewUserAccess(d)
		require.Error(t, err, name)
		require.NotContains(t, err.Error(), "app-secret", name)
	}
}
