package faucet_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/coder/faucet/faucet"
	"github.com/coder/quartz"
)

// fakeChecker answers like GitHub for the tokens in logins and the repos in
// repos, and counts the questions.
type fakeChecker struct {
	mu        sync.Mutex
	logins    map[string]string
	repos     map[string]int64
	err       error
	authCalls int
	repoCalls int
	// during runs inside each call, like the time a GitHub answer takes.
	during func()
	// afterAnswer runs once, after the next RepoID has decided and before it
	// returns, like the answer's way back while GitHub's state moves on.
	afterAnswer func()
}

func newFakeChecker() *fakeChecker {
	return &fakeChecker{logins: map[string]string{testToken: "octocat"}, repos: map[string]int64{testRepo: repoID, otherRepo: otherRepoID}}
}

func (c *fakeChecker) Authenticate(_ context.Context, token string) (faucet.Principal, error) {
	if c.during != nil {
		c.during()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.authCalls++
	if c.err != nil {
		return faucet.Principal{}, c.err
	}
	login, ok := c.logins[token]
	if !ok {
		return faucet.Principal{}, faucet.ErrInvalidToken
	}
	return faucet.NewPrincipal(login, token)
}

func (c *fakeChecker) RepoID(_ context.Context, _ faucet.Principal, repo string) (int64, error) {
	if c.during != nil {
		c.during()
	}
	id, err, afterAnswer := c.decide(repo)
	if afterAnswer != nil {
		afterAnswer()
	}
	return id, err
}

func (c *fakeChecker) decide(repo string) (int64, error, func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.repoCalls++
	afterAnswer := c.afterAnswer
	c.afterAnswer = nil
	if c.err != nil {
		return 0, c.err, afterAnswer
	}
	id, ok := c.repos[repo]
	if !ok {
		return 0, faucet.ErrNoAccess, afterAnswer
	}
	return id, nil, afterAnswer
}

func (c *fakeChecker) calls() (auth, repo int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.authCalls, c.repoCalls
}

func (c *fakeChecker) revoke(repo string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.repos, repo)
}

func (c *fakeChecker) grant(repo string, id int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.repos[repo] = id
}

func authenticateAndAuthorize(t *testing.T, cache *faucet.AccessCache, token, repo string) (int64, error) {
	t.Helper()
	p, err := cache.Authenticate(context.Background(), token)
	require.NoError(t, err)
	return cache.RepoID(context.Background(), p, repo)
}

func TestAccessCacheAsksGitHubOncePerMinuteAndThenAgain(t *testing.T) {
	t.Parallel()
	checker := newFakeChecker()
	clock := quartz.NewMock(t)
	cache := faucet.NewAccessCache(checker, clock)

	for range 3 {
		id, err := authenticateAndAuthorize(t, cache, testToken, testRepo)
		require.NoError(t, err)
		require.EqualValues(t, repoID, id)
	}
	auth, repo := checker.calls()
	require.Equal(t, [2]int{1, 1}, [2]int{auth, repo}, "three requests cost one call of each kind")
	requireNoToken(t, "the memory of the cache", fmt.Sprintf("%#v", cache))

	checker.revoke(testRepo)
	clock.Advance(59 * time.Second)
	_, err := authenticateAndAuthorize(t, cache, testToken, testRepo)
	require.NoError(t, err, "inside the minute the remembered grant is used")

	clock.Advance(time.Second)
	_, err = authenticateAndAuthorize(t, cache, testToken, testRepo)
	require.ErrorIs(t, err, faucet.ErrNoAccess, "at a minute GitHub is asked again and the revocation takes effect")
	auth, repo = checker.calls()
	require.Equal(t, [2]int{2, 2}, [2]int{auth, repo})
}

func TestAccessCacheRemembersASuccessAndNothingElse(t *testing.T) {
	t.Parallel()
	checker := newFakeChecker()
	cache := faucet.NewAccessCache(checker, quartz.NewMock(t))
	ctx := context.Background()

	for range 2 {
		_, err := cache.Authenticate(ctx, "ghp_rejected")
		require.ErrorIs(t, err, faucet.ErrInvalidToken)
	}
	checker.logins["ghp_rejected"] = "octocat"
	_, err := cache.Authenticate(ctx, "ghp_rejected")
	require.NoError(t, err, "a token that GitHub rejected is not refused again from memory")

	p := principal(t)
	for range 2 {
		_, err = cache.RepoID(ctx, p, "octo/private")
		require.ErrorIs(t, err, faucet.ErrNoAccess)
	}
	checker.repos["octo/private"] = 77
	id, err := cache.RepoID(ctx, p, "octo/private")
	require.NoError(t, err, "a repository that was denied is allowed at once when GitHub allows it")
	require.EqualValues(t, 77, id)

	checker.err = errors.New("GitHub rate limit reached")
	_, err = cache.RepoID(ctx, p, otherRepo)
	require.Error(t, err)
	checker.err = nil
	_, err = cache.RepoID(ctx, p, otherRepo)
	require.NoError(t, err, "an error is not remembered")
}

func TestAccessCacheNeverAnswersForAnotherTokenOrRepository(t *testing.T) {
	t.Parallel()
	checker := newFakeChecker()
	checker.logins["ghp_other"] = "hubot"
	cache := faucet.NewAccessCache(checker, quartz.NewMock(t))
	ctx := context.Background()

	mine, err := cache.Authenticate(ctx, testToken)
	require.NoError(t, err)
	theirs, err := cache.Authenticate(ctx, "ghp_other")
	require.NoError(t, err)
	require.Equal(t, [2]string{"octocat", "hubot"}, [2]string{mine.Login, theirs.Login}, "the login of one token is not given to another")

	for _, ask := range []struct {
		p    faucet.Principal
		repo string
	}{{mine, testRepo}, {mine, otherRepo}, {theirs, testRepo}, {mine, testRepo}} {
		_, err = cache.RepoID(ctx, ask.p, ask.repo)
		require.NoError(t, err)
	}
	_, repo := checker.calls()
	require.Equal(t, 3, repo, "another repository and another token each ask GitHub, and a repeat does not")
}

func TestAccessCacheStopsAddingAtTenThousandEntriesAndKeepsWhatItHas(t *testing.T) {
	t.Parallel()
	checker := newFakeChecker()
	cache := faucet.NewAccessCache(checker, quartz.NewMock(t))
	ctx := context.Background()
	for i := range 10000 {
		token := fmt.Sprintf("ghp_token%05d", i)
		checker.logins[token] = "octocat"
		_, err := cache.Authenticate(ctx, token)
		require.NoError(t, err)
	}

	checker.logins["ghp_overflow"] = "octocat"
	for _, token := range []string{"ghp_overflow", "ghp_overflow", "ghp_token00000"} {
		_, err := cache.Authenticate(ctx, token)
		require.NoError(t, err)
	}
	auth, _ := checker.calls()
	require.Equal(t, 10002, auth, "a full cache answers the extra token from GitHub each time and still serves the entries it holds")
}

func TestAccessCacheForgetsAGrantThatARenewalFindsWithdrawn(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("the repository is no longer readable", func(t *testing.T) {
		t.Parallel()
		checker := newFakeChecker()
		cache := faucet.NewAccessCache(checker, quartz.NewMock(t))
		_, err := authenticateAndAuthorize(t, cache, testToken, testRepo)
		require.NoError(t, err)

		checker.revoke(testRepo)
		_, err = cache.RepoIDUncached(ctx, principal(t), testRepo)
		require.ErrorIs(t, err, faucet.ErrNoAccess)
		_, err = cache.RepoID(ctx, principal(t), testRepo)
		require.ErrorIs(t, err, faucet.ErrNoAccess, "a poll after a renewal that lost access is refused, not answered from memory")
		_, err = cache.RepoID(ctx, principal(t), otherRepo)
		require.NoError(t, err, "another repository of the token keeps its answer")
	})

	t.Run("the token is rejected", func(t *testing.T) {
		t.Parallel()
		checker := newFakeChecker()
		cache := faucet.NewAccessCache(checker, quartz.NewMock(t))
		_, err := authenticateAndAuthorize(t, cache, testToken, testRepo)
		require.NoError(t, err)

		checker.err = faucet.ErrInvalidToken
		_, err = cache.RepoIDUncached(ctx, principal(t), testRepo)
		require.ErrorIs(t, err, faucet.ErrInvalidToken)
		_, err = cache.Authenticate(ctx, testToken)
		require.ErrorIs(t, err, faucet.ErrInvalidToken, "the remembered login of a rejected token is gone too")
	})

	t.Run("GitHub cannot be reached", func(t *testing.T) {
		t.Parallel()
		checker := newFakeChecker()
		cache := faucet.NewAccessCache(checker, quartz.NewMock(t))
		_, err := authenticateAndAuthorize(t, cache, testToken, testRepo)
		require.NoError(t, err)

		checker.err = errors.New("GitHub rate limit reached")
		_, err = cache.RepoIDUncached(ctx, principal(t), testRepo)
		require.Error(t, err)
		checker.err = nil
		_, repoCallsBefore := checker.calls()
		_, err = cache.RepoID(ctx, principal(t), testRepo)
		require.NoError(t, err)
		_, repoCalls := checker.calls()
		require.Equal(t, repoCallsBefore, repoCalls, "a failed check says nothing about access, so the answer stays")
	})
}

func TestAccessCacheCountsTheMinuteFromTheStartOfTheLookup(t *testing.T) {
	t.Parallel()
	checker := newFakeChecker()
	clock := quartz.NewMock(t)
	cache := faucet.NewAccessCache(checker, clock)
	checker.during = func() { clock.Advance(10 * time.Second) }
	ctx := context.Background()

	_, err := cache.RepoID(ctx, principal(t), testRepo)
	require.NoError(t, err)
	checker.during = nil

	clock.Advance(49 * time.Second)
	_, err = cache.RepoID(ctx, principal(t), testRepo)
	require.NoError(t, err)
	_, repoCalls := checker.calls()
	require.Equal(t, 1, repoCalls, "59 seconds after the lookup started the answer is still used")

	clock.Advance(time.Second)
	_, err = cache.RepoID(ctx, principal(t), testRepo)
	require.NoError(t, err)
	_, repoCalls = checker.calls()
	require.Equal(t, 2, repoCalls, "the 10 seconds that GitHub took do not add to the minute")
}

func TestAccessCacheRefusesAnEmptyRepositoryInsteadOfAnsweringFromTheLogin(t *testing.T) {
	t.Parallel()
	checker := newFakeChecker()
	checker.grant("", 5)
	cache := faucet.NewAccessCache(checker, quartz.NewMock(t))
	ctx := context.Background()
	p, err := cache.Authenticate(ctx, testToken)
	require.NoError(t, err)

	id, err := cache.RepoID(ctx, p, "")
	require.Error(t, err)
	require.Zero(t, id)
	_, err = cache.RepoIDUncached(ctx, p, "")
	require.Error(t, err, "a wrapped authorizer that would accept an empty repo is not asked")
	again, err := cache.Authenticate(ctx, testToken)
	require.NoError(t, err, "and nothing was stored where the remembered login lives")
	require.Equal(t, "octocat", again.Login)
}

func TestAccessCacheOrdersAnswersByTheStartOfTheirLookup(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	for name, delay := range map[string]time.Duration{"after": time.Second, "at the same instant as": 0} {
		t.Run("a grant that was in flight when a renewal started "+name+" its lookup, and found access withdrawn, is not stored", func(t *testing.T) {
			t.Parallel()
			checker := newFakeChecker()
			clock := quartz.NewMock(t)
			cache := faucet.NewAccessCache(checker, clock)
			checker.afterAnswer = func() {
				checker.revoke(testRepo)
				clock.Advance(delay)
				_, err := cache.RepoIDUncached(ctx, principal(t), testRepo)
				require.ErrorIs(t, err, faucet.ErrNoAccess)
			}

			id, err := cache.RepoID(ctx, principal(t), testRepo)
			require.NoError(t, err, "that poll gets what GitHub said when it asked")
			require.EqualValues(t, repoID, id)
			clock.Advance(time.Second)
			_, err = cache.RepoID(ctx, principal(t), testRepo)
			require.ErrorIs(t, err, faucet.ErrNoAccess, "the next poll is refused, not served the older grant for a minute")
		})
	}

	t.Run("a denial that was in flight does not remove a grant whose lookup started after it", func(t *testing.T) {
		t.Parallel()
		checker := newFakeChecker()
		clock := quartz.NewMock(t)
		cache := faucet.NewAccessCache(checker, clock)
		checker.revoke(testRepo)
		checker.afterAnswer = func() {
			checker.grant(testRepo, repoID)
			clock.Advance(time.Second)
			_, err := cache.RepoID(ctx, principal(t), testRepo)
			require.NoError(t, err)
		}

		_, err := cache.RepoIDUncached(ctx, principal(t), testRepo)
		require.ErrorIs(t, err, faucet.ErrNoAccess)
		_, callsBefore := checker.calls()
		_, err = cache.RepoID(ctx, principal(t), testRepo)
		require.NoError(t, err)
		_, calls := checker.calls()
		require.Equal(t, callsBefore, calls, "the fresher grant is still remembered")
	})

	t.Run("a grant whose lookup started after a denial is stored", func(t *testing.T) {
		t.Parallel()
		checker := newFakeChecker()
		clock := quartz.NewMock(t)
		cache := faucet.NewAccessCache(checker, clock)
		checker.revoke(testRepo)
		_, err := cache.RepoIDUncached(ctx, principal(t), testRepo)
		require.ErrorIs(t, err, faucet.ErrNoAccess)

		checker.grant(testRepo, repoID)
		clock.Advance(time.Millisecond)
		_, err = cache.RepoID(ctx, principal(t), testRepo)
		require.NoError(t, err)
		_, callsBefore := checker.calls()
		_, err = cache.RepoID(ctx, principal(t), testRepo)
		require.NoError(t, err)
		_, calls := checker.calls()
		require.Equal(t, callsBefore, calls, "a denial does not block the answers that come after it")
	})
}
