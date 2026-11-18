package oauth_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/coder/faucet/faucet"
	"github.com/coder/faucet/faucet/oauth"
)

// accessAnswer is the answer of an access check, with the repositories that
// GitHub was asked about while it ran.
type accessAnswer struct {
	id    int64
	err   error
	asked []string
}

// asking runs check and returns its answer with the repositories that GitHub
// was asked about meanwhile, sorted, as a refresh asks about several at
// once.
func (h *grantHarness) asking(check func() (int64, error)) accessAnswer {
	before := len(h.github.lookups())
	id, err := check()
	asked := h.github.lookups()[before:]
	slices.Sort(asked)
	return accessAnswer{id: id, err: err, asked: asked}
}

func (h *grantHarness) repoID(a oauth.UserAccess, p faucet.Principal, repo string) accessAnswer {
	return h.asking(func() (int64, error) { return a.RepoID(context.Background(), p, repo) })
}

func (h *grantHarness) repoIDFresh(a oauth.UserAccess, p faucet.Principal, repo string) accessAnswer {
	return h.asking(func() (int64, error) { return a.RepoIDFresh(context.Background(), p, repo) })
}

// refreshAsking refreshes s and returns the next tokens with the
// repositories that GitHub was asked about meanwhile, sorted.
func (h *grantHarness) refreshAsking(s grantSession) (grantSession, []string) {
	h.t.Helper()
	var next grantSession
	answer := h.asking(func() (int64, error) {
		next = h.refresh(s)
		return 0, nil
	})
	return next, answer.asked
}

// accessChecks returns the access checks of the GitHub user with this ID,
// the most recently used first.
func (h *grantHarness) accessChecks(userID int64) []oauth.AccessCheck {
	h.t.Helper()
	checks, err := h.store.RecentAccessChecks(context.Background(), oauth.ConnectionID(oauth.ProviderGitHub, userID), time.Time{}, 100)
	require.NoError(h.t, err)
	return checks
}

// checkedRepos returns the repositories of the access checks of the GitHub
// user with this ID, the most recently used first.
func (h *grantHarness) checkedRepos(userID int64) []string {
	h.t.Helper()
	var repos []string
	for _, c := range h.accessChecks(userID) {
		repos = append(repos, c.Repo)
	}
	return repos
}

// storeChecks stores a check of each repository by the GitHub user with this
// ID, all checked at the clock's time. Each is used a second after the one
// before it, so the last is the most recently used.
func (h *grantHarness) storeChecks(userID int64, repos []string) {
	h.t.Helper()
	now := h.clock.Now()
	for i, repo := range repos {
		require.NoError(h.t, h.store.StoreAccessCheck(context.Background(), oauth.AccessCheck{
			ConnectionID: oauth.ConnectionID(oauth.ProviderGitHub, userID), Repo: repo, RepoID: grantRepoID,
			CheckedAt: now, LastUsedAt: now.Add(time.Duration(i) * time.Second),
		}))
	}
}

// storeCheck stores the check of repo by the GitHub user with this ID, made
// and last used at the time at.
func (h *grantHarness) storeCheck(userID int64, repo string, at time.Time) {
	h.t.Helper()
	require.NoError(h.t, h.store.StoreAccessCheck(context.Background(), oauth.AccessCheck{
		ConnectionID: oauth.ConnectionID(oauth.ProviderGitHub, userID), Repo: repo, RepoID: grantRepoID,
		CheckedAt: at, LastUsedAt: at,
	}))
}

func numberedRepos(n int) []string {
	repos := make([]string, n)
	for i := range repos {
		repos[i] = fmt.Sprintf("o/r%02d", i)
	}
	return repos
}

// A check that passed since the session's refresh answers without GitHub,
// whatever the case of the name, and counts as a use.
func TestAFreshAccessCheckAnswersWithoutGitHub(t *testing.T) {
	t.Parallel()
	for name, newStore := range usertokensStores {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newGrantHarness(t, newStore(t))
			p := h.principal(h.redeem(h.approve("login-1", 1)))
			access := h.userAccess()
			require.Equal(t, []string{"o/x"}, h.repoID(access, p, "o/x").asked)
			h.clock.Advance(time.Minute)
			require.Equal(t, []string{"o/y"}, h.repoID(access, p, "o/y").asked)
			require.Equal(t, []string{"o/y", "o/x"}, h.checkedRepos(1))
			h.clock.Advance(time.Minute)

			got := h.repoID(access, p, "o/x")
			require.NoError(t, got.err)
			require.EqualValues(t, grantRepoID, got.id)
			require.Empty(t, got.asked, "the check of o/x answers")
			require.Equal(t, []string{"o/x", "o/y"}, h.checkedRepos(1), "the use moved o/x first")
			require.Empty(t, h.repoID(access, p, "O/X").asked, "GitHub ignores the case of a name, and so does the check")
		})
	}
}

// Until the next refresh a check stands, so a revoked access passes for at
// most one access token's life. The refresh asks GitHub about the checks,
// deletes the one that GitHub denies and keeps the other.
func TestARefreshFindsARevokedRepositoryAccess(t *testing.T) {
	t.Parallel()
	for name, newStore := range usertokensStores {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newGrantHarness(t, newStore(t))
			s := h.redeem(h.approve("login-1", 1))
			p := h.principal(s)
			access := h.userAccess()
			for _, repo := range []string{"o/x", "o/y"} {
				require.NoError(t, h.repoID(access, p, repo).err)
			}
			h.github.set(func(g *grantGitHub) { g.denied["o/x"] = true })
			h.clock.Advance(time.Minute)
			got := h.repoID(access, p, "o/x")
			require.NoError(t, got.err)
			require.Empty(t, got.asked, "before the refresh, the check of o/x stands")

			s, asked := h.refreshAsking(s)
			require.Equal(t, []string{"o/x", "o/y"}, asked, "the refresh asked GitHub about both")
			require.Equal(t, []string{"o/y"}, h.checkedRepos(1))
			p = h.principal(s)
			require.ErrorIs(t, h.repoID(access, p, "o/x").err, faucet.ErrNoAccess)
			got = h.repoID(access, p, "o/y")
			require.NoError(t, got.err)
			require.Empty(t, got.asked, "the refresh checked o/y again")
		})
	}
}

// The sessions of a user share the checks, and each trusts only those made
// since its own refresh. A denial deletes the check for every session.
func TestASessionTrustsOnlyTheChecksMadeSinceItsRefresh(t *testing.T) {
	t.Parallel()
	for name, newStore := range usertokensStores {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newGrantHarness(t, newStore(t))
			a := h.principal(h.redeem(h.approve("login-a", 1)))
			access := h.userAccess()
			require.Equal(t, []string{"o/x"}, h.repoID(access, a, "o/x").asked)
			require.Equal(t, []string{"o/x"}, h.checkedRepos(1))
			h.clock.Advance(time.Minute)
			b := h.principal(h.redeem(h.approve("login-b", 1)))
			h.github.set(func(g *grantGitHub) { g.denied["o/x"] = true })

			got := h.repoID(access, b, "o/x")
			require.ErrorIs(t, got.err, faucet.ErrNoAccess)
			require.Equal(t, []string{"o/x"}, got.asked, "b logged in after the check of o/x, so it asks GitHub")
			require.Empty(t, h.checkedRepos(1), "the denial deleted the check")
			got = h.repoID(access, a, "o/x")
			require.ErrorIs(t, got.err, faucet.ErrNoAccess)
			require.Equal(t, []string{"o/x"}, got.asked, "with the check gone, a asks GitHub too")

			require.Equal(t, []string{"o/y"}, h.repoID(access, a, "o/y").asked)
			got = h.repoID(access, b, "o/y")
			require.NoError(t, got.err)
			require.Empty(t, got.asked, "a check made since b logged in answers for b")
		})
	}
}

// Every renewal checks access again at GitHub (R3), with a fresh check at
// hand, and records the answer: a pass for the next check, a denial by
// deleting the check.
func TestARenewalAlwaysAsksGitHub(t *testing.T) {
	t.Parallel()
	h := newGrantHarness(t, newFakeStore())
	p := h.principal(h.redeem(h.approve("login-1", 1)))
	access := h.userAccess()
	require.NoError(t, h.repoID(access, p, "o/x").err)

	for range 2 {
		got := h.repoIDFresh(access, p, "o/x")
		require.NoError(t, got.err)
		require.EqualValues(t, grantRepoID, got.id)
		require.Equal(t, []string{"o/x"}, got.asked, "a renewal asks GitHub")
	}
	require.Equal(t, []string{"o/x"}, h.checkedRepos(1))
	h.github.set(func(g *grantGitHub) { g.denied["o/x"] = true })
	got := h.repoIDFresh(access, p, "o/x")
	require.ErrorIs(t, got.err, faucet.ErrNoAccess)
	require.Equal(t, []string{"o/x"}, got.asked)
	require.Empty(t, h.checkedRepos(1))
	got = h.repoID(access, p, "o/x")
	require.ErrorIs(t, got.err, faucet.ErrNoAccess, "the renewal's denial deleted the check")
	require.Equal(t, []string{"o/x"}, got.asked)
}

// A denial stores nothing, so the next check asks GitHub again.
func TestAnAccessDenialStoresNothing(t *testing.T) {
	t.Parallel()
	h := newGrantHarness(t, newFakeStore())
	p := h.principal(h.redeem(h.approve("login-1", 1)))
	access := h.userAccess()
	h.github.set(func(g *grantGitHub) { g.denied["o/x"] = true })

	for range 2 {
		got := h.repoID(access, p, "o/x")
		require.ErrorIs(t, got.err, faucet.ErrNoAccess)
		require.Equal(t, []string{"o/x"}, got.asked, "each check of a denied repository asks GitHub")
		require.Empty(t, h.checkedRepos(1))
	}
}

// A refresh checks again the 50 most recently used repositories. The others
// are checked again on their next use.
func TestARefreshChecksAgainTheFiftyMostRecentlyUsedRepositories(t *testing.T) {
	t.Parallel()
	for name, newStore := range usertokensStores {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newGrantHarness(t, newStore(t))
			s := h.redeem(h.approve("login-1", 1))
			access := h.userAccess()
			repos := numberedRepos(60)
			h.storeChecks(1, repos)
			h.clock.Advance(time.Minute)

			s, asked := h.refreshAsking(s)
			require.Equal(t, repos[10:], asked, "the refresh asked GitHub about the 50 most recently used")
			p := h.principal(s)
			for i, repo := range repos {
				got := h.repoID(access, p, repo)
				require.NoError(t, got.err, repo)
				if i < 10 {
					require.Equal(t, []string{repo}, got.asked, "%s was not checked again, so its use asks GitHub", repo)
				} else {
					require.Empty(t, got.asked, "the refresh checked %s again", repo)
				}
			}
		})
	}
}

// A refresh checks again only the repositories that a session used within
// one access token's life. The others wait for their next use.
func TestARefreshChecksAgainOnlyTheRepositoriesUsedWithinOneAccessTokensLife(t *testing.T) {
	t.Parallel()
	for name, newStore := range usertokensStores {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newGrantHarness(t, newStore(t))
			s := h.redeem(h.approve("login-1", 1))
			access := h.userAccess()
			h.clock.Advance(20 * time.Minute)
			now := h.clock.Now()
			h.storeCheck(1, "o/idle", now.Add(-16*time.Minute))
			h.storeCheck(1, "o/used", now.Add(-14*time.Minute))

			s, asked := h.refreshAsking(s)
			require.Equal(t, []string{"o/used"}, asked, "the refresh asked GitHub about the repository used 14 minutes before")
			p := h.principal(s)
			require.Equal(t, []string{"o/idle"}, h.repoID(access, p, "o/idle").asked, "a repository used 16 minutes before is asked on its next use")
			require.Empty(t, h.repoID(access, p, "o/used").asked, "the refresh checked o/used again")
		})
	}
}

// The purge deletes a check that no session used for a day, and keeps one
// used since.
func TestThePurgeDeletesTheChecksThatNoSessionUsedForADay(t *testing.T) {
	t.Parallel()
	for name, newStore := range usertokensStores {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newGrantHarness(t, newStore(t))
			h.redeem(h.approve("login-1", 1))
			start := h.clock.Now()
			h.storeCheck(1, "o/unused", start)
			h.storeCheck(1, "o/used", start.Add(time.Minute))
			h.clock.Advance(24*time.Hour + 30*time.Second)

			res, err := h.server.Purge(context.Background())
			require.NoError(t, err)
			require.EqualValues(t, 1, res.UnusedAccessChecks)
			require.Equal(t, []string{"o/used"}, h.checkedRepos(1))
		})
	}
}

// GitHub hangs on every lookup. The refresh still answers before its
// request's deadline, with tokens that work, and the checks that it could
// not finish stay, to be checked again on their next use.
func TestARefreshAnswersWithinItsDeadlineWhileGitHubHangs(t *testing.T) {
	t.Parallel()
	h := newGrantHarness(t, newFakeStore())
	s := h.redeem(h.approve("login-1", 1))
	access := h.userAccess()
	for _, repo := range []string{"o/x", "o/y"} {
		require.NoError(t, h.repoID(access, h.principal(s), repo).err)
	}
	released := make(chan struct{})
	t.Cleanup(func() { close(released) })
	h.github.set(func(g *grantGitHub) {
		g.onRepo = func(r *http.Request) {
			select {
			case <-r.Context().Done():
			case <-released:
			}
		}
	})
	h.clock.Advance(time.Minute)
	before := len(h.github.lookups())

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	deadline, _ := ctx.Deadline()
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, oauth.TokenPath, strings.NewReader(grantRefreshForm(s.refresh).Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.server.TokenHandler().ServeHTTP(rec, req)
	require.True(t, time.Now().Before(deadline), "the refresh answered before its deadline")
	reply := tokenReply{status: rec.Code, header: rec.Header(), rawBody: rec.Body.String()}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &reply.body), reply.rawBody)
	next := grantTokens(t, s.id, reply)
	require.NotEmpty(t, h.github.lookups()[before:], "the refresh asked GitHub")

	h.github.set(func(g *grantGitHub) { g.onRepo = nil })
	require.ElementsMatch(t, []string{"o/x", "o/y"}, h.checkedRepos(1), "the checks stay")
	got := h.repoID(access, h.principal(next), "o/x")
	require.NoError(t, got.err)
	require.Equal(t, []string{"o/x"}, got.asked, "a check that the refresh could not finish is asked on use")
}

// A check counts from the start of its lookup: one that started before a
// refresh of the session and answered after it does not answer for the
// session.
func TestACheckThatStartedBeforeARefreshDoesNotCountAfterIt(t *testing.T) {
	t.Parallel()
	h := newGrantHarness(t, newFakeStore())
	s := h.redeem(h.approve("login-1", 1))
	access := h.userAccess()
	h.clock.Advance(time.Minute)
	var (
		refreshed  tokenReply
		refreshErr error
	)
	h.github.set(func(g *grantGitHub) {
		g.onRepo = func(*http.Request) {
			h.github.set(func(g *grantGitHub) { g.onRepo = nil })
			h.clock.Advance(time.Minute)
			refreshed, refreshErr = h.send("", grantRefreshForm(s.refresh))
		}
	})

	got := h.repoID(access, h.principal(s), "o/x")
	require.NoError(t, got.err)
	require.NoError(t, refreshErr)
	next := grantTokens(t, s.id, refreshed)
	require.Equal(t, []string{"o/x"}, h.checkedRepos(1), "the answer is recorded")
	got = h.repoID(access, h.principal(next), "o/x")
	require.NoError(t, got.err)
	require.Equal(t, []string{"o/x"}, got.asked, "the lookup started before the refresh, so its answer is stale after it")
}

// A lookup that GitHub fails is neither a pass nor a denial. The refresh
// still succeeds, starts no more lookups, and leaves every check as it was.
func TestARefreshThatGitHubFailsLeavesTheChecksAsTheyWere(t *testing.T) {
	t.Parallel()
	for name, status := range map[string]int{"rate limit": http.StatusTooManyRequests, "server error": http.StatusBadGateway} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newGrantHarness(t, newFakeStore())
			s := h.redeem(h.approve("login-1", 1))
			repos := numberedRepos(10)
			h.storeChecks(1, repos)
			before := h.accessChecks(1)
			h.github.set(func(g *grantGitHub) { g.repoStatus = status })
			h.clock.Advance(time.Minute)

			next, asked := h.refreshAsking(s)
			require.NotEmpty(t, asked, "the refresh asked GitHub")
			require.Less(t, len(asked), len(repos), "after a failed lookup the refresh starts no more")
			require.Equal(t, before, h.accessChecks(1))
			h.principal(next)
		})
	}
}
