package oauth

import (
	"strconv"
	"time"

	"golang.org/x/xerrors"
)

// ProviderGitHub is the provider of a connection to GitHub.
const ProviderGitHub = "github"

// Connection is a user's login at an upstream provider, with the provider's
// tokens for that user. The tokens are sealed: a store never sees them.
type Connection struct {
	// ID is the provider and the user's ID there, such as github:583231, so
	// a user has one connection however often they log in.
	ID             string
	Provider       string
	UpstreamUserID int64
	// Login is the user's login at the provider when faucet last asked.
	Login string
	// AccessToken and RefreshToken are sealed under the key that KeyID
	// names.
	AccessToken      []byte
	RefreshToken     []byte
	KeyID            string
	AccessExpiresAt  time.Time
	RefreshExpiresAt time.Time
	UpdatedAt        time.Time
}

// ConnectionID returns the ID of the connection of a provider's user.
func ConnectionID(provider string, upstreamUserID int64) string {
	return provider + ":" + strconv.FormatInt(upstreamUserID, 10)
}

// Validate reports whether a store may keep c.
func (c Connection) Validate() error {
	switch {
	case c.Provider != ProviderGitHub:
		return xerrors.Errorf("unknown provider %q", c.Provider)
	case c.UpstreamUserID <= 0:
		return xerrors.New("a connection needs the user's ID at the provider")
	case c.ID != ConnectionID(c.Provider, c.UpstreamUserID):
		return xerrors.New("the ID of a connection is its provider and the user's ID there")
	case c.Login == "":
		return xerrors.New("a connection needs the user's login")
	case len(c.AccessToken) == 0 || len(c.RefreshToken) == 0 || c.KeyID == "":
		return xerrors.New("a connection needs both sealed tokens and the ID of their key")
	case c.AccessExpiresAt.IsZero() || c.RefreshExpiresAt.IsZero() || c.UpdatedAt.IsZero():
		return xerrors.New("a connection needs the expiry of both tokens and the time of its update")
	}
	return nil
}

// ConnectionChangeKind says what a refresh of a connection found.
type ConnectionChangeKind int

const (
	// KeepConnection changes nothing: the tokens are good as they are.
	KeepConnection ConnectionChangeKind = iota + 1
	// ReplaceConnection stores the change's Connection in place of the
	// current one.
	ReplaceConnection
	// EndConnectionSessions ends every live session of the connection,
	// because the provider refused its refresh token.
	EndConnectionSessions
)

// ConnectionChange is what the callback of Store.RefreshConnection decided.
type ConnectionChange struct {
	Kind ConnectionChangeKind
	// Connection is the new row, for ReplaceConnection only. Its ID is the
	// current one's.
	Connection Connection
}

// Validate reports whether a store may apply c to the connection with ID
// id.
func (c ConnectionChange) Validate(id string) error {
	switch c.Kind {
	case KeepConnection, EndConnectionSessions:
		if c.Connection.ID != "" {
			return xerrors.New("only a replacement carries a connection")
		}
		return nil
	case ReplaceConnection:
		if c.Connection.ID != id {
			return xerrors.New("a replacement keeps the ID of the connection")
		}
		return c.Connection.Validate()
	}
	return xerrors.New("unknown connection change kind")
}
