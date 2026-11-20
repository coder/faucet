package oauth

import (
	"context"
	"time"

	"golang.org/x/xerrors"

	"github.com/coder/superblink-events/faucet"
)

// SubjectKind says what an access token was issued to.
type SubjectKind string

const (
	// SubjectService is a service client. The subject is its client ID.
	SubjectService SubjectKind = "service"
	// SubjectUser is a user's login session. The subject is the session ID.
	SubjectUser SubjectKind = "user"
)

// subjectOf holds every subject kind. It returns the subject that a principal
// of that kind is, and false for a principal of another kind.
var subjectOf = map[SubjectKind]func(faucet.Principal) (string, bool){
	SubjectService: func(p faucet.Principal) (string, bool) {
		client, ok := p.ServiceClient()
		return client.ClientID, ok
	},
	SubjectUser: func(p faucet.Principal) (string, bool) {
		session, ok := p.OAuthSession()
		return session.SessionID, ok
	},
}

var (
	// ErrUnknownToken means the store holds no access token with that hash.
	ErrUnknownToken = xerrors.New("unknown access token")
	// ErrUnknownClient means the store holds no client with that ID.
	ErrUnknownClient = xerrors.New("unknown client")
	// ErrClientExists means the store already holds a client with that ID,
	// live or disabled.
	ErrClientExists = xerrors.New("a client with this ID exists")
	// ErrUnknownTrustRule means the client holds no trust rule with that ID.
	ErrUnknownTrustRule = xerrors.New("unknown trust rule")
)

// StoredTrustRule is a trust rule as a store keeps it for one client.
type StoredTrustRule struct {
	ID        int64
	Rule      TrustRule
	CreatedAt time.Time
}

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
	case subjectOf[t.SubjectKind] == nil:
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

// Store keeps faucet's access tokens, its service clients, the browser
// logins of its users and the repositories that their GitHub tokens could
// read. storetest.RunOAuth checks an implementation.
type Store interface {
	LoginStore
	AccessCheckStore

	// InsertAccessToken stores t. It fails when t is not valid, or when the
	// store holds a token with the same hash.
	InsertAccessToken(ctx context.Context, t AccessToken) error
	// AccessToken returns the token whose hash is hash as it is stored,
	// expired or not, or ErrUnknownToken. A stored token need not pass
	// Validate: a later build can write a scope that this one does not know.
	AccessToken(ctx context.Context, hash TokenHash) (AccessToken, error)
	// DeleteAccessToken deletes the access token whose hash is hash. An
	// unknown hash is no error.
	DeleteAccessToken(ctx context.Context, hash TokenHash) error
	// DeleteExpiredAccessTokens deletes every access token that expired at or
	// before now and returns how many it deleted.
	DeleteExpiredAccessTokens(ctx context.Context, now time.Time) (int64, error)

	// InsertClient stores c. It fails when c is not valid, and returns
	// ErrClientExists when the store holds a client with the same ID, even a
	// disabled one, so no ID is ever given to a second client.
	InsertClient(ctx context.Context, c Client) error
	// Client returns the client with this ID, live or disabled, or
	// ErrUnknownClient.
	Client(ctx context.Context, id string) (Client, error)
	// Clients returns every client, in the order of creation.
	Clients(ctx context.Context) ([]Client, error)
	// DisableClient disables the client with this ID at now and returns it.
	// A client that is disabled already keeps the time it was first
	// disabled. It returns ErrUnknownClient when there is no such client.
	DisableClient(ctx context.Context, id string, now time.Time) (Client, error)
	// UpdateClient sets the grants, the scopes or both of the live client
	// with this ID as u says, keeps the rest of the client as it is, and
	// returns the client. It fails when u is not valid, and returns
	// ErrUnknownClient when there is no such client or it is disabled.
	UpdateClient(ctx context.Context, id string, u ClientUpdate) (Client, error)

	// AddTrustRule adds r, at now, to the trust rules of the client with this
	// ID, live or disabled, and returns the rule's ID, which no other rule
	// ever gets. When the client holds a rule with the same issuer, claims
	// and key set already, it returns that rule's ID and changes nothing. The
	// same key set is the same JSON as Postgres holds it in jsonb: white
	// space, the order of members and a member named twice do not count, but
	// a number keeps its scale and every digit, so 1 and 1.0 make two rules.
	// It returns ErrUnknownClient when there is no such client. It does not
	// run ValidateTrustRule: the caller does, and the checker again at each
	// use, since a later build can refuse a rule.
	AddTrustRule(ctx context.Context, clientID string, r TrustRule, now time.Time) (int64, error)
	// TrustRules returns the trust rules of the client with this ID, in the
	// order they were added, and none for an unknown client. A rule's JWKS
	// holds the JSON value that was added, not always the same bytes but with
	// no white space, so a compact key set reads back no longer, and is empty
	// for a rule whose keys come from discovery.
	TrustRules(ctx context.Context, clientID string) ([]StoredTrustRule, error)
	// RemoveTrustRule removes the trust rule with this ID from the client
	// with this ID. It returns ErrUnknownTrustRule when the client holds no
	// such rule, as when another client holds it.
	RemoveTrustRule(ctx context.Context, clientID string, id int64) error
	// CountTrustRules returns how many trust rules each client holds, by
	// client ID, with no entry for a client that holds none.
	CountTrustRules(ctx context.Context) (map[string]int, error)
}
