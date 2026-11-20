package oauth_test

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/coder/superblink-events/faucet"
	"github.com/coder/superblink-events/faucet/oauth"
)

// A refresh token comes again within 30 seconds of its first use when
// requests in parallel send it, and is traded again. After that its use
// ends the session, since one of its two holders is not the client. Two
// processes that share a refresh token so log each other out (R14): the
// cost of rotation.
func TestARefreshTokenTradesAgainFor30SecondsThenEndsItsSession(t *testing.T) {
	t.Parallel()
	h := newGrantHarness(t, newFakeStore())
	first := h.redeem(h.approve("login-1", 1))
	second := h.refresh(first)
	require.NotEqual(t, first.refresh, second.refresh)
	require.NotEqual(t, first.access, second.access)

	h.clock.Advance(30*time.Second - time.Millisecond)
	again := h.refresh(first)
	require.NotEqual(t, second.refresh, again.refresh)
	require.True(t, h.session("login-1").Live())

	h.clock.Advance(time.Millisecond)
	grantRequireRefused(t, h.post("", grantRefreshForm(first.refresh)), http.StatusBadRequest, "invalid_grant", "the token 30 seconds after its first use")
	session := h.session("login-1")
	require.False(t, session.Live(), "the session ended")
	require.True(t, h.clock.Now().Equal(session.EndedAt))
	for name, s := range map[string]grantSession{"the token of the first use": second, "the token of the second use": again} {
		grantRequireRefused(t, h.post("", grantRefreshForm(s.refresh)), http.StatusBadRequest, "invalid_grant", name)
	}
}

// A thief who trades a stolen refresh token, and trades on later, leaves
// the stolen token known: when its holder presents it, however late, the
// session ends, and with it the thief's tokens.
func TestAStolenRefreshTokenEndsTheSessionWhenItsHolderComesBackAfterLaterRefreshes(t *testing.T) {
	t.Parallel()
	h := newGrantHarness(t, newFakeStore())
	victim := h.redeem(h.approve("login-1", 1))
	thief := h.refresh(victim)
	for range 3 {
		h.clock.Advance(15 * time.Minute)
		thief = h.refresh(thief)
	}

	grantRequireRefused(t, h.post("", grantRefreshForm(victim.refresh)), http.StatusBadRequest, "invalid_grant", "the stolen token")
	require.False(t, h.session("login-1").Live(), "the session ended")
	grantRequireRefused(t, h.post("", grantRefreshForm(thief.refresh)), http.StatusBadRequest, "invalid_grant", "the thief's token")
}

// Of the tokens that refreshes at once give, the client keeps one. A later
// refresh retires the others unused, and a retired token that comes again
// ends the session, since the client no longer holds it.
func TestARetiredRefreshTokenEndsTheSession(t *testing.T) {
	t.Parallel()
	h := newGrantHarness(t, newFakeStore())
	got := h.raceRefreshes(h.redeem(h.approve("login-1", 1)), 2)
	h.clock.Advance(15 * time.Minute)
	h.refresh(got[1])

	h.clock.Advance(time.Hour)
	grantRequireRefused(t, h.post("", grantRefreshForm(got[0].refresh)), http.StatusBadRequest, "invalid_grant", "the retired token")
	require.False(t, h.session("login-1").Live(), "the session ended")
}

// A refresh token that comes back after its reuse grace ends its session
// before faucet does anything else for the request: though the user's
// GitHub token is due for a refresh, faucet asks GitHub nothing and keeps
// the pair that it holds.
func TestALateReuseOfARefreshTokenEndsItsSessionWithoutAskingGitHub(t *testing.T) {
	t.Parallel()
	h := newGrantHarness(t, newFakeStore())
	first := h.redeem(h.approve("login-1", 1))
	h.refresh(first)
	h.clock.Advance(8*time.Hour - 10*time.Minute)
	id := oauth.ConnectionID(oauth.ProviderGitHub, 1)
	held, err := h.store.Connection(context.Background(), id)
	require.NoError(t, err)

	grantRequireRefused(t, h.post("", grantRefreshForm(first.refresh)), http.StatusBadRequest, "invalid_grant", "the token hours after its first use")
	require.False(t, h.session("login-1").Live(), "the session ended")
	refreshes, repoChecks := h.github.counts()
	require.Zero(t, refreshes, "faucet asks GitHub for no new pair for a request that it refuses")
	require.Zero(t, repoChecks)
	kept, err := h.store.Connection(context.Background(), id)
	require.NoError(t, err)
	require.Equal(t, held, kept, "the connection keeps its pair")
}

// grantCountingStore counts the reads of a session by its refresh token.
type grantCountingStore struct {
	oauth.Store
	sessionReads atomic.Int64
}

func (s *grantCountingStore) SessionByRefreshToken(ctx context.Context, refresh oauth.TokenHash) (oauth.Session, oauth.RefreshToken, error) {
	s.sessionReads.Add(1)
	return s.Store.SessionByRefreshToken(ctx, refresh)
}

// A refused refresh request changes nothing: the session keeps its refresh
// token, which then works.
func TestARefreshTokenStaysUsableAfterARefusedRefresh(t *testing.T) {
	t.Parallel()
	h := newGrantHarness(t, newFakeStore())
	s := h.redeem(h.approve("login-1", 1))
	without := func(name string) func(url.Values) { return func(f url.Values) { f.Del(name) } }
	with := func(name, value string) func(url.Values) { return func(f url.Values) { f.Set(name, value) } }

	for name, c := range map[string]struct {
		authorization string
		change        func(url.Values)
		status        int
		code          string
	}{
		"a Basic password":         {basic(grantClientID, "secret"), without("client_id"), http.StatusUnauthorized, "invalid_client"},
		"a bearer":                 {"Bearer " + s.access, nil, http.StatusUnauthorized, "invalid_client"},
		"no client_id":             {"", without("client_id"), http.StatusBadRequest, "invalid_request"},
		"a client_secret":          {"", with("client_secret", "secret"), http.StatusBadRequest, "invalid_request"},
		"Basic and form disagree":  {basic(grantClientID, ""), with("client_id", "https://other.example.com/client.json"), http.StatusBadRequest, "invalid_request"},
		"another client_id":        {"", with("client_id", "https://other.example.com/client.json"), http.StatusBadRequest, "invalid_grant"},
		"Basic of another client":  {basic("https://other.example.com/client.json", ""), without("client_id"), http.StatusBadRequest, "invalid_grant"},
		"no refresh_token":         {"", without("refresh_token"), http.StatusBadRequest, "invalid_request"},
		"an unknown refresh token": {"", with("refresh_token", "fcr_"+grantSecret(t)), http.StatusBadRequest, "invalid_grant"},
		"an access token":          {"", with("refresh_token", s.access), http.StatusBadRequest, "invalid_grant"},
		"no scope of the session":  {"", with("scope", "repo"), http.StatusBadRequest, "invalid_scope"},
		"another resource":         {"", with("resource", issuer+"/mcp"), http.StatusBadRequest, "invalid_target"},
		"another resource too": {"", func(f url.Values) {
			f.Add("resource", resource)
			f.Add("resource", issuer+"/mcp")
		}, http.StatusBadRequest, "invalid_target"},
	} {
		form := grantRefreshForm(s.refresh)
		if c.change != nil {
			c.change(form)
		}
		reply := h.post(c.authorization, form)
		grantRequireRefused(t, reply, c.status, c.code, name)
		require.NotContains(t, reply.rawBody, s.refresh, name)
	}
	session := h.session(s.id)
	require.True(t, session.Live())
	require.False(t, h.refreshToken(s.id, s.refresh).Used(), "the refresh token is unused")

	form := grantRefreshForm(s.refresh)
	form.Del("client_id")
	form.Set("resource", resource)
	grantTokens(t, s.id, h.post(basic(grantClientID, ""), form))
}

// A refresh token that faucet cannot have issued is refused before the
// store is asked.
func TestARefreshTokenOfAnotherFormIsRefusedWithoutAStoreRead(t *testing.T) {
	t.Parallel()
	store := &grantCountingStore{Store: newFakeStore()}
	h := newGrantHarness(t, store)
	s := h.redeem(h.approve("login-1", 1))
	body := strings.TrimPrefix(s.refresh, "fcr_")

	for name, token := range map[string]string{
		"an access token":           s.access,
		"no prefix":                 body,
		"an upper-case prefix":      "FCR_" + body,
		"one character short":       s.refresh[:len(s.refresh)-1],
		"one character more":        s.refresh + "A",
		"a character of base64":     s.refresh[:len(s.refresh)-1] + "+",
		"bits past the 32 bytes":    s.refresh[:len(s.refresh)-1] + "B",
		"a client secret's prefix":  "fcs_" + body,
		"a refresh token with a NL": s.refresh + "\n",
	} {
		grantRequireRefused(t, h.post("", grantRefreshForm(token)), http.StatusBadRequest, "invalid_grant", name)
	}
	require.Zero(t, store.sessionReads.Load())
	h.refresh(s)
	require.EqualValues(t, 1, store.sessionReads.Load())
}

// A refresh may ask for fewer scopes. The access token holds those, and the
// session keeps all of its own, so a later refresh can ask for them again.
func TestARefreshNarrowsTheScopesOfItsAccessTokenOnly(t *testing.T) {
	t.Parallel()
	h := newGrantHarness(t, newFakeStore())
	s := h.redeem(h.approve("login-1", 1))

	form := grantRefreshForm(s.refresh)
	form.Set("scope", "events:read repo")
	reply := h.post("", form)
	narrow := grantTokens(t, s.id, reply)
	require.Equal(t, "events:read", reply.body["scope"])
	require.EqualValues(t, 900, reply.body["expires_in"])
	stored, err := h.store.AccessToken(context.Background(), oauth.HashToken(narrow.access))
	require.NoError(t, err)
	require.Equal(t, oauth.AccessToken{
		Hash: oauth.HashToken(narrow.access), ExpiresAt: h.clock.Now().Add(15 * time.Minute), Scopes: []faucet.Scope{faucet.ScopeEventsRead},
		Resource: resource, SubjectKind: oauth.SubjectUser, Subject: s.id,
	}, stored)
	require.Equal(t, bothScopes, h.session(s.id).Scopes)

	reply = h.post("", grantRefreshForm(narrow.refresh))
	grantTokens(t, s.id, reply)
	require.Equal(t, "events:read events:subscribe", reply.body["scope"])
}

// A session whose refresh token went unused for 180 days ends when the
// token comes, as GitHub ends its own refresh tokens after six months.
func TestASessionEndsAfter180DaysWithoutARefresh(t *testing.T) {
	t.Parallel()
	h := newGrantHarness(t, newFakeStore())
	kept := h.redeem(h.approve("login-1", 1))
	idle := h.redeem(h.approve("login-2", 1))

	h.clock.Advance(180*24*time.Hour - time.Second)
	kept = h.refresh(kept)
	h.clock.Advance(time.Second)
	grantRequireRefused(t, h.post("", grantRefreshForm(idle.refresh)), http.StatusBadRequest, "invalid_grant", "an idle session")
	require.True(t, h.clock.Now().Equal(h.session(idle.id).EndedAt), "the idle session ended")
	h.refresh(kept)
}

// Refreshes with one refresh token at once, as the parallel tool calls of
// an agent make after their access token expired, all get tokens. The
// session lives on, and the refresh token that the client keeps, whichever
// it is, works later.
func TestRefreshesWithOneTokenAtOnceAllGetTokensThatWork(t *testing.T) {
	t.Parallel()
	for kept := range 3 {
		h := newGrantHarness(t, newFakeStore())
		got := h.raceRefreshes(h.redeem(h.approve("login-1", 1)), 3)
		h.clock.Advance(time.Minute)
		h.refresh(got[kept])
		require.True(t, h.session("login-1").Live(), "the client kept the token of reply %d", kept)
	}
}

// Two refreshes that both read the refresh token before either trades it
// both get tokens, and the refresh token that the client keeps works later.
func TestTwoRefreshesThatReadTheTokenBeforeEitherTradesItBothGetTokens(t *testing.T) {
	t.Parallel()
	for kept := range 2 {
		store := newGrantBarrierStore()
		h := newGrantHarness(t, store)
		s := h.redeem(h.approve("login-1", 1))
		store.hold(2)
		got := h.raceRefreshes(s, 2)
		h.clock.Advance(time.Minute)
		h.refresh(got[kept])
		require.True(t, h.session("login-1").Live(), "the client kept the token of reply %d", kept)
	}
}

// raceRefreshes sends n refreshes of s at once. Each must get tokens, and
// the session must live on.
func (h *grantHarness) raceRefreshes(s grantSession, n int) []grantSession {
	h.t.Helper()
	forms := make([]url.Values, n)
	for i := range forms {
		forms[i] = grantRefreshForm(s.refresh)
	}
	var got []grantSession
	for _, reply := range h.raceAll(forms...) {
		got = append(got, grantTokens(h.t, s.id, reply))
	}
	require.True(h.t, h.session(s.id).Live())
	return got
}

// The refresh grant refreshes the user's GitHub token when it expires
// within ten minutes, and leaves it alone before.
func TestTheRefreshGrantRefreshesAGitHubTokenThatExpiresWithinTenMinutes(t *testing.T) {
	t.Parallel()
	h := newGrantHarness(t, newFakeStore())
	s := h.redeem(h.approve("login-1", 1))

	h.clock.Advance(8*time.Hour - 10*time.Minute - time.Second)
	s = h.refresh(s)
	refreshes, _ := h.github.counts()
	require.Zero(t, refreshes)
	h.clock.Advance(time.Second)
	h.refresh(s)
	refreshes, _ = h.github.counts()
	require.Equal(t, 1, refreshes)
}

// A GitHub failure other than a refused refresh token does not stop the
// refresh grant: the client gets its tokens, and the connection keeps the
// pair that it had until a later refresh works.
func TestTheRefreshGrantGoesOnWhenGitHubIsDown(t *testing.T) {
	t.Parallel()
	h := newGrantHarness(t, newFakeStore())
	s := h.redeem(h.approve("login-1", 1))
	before := usertokensConnection(t, h, 1)
	h.github.set(func(g *grantGitHub) { g.tokenStatus = http.StatusBadGateway })
	h.clock.Advance(usertokensSoon)

	s = h.refresh(s)
	refreshes, _ := h.github.counts()
	require.Equal(t, 1, refreshes)
	require.Equal(t, before, usertokensConnection(t, h, 1), "the connection keeps its pair")

	h.github.set(func(g *grantGitHub) { g.tokenStatus = 0 })
	h.refresh(s)
	require.True(t, h.clock.Now().Add(8*time.Hour).Equal(usertokensConnection(t, h, 1).AccessExpiresAt))
}

// A refreshed pair that GitHub gives to another user is not stored, since
// the sessions of this user would then act as that one. The grant goes on
// with the pair that the connection had.
func TestARefreshedPairOfAnotherUserIsNotStored(t *testing.T) {
	t.Parallel()
	h := newGrantHarness(t, newFakeStore())
	s := h.redeem(h.approve("login-1", 1))
	before := usertokensConnection(t, h, 1)
	h.github.set(func(g *grantGitHub) { g.userAs, g.logins[2] = 2, "user2" })
	h.clock.Advance(usertokensSoon)

	s = h.refresh(s)
	require.Equal(t, before, usertokensConnection(t, h, 1))
	require.Equal(t, "user1", h.principal(s).Login)
}
