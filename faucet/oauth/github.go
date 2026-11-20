package oauth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"golang.org/x/xerrors"

	"github.com/coder/superblink-events/faucet"
	"github.com/coder/superblink-events/faucet/internal/githubapi"
)

const (
	maxTokenAnswerBytes = 64 << 10
	maxGitHubTokenBytes = 1024
)

var (
	// errBadRefreshToken is GitHub refusing a refresh token, which has
	// expired, was revoked or was used already.
	errBadRefreshToken = xerrors.New("GitHub refused the refresh token")
	// errTokensDoNotExpire is a token answer without a refresh token or an
	// access token lifetime: the App has "Expire user authorization tokens"
	// turned off, and faucet keeps no token that never expires.
	errTokensDoNotExpire = xerrors.New(`GitHub gave a token that does not expire: turn on "Expire user authorization tokens" in the GitHub App's settings`)

	githubErrorCode = regexp.MustCompile(`^[a-z_]{1,64}$`)
)

// GitHubApp is faucet's GitHub App, as the browser login uses it. No fmt
// verb prints its client secret.
type GitHubApp struct {
	ClientID     string
	ClientSecret string
	// WebURL is GitHub's web host, https://github.com, which serves the
	// authorize page and the token endpoint. APIURL is the API root.
	WebURL string
	APIURL string
	// Client makes the calls. It should have a timeout.
	Client *http.Client
}

// githubTokens are a GitHub user's tokens and how long each lasts from the
// time of the answer.
type githubTokens struct {
	access, refresh                 string
	accessLifetime, refreshLifetime time.Duration
}

// githubUser is the GitHub user that an access token belongs to.
type githubUser struct {
	id    int64
	login string
}

// Validate reports whether g can log users in. Its errors never hold the
// client secret or a URL.
func (g GitHubApp) Validate() error {
	switch {
	case g.ClientID == "":
		return xerrors.New("the GitHub App needs its client ID")
	case g.ClientSecret == "":
		return xerrors.New("the GitHub App needs its client secret")
	}
	if _, err := githubapi.ParseBaseURL(g.WebURL); err != nil {
		return xerrors.New("the GitHub web URL must be an absolute URL with a host, with https or http on a loopback host, and with no user info, query or fragment")
	}
	_, err := githubapi.ParseBaseURL(g.APIURL)
	return err
}

// Format implements fmt.Formatter so that no verb can print the client
// secret.
func (g GitHubApp) Format(f fmt.State, _ rune) {
	_, _ = fmt.Fprintf(f, "GitHubApp{ClientID:%q}", g.ClientID)
}

// authorizeURL is GitHub's authorize page for a login that comes back to
// redirectURI with state, under PKCE with the S256 challenge.
func (g GitHubApp) authorizeURL(redirectURI, state, challenge string) string {
	query := url.Values{
		"client_id":             {g.ClientID},
		"redirect_uri":          {redirectURI},
		"state":                 {state},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
	}
	return strings.TrimRight(g.WebURL, "/") + "/login/oauth/authorize?" + query.Encode()
}

// exchange trades the code that GitHub sent to redirectURI, and its PKCE
// verifier, for the user's tokens.
func (g GitHubApp) exchange(ctx context.Context, code, redirectURI, verifier string) (githubTokens, error) {
	return g.requestTokens(ctx, url.Values{
		"client_id":     {g.ClientID},
		"client_secret": {g.ClientSecret},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"code_verifier": {verifier},
	})
}

// refresh trades a refresh token for new tokens. It returns
// errBadRefreshToken when GitHub refuses the refresh token.
func (g GitHubApp) refresh(ctx context.Context, refreshToken string) (githubTokens, error) {
	return g.requestTokens(ctx, url.Values{
		"client_id":     {g.ClientID},
		"client_secret": {g.ClientSecret},
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
	})
}

// requestTokens posts form to GitHub's token endpoint. It follows no
// redirect, so the client secret reaches WebURL and nothing else. Its errors
// never hold the form or the answer, except GitHub's error code.
func (g GitHubApp) requestTokens(ctx context.Context, form url.Values) (githubTokens, error) {
	base, err := githubapi.ParseBaseURL(g.WebURL)
	if err != nil {
		return githubTokens{}, xerrors.New("the GitHub web URL is not valid")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base.String()+"/login/oauth/access_token", strings.NewReader(form.Encode()))
	if err != nil {
		return githubTokens{}, xerrors.Errorf("build the token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "coder-faucet")
	client := http.DefaultClient
	if g.Client != nil {
		client = g.Client
	}
	pinned := *client
	pinned.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := pinned.Do(req)
	if err != nil {
		var urlErr *url.Error
		if xerrors.As(err, &urlErr) {
			err = urlErr.Err
		}
		return githubTokens{}, xerrors.Errorf("call GitHub's token endpoint: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return githubTokens{}, xerrors.Errorf("GitHub answered the token request with status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxTokenAnswerBytes+1))
	if err != nil {
		return githubTokens{}, xerrors.Errorf("read GitHub's token answer: %w", err)
	}
	if len(body) > maxTokenAnswerBytes {
		return githubTokens{}, xerrors.Errorf("GitHub's token answer is longer than %d bytes", maxTokenAnswerBytes)
	}
	return parseGitHubTokens(body)
}

// parseGitHubTokens reads a token answer. GitHub answers a refused request
// with status 200 and an error member.
func parseGitHubTokens(body []byte) (githubTokens, error) {
	var answer struct {
		AccessToken           string `json:"access_token"`
		ExpiresIn             *int64 `json:"expires_in"`
		RefreshToken          string `json:"refresh_token"`
		RefreshTokenExpiresIn *int64 `json:"refresh_token_expires_in"`
		Error                 string `json:"error"`
	}
	if err := json.Unmarshal(body, &answer); err != nil {
		return githubTokens{}, xerrors.New("GitHub's token answer does not decode")
	}
	switch {
	case answer.Error == "bad_refresh_token":
		return githubTokens{}, errBadRefreshToken
	case githubErrorCode.MatchString(answer.Error):
		return githubTokens{}, xerrors.Errorf("GitHub refused the token request: %s", answer.Error)
	case answer.Error != "":
		return githubTokens{}, xerrors.New("GitHub refused the token request")
	case !validGitHubToken(answer.AccessToken):
		return githubTokens{}, xerrors.New("GitHub's token answer holds no valid access token")
	case answer.RefreshToken == "" || answer.ExpiresIn == nil:
		return githubTokens{}, errTokensDoNotExpire
	case !validGitHubToken(answer.RefreshToken):
		return githubTokens{}, xerrors.New("GitHub's token answer holds no valid refresh token")
	}
	accessLifetime, ok := lifetime(answer.ExpiresIn)
	if !ok {
		return githubTokens{}, xerrors.New("GitHub's token answer gives the access token no positive lifetime")
	}
	refreshLifetime, ok := lifetime(answer.RefreshTokenExpiresIn)
	if !ok {
		return githubTokens{}, xerrors.New("GitHub's token answer gives the refresh token no positive lifetime")
	}
	return githubTokens{
		access: answer.AccessToken, refresh: answer.RefreshToken,
		accessLifetime: accessLifetime, refreshLifetime: refreshLifetime,
	}, nil
}

// validGitHubToken accepts 1 to 1024 printable ASCII characters other than
// the space, which an Authorization header carries as they are.
func validGitHubToken(token string) bool {
	if token == "" || len(token) > maxGitHubTokenBytes {
		return false
	}
	for i := range len(token) {
		if token[i] <= ' ' || token[i] > '~' {
			return false
		}
	}
	return true
}

// lifetime reads a lifetime in seconds. It refuses one that is missing, not
// positive, or too long for a time.Duration.
func lifetime(seconds *int64) (time.Duration, bool) {
	if seconds == nil || *seconds <= 0 || *seconds > math.MaxInt64/int64(time.Second) {
		return 0, false
	}
	return time.Duration(*seconds) * time.Second, true
}

// user returns the user whose access token accessToken is, with GET
// <APIURL>/user. It returns faucet.ErrInvalidToken when GitHub rejects the
// token, and githubapi.ErrRateLimited when GitHub's rate limiter answers.
func (g GitHubApp) user(ctx context.Context, accessToken string) (githubUser, error) {
	base, err := githubapi.ParseBaseURL(g.APIURL)
	if err != nil {
		return githubUser{}, err
	}
	resp, err := githubapi.Call(ctx, g.Client, base, accessToken, http.MethodGet, "/user", "")
	if err != nil {
		return githubUser{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, githubapi.MaxResponseBytes))
	if err != nil {
		return githubUser{}, xerrors.Errorf("read GitHub's user answer: %w", err)
	}
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return githubUser{}, faucet.ErrInvalidToken
	case githubapi.RateLimited(resp.StatusCode, resp.Header, body):
		return githubUser{}, githubapi.ErrRateLimited
	case resp.StatusCode != http.StatusOK:
		return githubUser{}, xerrors.Errorf("GitHub answered the user lookup with status %d", resp.StatusCode)
	}
	var answer struct {
		ID    int64  `json:"id"`
		Login string `json:"login"`
	}
	if err := json.Unmarshal(body, &answer); err != nil {
		return githubUser{}, xerrors.New("GitHub's user answer does not decode")
	}
	if answer.ID <= 0 || answer.Login == "" || strings.Contains(answer.Login, ":") {
		return githubUser{}, xerrors.New("GitHub's user answer has no valid ID and login")
	}
	return githubUser{id: answer.ID, login: answer.Login}, nil
}
