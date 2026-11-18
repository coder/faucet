package oauth

import (
	"context"
	"net/http"
	"net/url"
	"slices"
	"time"

	"golang.org/x/xerrors"

	"cdr.dev/slog/v3"
)

const (
	tokenExchangeGrant = "urn:ietf:params:oauth:grant-type:token-exchange" //nolint:gosec // A URN, not a credential.
	accessTokenType    = "urn:ietf:params:oauth:token-type:access_token"   //nolint:gosec // A URN, not a credential.
)

var subjectTokenTypes = []string{"urn:ietf:params:oauth:token-type:jwt", "urn:ietf:params:oauth:token-type:id_token"}

// errWorkloadRefused is the one answer to a token exchange that faucet
// refuses before the subject token's signature verified under a rule of the
// client, so that it tells no caller which clients exist or what they
// trust. The client sends no Authorization header, so the answer is a 400
// with no challenge (RFC 6749 section 5.2).
var errWorkloadRefused = &tokenError{
	status: http.StatusBadRequest, code: "invalid_client",
	description: "the client takes no such subject token: check client_id and the token, and faucet's log for the reason",
}

// workloadClientError says why a client takes no token exchange. It is for
// the log: the caller gets errWorkloadRefused.
type workloadClientError string

func (e workloadClientError) Error() string { return string(e) }

func (s *Server) tokenExchange(r *http.Request, form url.Values) (tokenResponse, error) {
	switch {
	case len(r.Header.Values("Authorization")) > 0 || form.Has("client_secret") || form.Has("client_assertion") || form.Has("client_assertion_type"):
		return tokenResponse{}, badRequest("invalid_request", "a token exchange carries no client secret and no client assertion: send client_id in the form, and the workload's token as subject_token")
	case form.Get("client_id") == "":
		return tokenResponse{}, badRequest("invalid_request", "client_id is required: the client that the workload acts as")
	case form.Get("subject_token") == "":
		return tokenResponse{}, badRequest("invalid_request", "subject_token is required: the workload's token from its platform")
	case !slices.Contains(subjectTokenTypes, form.Get("subject_token_type")):
		return tokenResponse{}, badRequest("invalid_request", "subject_token_type must be "+subjectTokenTypes[0]+" or "+subjectTokenTypes[1])
	case form.Has("actor_token") || form.Has("actor_token_type"):
		return tokenResponse{}, badRequest("invalid_request", "faucet takes no actor_token: a workload acts as the client, for no other party")
	case form.Get("requested_token_type") != "" && form.Get("requested_token_type") != accessTokenType:
		return tokenResponse{}, badRequest("invalid_request", "requested_token_type can only be "+accessTokenType)
	}
	resp, err := s.exchange(r.Context(), form)
	if err != nil {
		return tokenResponse{}, s.exchangeAnswer(r.Context(), form.Get("client_id"), err)
	}
	return resp, nil
}

// exchange trades the subject token of form for a token of its client. It
// checks what the form asks for only after the token's signature verified
// under a rule of the client, so that no answer tells an unverified caller
// the client's endpoint or scopes.
func (s *Server) exchange(ctx context.Context, form url.Values) (tokenResponse, error) {
	c, rules, err := s.workloadClient(ctx, form.Get("client_id"))
	if err != nil {
		return tokenResponse{}, err
	}
	match, err := s.workloads.check(ctx, rules, form.Get("subject_token"), s.issuer, s.clock.Now())
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
	for _, audience := range form["audience"] {
		if audience != "" && audience != c.Resource {
			return tokenResponse{}, badRequest("invalid_target", "the tokens of this client are for "+c.Resource)
		}
	}
	now := s.clock.Now()
	lifetime := match.lifetime(now)
	if lifetime < minWorkloadTokenLifetime {
		return tokenResponse{}, refuseSigned("it has expired, or expires in less than a second")
	}
	resp, err := s.mintServiceToken(ctx, c, scopes, now, lifetime)
	if err != nil {
		return tokenResponse{}, err
	}
	resp.IssuedTokenType = accessTokenType
	s.logger.Debug(ctx, "token issued to a workload", slog.F("client_id", c.ID), slog.F("issuer", match.Rule.Issuer), slog.F("scope", resp.Scope), slog.F("expires_in", resp.ExpiresIn))
	return resp, nil
}

// exchangeAnswer is the answer to a token exchange of clientID that failed
// with err, which it logs with its reason.
func (s *Server) exchangeAnswer(ctx context.Context, clientID string, err error) error {
	var keys issuerKeysError
	var refusal workloadClientError
	var token subjectTokenError
	var answer *tokenError
	switch {
	case xerrors.As(err, &keys):
		log := s.logger.Info
		if keys.fetch != nil {
			log = s.logger.Warn
		}
		log(ctx, "a workload's token waits for the keys of its issuer", slog.F("client_id", clientID), slog.Error(err))
		return &tokenError{
			status: http.StatusServiceUnavailable, code: "temporarily_unavailable", retryAfter: max(time.Second, keys.retryAt.Sub(s.clock.Now())),
			description: "faucet cannot get the key of the subject token from its issuer yet: try again after Retry-After seconds",
		}
	case xerrors.As(err, &refusal), xerrors.As(err, &token) && !token.signed:
		answer = errWorkloadRefused
	case xerrors.As(err, &token):
		answer = badRequest("invalid_request", err.Error())
	case !xerrors.As(err, &answer):
		return err
	}
	s.logger.Info(ctx, "a token exchange was refused", slog.F("client_id", clientID), slog.F("reason", err.Error()))
	return answer
}

// workloadClient returns the client that id names and its trust rules, or a
// workloadClientError when the client takes no token exchange.
func (s *Server) workloadClient(ctx context.Context, id string) (Client, []TrustRule, error) {
	if ValidateClientID(id) != nil {
		return Client{}, nil, workloadClientError("client_id is not a client ID")
	}
	c, err := s.store.Client(ctx, id)
	switch {
	case xerrors.Is(err, ErrUnknownClient):
		return Client{}, nil, workloadClientError("no client has this ID")
	case err != nil:
		return Client{}, nil, xerrors.Errorf("read the client: %w", err)
	case !c.Live():
		return Client{}, nil, workloadClientError("the client is disabled")
	case !slices.Contains(s.resources, c.Resource):
		return Client{}, nil, workloadClientError("the client is for " + c.Resource + ", which this server does not serve")
	}
	stored, err := s.store.TrustRules(ctx, c.ID)
	switch {
	case err != nil:
		return Client{}, nil, xerrors.Errorf("read the trust rules of the client: %w", err)
	case len(stored) == 0:
		return Client{}, nil, workloadClientError("the client trusts no workload issuer: add one with faucet clients trust")
	}
	rules := make([]TrustRule, len(stored))
	for i, st := range stored {
		rules[i] = st.Rule
	}
	return c, rules, nil
}
