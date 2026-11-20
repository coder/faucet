package oauth

import (
	"time"

	"golang.org/x/xerrors"

	"github.com/coder/superblink-events/faucet"
)

// Session is the login of a user's client. Its refresh tokens keep it
// alive, and each use of one gives the next.
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
	// RefreshedAt is when the session last gave out a refresh token.
	RefreshedAt time.Time
	// EndedAt is when the session ended, zero while it is live.
	EndedAt time.Time
}

// Live reports whether s has not ended.
func (s Session) Live() bool { return s.EndedAt.IsZero() }

// RefreshToken is a refresh token of a session as a store keeps it: its
// hash, never the token. A client trades it for the next one. It can trade
// it again within refreshReuseGrace of its first use, since parallel
// requests send one token at once. A store keeps every token of a session
// until the session goes, so a later use, however late, ends the session.
type RefreshToken struct {
	Hash      TokenHash
	SessionID string
	IssuedAt  time.Time
	// UsedAt is when a client first traded the token, zero while it is
	// unused.
	UsedAt time.Time
	// RetiredAt is when a refresh of another token retired this one unused,
	// because no client still holds it. Zero while it is not retired.
	RetiredAt time.Time
}

// Used reports whether a client traded t.
func (t RefreshToken) Used() bool { return !t.UsedAt.IsZero() }

// Retired reports whether a refresh retired t unused.
func (t RefreshToken) Retired() bool { return !t.RetiredAt.IsZero() }

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
	// UsedAfter starts the reuse grace: a presented token that a client
	// traded already is traded again only when its first use came after
	// UsedAfter.
	UsedAfter time.Time
	Now       time.Time
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
	case r.UsedAfter.IsZero() || r.UsedAfter.After(r.Now):
		return xerrors.New("a rotation needs the start of the reuse grace, at or before its time")
	}
	for _, s := range r.Scopes {
		if _, err := faucet.ParseScope(string(s)); err != nil {
			return err
		}
	}
	return nil
}
