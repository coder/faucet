package oauth_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"cdr.dev/slog/v3"
	"github.com/coder/superblink-events/faucet"
	"github.com/coder/superblink-events/faucet/internal/githubapi"
	"github.com/coder/superblink-events/faucet/oauth"
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
			ConnectionID: oauth.ConnectionID(oauth.ProviderGitHub, userID), Repo: repo, RepoID: grantRepoID(repo),
			CheckedAt: now, LastUsedAt: now.Add(time.Duration(i) * time.Second),
		}))
	}
}

// storeCheck stores the check of repo by the GitHub user with this ID, made
// and last used at the time at.
func (h *grantHarness) storeCheck(userID int64, repo string, at time.Time) {
	h.t.Helper()
	require.NoError(h.t, h.store.StoreAccessCheck(context.Background(), oauth.AccessCheck{
		ConnectionID: oauth.ConnectionID(oauth.ProviderGitHub, userID), Repo: repo, RepoID: grantRepoID(repo),
		CheckedAt: at, LastUsedAt: at,
	}))
}

// cancelingRepos is a RepoChecker that can cancel a context right after the
// checker that it wraps answers, as when the caller gives up just then.
type cancelingRepos struct {
	oauth.RepoChecker
	mu     sync.Mutex
	cancel context.CancelFunc
}

// cancelAfterAnswer makes the next answer call cancel.
func (c *cancelingRepos) cancelAfterAnswer(cancel context.CancelFunc) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cancel = cancel
}

func (c *cancelingRepos) RepoIDFresh(ctx context.Context, p faucet.Principal, repo string) (int64, error) {
	id, err := c.RepoChecker.RepoIDFresh(ctx, p, repo)
	c.mu.Lock()
	cancel := c.cancel
	c.cancel = nil
	c.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return id, err
}

// serveToken sends form to the token endpoint of the server in a request
// with ctx, and returns the reply.
func (h *grantHarness) serveToken(ctx context.Context, form url.Values) tokenReply {
	h.t.Helper()
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, oauth.TokenPath, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.server.TokenHandler().ServeHTTP(rec, req)
	reply := tokenReply{status: rec.Code, header: rec.Header(), rawBody: rec.Body.String()}
	require.NoError(h.t, json.Unmarshal(rec.Body.Bytes(), &reply.body), reply.rawBody)
	return reply
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
			require.EqualValues(t, grantRepoID("o/x"), got.id)
			require.Empty(t, got.asked, "the check of o/x answers")
			require.Equal(t, []string{"o/x", "o/y"}, h.checkedRepos(1), "the use moved o/x first")
			require.Empty(t, h.repoID(access, p, "O/X").asked, "GitHub ignores the case of a name, and so does the check")
		})
	}
}

// A use of a fresh check moves its last use only once the last use is a
// minute old, so that most uses of a busy repository write nothing.
func TestAUseMovesTheLastUseOfACheckOnlyOnceItIsAMinuteOld(t *testing.T) {
	t.Parallel()
	for name, newStore := range usertokensStores {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newGrantHarness(t, newStore(t))
			p := h.principal(h.redeem(h.approve("login-1", 1)))
			access := h.userAccess()
			require.NoError(t, h.repoID(access, p, "o/x").err)
			h.clock.Advance(10 * time.Second)
			require.NoError(t, h.repoID(access, p, "o/y").err)
			h.clock.Advance(40 * time.Second)

			require.Empty(t, h.repoID(access, p, "o/x").asked)
			require.Equal(t, []string{"o/y", "o/x"}, h.checkedRepos(1), "a use 50 seconds after the last leaves it")
			h.clock.Advance(20 * time.Second)
			require.Empty(t, h.repoID(access, p, "o/x").asked)
			require.Equal(t, []string{"o/x", "o/y"}, h.checkedRepos(1), "a use 70 seconds after the last moves it")
		})
	}
}

// Until the next refresh a check stands, so a revoked access passes for at
// most one access token's life. The refresh asks GitHub about the checks,
// takes away the one that GitHub denies and keeps the other.
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
// since its own refresh. A denial takes the check away from every session.
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
			require.Empty(t, h.checkedRepos(1), "the denial took the check away")
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
// taking the check away.
func TestARenewalAlwaysAsksGitHub(t *testing.T) {
	t.Parallel()
	h := newGrantHarness(t, newFakeStore())
	p := h.principal(h.redeem(h.approve("login-1", 1)))
	access := h.userAccess()
	require.NoError(t, h.repoID(access, p, "o/x").err)

	for range 2 {
		got := h.repoIDFresh(access, p, "o/x")
		require.NoError(t, got.err)
		require.EqualValues(t, grantRepoID("o/x"), got.id)
		require.Equal(t, []string{"o/x"}, got.asked, "a renewal asks GitHub")
	}
	require.Equal(t, []string{"o/x"}, h.checkedRepos(1))
	h.github.set(func(g *grantGitHub) { g.denied["o/x"] = true })
	got := h.repoIDFresh(access, p, "o/x")
	require.ErrorIs(t, got.err, faucet.ErrNoAccess)
	require.Equal(t, []string{"o/x"}, got.asked)
	require.Empty(t, h.checkedRepos(1))
	got = h.repoID(access, p, "o/x")
	require.ErrorIs(t, got.err, faucet.ErrNoAccess, "the renewal's denial took the check away")
	require.Equal(t, []string{"o/x"}, got.asked)
}

// A denial answers no check, so each check of a denied repository asks
// GitHub again.
func TestADeniedRepositoryIsAskedAgainAtEachCheck(t *testing.T) {
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

// A pass whose lookup started first and that GitHub answers after a quick
// denial of the same repository never answers a check: the answer of the
// lookup that started later stands.
func TestAnOlderPassThatLandsAfterADenialDoesNotAnswer(t *testing.T) {
	t.Parallel()
	for name, newStore := range usertokensStores {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newGrantHarness(t, newStore(t))
			p := h.principal(h.redeem(h.approve("login-1", 1)))
			access := h.userAccess()
			h.clock.Advance(time.Minute)
			arrived, held := make(chan struct{}), make(chan struct{})
			release := sync.OnceFunc(func() { close(held) })
			t.Cleanup(release)
			var lookups atomic.Int32
			h.github.set(func(g *grantGitHub) {
				g.onRepo = func(*http.Request) {
					if lookups.Add(1) == 1 {
						close(arrived)
						<-held
					}
				}
			})
			slow := make(chan accessAnswer, 1)
			go func() { slow <- h.repoID(access, p, "o/x") }()
			<-arrived

			h.clock.Advance(time.Minute)
			h.github.set(func(g *grantGitHub) { g.denied["o/x"] = true })
			require.ErrorIs(t, h.repoIDFresh(access, p, "o/x").err, faucet.ErrNoAccess)
			h.github.set(func(g *grantGitHub) { delete(g.denied, "o/x") })
			release()
			require.NoError(t, (<-slow).err, "GitHub let the slow lookup pass")

			require.Empty(t, h.checkedRepos(1), "the denial started later, so it stands")
			got := h.repoID(access, p, "o/x")
			require.NoError(t, got.err)
			require.Equal(t, []string{"o/x"}, got.asked, "the older pass does not answer")
		})
	}
}

// A renamed repository that a user checks under its old and its new name
// holds a check under each. A denial of the new name takes away the check
// of the old one too, so the next check of the old name asks GitHub.
func TestADenialTakesAwayTheChecksOfARenamedRepository(t *testing.T) {
	t.Parallel()
	for name, newStore := range usertokensStores {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newGrantHarness(t, newStore(t))
			p := h.principal(h.redeem(h.approve("login-1", 1)))
			access := h.userAccess()
			h.github.set(func(g *grantGitHub) { g.renamed["org/old"] = "org/new" })
			require.NoError(t, h.repoID(access, p, "org/old").err)
			h.clock.Advance(time.Minute)
			require.NoError(t, h.repoID(access, p, "org/new").err)
			require.NoError(t, h.repoID(access, p, "o/x").err)
			require.Equal(t, []string{"o/x", "org/new", "org/old"}, h.checkedRepos(1))
			h.clock.Advance(time.Minute)

			h.github.set(func(g *grantGitHub) { g.denied["org/new"], g.denied["org/old"] = true, true })
			require.ErrorIs(t, h.repoIDFresh(access, p, "org/new").err, faucet.ErrNoAccess)
			require.Equal(t, []string{"o/x"}, h.checkedRepos(1), "the denial took both names away")
			got := h.repoID(access, p, "org/old")
			require.ErrorIs(t, got.err, faucet.ErrNoAccess)
			require.Equal(t, []string{"org/old"}, got.asked, "the old name asks GitHub")
		})
	}
}

// The purge deletes a denial 15 minutes after its lookup started, when no
// session takes a pass older than it any more, and keeps a newer one.
func TestThePurgeDeletesADenialAfterFifteenMinutes(t *testing.T) {
	t.Parallel()
	for name, newStore := range usertokensStores {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newGrantHarness(t, newStore(t))
			p := h.principal(h.redeem(h.approve("login-1", 1)))
			access := h.userAccess()
			h.github.set(func(g *grantGitHub) { g.denied["o/old"], g.denied["o/new"] = true, true })
			require.ErrorIs(t, h.repoID(access, p, "o/old").err, faucet.ErrNoAccess)
			h.clock.Advance(10 * time.Minute)
			require.ErrorIs(t, h.repoID(access, p, "o/new").err, faucet.ErrNoAccess)
			h.clock.Advance(5*time.Minute + 30*time.Second)

			res, err := h.server.Purge(context.Background())
			require.NoError(t, err)
			require.EqualValues(t, 1, res.UnusedAccessChecks, "the denial of o/old is 15 minutes old")
			h.clock.Advance(10 * time.Minute)
			res, err = h.server.Purge(context.Background())
			require.NoError(t, err)
			require.EqualValues(t, 1, res.UnusedAccessChecks, "the denial of o/new is 15 minutes old too")
		})
	}
}

// A caller that gives up right after GitHub denied a repository gets the
// denial, and the denial still takes the stored check away.
func TestADenialCountsWhenItsCallerGivesUpRightAfterIt(t *testing.T) {
	t.Parallel()
	for name, newStore := range usertokensStores {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newGrantHarness(t, newStore(t))
			p := h.principal(h.redeem(h.approve("login-1", 1)))
			access := h.userAccess()
			h.storeCheck(1, "o/x", h.clock.Now().Add(-time.Minute))
			h.github.set(func(g *grantGitHub) { g.denied["o/x"] = true })

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			h.repos.cancelAfterAnswer(cancel)
			_, err := access.RepoID(ctx, p, "o/x")
			require.ErrorIs(t, err, faucet.ErrNoAccess)
			require.NotErrorIs(t, err, context.Canceled)
			require.Empty(t, h.checkedRepos(1), "the denial took the check away")
		})
	}
}

// A denial stands when the store cannot record it. A pass that the
// store cannot keep is an error.
func TestADenialStandsWhenTheStoreCannotRecordIt(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	h := newGrantHarness(t, store)
	p := h.principal(h.redeem(h.approve("login-1", 1)))
	access := h.userAccess()
	broken := errors.New("the store is down")
	store.fail("StoreAccessCheck", broken)
	h.github.set(func(g *grantGitHub) { g.denied["o/x"] = true })

	require.ErrorIs(t, h.repoID(access, p, "o/x").err, faucet.ErrNoAccess)
	require.ErrorIs(t, h.repoID(access, p, "o/y").err, broken)
}

// A refresh whose request ends right after GitHub denied a repository that
// the refresh checks again still takes the check away, and answers with
// tokens that work.
func TestARecheckDenialCountsWhenTheRefreshRequestEndsRightAfterIt(t *testing.T) {
	t.Parallel()
	for name, newStore := range usertokensStores {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newGrantHarness(t, newStore(t))
			s := h.redeem(h.approve("login-1", 1))
			h.storeCheck(1, "o/x", h.clock.Now())
			h.github.set(func(g *grantGitHub) { g.denied["o/x"] = true })
			h.clock.Advance(time.Minute)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			h.repos.cancelAfterAnswer(cancel)
			next := grantTokens(t, s.id, h.serveToken(ctx, grantRefreshForm(s.refresh)))
			require.Empty(t, h.checkedRepos(1), "the denial took the check away")
			h.principal(next)
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

	// The re-check takes half of the 4 seconds, which leaves the grant 2
	// seconds to answer in, more than a slow run under -race needs.
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	deadline, _ := ctx.Deadline()
	reply := h.serveToken(ctx, grantRefreshForm(s.refresh))
	require.True(t, time.Now().Before(deadline), "the refresh answered before its deadline")
	next := grantTokens(t, s.id, reply)
	require.NotEmpty(t, h.github.lookups()[before:], "the refresh asked GitHub")

	h.github.set(func(g *grantGitHub) { g.onRepo = nil })
	require.ElementsMatch(t, []string{"o/x", "o/y"}, h.checkedRepos(1), "the checks stay")
	got := h.repoID(access, h.principal(next), "o/x")
	require.NoError(t, got.err)
	require.Equal(t, []string{"o/x"}, got.asked, "a check that the refresh could not finish is asked on use")
}

// Nothing that the re-check meets fails the refresh: the grant answers with
// tokens that work when the store cannot read the checks, and when it
// cannot keep the answer of a lookup.
func TestARefreshAnswersWithWorkingTokensWhenItsRecheckFails(t *testing.T) {
	t.Parallel()
	for method, asks := range map[string]bool{"RecentAccessChecks": false, "StoreAccessCheck": true} {
		t.Run(method, func(t *testing.T) {
			t.Parallel()
			store := newFakeStore()
			h := newGrantHarness(t, store)
			s := h.redeem(h.approve("login-1", 1))
			h.storeChecks(1, numberedRepos(3))
			h.clock.Advance(time.Minute)
			store.fail(method, errors.New("the store is down"))

			next, asked := h.refreshAsking(s)
			require.Equal(t, asks, len(asked) > 0, "the re-check asked GitHub: %v", asked)
			store.fail(method, nil)
			h.principal(next)
			h.refresh(next)
		})
	}
}

// When GitHub fails the refresh of the user's GitHub token, the grant goes
// on with the stored token and asks GitHub about no repository, and makes no
// second token request.
func TestARefreshWhoseGitHubRefreshFailsAsksGitHubAboutNoRepository(t *testing.T) {
	t.Parallel()
	h := newGrantHarness(t, newFakeStore())
	s := h.redeem(h.approve("login-1", 1))
	h.clock.Advance(usertokensSoon)
	h.storeChecks(1, numberedRepos(3))
	h.github.set(func(g *grantGitHub) { g.tokenStatus = http.StatusBadGateway })

	next, asked := h.refreshAsking(s)
	require.Empty(t, asked, "the refresh asked GitHub about no repository")
	refreshes, _ := h.github.counts()
	require.Equal(t, 1, refreshes, "GitHub saw one token request")
	h.principal(next)
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

// When GitHub rejects the token during the re-check, the denials of the
// checks that it did not get to share one time limit, so a stalled store
// holds the grant's answer back by that limit once, not once per check.
func TestTheDenialsAfterARejectedTokenShareOneTimeLimit(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	h := newGrantHarness(t, store)
	s := h.redeem(h.approve("login-1", 1))
	h.storeChecks(1, numberedRepos(12))
	stored, _, err := oauth.GrantOpen(h.login.Keys, usertokensConnection(t, h, 1))
	require.NoError(t, err)
	h.github.rejectAccess(stored)
	h.clock.Advance(time.Minute)
	// The lookups that started record their own answers; every write after
	// those stalls.
	store.stallAfter("StoreAccessCheck", 4)

	began := time.Now()
	h.refresh(s)
	require.Less(t, time.Since(began), 15*time.Second, "the denials of the checks left wait for the store once")
}

// The user revokes the App, which kills both of their GitHub tokens, and
// the refresh of one session meets the dead token as it checks again the
// repository of another session. The refresh asks GitHub for no new token
// and takes the check away. The other session's next check of it asks
// GitHub, refreshes the token, and GitHub refuses, which ends every session
// of the user.
func TestARecheckThatMeetsARevokedAppLeavesTheOtherSessionsNoCheck(t *testing.T) {
	t.Parallel()
	for name, newStore := range usertokensStores {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newGrantHarness(t, newStore(t))
			a := h.redeem(h.approve("login-a", 1))
			h.clock.Advance(5 * time.Minute)
			b := h.principal(h.redeem(h.approve("login-b", 1)))
			access := h.userAccess()
			require.NoError(t, h.repoID(access, b, "o/secret").err)
			h.clock.Advance(time.Minute)
			h.github.revoke(1)
			h.clock.Advance(9 * time.Minute)

			_, asked := h.refreshAsking(a)
			require.Equal(t, []string{"o/secret"}, asked)
			require.Empty(t, h.checkedRepos(1), "the rejected token took the check away")
			h.clock.Advance(time.Minute)
			got := h.repoID(access, b, "o/secret")
			require.ErrorIs(t, got.err, faucet.ErrInvalidToken)
			require.Equal(t, []string{"o/secret"}, got.asked, "the check of o/secret no longer answers")
			require.False(t, h.session("login-b").Live(), "GitHub refused the refresh token, which ends b")
			require.False(t, h.session("login-a").Live(), "and a")
		})
	}
}

// GitHub rejecting the user's token even after a refresh takes the check
// away, as a denial does: the next check asks GitHub.
func TestATokenThatGitHubRejectsAfterARefreshLeavesNoCheck(t *testing.T) {
	t.Parallel()
	for name, newStore := range usertokensStores {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newGrantHarness(t, newStore(t))
			p := h.principal(h.redeem(h.approve("login-1", 1)))
			access := h.userAccess()
			require.NoError(t, h.repoID(access, p, "o/x").err)
			h.github.set(func(g *grantGitHub) { g.repoStatus = http.StatusUnauthorized })

			got := h.repoIDFresh(access, p, "o/x")
			require.ErrorIs(t, got.err, faucet.ErrInvalidToken)
			require.Equal(t, []string{"o/x", "o/x"}, got.asked, "it refreshed the token and asked again")
			require.Empty(t, h.checkedRepos(1))
			h.github.set(func(g *grantGitHub) { g.repoStatus = 0 })
			got = h.repoID(access, p, "o/x")
			require.NoError(t, got.err)
			require.Equal(t, []string{"o/x"}, got.asked, "with the check gone, the next check asks GitHub")
		})
	}
}

// When GitHub rejects the stored token of the user as a refresh checks the
// repositories again, the refresh asks GitHub for no new token, because the
// rotation is stored and a slow GitHub would hold the grant's answer past
// the server's write deadline. It answers at once, and every check that it
// was to ask about becomes a denial, so each session's next check asks
// GitHub, and that check refreshes the token.
func TestARecheckThatGitHubRejectsTheTokenOfRefreshesNoTokenAndTakesTheChecksAway(t *testing.T) {
	t.Parallel()
	const tokenDelay = 5 * time.Second
	for name, newStore := range usertokensStores {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newGrantHarness(t, newStore(t))
			s := h.redeem(h.approve("login-1", 1))
			other := h.principal(h.redeem(h.approve("login-2", 1)))
			access := h.userAccess()
			repos := numberedRepos(6)
			h.storeChecks(1, repos)
			stored, _, err := oauth.GrantOpen(h.login.Keys, usertokensConnection(t, h, 1))
			require.NoError(t, err)
			h.github.rejectAccess(stored)
			h.github.set(func(g *grantGitHub) { g.onRefresh = func() { time.Sleep(tokenDelay) } })
			h.clock.Advance(time.Minute)

			began := time.Now()
			_, asked := h.refreshAsking(s)
			took := time.Since(began)
			refreshes, _ := h.github.counts()
			require.Zero(t, refreshes, "the refresh asked GitHub for no new token after the rotation")
			require.Less(t, took, tokenDelay/2, "the grant did not wait for GitHub's token endpoint")
			require.NotEmpty(t, asked, "the refresh checked the repositories again")
			require.Empty(t, h.checkedRepos(1), "every check that the refresh was to ask about is a denial now")

			h.github.set(func(g *grantGitHub) { g.onRefresh = nil })
			got := h.repoID(access, other, repos[0])
			require.NoError(t, got.err)
			require.Equal(t, []string{repos[0], repos[0]}, got.asked, "the other session asks GitHub, which rejects the token, refreshes it and asks again")
			refreshes, _ = h.github.counts()
			require.Equal(t, 1, refreshes)
		})
	}
}

// A name that only its lower case makes valid, as the Kelvin sign folds to
// k, is no repository name: the check refuses it as written, asks GitHub
// nothing and stores nothing.
func TestACheckRefusesANameThatOnlyItsLowerCaseMakesValid(t *testing.T) {
	t.Parallel()
	h := newGrantHarness(t, newFakeStore())
	p := h.principal(h.redeem(h.approve("login-1", 1)))
	access := h.userAccess()
	const kelvin = "o/\u212Ax"
	_, _, refused := githubapi.ParseRepo(kelvin)
	require.Error(t, refused)
	_, _, err := githubapi.ParseRepo(strings.ToLower(kelvin))
	require.NoError(t, err, "the lower case of the name is valid")

	for name, check := range map[string]func(oauth.UserAccess, faucet.Principal, string) accessAnswer{"RepoID": h.repoID, "RepoIDFresh": h.repoIDFresh} {
		got := check(access, p, kelvin)
		require.EqualError(t, got.err, refused.Error(), name)
		require.Empty(t, got.asked, name)
	}
	require.Empty(t, h.checkedRepos(1))
}

// rememberingRepos answers RepoID with the first grant that GitHub gave for
// a repository, as a repository checker that remembers answers does, and
// asks GitHub at every RepoIDFresh.
type rememberingRepos struct {
	faucet.TokenAuthorizer
	mu      sync.Mutex
	granted map[string]int64
}

func (r *rememberingRepos) RepoID(ctx context.Context, p faucet.Principal, repo string) (int64, error) {
	r.mu.Lock()
	id, ok := r.granted[repo]
	r.mu.Unlock()
	if ok {
		return id, nil
	}
	id, err := r.TokenAuthorizer.RepoID(ctx, p, repo)
	if err == nil {
		r.mu.Lock()
		r.granted[repo] = id
		r.mu.Unlock()
	}
	return id, err
}

// A repository checker that remembers answers still answers a renewal
// fresh: the access check asks it with RepoIDFresh, never with RepoID.
func TestARenewalThroughACachingRepositoryCheckerAsksGitHub(t *testing.T) {
	t.Parallel()
	h := newGrantHarness(t, newFakeStore())
	p := h.principal(h.redeem(h.approve("login-1", 1)))
	access, err := oauth.NewUserAccess(oauth.UserAccessDeps{
		Store: h.store, Clock: h.clock, Logger: slog.Make(), Login: *h.login,
		Repos: &rememberingRepos{TokenAuthorizer: h.github.repos(), granted: map[string]int64{}},
	})
	require.NoError(t, err)
	require.NoError(t, h.repoID(access, p, "o/x").err)
	h.github.set(func(g *grantGitHub) { g.denied["o/x"] = true })

	got := h.repoIDFresh(access, p, "o/x")
	require.ErrorIs(t, got.err, faucet.ErrNoAccess)
	require.Equal(t, []string{"o/x"}, got.asked, "the renewal asked GitHub")
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
