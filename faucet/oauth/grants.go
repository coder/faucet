package oauth

import (
	"context"
	"crypto/subtle"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"golang.org/x/xerrors"

	"cdr.dev/slog/v3"
	"github.com/coder/faucet/faucet"
)

const (
	minVerifierBytes = 43
	maxVerifierBytes = 128
)

// errUnknownCode is the answer to a code that is unknown, expired or used.
var errUnknownCode = badRequest("invalid_grant", "the code is unknown, expired or used: log in again")

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
		return tokenResponse{}, s.endSessionOfCode(ctx, req, now, "a code was used again, its session ended")
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
		return tokenResponse{}, s.endSessionOfCode(ctx, req, now, "a code lost the race to redeem it, its session ended")
	case err != nil:
		return tokenResponse{}, xerrors.Errorf("redeem the code: %w", err)
	}
	s.logger.Debug(ctx, "browser login redeemed", slog.F("session_id", session.ID), slog.F("client_id", session.ClientID), slog.F("connection_id", session.ConnectionID))
	return tokens.response(session.Scopes), nil
}

// endSessionOfCode ends the session that the code of req made, and returns
// the answer to the code.
func (s *Server) endSessionOfCode(ctx context.Context, req AuthRequest, now time.Time, msg string) error {
	if err := s.store.EndSession(ctx, req.ID, now); err != nil {
		return xerrors.Errorf("end the session of a code used twice: %w", err)
	}
	s.logger.Info(ctx, msg, slog.F("session_id", req.ID), slog.F("client_id", req.ClientID), slog.F("connection_id", req.ConnectionID))
	return errUnknownCode
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

// refreshToken is the refresh_token grant of a browser login.
func (s *Server) refreshToken(_ *http.Request, _ url.Values) (tokenResponse, error) {
	return tokenResponse{}, xerrors.New("not built yet")
}
