package oauth_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/coder/superblink-events/faucet"
	"github.com/coder/superblink-events/faucet/oauth"
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

// A token keeps the scopes it was issued with, less any that the client no
// longer holds, and gets the grants that the client holds now. An update
// that widens the client widens no token.
func TestAnUpdateOfTheClientReachesItsTokensAtOnce(t *testing.T) {
	t.Parallel()
	store, _, _ := newServiceAccess(t)
	ctx := context.Background()
	resolve := oauth.ServiceResolver(store)

	_, err := store.UpdateClient(ctx, "ci-watcher", oauth.ClientUpdate{Grants: []string{"repo:coder/coder"}, Scopes: []faucet.Scope{faucet.ScopeEventsRead}})
	require.NoError(t, err)
	token := bothScopes
	p, err := resolve(ctx, "ci-watcher", token)
	require.NoError(t, err)
	require.Equal(t, []faucet.Scope{faucet.ScopeEventsRead}, p.Scopes(), "a token loses a scope that the client lost")
	require.Equal(t, bothScopes, token, "the resolver leaves the token's scopes alone")
	client, ok := p.ServiceClient()
	require.True(t, ok)
	require.Equal(t, []string{"repo:coder/coder"}, client.Grants)
	_, err = resolve(ctx, "ci-watcher", []faucet.Scope{faucet.ScopeEventsSubscribe})
	require.ErrorIs(t, err, faucet.ErrInvalidToken, "a token whose scopes the client holds none of is refused")

	_, err = store.UpdateClient(ctx, "ci-watcher", oauth.ClientUpdate{Scopes: bothScopes})
	require.NoError(t, err)
	p, err = resolve(ctx, "ci-watcher", []faucet.Scope{faucet.ScopeEventsRead})
	require.NoError(t, err)
	require.Equal(t, []faucet.Scope{faucet.ScopeEventsRead}, p.Scopes(), "a token gains no scope that the client gained")
}

func TestAClientUpdateSetsSomethingValid(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		u    oauth.ClientUpdate
		says string
	}{
		"nothing":          {oauth.ClientUpdate{}, "sets the grants, the scopes or both"},
		"no grant":         {oauth.ClientUpdate{Grants: []string{}}, "at least one grant"},
		"no scope":         {oauth.ClientUpdate{Scopes: []faucet.Scope{}}, "at least one scope"},
		"an unknown scope": {oauth.ClientUpdate{Scopes: []faucet.Scope{"events:everything"}}, "events:everything"},
		"an unbound grant": {oauth.ClientUpdate{Grants: []string{"org:coder"}}, "names no account"},
	} {
		require.ErrorContains(t, tc.u.Validate(), tc.says, name)
	}
	require.NoError(t, oauth.ClientUpdate{Grants: []string{"org:coder#7", "all"}}.Validate())
	require.NoError(t, oauth.ClientUpdate{Scopes: bothScopes}.Validate())
}

// An update keeps an org grant that the client holds with no lookup, so a
// grant copied from the list survives a rename, and refuses an org:<login>
// that would move the grant to another account unseen.
func TestRebindGrantsKeepsTheAccountsThatTheClientHolds(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repos := fakeRepos{owners: map[string]faucet.Owner{"coder": {ID: 8, Login: "coder"}, "octo": {ID: 9, Login: "Octo"}}}
	held := []string{"org:Coder#7", "repo:coder/faucet"}

	got, err := oauth.RebindGrants(ctx, repos, held, []string{"org:coder#7", "repo:coder/coder", "org:octo"})
	require.NoError(t, err)
	require.Equal(t, []string{"org:Coder#7", "repo:coder/coder", "org:Octo#9"}, got, "a held grant stays as it is, and a new one is bound")

	_, err = oauth.RebindGrants(ctx, repos, held, []string{"org:coder"})
	require.ErrorContains(t, err, "the client holds org:Coder#7, and coder names account 8 now: write org:Coder#7 to keep that grant, or org:coder#8 for the account that holds the login now")
	got, err = oauth.RebindGrants(ctx, repos, held, []string{"org:coder#8"})
	require.NoError(t, err, "naming the account is a choice")
	require.Equal(t, []string{"org:coder#8"}, got)
	_, err = oauth.RebindGrants(ctx, repos, held, []string{"org:coder#6"})
	require.ErrorContains(t, err, "names account 6", "a grant that the client does not hold is bound as at creation")
	_, err = oauth.RebindGrants(ctx, repos, held, []string{"org:nobody"})
	require.ErrorIs(t, err, faucet.ErrUnknownOwner)
	got, err = oauth.RebindGrants(ctx, repos, []string{"org:octo"}, []string{"org:octo"})
	require.NoError(t, err, "an unbound grant that the client holds from before the migration is bound")
	require.Equal(t, []string{"org:Octo#9"}, got)
	_, err = oauth.RebindGrants(ctx, repos, []string{"org:gone#5"}, []string{"org:gone"})
	require.ErrorIs(t, err, faucet.ErrUnknownOwner)
	require.ErrorContains(t, err, "the client holds org:gone#5: write that to keep the grant", "a login that no account holds now points to the held grant")

	got, err = oauth.RebindGrants(ctx, repos, []string{"org:coder#7", "org:coder#8"}, []string{"org:coder"})
	require.NoError(t, err, "the client holds the login with the account that holds it now too")
	require.Equal(t, []string{"org:coder#8"}, got)

	repos.owners["coder"] = faucet.Owner{ID: 7, Login: "coder"}
	got, err = oauth.RebindGrants(ctx, repos, held, []string{"org:coder"})
	require.NoError(t, err)
	require.Equal(t, []string{"org:coder#7"}, got, "the login names the account that the client holds")
}
