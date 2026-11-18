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

	"github.com/coder/faucet/faucet"
	"github.com/coder/faucet/faucet/oauth"
)

// A refresh token that a rotation replaced ends its session when it comes
// again, since one of its two holders is not the client. Two processes that
// share a refresh token so log each other out (R14): the cost of rotation.
func TestARotatedOutRefreshTokenEndsItsSession(t *testing.T) {
	t.Parallel()
	h := newGrantHarness(t, newFakeStore())
	first := h.redeem(h.approve("login-1", 1))
	second := h.refresh(first)
	require.NotEqual(t, first.refresh, second.refresh)
	require.NotEqual(t, first.access, second.access)

	h.clock.Advance(time.Minute)
	grantRequireRefused(t, h.post("", grantRefreshForm(first.refresh)), http.StatusBadRequest, "invalid_grant", "the rotated-out token")
	session := h.session("login-1")
	require.False(t, session.Live(), "the session ended")
	require.True(t, h.clock.Now().Equal(session.EndedAt))
	grantRequireRefused(t, h.post("", grantRefreshForm(second.refresh)), http.StatusBadRequest, "invalid_grant", "the current token of the ended session")
}

// grantCountingStore counts the reads of a session by its refresh token.
type grantCountingStore struct {
	oauth.Store
	sessionReads atomic.Int64
}

func (s *grantCountingStore) SessionByRefreshToken(ctx context.Context, refresh oauth.TokenHash) (oauth.Session, bool, error) {
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
	require.Equal(t, oauth.HashToken(s.refresh), session.RefreshHash)

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

// Of two refreshes with one refresh token at once, one rotates, and the
// other then holds a token that is not current, which ends the session
// (R14).
func TestTwoRefreshesWithOneTokenAtOnceEndTheSession(t *testing.T) {
	t.Parallel()
	h := newGrantHarness(t, newFakeStore())
	s := h.redeem(h.approve("login-1", 1))
	replies := h.raceAll(grantRefreshForm(s.refresh), grantRefreshForm(s.refresh))
	require.ElementsMatch(t, []int{http.StatusOK, http.StatusBadRequest}, []int{replies[0].status, replies[1].status})
	require.False(t, h.session(s.id).Live())
}

// Two refreshes that both read the refresh token as current before either
// rotates: the one that loses the rotation ends the session, so the tokens
// that the winner got fail too (R14).
func TestARefreshThatLosesTheRotationEndsTheSession(t *testing.T) {
	t.Parallel()
	store := newGrantBarrierStore()
	h := newGrantHarness(t, store)
	s := h.redeem(h.approve("login-1", 1))
	store.hold(2)
	replies := h.raceAll(grantRefreshForm(s.refresh), grantRefreshForm(s.refresh))
	require.ElementsMatch(t, []int{http.StatusOK, http.StatusBadRequest}, []int{replies[0].status, replies[1].status})
	require.False(t, h.session(s.id).Live())
	for _, reply := range replies {
		if reply.status == http.StatusOK {
			won := grantTokens(t, s.id, reply)
			grantRequireRefused(t, h.post("", grantRefreshForm(won.refresh)), http.StatusBadRequest, "invalid_grant", "the winner's token")
		}
	}
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
func TestTheRefreshGrantGoesOnWhenGitHubFailsToRefresh(t *testing.T) {
	t.Parallel()
	for name, change := range map[string]func(g *grantGitHub){
		"GitHub is down":            func(g *grantGitHub) { g.tokenStatus = http.StatusBadGateway },
		"another user's token":      func(g *grantGitHub) { g.userAs, g.logins[2] = 2, "user2" },
		"GitHub is down, then back": func(g *grantGitHub) { g.tokenStatus = http.StatusServiceUnavailable },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newGrantHarness(t, newFakeStore())
			s := h.redeem(h.approve("login-1", 1))
			id := oauth.ConnectionID(oauth.ProviderGitHub, 1)
			before, err := h.store.Connection(context.Background(), id)
			require.NoError(t, err)
			h.github.set(change)
			h.clock.Advance(usertokensSoon)

			s = h.refresh(s)
			refreshes, _ := h.github.counts()
			require.Equal(t, 1, refreshes)
			after, err := h.store.Connection(context.Background(), id)
			require.NoError(t, err)
			require.Equal(t, before, after, "the connection keeps its pair")
			require.True(t, h.session(s.id).Live())
			if name == "GitHub is down, then back" {
				h.github.set(func(g *grantGitHub) { g.tokenStatus = 0 })
				h.refresh(s)
				after, err := h.store.Connection(context.Background(), id)
				require.NoError(t, err)
				require.True(t, h.clock.Now().Add(8*time.Hour).Equal(after.AccessExpiresAt))
			}
		})
	}
}
