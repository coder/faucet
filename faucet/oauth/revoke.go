package oauth

import (
	"context"
	"net/http"
	"net/url"

	"golang.org/x/xerrors"

	"cdr.dev/slog/v3"
)

// RevokePath is the revocation endpoint (RFC 7009) under the issuer.
const RevokePath = "/oauth/revoke"

// errOtherClient is the answer to a revocation of a token that faucet
// issued to another client, which stays as it is (RFC 7009 section 2.1).
var errOtherClient = badRequest("invalid_grant", "the token was issued to another client")

// RevokeHandler serves the revocation endpoint at RevokePath. It takes POST
// only. A refresh token ends its session, so a client logs out with it, and
// an access token stops working. A token that faucet does not hold is no
// error (RFC 7009 section 2.2).
func (s *Server) RevokeHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		if err := s.revoke(w, r); err != nil {
			s.writeTokenError(w, r, err, "revocation", "revoke the token")
			return
		}
		w.WriteHeader(http.StatusOK)
	})
}

func (s *Server) revoke(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	form, err := readTokenForm(w, r)
	if err != nil {
		return err
	}
	token := form.Get("token")
	if token == "" {
		return badRequest("invalid_request", "token is required: the refresh or access token to revoke")
	}
	client, err := s.revokingClient(ctx, r, form)
	if err != nil {
		return err
	}
	switch {
	case wellFormedRefreshToken(token):
		return s.revokeRefreshToken(ctx, client, HashToken(token))
	case wellFormedAccessToken(token):
		return s.revokeAccessToken(ctx, client, HashToken(token))
	}
	return nil
}

// revoker is the client of a revocation.
type revoker struct {
	id string
	// service is true for a service client, which authenticated with its
	// secret, and false for a client of a browser login.
	service bool
}

// revokingClient authenticates the client of a revocation as the token
// endpoint does: a service client with its secret (client_secret_basic),
// and, while the browser login is on, a client of the browser login by its
// ID (none).
func (s *Server) revokingClient(ctx context.Context, r *http.Request, form url.Values) (revoker, error) {
	if _, secret, ok := r.BasicAuth(); ok && secret != "" || s.login == nil {
		c, err := s.authenticateClient(ctx, r, form)
		if err != nil {
			return revoker{}, err
		}
		return revoker{id: c.ID, service: true}, nil
	}
	id, err := publicClient(r, form)
	if err != nil {
		return revoker{}, err
	}
	return revoker{id: id}, nil
}

// revokeRefreshToken ends the session of the refresh token with this hash.
func (s *Server) revokeRefreshToken(ctx context.Context, client revoker, hash TokenHash) error {
	session, _, err := s.store.SessionByRefreshToken(ctx, hash)
	switch {
	case xerrors.Is(err, ErrUnknownSession):
		return nil
	case err != nil:
		return xerrors.Errorf("read the session of the refresh token: %w", err)
	case client.service || session.ClientID != client.id:
		s.logger.Info(ctx, "a refresh token was revoked by another client", slog.F("session_id", session.ID), slog.F("client_id", session.ClientID), slog.F("sent_client_id", client.id))
		return errOtherClient
	case !session.Live():
		return nil
	}
	if err := s.store.EndSession(ctx, session.ID, s.clock.Now()); err != nil {
		return xerrors.Errorf("end session %s: %w", session.ID, err)
	}
	s.logger.Info(ctx, "a client revoked its refresh token, its session ended", slog.F("session_id", session.ID), slog.F("client_id", session.ClientID))
	return nil
}

// revokeAccessToken deletes the access token with this hash.
func (s *Server) revokeAccessToken(ctx context.Context, client revoker, hash TokenHash) error {
	token, err := s.store.AccessToken(ctx, hash)
	switch {
	case xerrors.Is(err, ErrUnknownToken):
		return nil
	case err != nil:
		return xerrors.Errorf("read the access token: %w", err)
	}
	owner := revoker{id: token.Subject, service: token.SubjectKind == SubjectService}
	if token.SubjectKind == SubjectUser {
		session, err := s.store.Session(ctx, token.Subject)
		switch {
		case xerrors.Is(err, ErrUnknownSession):
			// The token of a session that is gone works no more.
			return nil
		case err != nil:
			return xerrors.Errorf("read the session of the access token: %w", err)
		}
		owner.id = session.ClientID
	}
	if owner != client {
		s.logger.Info(ctx, "an access token was revoked by another client", slog.F("subject_kind", token.SubjectKind), slog.F("subject", token.Subject), slog.F("sent_client_id", client.id))
		return errOtherClient
	}
	if err := s.store.DeleteAccessToken(ctx, hash); err != nil {
		return xerrors.Errorf("delete the access token: %w", err)
	}
	s.logger.Info(ctx, "a client revoked an access token", slog.F("subject_kind", token.SubjectKind), slog.F("subject", token.Subject))
	return nil
}
