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

// fakeRepos holds the repositories that deliveries named, by full name, and
// the owner that the latest of them named by each login, in lower case.
type fakeRepos struct {
	repos  map[string]faucet.Repository
	owners map[string]faucet.Owner
}

func (r fakeRepos) RepositoryByName(_ context.Context, fullName string) (faucet.Repository, error) {
	for name, repo := range r.repos {
		if strings.EqualFold(name, fullName) {
			return repo, nil
		}
	}
	return faucet.Repository{}, faucet.ErrUnknownRepository
}

func (r fakeRepos) OwnerByLogin(_ context.Context, login string) (faucet.Owner, error) {
	o, ok := r.owners[strings.ToLower(login)]
	if !ok {
		return faucet.Owner{}, faucet.ErrUnknownOwner
	}
	return o, nil
}

func newServiceAccess(t *testing.T) (*fakeStore, oauth.ServiceAccess, faucet.Principal) {
	t.Helper()
	return newServiceAccessWith(t, "repo:coder/faucet", "repo:coder/new")
}

// newServiceAccessWith gives the client ci-watcher grants. Deliveries named
// two repositories of coder, account 7, one of octo, account 9, and one that
// coder owned under its old login, old-coder.
func newServiceAccessWith(t *testing.T, grants ...string) (*fakeStore, oauth.ServiceAccess, faucet.Principal) {
	t.Helper()
	ctx := context.Background()
	repos := fakeRepos{
		repos: map[string]faucet.Repository{
			"coder/faucet":   {ID: 42, OwnerID: 7, OwnerLogin: "coder", FullName: "coder/faucet"},
			"coder/coder":    {ID: 43, OwnerID: 7, OwnerLogin: "coder", FullName: "coder/coder"},
			"old-coder/tool": {ID: 44, OwnerID: 7, OwnerLogin: "old-coder", FullName: "old-coder/tool"},
			"octo/repo":      {ID: 45, OwnerID: 9, OwnerLogin: "octo", FullName: "octo/repo"},
		},
		owners: map[string]faucet.Owner{"coder": {ID: 7, Login: "coder"}, "octo": {ID: 9, Login: "octo"}},
	}
	bound, err := oauth.BindGrants(ctx, repos, grants)
	require.NoError(t, err)
	store := newFakeStore()
	c, _, err := oauth.NewClient("ci-watcher", resource, bothScopes, bound, time.Now())
	require.NoError(t, err)
	require.NoError(t, store.InsertClient(ctx, c))
	access, err := oauth.NewServiceAccess(store, repos)
	require.NoError(t, err)
	p, err := oauth.ServiceResolver(store)(ctx, "ci-watcher", bothScopes)
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
	_, access, _ := newServiceAccessWith(t, "all")
	person, err := faucet.NewPrincipal("alice", "ghp_x")
	require.NoError(t, err)
	ctx := context.Background()
	_, err = access.RepoID(ctx, person, "coder/faucet")
	require.ErrorIs(t, err, faucet.ErrNoAccess)
	_, err = access.RepoIDFresh(ctx, person, "coder/faucet")
	require.ErrorIs(t, err, faucet.ErrNoAccess)
	_, err = access.OwnerID(ctx, person, "coder")
	require.ErrorIs(t, err, faucet.ErrNoAccess)
	_, err = access.OwnerIDFresh(ctx, person, "coder")
	require.ErrorIs(t, err, faucet.ErrNoAccess)
	require.ErrorIs(t, access.CoversAll(ctx, person), faucet.ErrNoAccess)
	require.ErrorIs(t, access.CoversAllFresh(ctx, person), faucet.ErrNoAccess)
}

// An org grant covers the owner, and each repository whose delivery named
// the account that the grant's login names now, whatever the repository was
// called then.
func TestAnOrgGrantCoversTheRepositoriesOfTheOwnersAccount(t *testing.T) {
	t.Parallel()
	_, access, p := newServiceAccessWith(t, "org:Coder")
	ctx := context.Background()

	for _, check := range []func(context.Context, faucet.Principal, string) (int64, error){access.RepoID, access.RepoIDFresh} {
		for repo, want := range map[string]int64{"coder/faucet": 42, "CODER/coder": 43, "old-coder/tool": 44} {
			id, err := check(ctx, p, repo)
			require.NoError(t, err, repo)
			require.Equal(t, want, id, repo)
		}
		for _, repo := range []string{"octo/repo", "octo/unseen"} {
			_, err := check(ctx, p, repo)
			require.ErrorIs(t, err, faucet.ErrNoAccess, repo)
			require.ErrorContains(t, err, "no grant of this service names "+repo, "the answer does not tell which repositories of other owners faucet has seen")
		}
		_, err := check(ctx, p, "coder/new")
		require.ErrorContains(t, err, "faucet has received no webhook delivery from coder/new yet")
	}
	for _, check := range []func(context.Context, faucet.Principal, string) (int64, error){access.OwnerID, access.OwnerIDFresh} {
		id, err := check(ctx, p, "coder")
		require.NoError(t, err)
		require.Equal(t, int64(7), id, "a login matches as GitHub matches it, ignoring case")
		_, err = check(ctx, p, "octo")
		require.ErrorIs(t, err, faucet.ErrNoAccess)
		require.EqualError(t, err, "no grant of this service names the owner octo: repository not found or not accessible")
		_, err = check(ctx, p, "octo/repo")
		require.EqualError(t, err, "owner must be a GitHub login")
	}
	for _, check := range []func(context.Context, faucet.Principal) error{access.CoversAll, access.CoversAllFresh} {
		err := check(ctx, p)
		require.ErrorIs(t, err, faucet.ErrNoAccess)
		require.EqualError(t, err, "no grant of this service names every repository; that takes the grant all: repository not found or not accessible")
	}
}

func TestTheAllGrantCoversEveryRepositoryAndOwnerThatADeliveryNamed(t *testing.T) {
	t.Parallel()
	_, access, p := newServiceAccessWith(t, "all")
	ctx := context.Background()

	id, err := access.RepoIDFresh(ctx, p, "octo/repo")
	require.NoError(t, err)
	require.Equal(t, int64(45), id)
	id, err = access.OwnerID(ctx, p, "octo")
	require.NoError(t, err)
	require.Equal(t, int64(9), id)
	require.NoError(t, access.CoversAll(ctx, p))
	require.NoError(t, access.CoversAllFresh(ctx, p))

	_, err = access.OwnerIDFresh(ctx, p, "new-org")
	require.ErrorIs(t, err, faucet.ErrNoAccess)
	require.EqualError(t, err, "faucet has received no webhook delivery from a repository of new-org yet, so it cannot tell which owner the grant names; "+
		"it can once GitHub sends one: repository not found or not accessible")
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

	store.mu.Lock()
	store.clients[0].Grants = []string{"org:coder#7", "all"}
	store.mu.Unlock()
	_, err = access.OwnerID(ctx, p, "coder")
	require.ErrorIs(t, err, faucet.ErrNoAccess, "the cached check uses the grants of the principal")
	require.ErrorIs(t, access.CoversAll(ctx, p), faucet.ErrNoAccess)
	id, err = access.OwnerIDFresh(ctx, p, "coder")
	require.NoError(t, err)
	require.Equal(t, int64(7), id)
	require.NoError(t, access.CoversAllFresh(ctx, p))

	_, err = store.DisableClient(ctx, "ci-watcher", time.Now())
	require.NoError(t, err)
	_, err = access.RepoIDFresh(ctx, p, "coder/coder")
	require.ErrorIs(t, err, faucet.ErrInvalidToken, "a disabled client holds no access")
	_, err = access.OwnerIDFresh(ctx, p, "coder")
	require.ErrorIs(t, err, faucet.ErrInvalidToken)
	require.ErrorIs(t, access.CoversAllFresh(ctx, p), faucet.ErrInvalidToken)
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
