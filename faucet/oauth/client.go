package oauth

import (
	"crypto/rand"
	"encoding/base64"
	"slices"
	"strings"
	"time"

	"golang.org/x/xerrors"

	"github.com/coder/faucet/faucet"
	"github.com/coder/faucet/faucet/internal/githubapi"
)

const (
	maxClientIDBytes = 64
	repoGrantPrefix  = "repo:"
)

// Client is a service client as a store keeps it. An operator makes it with
// NewClient, and the service logs in with its ID and secret under the client
// credentials grant.
type Client struct {
	// ID is lower-case letters, digits and inner hyphens. The service's
	// login is "svc:" and the ID.
	ID string
	// SecretHash is the hash of the client secret, never the secret.
	SecretHash TokenHash
	// Resource is the one endpoint that the client's tokens are for, such as
	// https://faucet.example.com/github/mcp.
	Resource string
	// Grants name what the client may read, each "repo:<owner>/<name>".
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
	case c.SecretHash == TokenHash{}:
		return xerrors.New("a client needs the hash of its secret")
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
		if _, err := parseRepoGrant(g); err != nil {
			return err
		}
	}
	return nil
}

// NewClient returns a live client created at now, and its secret, which the
// caller must show to the operator once: the client keeps only its hash.
func NewClient(id, resource string, scopes []faucet.Scope, grants []string, now time.Time) (Client, string, error) {
	if len(grants) == 0 {
		return Client{}, "", xerrors.New("a client needs at least one grant, or it can read nothing")
	}
	secret, err := newSecret(clientSecretPrefix)
	if err != nil {
		return Client{}, "", err
	}
	c := Client{
		ID: id, SecretHash: HashToken(secret), Resource: resource,
		Grants: slices.Clone(grants), Scopes: slices.Clone(scopes), CreatedAt: now,
	}
	if err := c.Validate(); err != nil {
		return Client{}, "", err
	}
	return c, secret, nil
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

// parseRepoGrant returns the repository, written owner/name, that a grant
// names.
func parseRepoGrant(grant string) (string, error) {
	repo, ok := strings.CutPrefix(grant, repoGrantPrefix)
	if !ok {
		return "", xerrors.Errorf("grant %q is not repo:<owner>/<name>", grant)
	}
	if _, _, err := githubapi.ParseRepo(repo); err != nil {
		return "", xerrors.Errorf("grant %q is not repo:<owner>/<name>", grant)
	}
	return repo, nil
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
