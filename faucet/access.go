package faucet

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"golang.org/x/xerrors"
)

const (
	maxAPIResponseBytes = 1 << 20
	maxRedirects        = 3
)

var (
	// ErrNoAccess means the principal cannot see the repository. GitHub gives
	// the same answer for a repository that does not exist.
	ErrNoAccess     = xerrors.New("repository not found or not accessible")
	ErrInvalidToken = xerrors.New("GitHub rejected the token")

	// errRateLimited is GitHub's rate limiter answering, which says nothing
	// about the principal's access. It is a failed check, never a denial.
	errRateLimited = xerrors.New("GitHub rate limit reached")

	ownerPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,38}$`)
	namePattern  = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)
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
	_, err := parseBaseURL(a.BaseURL)
	return err
}

// RepoID implements Authorizer with GET {BaseURL}/repos/{owner}/{name}.
// 403 and 404 both give ErrNoAccess.
func (a TokenAuthorizer) RepoID(ctx context.Context, p Principal, repo string) (int64, error) {
	owner, name, err := parseRepo(repo)
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
// status and leaves out alone, except that a rate limit answer is errRateLimited.
func (a TokenAuthorizer) get(ctx context.Context, token, path string, out any) (int, error) {
	if token == "" {
		return 0, xerrors.New("principal has no token")
	}
	base, err := parseBaseURL(a.BaseURL)
	if err != nil {
		return 0, err
	}
	resp, err := callGitHub(ctx, a.Client, base, token, http.MethodGet, path, "")
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		answer, _ := io.ReadAll(io.LimitReader(resp.Body, maxAPIResponseBytes))
		if rateLimited(resp.StatusCode, resp.Header, answer) {
			return 0, errRateLimited
		}
		return resp.StatusCode, nil
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxAPIResponseBytes)).Decode(out); err != nil {
		return 0, xerrors.Errorf("decode GitHub response: %w", err)
	}
	return resp.StatusCode, nil
}

// callGitHub sends the token to base and nowhere else. A redirect is followed
// only when it keeps the scheme and host of base, so no redirect can carry the
// token to another host. The caller closes the response body.
func callGitHub(ctx context.Context, client *http.Client, base *url.URL, token, method, path, rawQuery string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, base.String()+path, nil)
	if err != nil {
		return nil, xerrors.Errorf("build GitHub request: %w", err)
	}
	req.URL.RawQuery = rawQuery
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "coder-faucet")

	if client == nil {
		client = http.DefaultClient
	}
	pinned := *client
	pinned.CheckRedirect = func(next *http.Request, via []*http.Request) error {
		if next.URL.Scheme != base.Scheme || next.URL.Host != base.Host {
			return xerrors.New("GitHub redirected to another host")
		}
		if len(via) > maxRedirects {
			return xerrors.New("GitHub redirected too many times")
		}
		return nil
	}
	resp, err := pinned.Do(req)
	if err != nil {
		// A *url.Error quotes the request URL, which can carry a cursor.
		var urlErr *url.Error
		if xerrors.As(err, &urlErr) {
			err = urlErr.Err
		}
		return nil, xerrors.Errorf("call GitHub: %w", err)
	}
	return resp, nil
}

// rateLimited tells GitHub's rate limiter from a denial. GitHub answers a rate
// limit with 429, or with 403 and one of three signs: no quota left, a
// Retry-After header (the secondary limit), or a message that names the limit
// (the secondary limit, also under its old name, the abuse detection mechanism).
func rateLimited(status int, header http.Header, answer []byte) bool {
	if status == http.StatusTooManyRequests {
		return true
	}
	if status != http.StatusForbidden {
		return false
	}
	if header.Get("X-RateLimit-Remaining") == "0" || header.Get("Retry-After") != "" {
		return true
	}
	var body struct {
		Message string `json:"message"`
	}
	_ = json.Unmarshal(answer, &body)
	message := strings.ToLower(body.Message)
	return strings.Contains(message, "rate limit") || strings.Contains(message, "abuse detection")
}

// parseBaseURL never puts raw in an error because it can carry credentials.
func parseBaseURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	switch {
	case err != nil || u.Host == "" || u.Hostname() == "":
		return nil, xerrors.New("GitHub API URL must be an absolute URL with a host")
	case u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "":
		return nil, xerrors.New("GitHub API URL must not have user info, a query or a fragment")
	case u.Scheme != "https" && (u.Scheme != "http" || !isLoopback(u.Hostname())):
		return nil, xerrors.New("GitHub API URL must use https, or http on a loopback host")
	}
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawPath = ""
	return u, nil
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// parseRepo splits owner/name. It is strict because the parts end up in a URL
// path that carries the caller's token.
func parseRepo(repo string) (owner, name string, err error) {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || !ownerPattern.MatchString(owner) || !namePattern.MatchString(name) || name == "." || name == ".." {
		return "", "", xerrors.New("repo must be written owner/name")
	}
	return owner, name, nil
}
