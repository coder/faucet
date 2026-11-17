package faucet

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"golang.org/x/xerrors"

	"github.com/coder/faucet/faucet/internal/githubapi"
)

var (
	// ErrNoAccess means the principal cannot see the repository. GitHub gives
	// the same answer for a repository that does not exist.
	ErrNoAccess     = xerrors.New("repository not found or not accessible")
	ErrInvalidToken = xerrors.New("GitHub rejected the token")
)

// Principal is an authenticated caller. The token is never exposed: every
// fmt verb prints the login only.
type Principal struct {
	Login string
	token string
}

// NewPrincipal returns a Principal. The token is what TokenAuthorizer sends to
// GitHub. An Authorizer that does not call GitHub can take an empty token.
func NewPrincipal(login, token string) (Principal, error) {
	if login == "" {
		return Principal{}, xerrors.New("login is required")
	}
	return Principal{Login: login, token: token}, nil
}

// Format implements fmt.Formatter so that no verb can print the token.
func (p Principal) Format(f fmt.State, _ rune) {
	_, _ = fmt.Fprintf(f, "Principal{Login:%q}", p.Login)
}

type Authorizer interface {
	// RepoID returns the numeric ID of repo, written owner/name, when p can
	// read it. It returns ErrNoAccess when p cannot.
	RepoID(ctx context.Context, p Principal, repo string) (int64, error)
}

// TokenAuthorizer asks the GitHub API whether the principal's own token can
// read a repository. It sends the token only to BaseURL.
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

// RepoID implements Authorizer with GET {BaseURL}/repos/{owner}/{name}.
// 403 and 404 both give ErrNoAccess.
func (a TokenAuthorizer) RepoID(ctx context.Context, p Principal, repo string) (int64, error) {
	owner, name, err := githubapi.ParseRepo(repo)
	if err != nil {
		return 0, err
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

// Authenticate resolves token to its owner with GET {BaseURL}/user.
// It returns ErrInvalidToken when GitHub rejects the token.
func (a TokenAuthorizer) Authenticate(ctx context.Context, token string) (Principal, error) {
	var body struct {
		Login string `json:"login"`
	}
	status, err := a.get(ctx, token, "/user", &body)
	if err != nil {
		return Principal{}, err
	}
	switch status {
	case http.StatusOK:
		return NewPrincipal(body.Login, token)
	case http.StatusUnauthorized, http.StatusForbidden:
		return Principal{}, ErrInvalidToken
	default:
		return Principal{}, xerrors.Errorf("GitHub answered the user lookup with status %d", status)
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
