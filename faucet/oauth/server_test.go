package oauth_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/clientcredentials"

	"cdr.dev/slog/v3"
	"cdr.dev/slog/v3/sloggers/sloghuman"
	"github.com/coder/quartz"
	"github.com/coder/superblink-events/faucet"
	"github.com/coder/superblink-events/faucet/oauth"
)

const issuer = "https://faucet.example.com"

type tokenHarness struct {
	t      *testing.T
	store  *fakeStore
	clock  *quartz.Mock
	srv    *httptest.Server
	server *oauth.Server
	secret string
	logs   *loginLogs
}

// newTokenHarness serves the authorization server over a store that holds
// the live client ci-watcher, whose tokens are for resource and which holds
// both scopes.
func newTokenHarness(t *testing.T) *tokenHarness {
	t.Helper()
	store := newFakeStore()
	clock := quartz.NewMock(t)
	c, secret, err := oauth.NewClient("ci-watcher", resource, bothScopes, []string{"repo:coder/faucet"}, clock.Now())
	require.NoError(t, err)
	require.NoError(t, store.InsertClient(context.Background(), c))
	logs := &loginLogs{}
	server, err := oauth.NewServer(oauth.ServerDeps{Store: store, Clock: clock, Logger: slog.Make(sloghuman.Sink(logs)), Issuer: issuer, Resources: []string{resource, issuer + "/mcp"}})
	require.NoError(t, err)
	mux := http.NewServeMux()
	mux.Handle("GET "+oauth.MetadataPath, server.MetadataHandler())
	mux.Handle("POST "+oauth.TokenPath, server.TokenHandler())
	mux.Handle(oauth.AuthorizePath, server.AuthorizeHandler())
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &tokenHarness{t: t, store: store, clock: clock, srv: srv, server: server, secret: secret, logs: logs}
}

type tokenReply struct {
	status  int
	header  http.Header
	body    map[string]any
	rawBody string
}

// post sends form to the token endpoint with authorization as the
// Authorization header, none when it is empty.
func (h *tokenHarness) post(authorization string, form url.Values) tokenReply {
	h.t.Helper()
	if authorization == "" {
		return h.postWith(nil, form)
	}
	return h.postWith([]string{authorization}, form)
}

func (h *tokenHarness) postWith(authorization []string, form url.Values) tokenReply {
	h.t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, h.srv.URL+oauth.TokenPath, strings.NewReader(form.Encode()))
	require.NoError(h.t, err)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header["Authorization"] = authorization
	resp, err := h.srv.Client().Do(req)
	require.NoError(h.t, err)
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(h.t, err)
	reply := tokenReply{status: resp.StatusCode, header: resp.Header, rawBody: string(raw)}
	require.NoError(h.t, json.Unmarshal(raw, &reply.body), "%s", raw)
	return reply
}

func basic(id, secret string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(url.QueryEscape(id)+":"+url.QueryEscape(secret)))
}

func clientCredentials(extra ...string) url.Values {
	form := url.Values{"grant_type": {"client_credentials"}}
	for i := 0; i < len(extra); i += 2 {
		form.Add(extra[i], extra[i+1])
	}
	return form
}

func (h *tokenHarness) verify(token, endpoint string) (faucet.Principal, error) {
	h.t.Helper()
	v, err := oauth.NewVerifier(h.store, h.clock, map[oauth.SubjectKind]oauth.SubjectResolver{oauth.SubjectService: oauth.ServiceResolver(h.store)})
	require.NoError(h.t, err)
	return v.Verify(context.Background(), token, endpoint)
}

func TestTheMetadataNamesTheGrantTable(t *testing.T) {
	t.Parallel()
	h := newTokenHarness(t)

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, h.srv.URL+oauth.MetadataPath, nil)
	require.NoError(t, err)
	resp, err := h.srv.Client().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "*", resp.Header.Get("Access-Control-Allow-Origin"))
	var got map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&got))
	require.Equal(t, map[string]any{
		"issuer":                                     issuer,
		"authorization_endpoint":                     issuer + "/oauth/authorize",
		"token_endpoint":                             issuer + "/oauth/token",
		"scopes_supported":                           []any{"events:read", "events:subscribe"},
		"response_types_supported":                   []any{},
		"grant_types_supported":                      []any{"client_credentials", "urn:ietf:params:oauth:grant-type:token-exchange"},
		"token_endpoint_auth_methods_supported":      []any{"client_secret_basic", "none"},
		"code_challenge_methods_supported":           []any{"S256"},
		"revocation_endpoint":                        issuer + "/oauth/revoke",
		"revocation_endpoint_auth_methods_supported": []any{"client_secret_basic"},
	}, got, "a workload sends no secret, so the revocation endpoint takes none of its clients")
}

// go-sdk's client reads the metadata with these checks before it asks for a
// token.
func TestGoSDKAcceptsTheMetadata(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	var srv *httptest.Server
	mux := http.NewServeMux()
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	server, err := oauth.NewServer(oauth.ServerDeps{Store: store, Clock: quartz.NewMock(t), Logger: slog.Make(), Issuer: srv.URL, Resources: []string{srv.URL + "/github/mcp"}})
	require.NoError(t, err)
	mux.Handle("GET "+oauth.MetadataPath, server.MetadataHandler())

	meta, err := oauthex.GetAuthServerMeta(context.Background(), srv.URL+oauth.MetadataPath, srv.URL, srv.Client())
	require.NoError(t, err)
	require.Equal(t, srv.URL+oauth.TokenPath, meta.TokenEndpoint)
}

func TestClientCredentialsIssuesAShortAccessTokenForTheClientsResource(t *testing.T) {
	t.Parallel()
	h := newTokenHarness(t)

	reply := h.post(basic("ci-watcher", h.secret), clientCredentials("scope", "events:read events:subscribe"))
	require.Equal(t, http.StatusOK, reply.status, reply.rawBody)
	require.Equal(t, "no-store", reply.header.Get("Cache-Control"))
	token, _ := reply.body["access_token"].(string)
	require.Regexp(t, `^fct_[A-Za-z0-9_-]{43}$`, token)
	require.Equal(t, map[string]any{"access_token": token, "token_type": "Bearer", "expires_in": float64(900), "scope": "events:read events:subscribe"}, reply.body,
		"no refresh token: the service asks again with its secret")

	stored, err := h.store.AccessToken(context.Background(), oauth.HashToken(token))
	require.NoError(t, err)
	require.Equal(t, oauth.AccessToken{
		Hash: oauth.HashToken(token), ExpiresAt: h.clock.Now().Add(15 * time.Minute), Scopes: bothScopes,
		Resource: resource, SubjectKind: oauth.SubjectService, Subject: "ci-watcher",
	}, stored, "the store keeps the hash, never the token")

	p, err := h.verify(token, resource)
	require.NoError(t, err)
	require.Equal(t, "svc:ci-watcher", p.Login)
	client, ok := p.ServiceClient()
	require.True(t, ok)
	require.Equal(t, []string{"repo:coder/faucet"}, client.Grants)
	_, err = h.verify(token, issuer+"/mcp")
	require.ErrorIs(t, err, faucet.ErrInvalidToken, "a token is for its resource only")
}

// go-sdk's client credentials handler sends no resource and asks for every
// scope of the 401 challenge (R4). Faucet gives such a request the client's
// resource and the scopes that the client holds.
func TestClientCredentialsGrantsTheScopesTheClientHoldsAndItsResource(t *testing.T) {
	t.Parallel()
	h := newTokenHarness(t)
	reader, secret, err := oauth.NewClient("reader", resource, []faucet.Scope{faucet.ScopeEventsRead}, []string{"repo:coder/faucet"}, h.clock.Now())
	require.NoError(t, err)
	require.NoError(t, h.store.InsertClient(context.Background(), reader))

	for name, c := range map[string]struct {
		form  url.Values
		scope string
	}{
		"every scope asked for": {clientCredentials("scope", "events:read events:subscribe"), "events:read"},
		"no scope asked for":    {clientCredentials(), "events:read"},
		"an empty scope":        {clientCredentials("scope", ""), "events:read"},
		"an unknown scope too":  {clientCredentials("scope", "events:read repo"), "events:read"},
		"its own resource":      {clientCredentials("resource", resource, "resource", resource), "events:read"},
	} {
		reply := h.post(basic("reader", secret), c.form)
		require.Equal(t, http.StatusOK, reply.status, "%s: %s", name, reply.rawBody)
		require.Equal(t, c.scope, reply.body["scope"], name)
		stored, err := h.store.AccessToken(context.Background(), oauth.HashToken(reply.body["access_token"].(string)))
		require.NoError(t, err)
		require.Equal(t, []faucet.Scope{faucet.ScopeEventsRead}, stored.Scopes, name)
		require.Equal(t, resource, stored.Resource, name)
	}

	reply := h.post(basic("reader", secret), clientCredentials("scope", "events:subscribe"))
	require.Equal(t, http.StatusBadRequest, reply.status)
	require.Equal(t, "invalid_scope", reply.body["error"])

	for _, other := range [][]string{{"resource", issuer + "/mcp"}, {"resource", resource, "resource", issuer + "/mcp"}, {"resource", resource + "/"}} {
		reply := h.post(basic("reader", secret), clientCredentials(other...))
		require.Equal(t, http.StatusBadRequest, reply.status, "%v", other)
		require.Equal(t, "invalid_target", reply.body["error"], "%v", other)
	}
	require.Equal(t, 5, h.store.tokenCount(), "a refused request stores no token")
}

// HTTP Basic authentication carries the client ID and secret URL-encoded
// (RFC 6749 section 2.3.1), and faucet decodes them (R6). golang.org/x/oauth2,
// which go-sdk uses, encodes them so.
func TestClientCredentialsDecodesTheBasicCredentials(t *testing.T) {
	t.Parallel()
	h := newTokenHarness(t)

	encoded := strings.ReplaceAll(url.QueryEscape(h.secret), "fcs_", "fcs%5F")
	header := "Basic " + base64.StdEncoding.EncodeToString([]byte("ci%2Dwatcher:"+encoded))
	reply := h.post(header, clientCredentials())
	require.Equal(t, http.StatusOK, reply.status, reply.rawBody)

	cfg := clientcredentials.Config{ClientID: "ci-watcher", ClientSecret: h.secret, TokenURL: h.srv.URL + oauth.TokenPath, AuthStyle: oauth2.AuthStyleInHeader}
	token, err := cfg.Token(context.WithValue(context.Background(), oauth2.HTTPClient, h.srv.Client()))
	require.NoError(t, err)
	_, err = h.verify(token.AccessToken, resource)
	require.NoError(t, err)

	reply = h.post("Basic "+base64.StdEncoding.EncodeToString([]byte("ci-watcher:%zz")), clientCredentials())
	require.Equal(t, http.StatusUnauthorized, reply.status, "a credential that is not URL-encoded right fails")
}

func TestClientCredentialsRefusesAClientThatDoesNotAuthenticate(t *testing.T) {
	t.Parallel()
	h := newTokenHarness(t)
	disabled, secret, err := oauth.NewClient("retired", resource, bothScopes, []string{"repo:coder/faucet"}, h.clock.Now())
	require.NoError(t, err)
	require.NoError(t, h.store.InsertClient(context.Background(), disabled))
	_, err = h.store.DisableClient(context.Background(), "retired", h.clock.Now())
	require.NoError(t, err)

	for name, authorization := range map[string]string{
		"no credentials":     "",
		"a wrong secret":     basic("ci-watcher", h.secret+"x"),
		"another's secret":   basic("ci-watcher", secret),
		"an unknown client":  basic("nobody", h.secret),
		"a disabled client":  basic("retired", secret),
		"a bearer":           "Bearer " + h.secret,
		"bad base64":         "Basic !!!",
		"no colon":           "Basic " + base64.StdEncoding.EncodeToString([]byte("ci-watcher")),
		"an empty secret":    basic("ci-watcher", ""),
		"a secret with case": basic("ci-watcher", strings.ToUpper(h.secret)),
	} {
		reply := h.post(authorization, clientCredentials())
		require.Equal(t, http.StatusUnauthorized, reply.status, name)
		require.Equal(t, map[string]any{"error": "invalid_client", "error_description": "client authentication failed"}, reply.body, name)
		require.Equal(t, `Basic realm="faucet"`, reply.header.Get("WWW-Authenticate"), name)
		require.NotContains(t, reply.rawBody, h.secret, name)
	}
	require.Zero(t, h.store.tokenCount())
}

func TestClientCredentialsRefusesAClientWithNoSecret(t *testing.T) {
	t.Parallel()
	h := newTokenHarness(t)
	workload, err := oauth.NewSecretlessClient("digest-bot", resource, bothScopes, []string{"repo:coder/faucet"}, h.clock.Now())
	require.NoError(t, err)
	require.NoError(t, h.store.InsertClient(context.Background(), workload))

	for name, secret := range map[string]string{
		"an empty secret":  "",
		"another's secret": h.secret,
		"a made-up secret": "fcs_" + strings.Repeat("A", 43),
	} {
		reply := h.post(basic("digest-bot", secret), clientCredentials())
		require.Equal(t, http.StatusUnauthorized, reply.status, name)
		require.Equal(t, map[string]any{"error": "invalid_client", "error_description": "client authentication failed"}, reply.body, name)
	}
	require.Zero(t, h.store.tokenCount())
}

func TestTheTokenEndpointRefusesAMalformedRequest(t *testing.T) {
	t.Parallel()
	h := newTokenHarness(t)
	auth := basic("ci-watcher", h.secret)

	for name, c := range map[string]struct {
		authorization string
		form          url.Values
		code          string
	}{
		"no grant type":         {auth, url.Values{}, "invalid_request"},
		"an unknown grant type": {auth, url.Values{"grant_type": {"password"}}, "unsupported_grant_type"},
		"authorization_code":    {auth, url.Values{"grant_type": {"authorization_code"}, "code": {"x"}}, "unsupported_grant_type"},
		"a repeated scope":      {auth, clientCredentials("scope", "events:read", "scope", "events:read"), "invalid_request"},
		"a repeated grant type": {auth, clientCredentials("grant_type", "client_credentials"), "invalid_request"},
		"a secret in the body":  {auth, clientCredentials("client_id", "ci-watcher", "client_secret", h.secret), "invalid_request"},
		"another client_id":     {auth, clientCredentials("client_id", "triage"), "invalid_request"},
	} {
		reply := h.post(c.authorization, c.form)
		require.Equal(t, http.StatusBadRequest, reply.status, name)
		require.Equal(t, c.code, reply.body["error"], name)
		require.NotContains(t, reply.rawBody, h.secret, name)
	}

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, h.srv.URL+oauth.TokenPath, strings.NewReader(`{"grant_type":"client_credentials"}`))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", auth)
	resp, err := h.srv.Client().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusBadRequest, resp.StatusCode, "the body is a form")
	require.Zero(t, h.store.tokenCount())
}

// A disabled client gets no new token, and the token it holds fails on its
// next request.
// A client made for another server's endpoint, as when an operator typed
// another origin or moved the server, gets no token it could not use, and
// hears why.
func TestClientCredentialsRefusesAClientForAnotherServer(t *testing.T) {
	t.Parallel()
	h := newTokenHarness(t)
	c, secret, err := oauth.NewClient("elsewhere", "https://other.example.com/github/mcp", bothScopes, []string{"repo:coder/faucet"}, h.clock.Now())
	require.NoError(t, err)
	require.NoError(t, h.store.InsertClient(context.Background(), c))

	reply := h.post(basic("elsewhere", secret), clientCredentials())
	require.Equal(t, http.StatusUnauthorized, reply.status, reply.rawBody)
	require.Equal(t, "invalid_client", reply.body["error"])
	require.Contains(t, reply.body["error_description"], "https://other.example.com/github/mcp, which this server does not serve")
	require.Zero(t, h.store.tokenCount())
}

func TestDisablingAClientFailsItsTokens(t *testing.T) {
	t.Parallel()
	h := newTokenHarness(t)
	reply := h.post(basic("ci-watcher", h.secret), clientCredentials())
	require.Equal(t, http.StatusOK, reply.status)
	token := reply.body["access_token"].(string)
	_, err := h.verify(token, resource)
	require.NoError(t, err)

	_, err = h.store.DisableClient(context.Background(), "ci-watcher", h.clock.Now())
	require.NoError(t, err)
	_, err = h.verify(token, resource)
	require.ErrorIs(t, err, faucet.ErrInvalidToken)
	require.ErrorContains(t, err, "disabled")
	require.Equal(t, http.StatusUnauthorized, h.post(basic("ci-watcher", h.secret), clientCredentials()).status)
}

func TestAnAccessTokenLastsFifteenMinutes(t *testing.T) {
	t.Parallel()
	h := newTokenHarness(t)
	token := h.post(basic("ci-watcher", h.secret), clientCredentials()).body["access_token"].(string)
	h.clock.Advance(15*time.Minute - time.Second)
	_, err := h.verify(token, resource)
	require.NoError(t, err)
	h.clock.Advance(time.Second)
	_, err = h.verify(token, resource)
	require.ErrorIs(t, err, faucet.ErrInvalidToken)
}

func TestNewServerNeedsABareIssuer(t *testing.T) {
	t.Parallel()
	resources := []string{resource}
	for _, d := range []oauth.ServerDeps{
		{Clock: quartz.NewMock(t), Issuer: issuer, Resources: resources},
		{Store: newFakeStore(), Issuer: issuer, Resources: resources},
		{Store: newFakeStore(), Clock: quartz.NewMock(t), Resources: resources},
		{Store: newFakeStore(), Clock: quartz.NewMock(t), Issuer: issuer + "/", Resources: resources},
		{Store: newFakeStore(), Clock: quartz.NewMock(t), Issuer: issuer},
	} {
		_, err := oauth.NewServer(d)
		require.Error(t, err)
	}
}

// No grant uses the authorization endpoint yet. It refuses every request in
// place and never redirects, since it knows no client's redirect URI.
func TestTheAuthorizationEndpointTakesNoResponseTypeYet(t *testing.T) {
	t.Parallel()
	h := newTokenHarness(t)
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		query := url.Values{"response_type": {"code"}, "client_id": {"https://agent.example.com/client.json"}, "redirect_uri": {"http://127.0.0.1:5555/cb"}, "state": {"s"}}
		req, err := http.NewRequestWithContext(context.Background(), method, h.srv.URL+oauth.AuthorizePath+"?"+query.Encode(), nil)
		require.NoError(t, err)
		client := h.srv.Client()
		client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		resp, err := client.Do(req)
		require.NoError(t, err)
		var body map[string]any
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
		_ = resp.Body.Close()
		require.Equal(t, http.StatusBadRequest, resp.StatusCode, method)
		require.Empty(t, resp.Header.Get("Location"))
		require.Equal(t, "unsupported_response_type", body["error"], method)
	}
}

// A client ID that no client can have is refused before the store is asked.
func TestAnImpossibleClientIDNeverReachesTheStore(t *testing.T) {
	t.Parallel()
	h := newTokenHarness(t)
	h.store.mu.Lock()
	h.store.err = errors.New("the store was asked")
	h.store.mu.Unlock()
	for _, credentials := range []string{"a%00b:secret", "%ff:secret", "CI:secret", ":secret", strings.Repeat("a", 65) + ":secret"} {
		reply := h.post("Basic "+base64.StdEncoding.EncodeToString([]byte(credentials)), clientCredentials())
		require.Equal(t, http.StatusUnauthorized, reply.status, "%s: %s", credentials, reply.rawBody)
		require.Equal(t, "invalid_client", reply.body["error"], credentials)
	}
}

func serve(h http.Handler, method, target string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), method, target, nil))
	return rec
}

// The token endpoint takes POST only, as RFC 6749 section 3.2 says, and the
// metadata is a document to GET, which a browser client may read across
// origins after a preflight that sends MCP-Protocol-Version.
func TestTheEndpointsAnswerOnlyTheirMethods(t *testing.T) {
	t.Parallel()
	server, err := oauth.NewServer(oauth.ServerDeps{Store: newFakeStore(), Clock: quartz.NewMock(t), Logger: slog.Make(), Issuer: issuer, Resources: []string{resource}})
	require.NoError(t, err)

	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodOptions} {
		rec := serve(server.TokenHandler(), method, oauth.TokenPath)
		require.Equal(t, http.StatusMethodNotAllowed, rec.Code, method)
		require.Equal(t, http.MethodPost, rec.Header().Get("Allow"), method)
	}

	metadata := server.MetadataHandler()
	require.Equal(t, http.StatusOK, serve(metadata, http.MethodHead, oauth.MetadataPath).Code)
	for _, method := range []string{http.MethodPost, http.MethodDelete} {
		rec := serve(metadata, method, oauth.MetadataPath)
		require.Equal(t, http.StatusMethodNotAllowed, rec.Code, method)
		require.Equal(t, "GET, HEAD, OPTIONS", rec.Header().Get("Allow"), method)
	}
	rec := serve(metadata, http.MethodOptions, oauth.MetadataPath)
	require.Equal(t, http.StatusNoContent, rec.Code)
	require.Equal(t, "*", rec.Header().Get("Access-Control-Allow-Origin"))
	require.Contains(t, rec.Header().Get("Access-Control-Allow-Headers"), "MCP-Protocol-Version")
	require.Empty(t, rec.Body.String())

	prm := oauth.ProtectedResourceHandler(resource, issuer)
	rec = serve(prm, http.MethodGet, "/.well-known/oauth-protected-resource/github/mcp")
	require.Equal(t, http.StatusOK, rec.Code)
	require.JSONEq(t, `{"resource":"`+resource+`","authorization_servers":["`+issuer+`"],"scopes_supported":["events:read","events:subscribe"]}`, rec.Body.String())
	rec = serve(prm, http.MethodOptions, "/.well-known/oauth-protected-resource/github/mcp")
	require.Equal(t, http.StatusNoContent, rec.Code)
	require.Contains(t, rec.Header().Get("Access-Control-Allow-Headers"), "MCP-Protocol-Version")
}
