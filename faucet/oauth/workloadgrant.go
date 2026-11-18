package oauth

import (
	"net/http"
	"net/url"
	"slices"

	"golang.org/x/xerrors"

	"cdr.dev/slog/v3"
)

const (
	tokenExchangeGrant = "urn:ietf:params:oauth:grant-type:token-exchange"
	accessTokenType    = "urn:ietf:params:oauth:token-type:access_token" //nolint:gosec // A URN, not a credential.
)

// subjectTokenTypes are the types of subject token that the token exchange
// takes. Both are a JWT: Vercel and Kubernetes give an OpenID Connect ID
// token.
var subjectTokenTypes = []string{"urn:ietf:params:oauth:token-type:jwt", "urn:ietf:params:oauth:token-type:id_token"}

// tokenExchange is the token exchange grant (RFC 8693) of a workload, such as
// a Vercel function or a Kubernetes pod. The workload names the client by
// client_id, and its subject token, a JWT that its platform signed for
// faucet, is the proof: it gets a token of the client when the subject token
// matches a trust rule of the client. It carries no secret, asks for no
// delegation and gets no refresh token, and its token ends no later than the
// subject token.
func (s *Server) tokenExchange(r *http.Request, form url.Values) (tokenResponse, error) {
	ctx := r.Context()
	switch {
	case r.Header.Get("Authorization") != "" || form.Has("client_secret"):
		return tokenResponse{}, badRequest("invalid_request", "a token exchange carries no client secret: send client_id in the form, and the workload's token as subject_token")
	case form.Get("client_id") == "":
		return tokenResponse{}, badRequest("invalid_request", "client_id is required: the client that the workload acts as")
	case form.Get("subject_token") == "":
		return tokenResponse{}, badRequest("invalid_request", "subject_token is required: the workload's token from its platform")
	case !slices.Contains(subjectTokenTypes, form.Get("subject_token_type")):
		return tokenResponse{}, badRequest("invalid_request", "subject_token_type must be "+subjectTokenTypes[0]+" or "+subjectTokenTypes[1])
	case form.Has("actor_token") || form.Has("actor_token_type"):
		return tokenResponse{}, badRequest("invalid_request", "faucet takes no actor_token: a workload acts as the client, for no other party")
	case form.Has("requested_token_type") && form.Get("requested_token_type") != accessTokenType:
		return tokenResponse{}, badRequest("invalid_request", "requested_token_type can only be "+accessTokenType)
	}
	c, err := s.workloadClient(r, form.Get("client_id"))
	if err != nil {
		return tokenResponse{}, err
	}
	scopes, err := grantedScopes(form, c.Scopes)
	if err != nil {
		return tokenResponse{}, err
	}
	if err := checkResources(form, c); err != nil {
		return tokenResponse{}, err
	}
	if form.Has("audience") && form.Get("audience") != c.Resource {
		return tokenResponse{}, badRequest("invalid_target", "the tokens of this client are for "+c.Resource)
	}
	stored, err := s.store.TrustRules(ctx, c.ID)
	if err != nil {
		return tokenResponse{}, xerrors.Errorf("read the trust rules of the client: %w", err)
	}
	if len(stored) == 0 {
		s.logger.Info(ctx, "a workload asked for a token of a client that trusts no issuer", slog.F("client_id", c.ID))
		return tokenResponse{}, &tokenError{
			status: http.StatusUnauthorized, code: "invalid_client",
			description: "the client trusts no workload issuer: add one with faucet clients trust",
		}
	}
	rules := make([]TrustRule, len(stored))
	for i, st := range stored {
		rules[i] = st.Rule
	}
	now := s.clock.Now()
	match, err := s.workloads.check(ctx, rules, form.Get("subject_token"), s.issuer, now)
	switch {
	case xerrors.Is(err, errIssuerKeys):
		s.logger.Warn(ctx, "a workload's token waits for the keys of its issuer", slog.F("client_id", c.ID), slog.Error(err))
		return tokenResponse{}, &tokenError{
			status: http.StatusServiceUnavailable, code: "temporarily_unavailable", retryAfter: keyFetchInterval,
			description: "faucet could not fetch the keys of the subject token's issuer: try again in a minute",
		}
	case xerrors.Is(err, errSubjectToken):
		s.logger.Info(ctx, "a workload's token was refused", slog.F("client_id", c.ID), slog.F("reason", err.Error()))
		return tokenResponse{}, badRequest("invalid_request", err.Error())
	case err != nil:
		return tokenResponse{}, xerrors.Errorf("check the subject token: %w", err)
	}
	resp, err := s.mintServiceToken(ctx, c, scopes, now, match.lifetime(now))
	if err != nil {
		return tokenResponse{}, err
	}
	resp.IssuedTokenType = accessTokenType
	s.logger.Debug(ctx, "token issued to a workload", slog.F("client_id", c.ID), slog.F("issuer", match.Rule.Issuer), slog.F("scope", resp.Scope), slog.F("expires_in", resp.ExpiresIn))
	return resp, nil
}

// workloadClient reads the live client that a workload names, which this
// server serves. An unknown or a disabled client gets the answer of a failed
// client authentication.
func (s *Server) workloadClient(r *http.Request, id string) (Client, error) {
	if ValidateClientID(id) != nil {
		return Client{}, errClientAuth
	}
	c, err := s.store.Client(r.Context(), id)
	switch {
	case xerrors.Is(err, ErrUnknownClient):
		return Client{}, errClientAuth
	case err != nil:
		return Client{}, xerrors.Errorf("read the client: %w", err)
	case !c.Live():
		return Client{}, errClientAuth
	}
	if err := s.servedClient(r.Context(), c); err != nil {
		return Client{}, err
	}
	return c, nil
}
