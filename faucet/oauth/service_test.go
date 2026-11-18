package oauth_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/coder/faucet/faucet"
	"github.com/coder/faucet/faucet/oauth"
)

type fakeRepos map[string]faucet.Repository

func (r fakeRepos) RepositoryByName(_ context.Context, fullName string) (faucet.Repository, error) {
	for name, repo := range r {
		if strings.EqualFold(name, fullName) {
			return repo, nil
		}
	}
	return faucet.Repository{}, faucet.ErrUnknownRepository
}

func newServiceAccess(t *testing.T) (*fakeStore, oauth.ServiceAccess, faucet.Principal) {
	t.Helper()
	store := newFakeStore()
	c, _, err := oauth.NewClient("ci-watcher", resource, bothScopes, []string{"repo:coder/faucet", "repo:coder/new"}, time.Now())
	require.NoError(t, err)
	require.NoError(t, store.InsertClient(context.Background(), c))
	access, err := oauth.NewServiceAccess(store, fakeRepos{
		"coder/faucet": {ID: 42, OwnerID: 7, OwnerLogin: "coder", FullName: "coder/faucet"},
		"coder/coder":  {ID: 43, OwnerID: 7, OwnerLogin: "coder", FullName: "coder/coder"},
	})
	require.NoError(t, err)
	p, err := oauth.ServiceResolver(store)(context.Background(), "ci-watcher", bothScopes)
	require.NoError(t, err)
	return store, access, p
}

func TestAServiceReadsTheRepositoriesItsGrantsName(t *testing.T) {
	t.Parallel()
	_, access, p := newServiceAccess(t)
	ctx := context.Background()

	for _, check := range []func(context.Context, faucet.Principal, string) (int64, error){access.RepoID, access.RepoIDFresh} {
		for _, repo := range []string{"coder/faucet", "Coder/Faucet"} {
			id, err := check(ctx, p, repo)
			require.NoError(t, err, repo)
			require.Equal(t, int64(42), id, "a grant matches a name as GitHub does, ignoring case")
		}

		_, err := check(ctx, p, "coder/coder")
		require.ErrorIs(t, err, faucet.ErrNoAccess, "a repository that no grant names")
		require.ErrorContains(t, err, "no grant of this service names coder/coder")

		_, err = check(ctx, p, "coder/new")
		require.ErrorIs(t, err, faucet.ErrNoAccess, "a granted repository that no delivery named yet")
		require.ErrorContains(t, err, "faucet has received no webhook delivery from coder/new yet")

		_, err = check(ctx, p, "coder")
		require.Error(t, err)
		require.NotErrorIs(t, err, faucet.ErrNoAccess, "a malformed name is the caller's mistake")
	}
}

func TestOnlyAServiceIsCheckedByGrants(t *testing.T) {
	t.Parallel()
	_, access, _ := newServiceAccess(t)
	person, err := faucet.NewPrincipal("alice", "ghp_x")
	require.NoError(t, err)
	_, err = access.RepoID(context.Background(), person, "coder/faucet")
	require.ErrorIs(t, err, faucet.ErrNoAccess)
	_, err = access.RepoIDFresh(context.Background(), person, "coder/faucet")
	require.ErrorIs(t, err, faucet.ErrNoAccess)
}

// A renewal checks access again at the source (R3): for a service, the
// client as the store holds it now.
func TestAFreshCheckReadsTheClientAgain(t *testing.T) {
	t.Parallel()
	store, access, p := newServiceAccess(t)
	ctx := context.Background()

	store.mu.Lock()
	store.clients[0].Grants = []string{"repo:coder/coder"}
	store.mu.Unlock()
	_, err := access.RepoID(ctx, p, "coder/faucet")
	require.NoError(t, err, "the cached check uses the grants of the principal")
	_, err = access.RepoIDFresh(ctx, p, "coder/faucet")
	require.ErrorIs(t, err, faucet.ErrNoAccess)
	id, err := access.RepoIDFresh(ctx, p, "coder/coder")
	require.NoError(t, err)
	require.Equal(t, int64(43), id)

	_, err = store.DisableClient(ctx, "ci-watcher", time.Now())
	require.NoError(t, err)
	_, err = access.RepoIDFresh(ctx, p, "coder/coder")
	require.ErrorIs(t, err, faucet.ErrInvalidToken, "a disabled client holds no access")
}

func TestTheServiceResolverFindsOnlyALiveClient(t *testing.T) {
	t.Parallel()
	store, _, p := newServiceAccess(t)
	ctx := context.Background()
	require.Equal(t, "svc:ci-watcher", p.Login)
	require.Equal(t, bothScopes, p.Scopes())

	resolve := oauth.ServiceResolver(store)
	narrow, err := resolve(ctx, "ci-watcher", []faucet.Scope{faucet.ScopeEventsRead})
	require.NoError(t, err)
	require.Equal(t, []faucet.Scope{faucet.ScopeEventsRead}, narrow.Scopes(), "the principal holds the token's scopes")

	_, err = resolve(ctx, "nobody", bothScopes)
	require.ErrorIs(t, err, faucet.ErrInvalidToken)
	_, err = store.DisableClient(ctx, "ci-watcher", time.Now())
	require.NoError(t, err)
	_, err = resolve(ctx, "ci-watcher", bothScopes)
	require.ErrorIs(t, err, faucet.ErrInvalidToken)
}
