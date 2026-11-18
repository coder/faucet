package oauth

import (
	"time"

	"golang.org/x/xerrors"

	"github.com/coder/faucet/faucet"
)

// Session is the login of a user's client. A refresh token keeps it alive,
// and each use of the token replaces it.
type Session struct {
	// ID is the ID of the authorization request that made the session.
	ID         string
	ClientID   string
	ClientName string
	// ConnectionID is the connection of the user who logged in.
	ConnectionID string
	// Resource is the one endpoint that the session's tokens are for.
	Resource string
	Scopes   []faucet.Scope
	// RefreshHash is the hash of the current refresh token.
	RefreshHash TokenHash
	// PreviousRefreshHash is the hash of the refresh token that the last
	// rotation replaced, zero before the first rotation. A client that
	// presents that token again ends the session.
	PreviousRefreshHash TokenHash
	// RefreshedAt is when the session got its current refresh token.
	RefreshedAt time.Time
	// EndedAt is when the session ended, zero while it is live.
	EndedAt time.Time
}

// Live reports whether s has not ended.
func (s Session) Live() bool { return s.EndedAt.IsZero() }

// Rotation is one use of a refresh token: the session gets the next refresh
// token and the next access token.
type Rotation struct {
	// Presented is the hash of the refresh token that the client sent, and
	// Next that of the one it gets.
	Presented TokenHash
	Next      TokenHash
	// Access is the hash of the next access token, which holds Scopes.
	Access          TokenHash
	AccessExpiresAt time.Time
	Scopes          []faucet.Scope
	Now             time.Time
}

// Validate reports whether a store may apply r.
func (r Rotation) Validate() error {
	switch {
	case r.Presented == TokenHash{} || r.Next == TokenHash{} || r.Access == TokenHash{}:
		return xerrors.New("a rotation needs the hashes of both refresh tokens and of the access token")
	case r.Presented == r.Next:
		return xerrors.New("a rotation replaces the refresh token")
	case len(r.Scopes) == 0:
		return xerrors.New("an access token needs at least one scope")
	case r.Now.IsZero() || !r.AccessExpiresAt.After(r.Now):
		return xerrors.New("an access token expires after it was issued")
	}
	for _, s := range r.Scopes {
		if _, err := faucet.ParseScope(string(s)); err != nil {
			return err
		}
	}
	return nil
}
