package oauth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"time"

	"golang.org/x/xerrors"

	"github.com/coder/quartz"
	"github.com/coder/superblink-events/faucet"
)

const (
	// GitHubCallbackPath is where GitHub sends the browser back to, under
	// the issuer. The GitHub App names this URL as its callback URL.
	GitHubCallbackPath = "/oauth/github/callback"

	// sessionIdleLifetime ends a session whose refresh token went unused
	// for this long, as GitHub ends its own refresh tokens after six months.
	sessionIdleLifetime = 180 * 24 * time.Hour
	// authRequestRetention is how long an authorization request stays after
	// it expired, so a code that is used again still ends its session.
	authRequestRetention = 24 * time.Hour

	idBytes = 16
)

// BrowserLogin is what a Server needs for the browser login: a user logs in
// at GitHub through faucet's GitHub App, and the client gets faucet's own
// tokens.
type BrowserLogin struct {
	GitHub GitHubApp
	// Keys seal the GitHub tokens that faucet keeps for its users.
	Keys TokenKeys
	// Documents sets up the client that fetches client ID metadata
	// documents. A client chooses their URL, so faucet builds that client
	// itself with faucet.NewHTTPClient, and the zero value is the one for
	// production.
	Documents faucet.HTTPClientOptions
}

// browserLogin is a BrowserLogin that passed its checks.
type browserLogin struct {
	github    GitHubApp
	keys      TokenKeys
	documents documentFetcher
}

func newBrowserLogin(d BrowserLogin, clock quartz.Clock) (*browserLogin, error) {
	if err := d.GitHub.Validate(); err != nil {
		return nil, err
	}
	if d.Keys.empty() {
		return nil, xerrors.New("a token key is required")
	}
	return &browserLogin{github: d.GitHub, keys: d.Keys, documents: newDocumentFetcher(faucet.NewHTTPClient(d.Documents), clock)}, nil
}

// newID returns the ID of a new authorization request: 16 random bytes in
// base64url, which a cookie name can hold.
func newID() (string, error) {
	b := make([]byte, idBytes)
	if _, err := rand.Read(b); err != nil {
		return "", xerrors.Errorf("read random bytes: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// derive returns a value that only a holder of secret can compute, and that
// does not give secret away: the SHA-256 of label, a NUL and secret, in
// base64url. Each use of a secret has its own label.
func derive(label, secret string) string {
	sum := sha256.Sum256([]byte(label + "\x00" + secret))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// s256 returns the PKCE challenge of verifier under the S256 method (RFC
// 7636 section 4.2).
func s256(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}
