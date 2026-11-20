package oauth

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"io"
	"maps"
	"mime"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"golang.org/x/xerrors"

	"cdr.dev/slog/v3"
	"github.com/coder/quartz"
	"github.com/coder/superblink-events/faucet"
)

const (
	// MetadataPath is where the authorization server's metadata (RFC 8414)
	// lives under its issuer.
	MetadataPath = "/.well-known/oauth-authorization-server"
	// TokenPath is the token endpoint under the issuer.
	TokenPath = "/oauth/token" //nolint:gosec // A URL path, not a credential.
	// AuthorizePath is the authorization endpoint under the issuer.
	AuthorizePath = "/oauth/authorize"

	accessTokenLifetime  = 15 * time.Minute
	maxTokenRequestBytes = 16 << 10
)

// grant is one row of the grant table: a grant type that the token endpoint
// takes. The metadata names what the table holds, so a new grant is a new
// row.
type grant struct {
	// authMethods are the ways a client authenticates for the grant, as
	// token_endpoint_auth_methods_supported names them.
	authMethods []string
	revokes     bool
	// responseType is what the grant asks the authorization endpoint for,
	// empty for a grant that does not use that endpoint.
	responseType string
	issue        func(s *Server, r *http.Request, form url.Values) (tokenResponse, error)
}

var grants = map[string]grant{
	"client_credentials": {authMethods: []string{"client_secret_basic"}, revokes: true, issue: (*Server).clientCredentials},
	tokenExchangeGrant:   {authMethods: []string{"none"}, issue: (*Server).tokenExchange},
}

// loginGrants are the rows that the browser login adds to the grant table
// of a server.
var loginGrants = map[string]grant{
	"authorization_code": {authMethods: []string{"none"}, revokes: true, responseType: "code", issue: (*Server).authorizationCode},
	"refresh_token":      {authMethods: []string{"none"}, revokes: true, issue: (*Server).refreshToken},
}

// ServerDeps are what a Server needs.
type ServerDeps struct {
	Store  Store
	Clock  quartz.Clock
	Logger slog.Logger
	// Issuer is faucet's public URL, such as https://faucet.example.com,
	// with no path. A client compares it byte for byte.
	Issuer string
	// Resources are the endpoints that this server serves. A client whose
	// resource is not one of them gets no token.
	Resources []string
	// Login turns on the browser login. Nil leaves it off: the server then
	// takes service clients only.
	Login *BrowserLogin
	// Repos asks GitHub whether the token of a user who logged in through
	// the browser can read a repository, when a refresh checks the user's
	// repositories again. It is required with Login, and asks GitHub at
	// every call, as RepoChecker says.
	Repos RepoChecker
	// WorkloadKeys sets up the client that fetches the keys of the issuers
	// that trust rules name. An issuer's discovery document chooses the URL
	// of its keys, so faucet builds that client itself with
	// faucet.NewHTTPClient, and the zero value is the one for production.
	WorkloadKeys faucet.HTTPClientOptions
}

// Server is faucet's authorization server: its metadata and its token
// endpoint.
type Server struct {
	store     Store
	clock     quartz.Clock
	logger    slog.Logger
	issuer    string
	resources []string
	// grants is the grant table of this server.
	grants    map[string]grant
	workloads *workloadChecker
	// login is nil when the browser login is off, and access is then the
	// zero value.
	login  *browserLogin
	access repoAccess
}

// NewServer returns the Server for d.
func NewServer(d ServerDeps) (*Server, error) {
	switch {
	case d.Store == nil:
		return nil, xerrors.New("store is required")
	case d.Clock == nil:
		return nil, xerrors.New("clock is required")
	case d.Issuer == "" || strings.HasSuffix(d.Issuer, "/"):
		return nil, xerrors.New("the issuer is faucet's public URL, with no trailing slash")
	case len(d.Resources) == 0:
		return nil, xerrors.New("the server needs the endpoints that it serves")
	}
	s := &Server{
		store: d.Store, clock: d.Clock, logger: d.Logger, issuer: d.Issuer,
		resources: slices.Clone(d.Resources), grants: maps.Clone(grants),
		workloads: newWorkloadChecker(faucet.NewHTTPClient(d.WorkloadKeys), d.Clock),
	}
	if d.WorkloadKeys.AllowInsecureLoopback {
		d.Logger.Warn(context.Background(), "the client that fetches the keys of workload issuers reaches loopback over http, which is for tests only")
	}
	if d.Login != nil {
		login, err := newBrowserLogin(*d.Login, d.Clock)
		switch {
		case err != nil:
			return nil, xerrors.Errorf("the browser login: %w", err)
		case d.Repos == nil:
			return nil, xerrors.New("the browser login needs the repository checker")
		}
		s.login = login
		s.access = repoAccess{tokens: newConnections(d.Store, d.Clock, d.Logger, login), repos: d.Repos}
		maps.Copy(s.grants, loginGrants)
		if d.Login.Documents.AllowInsecureLoopback {
			d.Logger.Warn(context.Background(), "the client that fetches client documents reaches loopback over http, which is for tests only")
		}
	}
	return s, nil
}

type authServerMetadata struct {
	Issuer string `json:"issuer"`
	// AuthorizationEndpoint is listed even when no grant uses it: the
	// TypeScript MCP SDK refuses metadata without it, whatever grant it asks
	// for.
	AuthorizationEndpoint             string   `json:"authorization_endpoint"`
	TokenEndpoint                     string   `json:"token_endpoint"`
	ScopesSupported                   []string `json:"scopes_supported"`
	ResponseTypesSupported            []string `json:"response_types_supported"`
	GrantTypesSupported               []string `json:"grant_types_supported"`
	TokenEndpointAuthMethodsSupported []string `json:"token_endpoint_auth_methods_supported"`
	// CodeChallengeMethodsSupported is listed even without a grant that uses
	// it: go-sdk refuses metadata that lists no PKCE method, whatever grant
	// it asks for.
	CodeChallengeMethodsSupported []string `json:"code_challenge_methods_supported"`
	// ClientIDMetadataDocumentSupported and
	// AuthorizationResponseIssParameterSupported are true when a grant uses
	// the authorization endpoint: that endpoint takes a client by the URL of
	// its metadata document, and its redirect back names the issuer (RFC
	// 9207).
	ClientIDMetadataDocumentSupported          bool     `json:"client_id_metadata_document_supported,omitempty"`
	AuthorizationResponseIssParameterSupported bool     `json:"authorization_response_iss_parameter_supported,omitempty"`
	RevocationEndpoint                         string   `json:"revocation_endpoint"`
	RevocationEndpointAuthMethodsSupported     []string `json:"revocation_endpoint_auth_methods_supported"`
}

// metadataOf describes the authorization server at issuer that takes the
// grants of table.
func metadataOf(issuer string, table map[string]grant) authServerMetadata {
	m := authServerMetadata{
		Issuer:                        issuer,
		AuthorizationEndpoint:         issuer + AuthorizePath,
		TokenEndpoint:                 issuer + TokenPath,
		RevocationEndpoint:            issuer + RevokePath,
		ResponseTypesSupported:        []string{},
		GrantTypesSupported:           slices.Sorted(maps.Keys(table)),
		CodeChallengeMethodsSupported: []string{"S256"},
	}
	for _, s := range faucet.AllScopes() {
		m.ScopesSupported = append(m.ScopesSupported, string(s))
	}
	for _, g := range table {
		m.TokenEndpointAuthMethodsSupported = append(m.TokenEndpointAuthMethodsSupported, g.authMethods...)
		if g.revokes {
			m.RevocationEndpointAuthMethodsSupported = append(m.RevocationEndpointAuthMethodsSupported, g.authMethods...)
		}
		if g.responseType != "" {
			m.ResponseTypesSupported = append(m.ResponseTypesSupported, g.responseType)
		}
	}
	slices.Sort(m.TokenEndpointAuthMethodsSupported)
	m.TokenEndpointAuthMethodsSupported = slices.Compact(m.TokenEndpointAuthMethodsSupported)
	slices.Sort(m.RevocationEndpointAuthMethodsSupported)
	m.RevocationEndpointAuthMethodsSupported = slices.Compact(m.RevocationEndpointAuthMethodsSupported)
	slices.Sort(m.ResponseTypesSupported)
	m.ClientIDMetadataDocumentSupported = len(m.ResponseTypesSupported) > 0
	m.AuthorizationResponseIssParameterSupported = len(m.ResponseTypesSupported) > 0
	return m
}

// MetadataHandler serves the authorization server's metadata (RFC 8414) at
// MetadataPath, as a discovery document.
func (s *Server) MetadataHandler() http.Handler {
	return document(metadataOf(s.issuer, s.grants))
}

// PurgeResult is what Server.Purge deleted.
type PurgeResult struct {
	// ExpiredAccessTokens is how many access tokens had expired. A token is
	// worth nothing once it expires, so none is kept.
	ExpiredAccessTokens int64
	// ExpiredAuthRequests is how many authorization requests had expired:
	// at once for one without a code, from which nothing can come, and more
	// than a day ago for one with a code, so that a code used again within
	// that day still ends the session it made.
	ExpiredAuthRequests int64
	// EndedSessions is how many sessions had ended or gone unused for 180
	// days. Their refresh tokens went with them.
	EndedSessions int64
	// UnusedConnections is how many connections no session and no request
	// named, a day after their last update. The GitHub tokens that faucet
	// kept for their users went with them.
	UnusedConnections int64
	// UnusedAccessChecks is how many access checks no session had used for
	// a day, and how many denials were 15 minutes old.
	UnusedAccessChecks int64
}

// Purge deletes the state of the login server that is past its use: every
// access token that has expired, every authorization request that expired
// more than a day ago, every session that ended or went unused for 180
// days, every access check that no session used for a day, every denial 15
// minutes after its lookup started, and then every connection that nothing
// names any more.
func (s *Server) Purge(ctx context.Context) (PurgeResult, error) {
	now := s.clock.Now()
	var res PurgeResult
	var err error
	if res.ExpiredAccessTokens, err = s.store.DeleteExpiredAccessTokens(ctx, now); err != nil {
		return PurgeResult{}, err
	}
	if res.ExpiredAuthRequests, err = s.store.DeleteExpiredAuthRequests(ctx, now, now.Add(-authRequestRetention)); err != nil {
		return PurgeResult{}, err
	}
	if res.EndedSessions, err = s.store.DeleteEndedSessions(ctx, now, now.Add(-sessionIdleLifetime)); err != nil {
		return PurgeResult{}, err
	}
	if res.UnusedAccessChecks, err = s.store.DeleteUnusedAccessChecks(ctx, now.Add(-accessCheckRetention), now.Add(-accessDenialRetention)); err != nil {
		return PurgeResult{}, err
	}
	// A login stores its connection a moment before the request that names
	// it, so a connection that a login just stored is a day from going.
	if res.UnusedConnections, err = s.store.DeleteUnusedConnections(ctx, now.Add(-authRequestRetention)); err != nil {
		return PurgeResult{}, err
	}
	return res, nil
}

// AuthorizeHandler serves the authorization endpoint at AuthorizePath: the
// consent page of the browser login, and the form that the page posts.
// Without the browser login no grant uses the endpoint, so it answers every
// request with unsupported_response_type, and never redirects, because it
// knows no client's redirect URI.
func (s *Server) AuthorizeHandler() http.Handler {
	if s.login != nil {
		return s.browserAuthorizeHandler()
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		s.logger.Info(r.Context(), "authorization request refused, no grant uses the endpoint")
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "unsupported_response_type", "error_description": "faucet takes no grant through the authorization endpoint: its browser login is off",
		})
	})
}

// tokenError is an error answer of the token endpoint (RFC 6749 section
// 5.2). Its description says what the client must change and holds no
// secret.
type tokenError struct {
	status      int
	code        string
	description string
	retryAfter  time.Duration
}

func (e *tokenError) Error() string { return e.code + ": " + e.description }

func badRequest(code, description string) *tokenError {
	return &tokenError{status: http.StatusBadRequest, code: code, description: description}
}

// errClientAuth is the one answer to every failed client authentication, so
// it does not tell an unknown client from a wrong secret or a disabled
// client.
var errClientAuth = &tokenError{status: http.StatusUnauthorized, code: "invalid_client", description: "client authentication failed"}

type tokenResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int    `json:"expires_in"`
	Scope       string `json:"scope"`
	// RefreshToken is set by the grants of a browser login.
	RefreshToken string `json:"refresh_token,omitempty"`
	// IssuedTokenType is set by the token exchange (RFC 8693 section 2.2.1).
	IssuedTokenType string `json:"issued_token_type,omitempty"`
}

// TokenHandler serves the token endpoint at TokenPath. It takes POST only,
// with the grants of the grant table.
func (s *Server) TokenHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Pragma", "no-cache")
		resp, err := s.token(w, r)
		if err != nil {
			s.writeTokenError(w, r, err, "token request", "issue a token")
			return
		}
		writeJSON(w, http.StatusOK, resp)
	})
}

// writeTokenError answers a request to the token endpoint or the
// revocation endpoint that failed with err: a tokenError as itself (RFC
// 6749 section 5.2), any other error as server_error. request names the
// request and work what failed, in the log and in the answer.
func (s *Server) writeTokenError(w http.ResponseWriter, r *http.Request, err error, request, work string) {
	var te *tokenError
	if !xerrors.As(err, &te) {
		s.logger.Warn(r.Context(), "could not "+work, slog.Error(err))
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "server_error", "error_description": "could not " + work + ", try again"})
		return
	}
	s.logger.Info(r.Context(), request+" refused", slog.F("error", te.code), slog.F("reason", te.description))
	if te.status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", `Basic realm="faucet"`)
	}
	if te.retryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int((te.retryAfter+time.Second-1)/time.Second)))
	}
	writeJSON(w, te.status, map[string]string{"error": te.code, "error_description": te.description})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func (s *Server) token(w http.ResponseWriter, r *http.Request) (tokenResponse, error) {
	form, err := readTokenForm(w, r)
	if err != nil {
		return tokenResponse{}, err
	}
	name := form.Get("grant_type")
	g, ok := s.grants[name]
	switch {
	case name == "":
		return tokenResponse{}, badRequest("invalid_request", "grant_type is required")
	case !ok:
		return tokenResponse{}, badRequest("unsupported_grant_type", "faucet takes the grant types "+strings.Join(slices.Sorted(maps.Keys(s.grants)), ", "))
	}
	return g.issue(s, r, form)
}

// Every parameter but resource and audience, which RFC 8707 and RFC 8693 let
// a client repeat, appears at most once (RFC 6749 section 3.2).
func readTokenForm(w http.ResponseWriter, r *http.Request) (url.Values, error) {
	if media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || media != "application/x-www-form-urlencoded" {
		return nil, badRequest("invalid_request", "the body is a form, application/x-www-form-urlencoded")
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxTokenRequestBytes))
	if err != nil {
		return nil, badRequest("invalid_request", "could not read the body, or it is over 16 KiB")
	}
	form, err := url.ParseQuery(string(body))
	if err != nil {
		return nil, badRequest("invalid_request", "the body is not a valid form")
	}
	for name, values := range form {
		if len(values) > 1 && name != "resource" && name != "audience" {
			return nil, badRequest("invalid_request", name+" appears more than once")
		}
	}
	return form, nil
}

// clientCredentials is the client_credentials grant (RFC 6749 section 4.4)
// of a service client.
func (s *Server) clientCredentials(r *http.Request, form url.Values) (tokenResponse, error) {
	ctx := r.Context()
	c, err := s.authenticateClient(ctx, r, form)
	if err != nil {
		return tokenResponse{}, err
	}
	if err := s.checkServed(ctx, c); err != nil {
		return tokenResponse{}, err
	}
	scopes, err := grantedScopes(form, c.Scopes)
	if err != nil {
		return tokenResponse{}, err
	}
	if err := checkResources(form, c); err != nil {
		return tokenResponse{}, err
	}
	resp, err := s.mintServiceToken(ctx, c, scopes, s.clock.Now(), accessTokenLifetime)
	if err != nil {
		return tokenResponse{}, err
	}
	s.logger.Debug(ctx, "token issued", slog.F("client_id", c.ID), slog.F("scope", resp.Scope))
	return resp, nil
}

func (s *Server) checkServed(ctx context.Context, c Client) error {
	if slices.Contains(s.resources, c.Resource) {
		return nil
	}
	s.logger.Warn(ctx, "a client is for an endpoint that this server does not serve", slog.F("client_id", c.ID), slog.F("resource", c.Resource))
	return &tokenError{
		status: http.StatusUnauthorized, code: "invalid_client",
		description: "the client is for " + c.Resource + ", which this server does not serve; an operator must make a client for this server",
	}
}

func checkResources(form url.Values, c Client) error {
	for _, resource := range form["resource"] {
		if resource != c.Resource {
			return badRequest("invalid_target", "the tokens of this client are for "+c.Resource)
		}
	}
	return nil
}

func (s *Server) mintServiceToken(ctx context.Context, c Client, scopes []faucet.Scope, now time.Time, lifetime time.Duration) (tokenResponse, error) {
	token, err := newSecret(accessTokenPrefix)
	if err != nil {
		return tokenResponse{}, err
	}
	if err := s.store.InsertAccessToken(ctx, AccessToken{
		Hash: HashToken(token), ExpiresAt: now.Add(lifetime), Scopes: scopes,
		Resource: c.Resource, SubjectKind: SubjectService, Subject: c.ID,
	}); err != nil {
		return tokenResponse{}, xerrors.Errorf("store the access token: %w", err)
	}
	names := make([]string, len(scopes))
	for i, scope := range scopes {
		names[i] = string(scope)
	}
	return tokenResponse{
		AccessToken: token, TokenType: "Bearer", ExpiresIn: int(lifetime / time.Second), Scope: strings.Join(names, " "),
	}, nil
}

// authenticateClient checks the client's ID and secret, which come only in
// HTTP Basic authentication (client_secret_basic), each URL-encoded first as
// RFC 6749 section 2.3.1 says. It fails before the grant changes anything.
func (s *Server) authenticateClient(ctx context.Context, r *http.Request, form url.Values) (Client, error) {
	if form.Has("client_secret") {
		return Client{}, badRequest("invalid_request", "send the client secret in the Authorization header only, as client_secret_basic")
	}
	rawID, rawSecret, ok := r.BasicAuth()
	if !ok {
		return Client{}, errClientAuth
	}
	id, idErr := url.QueryUnescape(rawID)
	secret, secretErr := url.QueryUnescape(rawSecret)
	if idErr != nil || secretErr != nil {
		return Client{}, errClientAuth
	}
	if form.Has("client_id") && form.Get("client_id") != id {
		return Client{}, badRequest("invalid_request", "client_id differs from the client of the Authorization header")
	}
	// An ID that no client has never reaches the store, which may not be
	// able to compare it at all.
	if ValidateClientID(id) != nil {
		return Client{}, errClientAuth
	}
	c, err := s.store.Client(ctx, id)
	switch {
	case xerrors.Is(err, ErrUnknownClient):
		return Client{}, errClientAuth
	case err != nil:
		return Client{}, xerrors.Errorf("read the client: %w", err)
	}
	if c.SecretHash == nil {
		return Client{}, errClientAuth
	}
	hash := HashToken(secret)
	if subtle.ConstantTimeCompare(hash[:], c.SecretHash[:]) != 1 || !c.Live() {
		return Client{}, errClientAuth
	}
	return c, nil
}

// grantedScopes returns the scopes that the request asks for and the client
// holds, in a fixed order. A request that names no scope asks for all of
// the client's.
func grantedScopes(form url.Values, held []faucet.Scope) ([]faucet.Scope, error) {
	requested := strings.Fields(form.Get("scope"))
	var granted []faucet.Scope
	for _, s := range faucet.AllScopes() {
		if slices.Contains(held, s) && (len(requested) == 0 || slices.Contains(requested, string(s))) {
			granted = append(granted, s)
		}
	}
	if len(granted) == 0 {
		return nil, badRequest("invalid_scope", "the client holds none of the scopes asked for")
	}
	return granted, nil
}

type protectedResourceMetadata struct {
	Resource             string   `json:"resource"`
	AuthorizationServers []string `json:"authorization_servers"`
	ScopesSupported      []string `json:"scopes_supported"`
}

// ProtectedResourceHandler serves the metadata (RFC 9728) of the endpoint
// resource, whose authorization server is issuer, as a discovery document.
func ProtectedResourceHandler(resource, issuer string) http.Handler {
	m := protectedResourceMetadata{Resource: resource, AuthorizationServers: []string{issuer}}
	for _, s := range faucet.AllScopes() {
		m.ScopesSupported = append(m.ScopesSupported, string(s))
	}
	return document(m)
}

// corsAllowHeaders are the request headers that a browser client may send
// for a discovery document. An MCP client sends MCP-Protocol-Version with
// every request.
const corsAllowHeaders = "Content-Type, MCP-Protocol-Version"

// document serves v as JSON to GET and HEAD from any origin, since a
// discovery document is public, and answers a CORS preflight.
func document(v any) http.Handler {
	body, err := json.Marshal(v)
	if err != nil {
		panic(xerrors.Errorf("encode a discovery document: %w", err))
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Access-Control-Allow-Origin", "*")
		switch r.Method {
		case http.MethodGet, http.MethodHead:
			h.Set("Content-Type", "application/json")
			_, _ = w.Write(body)
		case http.MethodOptions:
			h.Set("Access-Control-Allow-Methods", "GET, HEAD, OPTIONS")
			h.Set("Access-Control-Allow-Headers", corsAllowHeaders)
			w.WriteHeader(http.StatusNoContent)
		default:
			h.Set("Allow", "GET, HEAD, OPTIONS")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
}
