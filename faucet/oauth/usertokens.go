package oauth

import (
	"context"
	"strings"
	"time"

	"golang.org/x/xerrors"

	"cdr.dev/slog/v3"
	"github.com/coder/faucet/faucet"
	"github.com/coder/faucet/faucet/internal/githubapi"
	"github.com/coder/quartz"
)

const (
	// githubRefreshTimeout bounds a refresh at GitHub, which holds the lock
	// of the connection while it runs.
	githubRefreshTimeout = 30 * time.Second
	// userTokenMargin is how long the GitHub token of a user must last when
	// the access check sends it, so that it does not expire on the way.
	userTokenMargin = time.Minute
)

// staleToken reports whether the stored GitHub access token of a connection
// will not do, so that the connection needs a new pair.
type staleToken func(c Connection, access string) bool

// githubToken is a GitHub access token of a user, with the user's login, as
// a connection holds them.
type githubToken struct{ login, access string }

// principal is the caller that the token is, for a RepoChecker.
func (t githubToken) principal() (faucet.Principal, error) {
	return faucet.NewPrincipal(t.login, t.access)
}

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
// ID, with its login. When stale calls the stored token stale, it refreshes
// the pair at GitHub first, one refresh per connection at a time: a caller
// that waited for another's refresh asks stale about the pair that it finds,
// and uses that pair when it will do.
//
// GitHub refusing the refresh token ends every session of the connection,
// and the error wraps faucet.ErrInvalidToken. A token that does not open is
// an error that wraps ErrUnreadableToken and changes nothing, so a wrong key
// never ends a session. Any other failure changes nothing either.
func (c connections) accessToken(ctx context.Context, id string, stale staleToken) (githubToken, error) {
	conn, err := c.store.Connection(ctx, id)
	if err != nil {
		return githubToken{}, xerrors.Errorf("read connection %s: %w", id, err)
	}
	access, err := c.keys.accessToken(conn)
	if err != nil {
		return githubToken{}, xerrors.Errorf("open the GitHub token of connection %s: %w", id, err)
	}
	if !stale(conn, access) {
		return githubToken{login: conn.Login, access: access}, nil
	}
	return c.refresh(ctx, id, stale)
}

// refresh gives the connection a new pair under its lock, unless the pair
// that it finds there will do.
func (c connections) refresh(ctx context.Context, id string, stale staleToken) (githubToken, error) {
	// GitHub's refresh token works once, and GitHub can spend it even when
	// the caller gives up during the call. The refresh runs to its end and
	// stores the new pair however the caller's request ends, or the user's
	// next refresh fails and ends every session.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), githubRefreshTimeout)
	defer cancel()
	var token githubToken
	ended := false
	_, err := c.store.RefreshConnection(ctx, id, c.clock.Now(), func(current Connection) (ConnectionChange, error) {
		access, err := c.keys.accessToken(current)
		if err != nil {
			return ConnectionChange{}, err
		}
		if !stale(current, access) {
			token = githubToken{login: current.Login, access: access}
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
		token = githubToken{login: next.Login, access: fresh}
		return ConnectionChange{Kind: ReplaceConnection, Connection: next}, nil
	})
	switch {
	case err != nil:
		return githubToken{}, xerrors.Errorf("refresh the GitHub token of connection %s: %w", id, err)
	case ended:
		c.logger.Info(ctx, "GitHub refused the refresh token of a connection, its sessions ended", slog.F("connection_id", id))
		return githubToken{}, xerrors.Errorf("GitHub refused the refresh token of connection %s, so its sessions ended: %w", id, faucet.ErrInvalidToken)
	}
	return token, nil
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

// authorized returns nil when GitHub still takes the pair of current, a
// connection whose pair a login replaces: GitHub names the connection's user
// for the access token while it lasts past userTokenMargin, and trades the
// refresh token after that. Any other answer is an error, so a user who
// revoked faucet's App at GitHub and a GitHub that does not answer look the
// same. The pair that a refresh gives is dropped, as the login replaces it.
// authorized does not call the store.
func (c connections) authorized(ctx context.Context, current Connection) error {
	ctx, cancel := context.WithTimeout(ctx, githubRefreshTimeout)
	defer cancel()
	if current.AccessExpiresAt.After(c.clock.Now().Add(userTokenMargin)) {
		access, err := c.keys.accessToken(current)
		if err != nil {
			return err
		}
		user, err := c.github.user(ctx, access)
		switch {
		case err != nil:
			return xerrors.Errorf("ask GitHub whose token connection %s holds: %w", current.ID, err)
		case user.id != current.UpstreamUserID:
			return xerrors.Errorf("connection %s holds the token of GitHub user %d", current.ID, user.id)
		}
		return nil
	}
	refresh, err := c.keys.refreshToken(current)
	if err != nil {
		return err
	}
	if _, err := c.github.refresh(ctx, refresh); err != nil {
		return xerrors.Errorf("refresh the GitHub token of connection %s: %w", current.ID, err)
	}
	return nil
}

// UserResolver returns the SubjectResolver of users who logged in through a
// browser. The subject is a session ID, and the principal is an OAuth user
// of that session, never of another session of the same user. An unknown
// or ended session is faucet.ErrInvalidToken.
func UserResolver(store Store) SubjectResolver {
	return func(ctx context.Context, id string, scopes []faucet.Scope) (faucet.Principal, error) {
		session, err := store.Session(ctx, id)
		switch {
		case xerrors.Is(err, ErrUnknownSession):
			return faucet.Principal{}, rejected("unknown session")
		case err != nil:
			return faucet.Principal{}, xerrors.Errorf("read the session: %w", err)
		case !session.Live():
			return faucet.Principal{}, rejected("the session ended")
		}
		c, err := store.Connection(ctx, session.ConnectionID)
		if err != nil {
			return faucet.Principal{}, xerrors.Errorf("read the connection of the session: %w", err)
		}
		return faucet.NewOAuthUser(c.Login, faucet.OAuthSession{SessionID: session.ID, ConnectionID: session.ConnectionID}, scopes)
	}
}

// RepoChecker asks GitHub whether the principal's own token can read a
// repository. Every faucet.Authorizer is one, faucet.TokenAuthorizer among
// them. The access check calls only RepoIDFresh, which must ask GitHub at
// that call: an access check counts from the start of its lookup, and an
// answer that GitHub gave before that start could count for a session that
// refreshed after GitHub last answered.
type RepoChecker interface {
	RepoIDFresh(ctx context.Context, p faucet.Principal, repo string) (int64, error)
}

// UserAccessDeps are what a UserAccess needs.
type UserAccessDeps struct {
	Store  Store
	Clock  quartz.Clock
	Logger slog.Logger
	Login  BrowserLogin
	// Repos asks GitHub whether the user's token can read a repository, at
	// every call, as RepoChecker says.
	Repos RepoChecker
}

// UserAccess is the faucet.Authorizer of users who logged in through a
// browser. GitHub answers with the GitHub token that faucet keeps for the
// user, so a user reads what their GitHub login can read. A repository that
// GitHub let the user read since their session's last refresh needs no new
// answer until the next one, which asks GitHub again.
type UserAccess struct {
	access repoAccess
}

var _ faucet.Authorizer = UserAccess{}

// NewUserAccess returns the UserAccess for d.
func NewUserAccess(d UserAccessDeps) (UserAccess, error) {
	switch {
	case d.Store == nil:
		return UserAccess{}, xerrors.New("store is required")
	case d.Clock == nil:
		return UserAccess{}, xerrors.New("clock is required")
	case d.Repos == nil:
		return UserAccess{}, xerrors.New("the repository checker is required")
	}
	login, err := newBrowserLogin(d.Login, d.Clock)
	if err != nil {
		return UserAccess{}, xerrors.Errorf("the browser login: %w", err)
	}
	return UserAccess{access: repoAccess{tokens: newConnections(d.Store, d.Clock, d.Logger, login), repos: d.Repos}}, nil
}

// RepoIDFresh implements faucet.Authorizer: it asks the RepoChecker with the
// user's GitHub token, refreshed first when it expires within a minute.
// When GitHub rejects the token, it refreshes the token once and asks once
// more. A pass becomes the connection's check of the repository. A denial
// takes the check away, and so does a token that GitHub rejects even after
// the refresh. A principal that is not an OAuth user is faucet.ErrNoAccess, and
// repo must be owner/name as the caller wrote it, before any change of case.
func (a UserAccess) RepoIDFresh(ctx context.Context, p faucet.Principal, repo string) (int64, error) {
	repo, err := userRepo(repo)
	if err != nil {
		return 0, err
	}
	session, err := userSession(p)
	if err != nil {
		return 0, err
	}
	return a.access.ask(ctx, session, repo)
}

// RepoID implements faucet.Authorizer. A check of repo by the user's
// connection that started at or after the last refresh of the user's
// session answers, and counts as a use. Otherwise it asks GitHub, as
// RepoIDFresh does.
func (a UserAccess) RepoID(ctx context.Context, p faucet.Principal, repo string) (int64, error) {
	repo, err := userRepo(repo)
	if err != nil {
		return 0, err
	}
	session, err := userSession(p)
	if err != nil {
		return 0, err
	}
	now := a.access.tokens.clock.Now()
	id, ok, err := a.access.tokens.store.FreshAccessCheck(ctx, session.SessionID, repo, now, now.Add(-accessCheckUseStep))
	switch {
	case err != nil:
		return 0, xerrors.Errorf("read the access check of %s: %w", repo, err)
	case ok:
		return id, nil
	}
	return a.access.ask(ctx, session, repo)
}

// userRepo returns repo, owner/name as the caller wrote it, in lower case,
// once it is a repository name as written. Lower case alone can make a name
// valid, as the Kelvin sign folds to k.
func userRepo(repo string) (string, error) {
	if _, _, err := githubapi.ParseRepo(repo); err != nil {
		return "", err
	}
	return strings.ToLower(repo), nil
}

// userSession returns the session of p. A principal that is not an OAuth
// user is faucet.ErrNoAccess.
func userSession(p faucet.Principal) (faucet.OAuthSession, error) {
	session, ok := p.OAuthSession()
	if !ok {
		return faucet.OAuthSession{}, xerrors.Errorf("only a user who logged in through a browser is checked with the GitHub token that faucet keeps: %w", faucet.ErrNoAccess)
	}
	return session, nil
}
