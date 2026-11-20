package oauth

import (
	"context"
	"slices"
	"strings"

	"golang.org/x/xerrors"

	"github.com/coder/superblink-events/faucet"
	"github.com/coder/superblink-events/faucet/internal/githubapi"
)

// ServiceResolver returns the SubjectResolver of service clients in store.
// The subject of a live client is a service with the grants that the client
// holds now and the token's scopes that it still holds. An unknown or
// disabled client, or one that holds none of the token's scopes, is
// faucet.ErrInvalidToken. So an operator who disables a client or updates
// what it holds changes what its tokens do on their next request.
func ServiceResolver(store Store) SubjectResolver {
	return func(ctx context.Context, id string, scopes []faucet.Scope) (faucet.Principal, error) {
		c, err := liveClient(ctx, store, id)
		if err != nil {
			return faucet.Principal{}, err
		}
		var held []faucet.Scope
		for _, s := range scopes {
			if slices.Contains(c.Scopes, s) {
				held = append(held, s)
			}
		}
		if len(held) == 0 {
			return faucet.Principal{}, rejected("the client holds none of the token's scopes")
		}
		return faucet.NewService(faucet.ServiceClient{ClientID: c.ID, Grants: c.Grants}, held)
	}
}

// liveClient returns the client with this ID, and faucet.ErrInvalidToken
// when it is unknown or disabled.
func liveClient(ctx context.Context, store Store, id string) (Client, error) {
	c, err := store.Client(ctx, id)
	switch {
	case xerrors.Is(err, ErrUnknownClient):
		return Client{}, rejected("unknown client")
	case err != nil:
		return Client{}, xerrors.Errorf("read the client: %w", err)
	case !c.Live():
		return Client{}, rejected("the client is disabled")
	}
	return c, nil
}

// Repositories finds the repositories that deliveries named, and their
// owners. faucet.Store does.
type Repositories interface {
	RepositoryByName(ctx context.Context, fullName string) (faucet.Repository, error)
	OwnerByLogin(ctx context.Context, login string) (faucet.Owner, error)
}

// ServiceAccess is the faucet.Authorizer and the faucet.ServiceGrants of
// services. A grant names a repository, an account or every repository, and a
// grant of an account covers its repositories by the account's ID. Names
// match as GitHub matches them, ignoring case. The IDs come from the
// deliveries that named the repositories, so a service never calls GitHub,
// and a repository or an owner that no delivery has named yet is not
// readable.
type ServiceAccess struct {
	clients Store
	repos   Repositories
}

var (
	_ faucet.Authorizer    = ServiceAccess{}
	_ faucet.ServiceGrants = ServiceAccess{}
)

// NewServiceAccess returns the ServiceAccess over the clients of clients and
// the repositories of repos.
func NewServiceAccess(clients Store, repos Repositories) (ServiceAccess, error) {
	if clients == nil || repos == nil {
		return ServiceAccess{}, xerrors.New("the client store and the repositories are required")
	}
	return ServiceAccess{clients: clients, repos: repos}, nil
}

// RepoID implements faucet.Authorizer with the grants that p holds.
func (a ServiceAccess) RepoID(ctx context.Context, p faucet.Principal, repo string) (int64, error) {
	grants, err := heldGrants(p)
	if err != nil {
		return 0, err
	}
	return a.repoGranted(ctx, grants, repo)
}

// RepoIDFresh implements faucet.Authorizer with the grants that the client
// holds in the store now. A client that is gone or disabled is
// faucet.ErrInvalidToken.
func (a ServiceAccess) RepoIDFresh(ctx context.Context, p faucet.Principal, repo string) (int64, error) {
	grants, err := a.liveGrants(ctx, p)
	if err != nil {
		return 0, err
	}
	return a.repoGranted(ctx, grants, repo)
}

// OwnerID implements faucet.ServiceGrants with the grants that p holds.
func (a ServiceAccess) OwnerID(ctx context.Context, p faucet.Principal, owner string) (int64, error) {
	grants, err := heldGrants(p)
	if err != nil {
		return 0, err
	}
	return a.ownerGranted(ctx, grants, owner)
}

// OwnerIDFresh implements faucet.ServiceGrants with the grants that the
// client holds in the store now.
func (a ServiceAccess) OwnerIDFresh(ctx context.Context, p faucet.Principal, owner string) (int64, error) {
	grants, err := a.liveGrants(ctx, p)
	if err != nil {
		return 0, err
	}
	return a.ownerGranted(ctx, grants, owner)
}

// CoversAll implements faucet.ServiceGrants with the grants that p holds.
func (ServiceAccess) CoversAll(_ context.Context, p faucet.Principal) error {
	grants, err := heldGrants(p)
	if err != nil {
		return err
	}
	return allGranted(grants)
}

// CoversAllFresh implements faucet.ServiceGrants with the grants that the
// client holds in the store now.
func (a ServiceAccess) CoversAllFresh(ctx context.Context, p faucet.Principal) error {
	grants, err := a.liveGrants(ctx, p)
	if err != nil {
		return err
	}
	return allGranted(grants)
}

// heldGrants returns the grants that the service p holds.
func heldGrants(p faucet.Principal) ([]clientGrant, error) {
	client, ok := p.ServiceClient()
	if !ok {
		return nil, xerrors.Errorf("only a service is checked by its grants: %w", faucet.ErrNoAccess)
	}
	return parseGrants(client.Grants), nil
}

// liveGrants returns the grants that the client of the service p holds in
// the store now.
func (a ServiceAccess) liveGrants(ctx context.Context, p faucet.Principal) ([]clientGrant, error) {
	client, ok := p.ServiceClient()
	if !ok {
		return nil, xerrors.Errorf("only a service is checked by its grants: %w", faucet.ErrNoAccess)
	}
	c, err := liveClient(ctx, a.clients, client.ClientID)
	if err != nil {
		return nil, err
	}
	return parseGrants(c.Grants), nil
}

// repoGranted returns the ID of repo when a grant names it, names every
// repository, or names the account that owns it.
func (a ServiceAccess) repoGranted(ctx context.Context, grants []clientGrant, repo string) (int64, error) {
	repoOwner, _, err := githubapi.ParseRepo(repo)
	if err != nil {
		return 0, err
	}
	named := slices.ContainsFunc(grants, func(g clientGrant) bool { return g.all || strings.EqualFold(g.repo, repo) })
	notGranted := xerrors.Errorf("no grant of this service names %s: %w", repo, faucet.ErrNoAccess)
	if !named && !slices.ContainsFunc(grants, clientGrant.namesAnAccount) {
		return 0, notGranted
	}
	r, err := a.repos.RepositoryByName(ctx, repo)
	switch {
	case xerrors.Is(err, faucet.ErrUnknownRepository):
		if !named {
			// Only a grant that names the repository or its owner learns
			// that faucet has not seen it, so a service cannot probe which
			// repositories of other owners faucet hears from.
			granted, err := a.ownerOfGranted(ctx, grants, repoOwner)
			switch {
			case err != nil:
				return 0, err
			case !granted:
				return 0, notGranted
			}
		}
		return 0, xerrors.Errorf("faucet has received no webhook delivery from %s yet, so it cannot tell which repository the grant names; it can once GitHub sends one: %w", repo, faucet.ErrNoAccess)
	case err != nil:
		return 0, xerrors.Errorf("look up the repository: %w", err)
	case named || slices.ContainsFunc(grants, func(g clientGrant) bool { return g.accountID == r.OwnerID }):
		return r.ID, nil
	}
	return 0, notGranted
}

// ownerOfGranted reports whether a grant names the account that holds login.
func (a ServiceAccess) ownerOfGranted(ctx context.Context, grants []clientGrant, login string) (bool, error) {
	o, err := a.repos.OwnerByLogin(ctx, login)
	switch {
	case xerrors.Is(err, faucet.ErrUnknownOwner):
		return false, nil
	case err != nil:
		return false, xerrors.Errorf("look up the owner: %w", err)
	}
	return slices.ContainsFunc(grants, func(g clientGrant) bool { return g.accountID == o.ID }), nil
}

// ownerGranted returns the account ID of owner when a grant names that
// account or every repository.
func (a ServiceAccess) ownerGranted(ctx context.Context, grants []clientGrant, owner string) (int64, error) {
	if !githubapi.ValidOwner(owner) {
		return 0, xerrors.New("owner must be a GitHub login")
	}
	all := slices.ContainsFunc(grants, func(g clientGrant) bool { return g.all })
	notGranted := xerrors.Errorf("no grant of this service names the owner %s: %w", owner, faucet.ErrNoAccess)
	if i := slices.IndexFunc(grants, func(g clientGrant) bool { return g.namesAnAccount() && strings.EqualFold(g.login, owner) }); i >= 0 {
		// The grant was made for the account that held this login then.
		notGranted = xerrors.Errorf("the grant org:%s names an account that holds another login now; name the owner by the login of its latest delivery: %w", grants[i].login, faucet.ErrNoAccess)
	}
	if !all && !slices.ContainsFunc(grants, clientGrant.namesAnAccount) {
		return 0, notGranted
	}
	o, err := a.repos.OwnerByLogin(ctx, owner)
	switch {
	case xerrors.Is(err, faucet.ErrUnknownOwner) && all:
		return 0, xerrors.Errorf("faucet has received no webhook delivery from a repository of %s yet, so it cannot tell which owner the grant names; it can once GitHub sends one: %w", owner, faucet.ErrNoAccess)
	case xerrors.Is(err, faucet.ErrUnknownOwner):
		return 0, notGranted
	case err != nil:
		return 0, xerrors.Errorf("look up the owner: %w", err)
	case all || slices.ContainsFunc(grants, func(g clientGrant) bool { return g.accountID == o.ID }):
		return o.ID, nil
	}
	return 0, notGranted
}

func allGranted(grants []clientGrant) error {
	if !slices.ContainsFunc(grants, func(g clientGrant) bool { return g.all }) {
		return xerrors.Errorf("no grant of this service names every repository; that takes the grant all: %w", faucet.ErrNoAccess)
	}
	return nil
}
