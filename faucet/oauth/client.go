package oauth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"slices"
	"strconv"
	"strings"
	"time"

	"golang.org/x/xerrors"

	"github.com/coder/superblink-events/faucet"
	"github.com/coder/superblink-events/faucet/internal/githubapi"
)

const (
	maxClientIDBytes = 64
	repoGrantPrefix  = "repo:"
	orgGrantPrefix   = "org:"
	allGrant         = "all"
	// accountSeparator comes between the login and the account ID of a
	// bound org grant. No GitHub login holds it.
	accountSeparator = "#"
)

// Client is a service client as a store keeps it. An operator makes it with
// NewClient, and the service logs in with its ID and secret under the client
// credentials grant. NewSecretlessClient makes a client for workloads, which
// has no secret.
type Client struct {
	// ID is lower-case letters, digits and inner hyphens. The service's
	// login is "svc:" and the ID.
	ID string
	// SecretHash is the hash of the client secret, never the secret, and nil
	// for a client that has no secret.
	SecretHash *TokenHash
	// Resource is the one endpoint that the client's tokens are for, such as
	// https://faucet.example.com/github/mcp.
	Resource string
	// Grants name what the client may read: "repo:<owner>/<name>" one
	// repository, "org:<login>#<account ID>" every repository of a user or
	// organization, and "all" every repository. BindGrants turns the
	// "org:<login>" that an operator writes into the second form.
	Grants []string
	// Scopes are the most that a token of the client can hold.
	Scopes    []faucet.Scope
	CreatedAt time.Time
	// DisabledAt is when an operator disabled the client, zero while it is
	// live. A disabled client gets no token, and the tokens it holds fail on
	// their next request.
	DisabledAt time.Time
}

// Live reports whether c has not been disabled.
func (c Client) Live() bool { return c.DisabledAt.IsZero() }

// Validate reports whether a store may keep c.
func (c Client) Validate() error {
	if err := ValidateClientID(c.ID); err != nil {
		return err
	}
	switch {
	case c.SecretHash != nil && *c.SecretHash == TokenHash{}:
		return xerrors.New("a client's secret hash cannot be zero: a client with no secret has no hash")
	case c.Resource == "":
		return xerrors.New("a client needs a resource")
	case len(c.Scopes) == 0:
		return xerrors.New("a client needs at least one scope")
	case c.CreatedAt.IsZero():
		return xerrors.New("a client needs its creation time")
	}
	for _, s := range c.Scopes {
		if _, err := faucet.ParseScope(string(s)); err != nil {
			return err
		}
	}
	for _, g := range c.Grants {
		if _, err := parseGrant(g); err != nil {
			return err
		}
	}
	return nil
}

// NewClient returns a live client created at now, and its secret, which the
// caller must show to the operator once: the client keeps only its hash.
func NewClient(id, resource string, scopes []faucet.Scope, grants []string, now time.Time) (Client, string, error) {
	c, err := NewSecretlessClient(id, resource, scopes, grants, now)
	if err != nil {
		return Client{}, "", err
	}
	secret, err := newSecret(clientSecretPrefix)
	if err != nil {
		return Client{}, "", err
	}
	hash := HashToken(secret)
	c.SecretHash = &hash
	return c, secret, nil
}

// NewSecretlessClient returns a live client created at now that has no
// secret, so the client credentials grant refuses it.
func NewSecretlessClient(id, resource string, scopes []faucet.Scope, grants []string, now time.Time) (Client, error) {
	if len(grants) == 0 {
		return Client{}, xerrors.New("a client needs at least one grant, or it can read nothing")
	}
	c := Client{ID: id, Resource: resource, Grants: slices.Clone(grants), Scopes: slices.Clone(scopes), CreatedAt: now}
	if err := c.Validate(); err != nil {
		return Client{}, err
	}
	return c, nil
}

// ClientUpdate is what Store.UpdateClient sets. A nil set keeps the one
// that the client holds, so an update of the scopes alone cannot undo an
// update of the grants that ran at the same time.
type ClientUpdate struct {
	// Grants are as BindGrants or RebindGrants returns them.
	Grants []string
	Scopes []faucet.Scope
}

// Validate reports whether a store may apply u.
func (u ClientUpdate) Validate() error {
	switch {
	case u.Grants == nil && u.Scopes == nil:
		return xerrors.New("an update sets the grants, the scopes or both")
	case u.Grants != nil && len(u.Grants) == 0:
		return xerrors.New("a client needs at least one grant, or it can read nothing")
	case u.Scopes != nil && len(u.Scopes) == 0:
		return xerrors.New("a client needs at least one scope")
	}
	for _, s := range u.Scopes {
		if _, err := faucet.ParseScope(string(s)); err != nil {
			return err
		}
	}
	for _, g := range u.Grants {
		if _, err := parseGrant(g); err != nil {
			return err
		}
	}
	return nil
}

// errClientID is the error of an ID that ValidateClientID refuses.
var errClientID = xerrors.Errorf("a client ID is 1 to %d lower-case letters, digits and hyphens, with no hyphen at either end", maxClientIDBytes)

// ValidateClientID accepts 1 to 64 lower-case letters, digits and hyphens,
// with a letter or digit at each end, which every client ID is. No such ID
// changes when a client URL-encodes it for HTTP Basic authentication, and
// none holds a byte that a store cannot compare, such as NUL.
func ValidateClientID(id string) error {
	ok := id != "" && len(id) <= maxClientIDBytes && id[0] != '-' && id[len(id)-1] != '-'
	for _, c := range id {
		ok = ok && ('a' <= c && c <= 'z' || '0' <= c && c <= '9' || c == '-')
	}
	if !ok {
		return errClientID
	}
	return nil
}

// clientGrant is what one grant names: a repository, written owner/name,
// every repository of an account, by its ID, or every repository. login is
// the account's login when the grant was set, which only says what an
// operator granted.
type clientGrant struct {
	repo      string
	login     string
	accountID int64
	all       bool
}

// String returns g as a client holds it.
func (g clientGrant) String() string {
	switch {
	case g.all:
		return allGrant
	case g.namesAnAccount():
		return orgGrantPrefix + g.login + accountSeparator + strconv.FormatInt(g.accountID, 10)
	}
	return repoGrantPrefix + g.repo
}

// namesAnAccount reports whether g is an org grant.
func (g clientGrant) namesAnAccount() bool { return g.accountID > 0 }

// parseGrant reads a grant in one of its three forms as a client holds it.
func parseGrant(g string) (clientGrant, error) {
	if g == allGrant {
		return clientGrant{all: true}, nil
	}
	if rest, ok := strings.CutPrefix(g, orgGrantPrefix); ok {
		login, id, bound := strings.Cut(rest, accountSeparator)
		accountID, err := strconv.ParseInt(id, 10, 64)
		switch {
		case bound && githubapi.ValidOwner(login) && err == nil && accountID > 0 && strconv.FormatInt(accountID, 10) == id:
			return clientGrant{login: login, accountID: accountID}, nil
		case !bound && githubapi.ValidOwner(login):
			return clientGrant{}, xerrors.Errorf("grant %q names no account: BindGrants binds it to the account that holds the login", g)
		}
	}
	if repo, ok := strings.CutPrefix(g, repoGrantPrefix); ok {
		if _, _, err := githubapi.ParseRepo(repo); err == nil {
			return clientGrant{repo: repo}, nil
		}
	}
	return clientGrant{}, xerrors.Errorf("grant %q is not repo:<owner>/<name>, org:<login>#<account ID> or all", g)
}

// parseGrants reads the grants that a client holds. A store keeps only grants
// that parse, so one that does not names nothing. That includes an org grant
// of a client made before org grants named an account, which the migration
// could not bind because no account held its login.
func parseGrants(grants []string) []clientGrant {
	parsed := make([]clientGrant, 0, len(grants))
	for _, g := range grants {
		if p, err := parseGrant(g); err == nil {
			parsed = append(parsed, p)
		}
	}
	return parsed
}

// BindGrants returns grants as an operator writes them, as a client holds
// them: each "org:<login>" becomes "org:<login>#<account ID>", under the
// login that owners holds, for the account that holds the login now. So the
// grant follows that account through a rename, and no later holder of the
// login gets it. An operator who writes "org:<login>#<account ID>" gets it
// only when that account holds the login now. A login that no delivery has
// named is an error that wraps faucet.ErrUnknownOwner, because faucet cannot
// tell which account it is.
func BindGrants(ctx context.Context, owners Repositories, grants []string) ([]string, error) {
	bound := make([]string, 0, len(grants))
	for _, g := range grants {
		rest, ok := strings.CutPrefix(g, orgGrantPrefix)
		if !ok {
			if _, err := parseGrant(g); err != nil {
				return nil, errOperatorGrant(g)
			}
			bound = append(bound, g)
			continue
		}
		login, _, hasID := strings.Cut(rest, accountSeparator)
		named, err := parseGrant(g)
		if hasID && err != nil || !hasID && !githubapi.ValidOwner(login) {
			return nil, errOperatorGrant(g)
		}
		o, err := owners.OwnerByLogin(ctx, login)
		switch {
		case xerrors.Is(err, faucet.ErrUnknownOwner):
			return nil, xerrors.Errorf("faucet has received no webhook delivery from %s or a repository of it, so it cannot tell which account %s names; grant it once GitHub has sent one: %w", login, g, err)
		case err != nil:
			return nil, xerrors.Errorf("look up the owner of %s: %w", g, err)
		case hasID && o.ID != named.accountID:
			return nil, xerrors.Errorf("%s names account %d, and faucet's latest delivery gives %s to account %d", g, named.accountID, login, o.ID)
		}
		bound = append(bound, orgGrantPrefix+o.Login+accountSeparator+strconv.FormatInt(o.ID, 10))
	}
	return bound, nil
}

// RebindGrants is BindGrants for an update of a client that holds held. An
// org grant that the client holds, as faucet clients list shows it, stays as
// it is with no lookup, so it survives a rename of its account or a new
// holder of its login. An "org:<login>" is refused when the client holds the
// login with another account than the one that holds it now, because the
// operator could mean either.
func RebindGrants(ctx context.Context, owners Repositories, held, grants []string) ([]string, error) {
	heldByLogin := map[string][]clientGrant{}
	for _, g := range parseGrants(held) {
		if g.namesAnAccount() {
			heldByLogin[strings.ToLower(g.login)] = append(heldByLogin[strings.ToLower(g.login)], g)
		}
	}
	rebound := make([]string, 0, len(grants))
	for _, g := range grants {
		// An org grant that a build before bound grants stored as
		// org:<login> names no account, so it is bound as a new one is.
		if i := slices.IndexFunc(held, func(h string) bool { return strings.EqualFold(h, g) }); i >= 0 && strings.HasPrefix(g, orgGrantPrefix) {
			if _, err := parseGrant(held[i]); err == nil {
				rebound = append(rebound, held[i])
				continue
			}
		}
		bound, err := BindGrants(ctx, owners, []string{g})
		if xerrors.Is(err, faucet.ErrUnknownOwner) {
			if h := heldByLogin[strings.ToLower(strings.TrimPrefix(g, orgGrantPrefix))]; len(h) > 0 {
				return nil, xerrors.Errorf("no delivery names the account of %s now, and the client holds %s: write that to keep the grant: %w", g, h[0].String(), err)
			}
		}
		if err != nil {
			return nil, err
		}
		if err := movesAHeldGrant(g, bound[0], heldByLogin); err != nil {
			return nil, err
		}
		rebound = append(rebound, bound[0])
	}
	return rebound, nil
}

// movesAHeldGrant refuses g, an "org:<login>" that an operator wrote and that
// binds to bound, when the client holds the login only with other accounts.
func movesAHeldGrant(g, bound string, heldByLogin map[string][]clientGrant) error {
	login, ok := strings.CutPrefix(g, orgGrantPrefix)
	if !ok || strings.Contains(login, accountSeparator) {
		return nil
	}
	now, err := parseGrant(bound)
	if err != nil {
		return err
	}
	held := heldByLogin[strings.ToLower(login)]
	if len(held) == 0 || slices.ContainsFunc(held, func(h clientGrant) bool { return h.accountID == now.accountID }) {
		return nil
	}
	keep := held[0].String()
	return xerrors.Errorf("the client holds %s, and %s names account %d now: write %s to keep that grant, or %s for the account that holds the login now",
		keep, login, now.accountID, keep, bound)
}

func errOperatorGrant(g string) error {
	return xerrors.Errorf("grant %q is not repo:<owner>/<name>, org:<login>, org:<login>#<account ID> or all", g)
}

// newSecret returns prefix and 32 random bytes in base64url without padding.
// The alphabet has no '+', '/' or '=', so URL-encoding for HTTP Basic
// authentication leaves the secret as it is.
func newSecret(prefix string) (string, error) {
	b := make([]byte, secretBytes)
	if _, err := rand.Read(b); err != nil {
		return "", xerrors.Errorf("read random bytes: %w", err)
	}
	return prefix + base64.RawURLEncoding.EncodeToString(b), nil
}
