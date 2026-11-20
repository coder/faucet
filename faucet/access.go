package faucet

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"golang.org/x/xerrors"

	"github.com/coder/superblink-events/faucet/internal/githubapi"
)

var (
	// ErrNoAccess means the principal cannot see the repository. GitHub gives
	// the same answer for a repository that does not exist.
	ErrNoAccess     = xerrors.New("repository not found or not accessible")
	ErrInvalidToken = xerrors.New("GitHub rejected the token")
)

// Scope is a permission that a principal holds. Each MCP method needs one.
type Scope string

const (
	// ScopeEventsRead lets a caller wait for events and poll them.
	ScopeEventsRead Scope = "events:read"
	// ScopeEventsSubscribe lets a caller subscribe to events and unsubscribe.
	ScopeEventsSubscribe Scope = "events:subscribe"
)

// knownScopes holds every scope, each at its bit in a scopeSet.
var knownScopes = []Scope{ScopeEventsRead, ScopeEventsSubscribe}

// AllScopes returns every scope that faucet knows, in a fixed order.
func AllScopes() []Scope { return slices.Clone(knownScopes) }

// ParseScope returns the scope named s, or an error when faucet knows no
// such scope.
func ParseScope(s string) (Scope, error) {
	for _, known := range knownScopes {
		if string(known) == s {
			return known, nil
		}
	}
	return "", xerrors.Errorf("unknown scope %q", s)
}

type scopeSet uint8

func newScopeSet(scopes []Scope) (scopeSet, error) {
	var set scopeSet
	for _, s := range scopes {
		i := slices.Index(knownScopes, s)
		if i < 0 {
			return 0, xerrors.Errorf("unknown scope %q", s)
		}
		set |= 1 << i
	}
	if set == 0 {
		return 0, xerrors.New("at least one scope is required")
	}
	return set, nil
}

// principalKind says how a caller authenticated, and so how its repository
// access is checked.
type principalKind int

const (
	// kindGitHubToken holds a GitHub user's GitHub token, with which
	// TokenAuthorizer asks GitHub for its access. Faucet takes no GitHub
	// token from a caller: the browser login builds such a principal from
	// the token that faucet keeps for a user.
	kindGitHubToken principalKind = iota + 1
	// kindOAuthUser logged in through faucet's OAuth server.
	kindOAuthUser
	// kindService is a client that an operator made, with preset grants.
	kindService
)

// OAuthSession names the faucet login session of an OAuth user and the
// GitHub connection that the session uses. An access check that one session
// made is not fresh for another session of the same connection.
type OAuthSession struct {
	SessionID    string
	ConnectionID string
}

// ServiceClient is a client that an operator made, and the repositories its
// grants name. No grant is empty or holds a NUL byte.
type ServiceClient struct {
	ClientID string
	Grants   []string
}

// grantSeparator joins the grants of a service into one string, which keeps
// Principal comparable and its grants out of a caller's reach.
const grantSeparator = "\x00"

// Principal is an authenticated caller: a login, the scopes it holds, and the
// kind of caller it is. Build one with NewPrincipal, NewOAuthUser or
// NewService. The zero value holds no scope, so every method that needs one
// denies it. A GitHub token is never exposed: every fmt verb prints the login
// only.
type Principal struct {
	Login  string
	kind   principalKind
	scopes scopeSet
	// token is the GitHub token of a kindGitHubToken principal.
	token   string
	session OAuthSession
	// clientID and grants are those of a kindService caller, the grants
	// joined by grantSeparator.
	clientID string
	grants   string
}

// githubLogin rejects a login that GitHub cannot give, so that only a service
// has a login with a colon.
func githubLogin(login string) error {
	switch {
	case login == "":
		return xerrors.New("login is required")
	case strings.Contains(login, ":"):
		return xerrors.New("a GitHub login holds no colon")
	}
	return nil
}

// NewPrincipal returns the GitHub user whose GitHub token is token, for
// TokenAuthorizer, which sends the token to GitHub. It holds every scope. An
// Authorizer that does not call GitHub can take an empty token.
func NewPrincipal(login, token string) (Principal, error) {
	if err := githubLogin(login); err != nil {
		return Principal{}, err
	}
	return Principal{Login: login, kind: kindGitHubToken, scopes: 1<<len(knownScopes) - 1, token: token}, nil
}

// NewOAuthUser returns a caller who logged in through faucet's OAuth server.
// login is the bare GitHub login, so the wait limit and the subscriptions of
// a person are the same whichever way they authenticate.
func NewOAuthUser(login string, session OAuthSession, scopes []Scope) (Principal, error) {
	if err := githubLogin(login); err != nil {
		return Principal{}, err
	}
	if session.SessionID == "" || session.ConnectionID == "" {
		return Principal{}, xerrors.New("an OAuth user needs a session ID and a connection ID")
	}
	set, err := newScopeSet(scopes)
	if err != nil {
		return Principal{}, err
	}
	return Principal{Login: login, kind: kindOAuthUser, scopes: set, session: session}, nil
}

// ServiceLogin is the login of the service whose client ID is clientID:
// "svc:" and the ID. A GitHub login cannot hold a colon, so it never equals a
// person's.
func ServiceLogin(clientID string) string { return "svc:" + clientID }

// NewService returns a service client, whose login is ServiceLogin of its
// client ID.
func NewService(client ServiceClient, scopes []Scope) (Principal, error) {
	if client.ClientID == "" {
		return Principal{}, xerrors.New("a service needs a client ID")
	}
	for _, grant := range client.Grants {
		if grant == "" || strings.Contains(grant, grantSeparator) {
			return Principal{}, xerrors.New("a grant is not empty and holds no NUL byte")
		}
	}
	set, err := newScopeSet(scopes)
	if err != nil {
		return Principal{}, err
	}
	return Principal{
		Login: ServiceLogin(client.ClientID), kind: kindService, scopes: set,
		clientID: client.ClientID, grants: strings.Join(client.Grants, grantSeparator),
	}, nil
}

// HasScope reports whether p holds scope.
func (p Principal) HasScope(scope Scope) bool {
	i := slices.Index(knownScopes, scope)
	return i >= 0 && p.scopes&(1<<i) != 0
}

// Scopes returns the scopes that p holds, in a fixed order.
func (p Principal) Scopes() []Scope {
	return slices.DeleteFunc(slices.Clone(knownScopes), func(s Scope) bool { return !p.HasScope(s) })
}

// Authenticated reports whether p came from one of the constructors and
// still has the login that it gave: a GitHub login for a caller with a GitHub
// token or an OAuth user, and "svc:" and the client ID for a service. A
// Principal built by hand, or one whose Login was changed, is no kind of
// caller.
func (p Principal) Authenticated() bool {
	switch p.kind {
	case kindGitHubToken, kindOAuthUser:
		return githubLogin(p.Login) == nil
	case kindService:
		return p.Login == ServiceLogin(p.clientID)
	}
	return false
}

// OAuthSession returns the session of an OAuth user, and false for any other
// kind of caller.
func (p Principal) OAuthSession() (OAuthSession, bool) {
	return p.session, p.kind == kindOAuthUser
}

// ServiceClient returns the client of a service, and false for any other kind
// of caller.
func (p Principal) ServiceClient() (ServiceClient, bool) {
	if p.kind != kindService {
		return ServiceClient{}, false
	}
	client := ServiceClient{ClientID: p.clientID}
	if p.grants != "" {
		client.Grants = strings.Split(p.grants, grantSeparator)
	}
	return client, true
}

// Format implements fmt.Formatter so that no verb can print the token.
func (p Principal) Format(f fmt.State, _ rune) {
	_, _ = fmt.Fprintf(f, "Principal{Login:%q}", p.Login)
}

// Authorizer decides which repositories a principal may read. It answers for
// every kind of principal that its host authenticates, and an Authorizer
// that sends each kind to another Authorizer must send both calls on.
type Authorizer interface {
	// RepoID returns the numeric ID of repo, written owner/name, when p can
	// read it. It returns ErrNoAccess when p cannot, or an error that wraps it
	// and says why: the caller gets that error's text, so it holds nothing
	// the caller may not read. It may answer from what it remembers.
	RepoID(ctx context.Context, p Principal, repo string) (int64, error)
	// RepoIDFresh is RepoID that never answers from what it remembers: it
	// asks the source of the answer again. events/subscribe calls it, because
	// every renewal must check access again.
	RepoIDFresh(ctx context.Context, p Principal, repo string) (int64, error)
}

// ServiceGrants decides which owners a service may read every repository of,
// and whether it may read every repository, from the grants that an operator
// gave it. A host that has services passes one in Deps. Each method gets a
// service, and returns ErrNoAccess, or an error that wraps it and says why,
// when the grants do not cover the read: the caller gets that error's text.
type ServiceGrants interface {
	// OwnerID returns the account ID of owner, a GitHub login, when p's
	// grants cover every repository of it. It may answer from the grants
	// that p holds.
	OwnerID(ctx context.Context, p Principal, owner string) (int64, error)
	// OwnerIDFresh is OwnerID with the grants that the service holds now. A
	// service that is gone or disabled is ErrInvalidToken.
	OwnerIDFresh(ctx context.Context, p Principal, owner string) (int64, error)
	// CoversAll returns nil when p's grants cover every repository.
	CoversAll(ctx context.Context, p Principal) error
	// CoversAllFresh is CoversAll with the grants that the service holds now.
	CoversAllFresh(ctx context.Context, p Principal) error
}

// TokenAuthorizer asks the GitHub API whether the principal's own GitHub
// token can read a repository. It sends the token only to BaseURL.
type TokenAuthorizer struct {
	// BaseURL is the GitHub API root, for example https://api.github.com.
	// Plain http is accepted for loopback hosts only.
	BaseURL string
	// Client defaults to http.DefaultClient.
	Client *http.Client
}

var _ Authorizer = TokenAuthorizer{}

// Validate reports whether BaseURL is a safe place to send tokens.
func (a TokenAuthorizer) Validate() error {
	_, err := githubapi.ParseBaseURL(a.BaseURL)
	return err
}

// RepoIDFresh implements Authorizer. TokenAuthorizer remembers nothing, so
// it is RepoID.
func (a TokenAuthorizer) RepoIDFresh(ctx context.Context, p Principal, repo string) (int64, error) {
	return a.RepoID(ctx, p, repo)
}

// RepoID implements Authorizer with GET {BaseURL}/repos/{owner}/{name}.
// 403 and 404 both give ErrNoAccess, and so does a caller without a GitHub
// token, for whom GitHub cannot answer.
func (a TokenAuthorizer) RepoID(ctx context.Context, p Principal, repo string) (int64, error) {
	owner, name, err := githubapi.ParseRepo(repo)
	if err != nil {
		return 0, err
	}
	if p.token == "" {
		return 0, ErrNoAccess
	}
	var body struct {
		ID int64 `json:"id"`
	}
	status, err := a.get(ctx, p.token, "/repos/"+url.PathEscape(owner)+"/"+url.PathEscape(name), &body)
	if err != nil {
		return 0, err
	}
	switch status {
	case http.StatusOK:
		if body.ID <= 0 {
			return 0, xerrors.New("GitHub returned a repository without an ID")
		}
		return body.ID, nil
	case http.StatusNotFound, http.StatusForbidden:
		return 0, ErrNoAccess
	case http.StatusUnauthorized:
		return 0, ErrInvalidToken
	default:
		return 0, xerrors.Errorf("GitHub answered the repository lookup with status %d", status)
	}
}

// get decodes a 200 response into out. For any other status it returns the
// status and leaves out alone, except that a rate limit answer is githubapi.ErrRateLimited.
func (a TokenAuthorizer) get(ctx context.Context, token, path string, out any) (int, error) {
	if token == "" {
		return 0, xerrors.New("principal has no token")
	}
	base, err := githubapi.ParseBaseURL(a.BaseURL)
	if err != nil {
		return 0, err
	}
	resp, err := githubapi.Call(ctx, a.Client, base, token, http.MethodGet, path, "")
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		answer, _ := io.ReadAll(io.LimitReader(resp.Body, githubapi.MaxResponseBytes))
		if githubapi.RateLimited(resp.StatusCode, resp.Header, answer) {
			return 0, githubapi.ErrRateLimited
		}
		return resp.StatusCode, nil
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, githubapi.MaxResponseBytes)).Decode(out); err != nil {
		return 0, xerrors.Errorf("decode GitHub response: %w", err)
	}
	return resp.StatusCode, nil
}
