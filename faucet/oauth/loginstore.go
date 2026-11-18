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
	// InsertAuthRequest stores r, a new pending request. It fails when r is
	// not valid, or when the store holds a request with the same ID.
	InsertAuthRequest(ctx context.Context, r AuthRequest) error
	// ConsentAuthRequest records that the user pressed Continue: the
	// pending request with this ID and this browser hash that has not
	// expired at now gets githubState, the hash of the state that faucet
	// sends to GitHub. A later call replaces an earlier hash. It returns
	// ErrUnknownAuthRequest, and changes nothing, when there is no such
	// request.
	ConsentAuthRequest(ctx context.Context, id string, browser, githubState TokenHash, now time.Time) error
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
	// expiry. It applies only to the pending request with a.RequestID that
	// holds a.GitHubState and has not expired at a.Now. Otherwise it
	// returns ErrUnknownAuthRequest and changes nothing, the connection
	// included. It fails when a is not valid.
	ApproveAuthRequest(ctx context.Context, a Approval) error
	// AuthRequestByCode returns the request whose code has this hash, in
	// whatever state it is, or ErrUnknownAuthRequest.
	AuthRequestByCode(ctx context.Context, code TokenHash) (AuthRequest, error)
	// RedeemAuthRequest moves the approved request whose code has the hash
	// r.Code, and which has not expired at r.Now, to redeemed. In the same
	// step it stores the session of the request and the session's first
	// access token, and returns the session. The session has the request's
	// ID, client, connection, resource and scopes, the refresh hash
	// r.Refresh, and was refreshed at r.Now. The access token has the hash
	// r.Access, expires at r.AccessExpiresAt, holds the request's resource
	// and scopes, and its subject is the session. Otherwise it returns
	// ErrUnknownAuthRequest and changes nothing. It fails when r is not
	// valid.
	RedeemAuthRequest(ctx context.Context, r Redemption) (Session, error)

	// SessionByRefreshToken returns the session that the refresh token with
	// this hash belongs to, live or ended. current says whether the token
	// is the session's current one. It is false for the token that the last
	// rotation replaced. It returns ErrUnknownSession for any other hash.
	SessionByRefreshToken(ctx context.Context, refresh TokenHash) (s Session, current bool, err error)
	// Session returns the session with this ID, live or ended, or
	// ErrUnknownSession.
	Session(ctx context.Context, id string) (Session, error)
	// RotateSession gives the live session whose current refresh token has
	// the hash r.Presented the next one: r.Presented becomes the previous
	// hash, r.Next the current one, and the session was refreshed at r.Now.
	// In the same step it stores the session's next access token, with the
	// hash r.Access, the scopes r.Scopes and the session's resource, and
	// returns the session as it is after. When no live session has that
	// current refresh token, or the session lacks a scope of r.Scopes, it
	// returns ErrUnknownSession and changes nothing. It fails when r is not
	// valid.
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

	// DeleteExpiredAuthRequests deletes the requests that expired before the time,
	// whatever their state, and returns how many. A session outlives the
	// request that made it.
	DeleteExpiredAuthRequests(ctx context.Context, before time.Time) (int64, error)
}
