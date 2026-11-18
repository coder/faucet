package oauth

import (
	"context"
	"time"

	"golang.org/x/xerrors"

	"cdr.dev/slog/v3"
	"github.com/coder/faucet/faucet"
	"github.com/coder/quartz"
)

// githubRefreshTimeout bounds a refresh at GitHub, which holds the lock of
// the connection while it runs.
const githubRefreshTimeout = 30 * time.Second

// staleToken reports whether the stored GitHub access token of a connection
// will not do, so that the connection needs a new pair.
type staleToken func(c Connection, access string) bool

// connections keeps the GitHub tokens of the users fresh. It holds no state:
// all of it is in the store, so the token endpoint and the access check each
// build their own.
type connections struct {
	store  Store
	clock  quartz.Clock
	logger slog.Logger
	github GitHubApp
	keys   TokenKeys
}

func newConnections(store Store, clock quartz.Clock, logger slog.Logger, login *browserLogin) connections {
	return connections{store: store, clock: clock, logger: logger, github: login.github, keys: login.keys}
}

// expiresWithin calls a token stale when it expires within d of the time of
// the check.
func (c connections) expiresWithin(d time.Duration) staleToken {
	return func(conn Connection, _ string) bool { return !conn.AccessExpiresAt.After(c.clock.Now().Add(d)) }
}

// accessToken returns the GitHub access token of the connection with this
// ID. When stale calls the stored token stale, it refreshes the pair at
// GitHub first, one refresh per connection at a time: a caller that waited
// for another's refresh asks stale about the pair that it finds, and uses
// that pair when it will do.
//
// GitHub refusing the refresh token ends every session of the connection,
// and the error wraps faucet.ErrInvalidToken. A token that does not open is
// an error that wraps ErrUnreadableToken and changes nothing, so a wrong key
// never ends a session. Any other failure changes nothing either.
func (c connections) accessToken(ctx context.Context, id string, stale staleToken) (string, error) {
	conn, err := c.store.Connection(ctx, id)
	if err != nil {
		return "", xerrors.Errorf("read connection %s: %w", id, err)
	}
	access, err := c.keys.accessToken(conn)
	if err != nil {
		return "", xerrors.Errorf("open the GitHub token of connection %s: %w", id, err)
	}
	if !stale(conn, access) {
		return access, nil
	}
	return c.refresh(ctx, id, stale)
}

// refresh gives the connection a new pair under its lock, unless the pair
// that it finds there will do.
func (c connections) refresh(ctx context.Context, id string, stale staleToken) (string, error) {
	// GitHub's refresh token works once. Once GitHub has answered, the new
	// pair must reach the store even when the caller gives up, or the
	// user's next refresh fails and ends every session.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), githubRefreshTimeout)
	defer cancel()
	var access string
	ended := false
	_, err := c.store.RefreshConnection(ctx, id, c.clock.Now(), func(current Connection) (ConnectionChange, error) {
		var err error
		if access, err = c.keys.accessToken(current); err != nil {
			return ConnectionChange{}, err
		}
		if !stale(current, access) {
			return ConnectionChange{Kind: KeepConnection}, nil
		}
		next, fresh, err := c.renew(ctx, current)
		switch {
		case xerrors.Is(err, errBadRefreshToken):
			ended = true
			return ConnectionChange{Kind: EndConnectionSessions}, nil
		case err != nil:
			return ConnectionChange{}, err
		}
		access = fresh
		return ConnectionChange{Kind: ReplaceConnection, Connection: next}, nil
	})
	switch {
	case err != nil:
		return "", xerrors.Errorf("refresh the GitHub token of connection %s: %w", id, err)
	case ended:
		c.logger.Info(ctx, "GitHub refused the refresh token of a connection, its sessions ended", slog.F("connection_id", id))
		return "", xerrors.Errorf("GitHub refused the refresh token of connection %s, so its sessions ended: %w", id, faucet.ErrInvalidToken)
	}
	return access, nil
}

// renew trades the refresh token of current for a new pair at GitHub, and
// returns the connection that holds the pair, sealed, and the new access
// token. The connection takes the login that GitHub gives for the new
// token when GitHub can say, and keeps its own otherwise.
func (c connections) renew(ctx context.Context, current Connection) (Connection, string, error) {
	refresh, err := c.keys.refreshToken(current)
	if err != nil {
		return Connection{}, "", err
	}
	tokens, err := c.github.refresh(ctx, refresh)
	if err != nil {
		return Connection{}, "", err
	}
	next := current
	user, err := c.github.user(ctx, tokens.access)
	switch {
	case err != nil:
		c.logger.Info(ctx, "could not read the login of a refreshed connection, it keeps its login", slog.F("connection_id", current.ID), slog.Error(err))
	case user.id != current.UpstreamUserID:
		// The pair is another user's: storing it would let the sessions of
		// this user act as that one.
		return Connection{}, "", xerrors.Errorf("GitHub gave connection %s the tokens of GitHub user %d", current.ID, user.id)
	default:
		next.Login = user.login
	}
	now := c.clock.Now()
	next.AccessExpiresAt, next.RefreshExpiresAt, next.UpdatedAt = now.Add(tokens.accessLifetime), now.Add(tokens.refreshLifetime), now
	if err := c.keys.seal(&next, tokens.access, tokens.refresh); err != nil {
		return Connection{}, "", err
	}
	c.logger.Debug(ctx, "refreshed the GitHub token of a connection", slog.F("connection_id", current.ID))
	return next, tokens.access, nil
}

// UserResolver returns the SubjectResolver of users who logged in through a
// browser.
func UserResolver(_ Store) SubjectResolver {
	return func(context.Context, string, []faucet.Scope) (faucet.Principal, error) {
		return faucet.Principal{}, xerrors.New("not built yet")
	}
}

// RepoChecker asks GitHub whether the principal's own token can read a
// repository. faucet.TokenAuthorizer is one.
type RepoChecker interface {
	RepoID(ctx context.Context, p faucet.Principal, repo string) (int64, error)
}

// UserAccessDeps are what a UserAccess needs.
type UserAccessDeps struct {
	Store  Store
	Clock  quartz.Clock
	Logger slog.Logger
	Login  BrowserLogin
	Repos  RepoChecker
}

// UserAccess is the faucet.Authorizer of users who logged in through a
// browser.
type UserAccess struct{}

// NewUserAccess returns the UserAccess for d.
func NewUserAccess(UserAccessDeps) (UserAccess, error) { return UserAccess{}, nil }

// RepoID implements faucet.Authorizer.
func (UserAccess) RepoID(context.Context, faucet.Principal, string) (int64, error) {
	return 0, xerrors.New("not built yet")
}

// RepoIDFresh implements faucet.Authorizer.
func (UserAccess) RepoIDFresh(context.Context, faucet.Principal, string) (int64, error) {
	return 0, xerrors.New("not built yet")
}
