package oauth

import (
	"time"

	"golang.org/x/xerrors"

	"github.com/coder/superblink-events/faucet"
)

// AuthRequestState is where a browser login stands.
type AuthRequestState string

const (
	// AuthRequestPending waits for the user: first for Continue on the
	// consent page, then for GitHub.
	AuthRequestPending AuthRequestState = "pending"
	// AuthRequestApproved holds a code that the client has not redeemed.
	AuthRequestApproved AuthRequestState = "approved"
	// AuthRequestRedeemed made a session. Its code works no more.
	AuthRequestRedeemed AuthRequestState = "redeemed"
	// AuthRequestExpired was refused or given up. It makes nothing.
	AuthRequestExpired AuthRequestState = "expired"
)

const (
	// authRequestLifetime is how long a user has to finish a login in the
	// browser.
	authRequestLifetime = 10 * time.Minute
	// codeLifetime is how long a client has to redeem its code.
	codeLifetime = 60 * time.Second
)

// AuthRequest is one browser login of a client, from its authorization
// request to the code that it redeems. A store keeps the hash of every
// secret of the login, never the secret. The time also ends a request: no
// step applies to one that is past ExpiresAt, whatever State says.
type AuthRequest struct {
	ID    string
	State AuthRequestState
	// ClientID is the URL of the client's metadata document. ClientName is
	// the name that the document gives, which nothing vouches for.
	ClientID    string
	ClientName  string
	RedirectURI string
	// ClientState is the client's state parameter, which the redirect back
	// returns. It can be empty.
	ClientState string
	// Resource is the endpoint that the login is for.
	Resource string
	Scopes   []faucet.Scope
	// CodeChallenge is the client's PKCE challenge, made with S256.
	CodeChallenge string
	// BrowserHash is the hash of the cookie that the consent page set.
	BrowserHash TokenHash
	// GitHubStateHash is the hash of the state that faucet sent to GitHub,
	// zero until the user pressed Continue.
	GitHubStateHash TokenHash
	// CodeHash is the hash of the authorization code, zero until the request
	// was approved.
	CodeHash TokenHash
	// ConnectionID is the connection of the user who logged in, empty until
	// the request was approved.
	ConnectionID string
	CreatedAt    time.Time
	// ExpiresAt ends a pending request 10 minutes after CreatedAt, and an
	// approved one 60 seconds after its approval.
	ExpiresAt time.Time
}

// Validate reports whether a store may keep r as a new request: a pending
// one whose user pressed Continue, which holds its GitHub state and nothing
// of the later steps.
func (r AuthRequest) Validate() error {
	switch {
	case r.ID == "":
		return xerrors.New("an authorization request needs an ID")
	case r.State != AuthRequestPending:
		return xerrors.New("a new authorization request is pending")
	case r.ClientID == "" || r.RedirectURI == "" || r.Resource == "" || r.CodeChallenge == "":
		return xerrors.New("an authorization request needs a client ID, a redirect URI, a resource and a code challenge")
	case len(r.Scopes) == 0:
		return xerrors.New("an authorization request needs at least one scope")
	case r.BrowserHash == TokenHash{}:
		return xerrors.New("an authorization request needs the hash of its browser cookie")
	case r.GitHubStateHash == TokenHash{}:
		return xerrors.New("an authorization request is stored once its user pressed Continue, with the hash of its GitHub state")
	case r.CodeHash != TokenHash{} || r.ConnectionID != "":
		return xerrors.New("a new authorization request holds no code or connection")
	case r.CreatedAt.IsZero() || !r.ExpiresAt.After(r.CreatedAt):
		return xerrors.New("an authorization request expires after it was made")
	}
	for _, s := range r.Scopes {
		if _, err := faucet.ParseScope(string(s)); err != nil {
			return err
		}
	}
	return nil
}

// Approval is the step from pending to approved: the user logged in at
// GitHub, and the client gets a code.
type Approval struct {
	RequestID string
	// GitHubState is the hash of the state that GitHub sent back. The
	// request must still hold it, so a later Continue outdates this answer.
	GitHubState TokenHash
	// Connection is the user's connection, with the tokens of this login.
	Connection Connection
	// Code is the hash of the authorization code.
	Code TokenHash
	Now  time.Time
	// ExpiresAt is when the code expires.
	ExpiresAt time.Time
}

// Validate reports whether a store may apply a.
func (a Approval) Validate() error {
	switch {
	case a.RequestID == "":
		return xerrors.New("an approval names its request")
	case a.GitHubState == TokenHash{} || a.Code == TokenHash{}:
		return xerrors.New("an approval needs the hashes of the GitHub state and of the code")
	case a.Now.IsZero() || !a.ExpiresAt.After(a.Now):
		return xerrors.New("a code expires after its approval")
	}
	return a.Connection.Validate()
}

// Redemption is the step from approved to redeemed: the client trades its
// code for a session and the session's first access token.
type Redemption struct {
	// Code is the hash of the authorization code.
	Code TokenHash
	// Refresh is the hash of the session's first refresh token, and Access
	// that of its first access token.
	Refresh         TokenHash
	Access          TokenHash
	AccessExpiresAt time.Time
	Now             time.Time
}

// Validate reports whether a store may apply r.
func (r Redemption) Validate() error {
	switch {
	case r.Code == TokenHash{} || r.Refresh == TokenHash{} || r.Access == TokenHash{}:
		return xerrors.New("a redemption needs the hashes of the code, the refresh token and the access token")
	case r.Now.IsZero() || !r.AccessExpiresAt.After(r.Now):
		return xerrors.New("an access token expires after it was issued")
	}
	return nil
}
