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
	"strings"
	"time"

	"golang.org/x/xerrors"

	"cdr.dev/slog/v3"
	"github.com/coder/faucet/faucet"
	"github.com/coder/quartz"
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
	// responseType is what the grant asks the authorization endpoint for,
	// empty for a grant that does not use that endpoint.
	responseType string
	issue        func(s *Server, r *http.Request, form url.Values) (tokenResponse, error)
}

var grants = map[string]grant{
	"client_credentials": {authMethods: []string{"client_secret_basic"}, issue: (*Server).clientCredentials},
}

// loginGrants are the rows that the browser login adds to the grant table
// of a server.
var loginGrants = map[string]grant{
	"authorization_code": {authMethods: []string{"none"}, responseType: "code", issue: (*Server).authorizationCode},
	"refresh_token":      {authMethods: []string{"none"}, issue: (*Server).refreshToken},
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
	grants map[string]grant
	// login is nil when the browser login is off.
	login *browserLogin
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
	}
	if d.Login != nil {
		login, err := newBrowserLogin(*d.Login)
		if err != nil {
			return nil, xerrors.Errorf("the browser login: %w", err)
		}
		s.login = login
		maps.Copy(s.grants, loginGrants)
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
	ClientIDMetadataDocumentSupported          bool `json:"client_id_metadata_document_supported,omitempty"`
	AuthorizationResponseIssParameterSupported bool `json:"authorization_response_iss_parameter_supported,omitempty"`
}

// metadataOf describes the authorization server at issuer that takes the
// grants of table.
func metadataOf(issuer string, table map[string]grant) authServerMetadata {
	m := authServerMetadata{
		Issuer:                        issuer,
		AuthorizationEndpoint:         issuer + AuthorizePath,
		TokenEndpoint:                 issuer + TokenPath,
		ResponseTypesSupported:        []string{},
		GrantTypesSupported:           slices.Sorted(maps.Keys(table)),
		CodeChallengeMethodsSupported: []string{"S256"},
	}
	for _, s := range faucet.AllScopes() {
		m.ScopesSupported = append(m.ScopesSupported, string(s))
	}
	for _, g := range table {
		m.TokenEndpointAuthMethodsSupported = append(m.TokenEndpointAuthMethodsSupported, g.authMethods...)
		if g.responseType != "" {
			m.ResponseTypesSupported = append(m.ResponseTypesSupported, g.responseType)
		}
	}
	slices.Sort(m.TokenEndpointAuthMethodsSupported)
	m.TokenEndpointAuthMethodsSupported = slices.Compact(m.TokenEndpointAuthMethodsSupported)
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
	// ExpiredAuthRequests is how many authorization requests had expired
	// more than a day ago. A request stays for that day, so a code that is
	// used again still ends the session it made.
	ExpiredAuthRequests int64
}

// Purge deletes the state of the login server that is past its use: every
// access token that has expired, and every authorization request that
// expired more than a day ago.
func (s *Server) Purge(ctx context.Context) (PurgeResult, error) {
	now := s.clock.Now()
	tokens, err := s.store.DeleteExpiredAccessTokens(ctx, now)
	if err != nil {
		return PurgeResult{}, err
	}
	requests, err := s.store.DeleteExpiredAuthRequests(ctx, now.Add(-authRequestRetention))
	if err != nil {
		return PurgeResult{}, err
	}
	return PurgeResult{ExpiredAccessTokens: tokens, ExpiredAuthRequests: requests}, nil
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
		var te *tokenError
		switch {
		case xerrors.As(err, &te):
			s.logger.Info(r.Context(), "token request refused", slog.F("error", te.code), slog.F("reason", te.description))
			if te.status == http.StatusUnauthorized {
				w.Header().Set("WWW-Authenticate", `Basic realm="faucet"`)
			}
			writeJSON(w, te.status, map[string]string{"error": te.code, "error_description": te.description})
		case err != nil:
			s.logger.Warn(r.Context(), "could not issue a token", slog.Error(err))
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "server_error", "error_description": "could not issue a token, try again"})
		default:
			writeJSON(w, http.StatusOK, resp)
		}
	})
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

// readTokenForm reads the form in the body of a token request. Every
// parameter but resource, which RFC 8707 lets a client repeat, appears at
// most once (RFC 6749 section 3.2).
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
		if len(values) > 1 && name != "resource" {
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
	if !slices.Contains(s.resources, c.Resource) {
		s.logger.Warn(ctx, "a client is for an endpoint that this server does not serve", slog.F("client_id", c.ID), slog.F("resource", c.Resource))
		return tokenResponse{}, &tokenError{
			status: http.StatusUnauthorized, code: "invalid_client",
			description: "the client is for " + c.Resource + ", which this server does not serve; an operator must make a client for this server",
		}
	}
	scopes, err := grantedScopes(form, c.Scopes)
	if err != nil {
		return tokenResponse{}, err
	}
	for _, resource := range form["resource"] {
		if resource != c.Resource {
			return tokenResponse{}, badRequest("invalid_target", "the tokens of this client are for "+c.Resource)
		}
	}
	token, err := newSecret(accessTokenPrefix)
	if err != nil {
		return tokenResponse{}, err
	}
	if err := s.store.InsertAccessToken(ctx, AccessToken{
		Hash: HashToken(token), ExpiresAt: s.clock.Now().Add(accessTokenLifetime), Scopes: scopes,
		Resource: c.Resource, SubjectKind: SubjectService, Subject: c.ID,
	}); err != nil {
		return tokenResponse{}, xerrors.Errorf("store the access token: %w", err)
	}
	names := make([]string, len(scopes))
	for i, scope := range scopes {
		names[i] = string(scope)
	}
	s.logger.Debug(ctx, "token issued", slog.F("client_id", c.ID), slog.F("scope", names))
	return tokenResponse{
		AccessToken: token, TokenType: "Bearer", ExpiresIn: int(accessTokenLifetime / time.Second), Scope: strings.Join(names, " "),
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
