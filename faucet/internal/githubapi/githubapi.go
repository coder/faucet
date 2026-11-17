// Package githubapi holds the helpers that faucet's GitHub clients share:
// the guarded call, the rate limit test and the checks of a base URL and of a
// repository name.
package githubapi

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"golang.org/x/xerrors"
)

const (
	// MaxResponseBytes is the most of a GitHub answer that a client reads.
	MaxResponseBytes = 1 << 20
	maxRedirects     = 3
)

var (
	// ErrRateLimited is GitHub's rate limiter answering, which says nothing
	// about the principal's access. It is a failed check, never a denial.
	ErrRateLimited = xerrors.New("GitHub rate limit reached")

	ownerPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,38}$`)
	namePattern  = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)
)

// Call sends the token to base and nowhere else. A redirect is followed
// only when it keeps the scheme and host of base, so no redirect can carry the
// token to another host. The caller closes the response body.
func Call(ctx context.Context, client *http.Client, base *url.URL, token, method, path, rawQuery string) (*http.Response, error) {
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

// RateLimited tells GitHub's rate limiter from a denial. GitHub answers a rate
// limit with 429, or with 403 and one of three signs: no quota left, a
// Retry-After header (the secondary limit), or a message that names the limit
// (the secondary limit, also under its old name, the abuse detection mechanism).
func RateLimited(status int, header http.Header, answer []byte) bool {
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

// ParseBaseURL never puts raw in an error because it can carry credentials.
func ParseBaseURL(raw string) (*url.URL, error) {
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

// ValidOwner says whether owner is a valid GitHub user or organization name.
func ValidOwner(owner string) bool { return ownerPattern.MatchString(owner) }

// ParseRepo splits owner/name. It is strict because the parts end up in a URL
// path that carries the caller's token.
func ParseRepo(repo string) (owner, name string, err error) {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || !ownerPattern.MatchString(owner) || !namePattern.MatchString(name) || name == "." || name == ".." {
		return "", "", xerrors.New("repo must be written owner/name")
	}
	return owner, name, nil
}
