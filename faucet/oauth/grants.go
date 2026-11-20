package oauth

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"golang.org/x/xerrors"

	"cdr.dev/slog/v3"
	"github.com/coder/superblink-events/faucet"
)

const (
	minVerifierBytes = 43
	maxVerifierBytes = 128
	// githubRefreshMargin is how long the GitHub token of a user must last
	// when the refresh grant issues tokens. The grant refreshes a token that
	// expires sooner, so the access check seldom has to.
	githubRefreshMargin = 10 * time.Minute
	// refreshReuseGrace is how long after its first use a refresh token
	// still trades for tokens. The parallel requests of one client send
	// the same token, and only a use after the grace counts as a second
	// holder.
	refreshReuseGrace = 30 * time.Second
)

var (
	// errUnknownCode is the answer to a code that is unknown, expired or
	// used.
	errUnknownCode = badRequest("invalid_grant", "the code is unknown, expired or used: log in again")
	// errUnknownRefreshToken is the answer to a refresh token that faucet
	// did not issue or no longer knows.
	errUnknownRefreshToken = badRequest("invalid_grant", "the refresh token is unknown or revoked: log in again")
	// errSessionEnded is the answer to a refresh token whose session ended.
	errSessionEnded = badRequest("invalid_grant", "the session of the refresh token ended: log in again")
)

// publicClient returns the client ID of a client that logs in through a
// browser, which has no secret (auth method none). The ID comes in the
// form, or URL-encoded (RFC 6749 section 2.3.1) as HTTP Basic credentials
// with an empty password, which golang.org/x/oauth2 tries first when it
// detects the style. It reads nothing from the store, so a request that it
// refuses changes nothing.
func publicClient(r *http.Request, form url.Values) (string, error) {
	if form.Has("client_secret") {
		return "", badRequest("invalid_request", "a client that logs in through a browser has no secret: send client_id without client_secret")
	}
	id := form.Get("client_id")
	if r.Header.Get("Authorization") != "" {
		rawID, rawSecret, ok := r.BasicAuth()
		basicID, idErr := url.QueryUnescape(rawID)
		secret, secretErr := url.QueryUnescape(rawSecret)
		switch {
		case !ok || idErr != nil || secretErr != nil || basicID == "" || secret != "":
			return "", &tokenError{
				status: http.StatusUnauthorized, code: "invalid_client",
				description: "a client that logs in through a browser has no secret: send client_id in the form, or as HTTP Basic credentials with an empty password",
			}
		case form.Has("client_id") && id != basicID:
			return "", badRequest("invalid_request", "client_id differs from the client of the Authorization header")
		}
		id = basicID
	}
	if id == "" {
		return "", badRequest("invalid_request", "client_id is required, in the form or as HTTP Basic credentials with an empty password")
	}
	return id, nil
}

// authorizationCode is the authorization_code grant (RFC 6749 section 4.1.3)
// of a browser login: the client trades its code and the PKCE verifier for
// the session's first tokens. A refused request changes nothing, except
// that a code used twice ends the session that it made (section 4.1.2).
func (s *Server) authorizationCode(r *http.Request, form url.Values) (tokenResponse, error) {
	ctx := r.Context()
	clientID, err := publicClient(r, form)
	if err != nil {
		return tokenResponse{}, err
	}
	code, verifier, redirectURI := form.Get("code"), form.Get("code_verifier"), form.Get("redirect_uri")
	switch {
	case code == "":
		return tokenResponse{}, badRequest("invalid_request", "code is required")
	case !validVerifier(verifier):
		return tokenResponse{}, badRequest("invalid_request", "code_verifier is required: the PKCE verifier of the authorization request, 43 to 128 unreserved characters")
	case redirectURI == "":
		return tokenResponse{}, badRequest("invalid_request", "redirect_uri is required: the one of the authorization request")
	}
	codeHash := HashToken(code)
	req, err := s.store.AuthRequestByCode(ctx, codeHash)
	switch {
	case xerrors.Is(err, ErrUnknownAuthRequest):
		return tokenResponse{}, errUnknownCode
	case err != nil:
		return tokenResponse{}, xerrors.Errorf("read the authorization request of the code: %w", err)
	}
	now := s.clock.Now()
	switch {
	case req.State == AuthRequestRedeemed:
		return tokenResponse{}, s.endSession(ctx, req.ID, now, errUnknownCode, "a code was used again, its session ended", slog.F("client_id", req.ClientID))
	case req.State != AuthRequestApproved || !req.ExpiresAt.After(now):
		return tokenResponse{}, errUnknownCode
	case req.ClientID != clientID:
		s.logger.Info(ctx, "a code was sent by another client", slog.F("request_id", req.ID), slog.F("client_id", req.ClientID), slog.F("sent_client_id", clientID))
		return tokenResponse{}, badRequest("invalid_grant", "the code was issued to another client")
	case req.RedirectURI != redirectURI:
		return tokenResponse{}, badRequest("invalid_grant", "redirect_uri differs from the one of the authorization request")
	case subtle.ConstantTimeCompare([]byte(s256(verifier)), []byte(req.CodeChallenge)) != 1:
		return tokenResponse{}, badRequest("invalid_grant", "code_verifier does not match the code challenge of the authorization request")
	}
	if err := s.servedResource(form, req.Resource); err != nil {
		return tokenResponse{}, err
	}
	tokens, err := newUserTokens()
	if err != nil {
		return tokenResponse{}, err
	}
	session, err := s.store.RedeemAuthRequest(ctx, Redemption{
		Code: codeHash, Refresh: HashToken(tokens.refresh), Access: HashToken(tokens.access),
		AccessExpiresAt: now.Add(accessTokenLifetime), Now: now,
	})
	switch {
	case xerrors.Is(err, ErrUnknownAuthRequest):
		// Between the read and this step another request redeemed the code,
		// so it was used twice, or the code expired and there is no session.
		return tokenResponse{}, s.endSession(ctx, req.ID, now, errUnknownCode, "a code lost the race to redeem it, its session ended", slog.F("client_id", req.ClientID))
	case err != nil:
		return tokenResponse{}, xerrors.Errorf("redeem the code: %w", err)
	}
	s.logger.Debug(ctx, "browser login redeemed", slog.F("session_id", session.ID), slog.F("client_id", session.ClientID), slog.F("connection_id", session.ConnectionID))
	return tokens.response(session.Scopes), nil
}

// endSession ends the session with this ID at now, logs msg, and returns
// answer, the refusal of the request that ended it.
func (s *Server) endSession(ctx context.Context, id string, now time.Time, answer *tokenError, msg string, fields ...slog.Field) error {
	if err := s.store.EndSession(ctx, id, now); err != nil {
		return xerrors.Errorf("end session %s: %w", id, err)
	}
	s.logger.Info(ctx, msg, append([]slog.Field{slog.F("session_id", id)}, fields...)...)
	return answer
}

// servedResource refuses the tokens of a login whose resource is not one
// that this server serves, and a request whose resource parameters (RFC
// 8707) name another endpoint than the login's. A request that names none
// asks for the login's.
func (s *Server) servedResource(form url.Values, login string) error {
	if !slices.Contains(s.resources, login) {
		return badRequest("invalid_grant", "the login is for "+login+", which this server does not serve: log in again")
	}
	for _, resource := range form["resource"] {
		if resource != login {
			return badRequest("invalid_target", "the tokens of this login are for "+login)
		}
	}
	return nil
}

// userTokens are the refresh token and the access token that a grant of a
// browser login issues.
type userTokens struct{ refresh, access string }

func newUserTokens() (userTokens, error) {
	refresh, err := newSecret(refreshTokenPrefix)
	if err != nil {
		return userTokens{}, err
	}
	access, err := newSecret(accessTokenPrefix)
	if err != nil {
		return userTokens{}, err
	}
	return userTokens{refresh: refresh, access: access}, nil
}

func (t userTokens) response(scopes []faucet.Scope) tokenResponse {
	names := make([]string, len(scopes))
	for i, scope := range scopes {
		names[i] = string(scope)
	}
	return tokenResponse{
		AccessToken: t.access, TokenType: "Bearer", ExpiresIn: int(accessTokenLifetime / time.Second),
		Scope: strings.Join(names, " "), RefreshToken: t.refresh,
	}
}

// validVerifier reports whether v is a PKCE code verifier: 43 to 128
// unreserved characters (RFC 7636 section 4.1).
func validVerifier(v string) bool {
	if len(v) < minVerifierBytes || len(v) > maxVerifierBytes {
		return false
	}
	for i := range len(v) {
		if c := v[i]; (c < 'A' || c > 'Z') && (c < 'a' || c > 'z') && (c < '0' || c > '9') && strings.IndexByte("-._~", c) < 0 {
			return false
		}
	}
	return true
}

// refreshToken is the refresh_token grant (RFC 6749 section 6) of a browser
// login: the client trades its refresh token for the next one and a new
// access token. A refused request changes nothing, except that a refresh
// token used again more than refreshReuseGrace after its first use, and one
// whose session went unused for 180 days, end the session.
func (s *Server) refreshToken(r *http.Request, form url.Values) (tokenResponse, error) {
	ctx := r.Context()
	clientID, err := publicClient(r, form)
	if err != nil {
		return tokenResponse{}, err
	}
	presented := form.Get("refresh_token")
	switch {
	case presented == "":
		return tokenResponse{}, badRequest("invalid_request", "refresh_token is required")
	case !wellFormedRefreshToken(presented):
		return tokenResponse{}, errUnknownRefreshToken
	}
	presentedHash := HashToken(presented)
	session, token, err := s.store.SessionByRefreshToken(ctx, presentedHash)
	switch {
	case xerrors.Is(err, ErrUnknownSession):
		return tokenResponse{}, errUnknownRefreshToken
	case err != nil:
		return tokenResponse{}, xerrors.Errorf("read the session of the refresh token: %w", err)
	}
	now := s.clock.Now()
	usedAfter := now.Add(-refreshReuseGrace)
	switch {
	case !session.Live():
		return tokenResponse{}, errSessionEnded
	case token.Retired():
		// The client holds a later token, so this one has another holder.
		return tokenResponse{}, s.endSession(ctx, session.ID, now, errSessionEnded, "a retired refresh token came, its session ended", slog.F("client_id", session.ClientID))
	case token.Used() && !token.UsedAt.After(usedAfter):
		// The token has two holders, and one of them is not the client (R14).
		return tokenResponse{}, s.endSession(ctx, session.ID, now, errSessionEnded, "a refresh token came again after its reuse grace, its session ended", slog.F("client_id", session.ClientID))
	case !now.Before(session.RefreshedAt.Add(sessionIdleLifetime)):
		return tokenResponse{}, s.endSession(ctx, session.ID, now, errSessionEnded, "a session went unused for 180 days, it ended", slog.F("client_id", session.ClientID))
	case session.ClientID != clientID:
		s.logger.Info(ctx, "a refresh token was sent by another client", slog.F("session_id", session.ID), slog.F("client_id", session.ClientID), slog.F("sent_client_id", clientID))
		return tokenResponse{}, badRequest("invalid_grant", "the refresh token was issued to another client")
	}
	scopes, err := grantedScopes(form, session.Scopes)
	if err != nil {
		return tokenResponse{}, err
	}
	if err := s.servedResource(form, session.Resource); err != nil {
		return tokenResponse{}, err
	}
	// The GitHub token is refreshed before the rotation, never after: a
	// failure after the rotation would leave the client with a used token,
	// which ends its session when it tries again after the grace.
	github, err := s.freshenGitHubToken(ctx, session)
	if err != nil {
		return tokenResponse{}, err
	}
	tokens, err := newUserTokens()
	if err != nil {
		return tokenResponse{}, err
	}
	now = s.clock.Now()
	rotated, err := s.store.RotateSession(ctx, Rotation{
		Presented: presentedHash, Next: HashToken(tokens.refresh), Access: HashToken(tokens.access),
		AccessExpiresAt: now.Add(accessTokenLifetime), Scopes: scopes, UsedAfter: usedAfter, Now: now,
	})
	switch {
	case xerrors.Is(err, ErrUnknownSession):
		// Since the read, the session ended, or a use of another of its
		// tokens retired this one: the token had two holders (R14).
		return tokenResponse{}, s.endSession(ctx, session.ID, now, errSessionEnded, "a refresh token was retired while it was traded, its session ended", slog.F("client_id", session.ClientID))
	case err != nil:
		return tokenResponse{}, xerrors.Errorf("rotate the session: %w", err)
	}
	s.logger.Debug(ctx, "session refreshed", slog.F("session_id", rotated.ID), slog.F("client_id", rotated.ClientID), slog.F("connection_id", rotated.ConnectionID), slog.F("reused", token.Used()))
	// The rotation made the older access checks stale for the session, so
	// the refresh checks the most recently used again. Nothing after the
	// rotation may fail the grant, and the re-check fails nothing.
	s.access.recheck(ctx, rotated, now, github)
	return tokens.response(scopes), nil
}

// freshenGitHubToken refreshes the GitHub token of the session's user when it
// expires within githubRefreshMargin, and returns the token. GitHub refusing
// the refresh token ended the user's sessions, and refuses the grant. A
// token that does not open is an error. Any other failure is logged, and the
// grant goes on with the stored token, which the access check refreshes when
// it must. The token returned is then the zero value.
func (s *Server) freshenGitHubToken(ctx context.Context, session Session) (githubToken, error) {
	tokens := s.access.tokens
	token, err := tokens.accessToken(ctx, session.ConnectionID, tokens.expiresWithin(githubRefreshMargin))
	switch {
	case err == nil:
		return token, nil
	case xerrors.Is(err, faucet.ErrInvalidToken):
		return githubToken{}, badRequest("invalid_grant", "GitHub ended the login of this user: log in again")
	case xerrors.Is(err, ErrUnreadableToken):
		return githubToken{}, err
	}
	s.logger.Warn(ctx, "could not refresh the GitHub token of a session, the grant goes on", slog.F("session_id", session.ID), slog.F("connection_id", session.ConnectionID), slog.Error(err))
	return githubToken{}, nil
}

// wellFormedRefreshToken reports whether token has the form of a refresh
// token: the prefix and 32 bytes in base64url without padding.
func wellFormedRefreshToken(token string) bool {
	body, ok := strings.CutPrefix(token, refreshTokenPrefix)
	if !ok || len(body) != base64.RawURLEncoding.EncodedLen(secretBytes) {
		return false
	}
	_, err := base64.RawURLEncoding.Strict().DecodeString(body)
	return err == nil
}
