package oauth

import (
	"context"
	"time"

	"golang.org/x/xerrors"
)

var (
	// ErrUnknownAuthRequest means that no authorization request is in the
	// state that the step needs: it does not exist, it is in another state,
	// or it has expired.
	ErrUnknownAuthRequest = xerrors.New("no such authorization request")
	// ErrUnknownSession means that the store holds no such session, or no
	// live one where the step needs a live one.
	ErrUnknownSession = xerrors.New("no such session")
	// ErrUnknownConnection means that the store holds no connection with
	// that ID.
	ErrUnknownConnection = xerrors.New("no such connection")
)

// LoginStore keeps the browser logins: authorization requests, the sessions
// they make and the connections of their users. Each method that changes a
// row is one step of a login and applies as a whole or not at all, so two
// callers that race for the same step cannot both take it.
// storetest.RunOAuth checks an implementation.
type LoginStore interface {
	// ConsentAuthRequest stores r, the request of a user who pressed
	// Continue on its consent page: a pending request that holds
	// r.GitHubStateHash, the hash of the state that faucet sends to GitHub.
	// Nothing of a login is stored before. When the store holds the
	// pending request with r's ID and r's browser hash, which has not
	// expired at now, it replaces only that request's GitHub state, as a
	// second press of Continue does. It returns ErrUnknownAuthRequest, and
	// changes nothing, when r has expired at now, or when the store holds
	// another request with r's ID. It fails when r is not valid, or when
	// another request holds r's GitHub state.
	ConsentAuthRequest(ctx context.Context, r AuthRequest, now time.Time) error
	// ExpireAuthRequest moves the pending request with this ID to expired.
	// A request in another state, and an unknown ID, are left alone and are
	// no error.
	ExpireAuthRequest(ctx context.Context, id string) error
	// AuthRequestByGitHubState returns the request whose GitHub state has
	// this hash, in whatever state it is, or ErrUnknownAuthRequest.
	AuthRequestByGitHubState(ctx context.Context, githubState TokenHash) (AuthRequest, error)
	// ApproveAuthRequest stores a.Connection, in place of the connection
	// with the same ID if there is one, and moves the request from pending
	// to approved with the code's hash, the connection's ID and the code's
	// expiry. Before it replaces a connection, it calls alive with the
	// connection as stored, and no RefreshConnection and no other
	// ApproveAuthRequest changes that connection until the step ends.
	// Unless alive returned true, the same step ends at a.Now every live
	// session of the connection, and moves the connection's other approved
	// requests to expired without their codes, so that no client redeems
	// them. It applies only to the pending request with a.RequestID that
	// holds a.GitHubState and has not expired at a.Now. Otherwise it
	// returns ErrUnknownAuthRequest and changes nothing, the connection, its
	// sessions and its requests included, even when alive ran. It fails
	// when a is not valid. alive must not call the store.
	ApproveAuthRequest(ctx context.Context, a Approval, alive func(current Connection) bool) error
	// AuthRequestByCode returns the request whose code has this hash, in
	// whatever state it is, or ErrUnknownAuthRequest.
	AuthRequestByCode(ctx context.Context, code TokenHash) (AuthRequest, error)
	// RedeemAuthRequest moves the approved request whose code has the hash
	// r.Code, and which has not expired at r.Now, to redeemed. In the same
	// step it stores the session of the request, its first refresh token
	// and its first access token, and returns the session. The session has
	// the request's ID, client, connection, resource and scopes, and was
	// refreshed at r.Now. The refresh token has the hash r.Refresh and was
	// issued at r.Now. The access token has the hash r.Access, expires at
	// r.AccessExpiresAt, holds the request's resource and scopes, and its
	// subject is the session. Otherwise it returns ErrUnknownAuthRequest
	// and changes nothing. It fails when r is not valid, or when a stored
	// token has one of the hashes.
	RedeemAuthRequest(ctx context.Context, r Redemption) (Session, error)

	// SessionByRefreshToken returns the refresh token with this hash and
	// the session that it belongs to, live or ended. It returns
	// ErrUnknownSession for a hash that the store does not hold.
	SessionByRefreshToken(ctx context.Context, refresh TokenHash) (Session, RefreshToken, error)
	// Session returns the session with this ID, live or ended, or
	// ErrUnknownSession.
	Session(ctx context.Context, id string) (Session, error)
	// RotateSession trades the refresh token r.Presented of a live session
	// for r.Next. It applies when the token is unused and not retired, or
	// when its first use came after r.UsedAfter. In one step it marks
	// r.Presented used at r.Now unless it was used already, stores r.Next
	// unused and issued at r.Now, sets the session's refreshed time to
	// r.Now unless it is later already, stores the next access token, with
	// the hash r.Access, the scopes r.Scopes and the session's resource,
	// and returns the session as it is after. The refreshed time never
	// moves back, so a rotation by an instance whose clock lags makes no
	// older access check fresh again. In the same step it retires at r.Now
	// the session's other unused tokens that were issued at or before
	// r.UsedAfter, which no client still holds. It deletes no token. When
	// no live session holds such a token, as when a rotation of another
	// token at once retires it, or the session lacks a scope of r.Scopes,
	// it returns ErrUnknownSession and changes nothing. It fails when r is
	// not valid, or when a stored token has one of the next hashes.
	RotateSession(ctx context.Context, r Rotation) (Session, error)
	// EndSession ends the live session with this ID at now. A session that
	// ended already keeps its time, and an unknown ID is no error.
	EndSession(ctx context.Context, id string, now time.Time) error

	// Connection returns the connection with this ID, or
	// ErrUnknownConnection.
	Connection(ctx context.Context, id string) (Connection, error)
	// RefreshConnection calls refresh with the connection as it is stored,
	// and applies what refresh returns: nothing, a replacement, or the end
	// at now of every live session of the connection. It returns the
	// connection as it is after. No other RefreshConnection and no
	// ApproveAuthRequest changes the connection while refresh runs: a
	// second call for the same connection waits, and its refresh then sees
	// what the first one stored. An error of refresh, and a change that is
	// not valid, change nothing and are returned. It returns
	// ErrUnknownConnection, without calling refresh, when there is no such
	// connection. refresh must not call the store.
	RefreshConnection(ctx context.Context, id string, now time.Time, refresh func(current Connection) (ConnectionChange, error)) (Connection, error)

	// DeleteEndedSessions deletes every session that ended at or before now,
	// and every live one last refreshed at or before idleSince, which ends
	// at its next refresh, with their refresh tokens. It returns how many
	// sessions it deleted.
	DeleteEndedSessions(ctx context.Context, now, idleSince time.Time) (int64, error)
	// DeleteUnusedConnections deletes every connection that no session and
	// no authorization request names, and that was last updated before
	// updatedBefore, and returns how many. Its access checks go with it. A
	// connection that a login stores while the deletion runs stays.
	DeleteUnusedConnections(ctx context.Context, updatedBefore time.Time) (int64, error)
	// DeleteExpiredAuthRequests deletes every request without a code, pending
	// or expired, that expired at or before now, and every request with a
	// code that expired before codesBefore, and returns how many. A session
	// outlives the request that made it.
	DeleteExpiredAuthRequests(ctx context.Context, now, codesBefore time.Time) (int64, error)
}
