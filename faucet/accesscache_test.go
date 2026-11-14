package faucet_test

import (
	"context"
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
	authCalls []string
	repoCalls []string
}

func newFakeChecker() *fakeChecker {
	return &fakeChecker{logins: map[string]string{testToken: "octocat"}, repos: map[string]int64{testRepo: repoID, otherRepo: otherRepoID}}
}

func (c *fakeChecker) Authenticate(_ context.Context, token string) (faucet.Principal, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.authCalls = append(c.authCalls, token)
	if c.err != nil {
		return faucet.Principal{}, c.err
	}
	login, ok := c.logins[token]
	if !ok {
		return faucet.Principal{}, faucet.ErrInvalidToken
	}
	return faucet.NewPrincipal(login, token)
}

func (c *fakeChecker) RepoID(_ context.Context, p faucet.Principal, repo string) (int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.repoCalls = append(c.repoCalls, p.Login+" "+repo)
	if c.err != nil {
		return 0, c.err
	}
	id, ok := c.repos[repo]
	if !ok {
		return 0, faucet.ErrNoAccess
	}
	return id, nil
}

func (c *fakeChecker) calls() (auth, repo int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.authCalls), len(c.repoCalls)
}

func (c *fakeChecker) revoke(repo string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.repos, repo)
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

	_, err := cache.Authenticate(ctx, "ghp_rejected")
	require.ErrorIs(t, err, faucet.ErrInvalidToken)
	_, err = cache.Authenticate(ctx, "ghp_rejected")
	require.ErrorIs(t, err, faucet.ErrInvalidToken)
	checker.logins["ghp_rejected"] = "octocat"
	_, err = cache.Authenticate(ctx, "ghp_rejected")
	require.NoError(t, err, "a token that GitHub rejected is not refused again from memory")

	p := principal(t)
	denied := "octo/private"
	for range 2 {
		_, err = cache.RepoID(ctx, p, denied)
		require.ErrorIs(t, err, faucet.ErrNoAccess)
	}
	checker.repos[denied] = 77
	id, err := cache.RepoID(ctx, p, denied)
	require.NoError(t, err, "a repository that was denied is allowed at once when GitHub allows it")
	require.EqualValues(t, 77, id)

	checker.err = fmt.Errorf("GitHub rate limit reached")
	_, err = cache.RepoID(ctx, p, otherRepo)
	require.Error(t, err)
	checker.err = nil
	id, err = cache.RepoID(ctx, p, otherRepo)
	require.NoError(t, err, "an error is not remembered")
	require.EqualValues(t, otherRepoID, id)
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
	require.Equal(t, "octocat", mine.Login)
	require.Equal(t, "hubot", theirs.Login, "the login of one token is not given to another")

	_, err = cache.RepoID(ctx, mine, testRepo)
	require.NoError(t, err)
	_, err = cache.RepoID(ctx, mine, otherRepo)
	require.NoError(t, err)
	_, err = cache.RepoID(ctx, theirs, testRepo)
	require.NoError(t, err)
	_, repo := checker.calls()
	require.Equal(t, 3, repo, "another repository and another token each ask GitHub")

	again, err := cache.Authenticate(ctx, testToken)
	require.NoError(t, err)
	require.Equal(t, "octocat", again.Login)
	_, err = cache.RepoID(ctx, again, testRepo)
	require.NoError(t, err)
	_, repo = checker.calls()
	require.Equal(t, 3, repo, "the principal of a cached login still reaches the cached answers")
}

func TestAccessCacheNeverHoldsTheToken(t *testing.T) {
	t.Parallel()
	cache := faucet.NewAccessCache(newFakeChecker(), quartz.NewMock(t))
	_, err := authenticateAndAuthorize(t, cache, testToken, testRepo)
	require.NoError(t, err)

	requireNoToken(t, "the memory of the cache", fmt.Sprintf("%#v", cache))
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
	auth, _ := checker.calls()
	require.Equal(t, 10000, auth)

	checker.logins["ghp_overflow"] = "octocat"
	for range 2 {
		_, err := cache.Authenticate(ctx, "ghp_overflow")
		require.NoError(t, err)
	}
	_, err := cache.Authenticate(ctx, "ghp_token00000")
	require.NoError(t, err)
	auth, _ = checker.calls()
	require.Equal(t, 10002, auth, "a full cache answers the extra token from GitHub each time and still serves the entries it holds")
}
