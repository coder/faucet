package oauth

import (
	"context"
	"slices"
	"time"

	"golang.org/x/xerrors"

	"github.com/coder/faucet/faucet"
)

// SubjectKind says what an access token was issued to.
type SubjectKind string

const (
	// SubjectService is a service client. The subject is its client ID.
	SubjectService SubjectKind = "service"
	// SubjectUser is a user's login session. The subject is the session ID.
	SubjectUser SubjectKind = "user"
)

var subjectKinds = []SubjectKind{SubjectService, SubjectUser}

// ErrUnknownToken means the store holds no access token with that hash.
var ErrUnknownToken = xerrors.New("unknown access token")

// AccessToken is an access token as a store keeps it: its hash, never the
// token.
type AccessToken struct {
	Hash      TokenHash
	ExpiresAt time.Time
	Scopes    []faucet.Scope
	// Resource is the one endpoint that the token was issued for, such as
	// https://faucet.example.com/github/mcp.
	Resource    string
	SubjectKind SubjectKind
	// Subject is the client ID of a service or the session ID of a user.
	Subject string
}

// Validate reports whether a store may keep t.
func (t AccessToken) Validate() error {
	switch {
	case t.Hash == TokenHash{}:
		return xerrors.New("an access token needs its hash")
	case t.ExpiresAt.IsZero():
		return xerrors.New("an access token needs an expiry")
	case len(t.Scopes) == 0:
		return xerrors.New("an access token needs at least one scope")
	case t.Resource == "":
		return xerrors.New("an access token needs a resource")
	case !slices.Contains(subjectKinds, t.SubjectKind):
		return xerrors.Errorf("unknown subject kind %q", t.SubjectKind)
	case t.Subject == "":
		return xerrors.New("an access token needs a subject")
	}
	for _, s := range t.Scopes {
		if _, err := faucet.ParseScope(string(s)); err != nil {
			return err
		}
	}
	return nil
}

// Store keeps faucet's access tokens. storetest.RunOAuth checks an
// implementation.
type Store interface {
	// InsertAccessToken stores t. It fails when t is not valid, or when the
	// store holds a token with the same hash.
	InsertAccessToken(ctx context.Context, t AccessToken) error
	// AccessToken returns the token whose hash is hash, expired or not, or
	// ErrUnknownToken.
	AccessToken(ctx context.Context, hash TokenHash) (AccessToken, error)
}
