package oauth

import (
	"context"
	"slices"
	"strings"

	"golang.org/x/xerrors"

	"github.com/coder/faucet/faucet"
	"github.com/coder/faucet/faucet/internal/githubapi"
)

// ServiceResolver returns the SubjectResolver of service clients in store.
// The subject of a live client is a service with the client's grants and
// the token's scopes. An unknown or disabled client is
// faucet.ErrInvalidToken, so disabling a client fails its tokens on their
// next request.
func ServiceResolver(store Store) SubjectResolver {
	return func(ctx context.Context, id string, scopes []faucet.Scope) (faucet.Principal, error) {
		c, err := liveClient(ctx, store, id)
		if err != nil {
			return faucet.Principal{}, err
		}
		return faucet.NewService(faucet.ServiceClient{ClientID: c.ID, Grants: c.Grants}, scopes)
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

// Repositories finds the repositories that deliveries named. faucet.Store
// does.
type Repositories interface {
	RepositoryByName(ctx context.Context, fullName string) (faucet.Repository, error)
}

// ServiceAccess is the faucet.Authorizer of services. A service may read a
// repository that one of its grants names, matched by name as GitHub does,
// ignoring case. The repository's ID comes from the deliveries that named
// it, so a service never calls GitHub, and a repository that no delivery
// has named yet is not readable.
type ServiceAccess struct {
	clients Store
	repos   Repositories
}

var _ faucet.Authorizer = ServiceAccess{}

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
	client, ok := p.ServiceClient()
	if !ok {
		return 0, xerrors.Errorf("only a service is checked by its grants: %w", faucet.ErrNoAccess)
	}
	return a.granted(ctx, client.Grants, repo)
}

// RepoIDFresh implements faucet.Authorizer with the grants that the client
// holds in the store now. A client that is gone or disabled is
// faucet.ErrInvalidToken.
func (a ServiceAccess) RepoIDFresh(ctx context.Context, p faucet.Principal, repo string) (int64, error) {
	client, ok := p.ServiceClient()
	if !ok {
		return 0, xerrors.Errorf("only a service is checked by its grants: %w", faucet.ErrNoAccess)
	}
	c, err := liveClient(ctx, a.clients, client.ClientID)
	if err != nil {
		return 0, err
	}
	return a.granted(ctx, c.Grants, repo)
}

func (a ServiceAccess) granted(ctx context.Context, grants []string, repo string) (int64, error) {
	if _, _, err := githubapi.ParseRepo(repo); err != nil {
		return 0, err
	}
	if !slices.ContainsFunc(grants, func(g string) bool {
		named, err := parseRepoGrant(g)
		return err == nil && strings.EqualFold(named, repo)
	}) {
		return 0, xerrors.Errorf("no grant of this service names %s: %w", repo, faucet.ErrNoAccess)
	}
	r, err := a.repos.RepositoryByName(ctx, repo)
	switch {
	case xerrors.Is(err, faucet.ErrUnknownRepository):
		return 0, xerrors.Errorf("faucet has received no webhook delivery from %s yet, so it cannot tell which repository the grant names; it can once GitHub sends one: %w", repo, faucet.ErrNoAccess)
	case err != nil:
		return 0, xerrors.Errorf("look up the repository: %w", err)
	}
	return r.ID, nil
}
