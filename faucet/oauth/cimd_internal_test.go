package oauth

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/coder/quartz"
	"github.com/coder/superblink-events/faucet"
)

// documentServer serves client ID metadata documents over TLS on loopback
// and counts the requests that reach it.
type documentServer struct {
	*httptest.Server
	requests atomic.Int64
}

func newDocumentServer(t *testing.T, handler http.HandlerFunc) *documentServer {
	t.Helper()
	s := &documentServer{}
	s.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.requests.Add(1)
		handler(w, r)
	}))
	t.Cleanup(s.Close)
	return s
}

// client returns the guarded client of faucet, trusting the server's
// certificate.
func (s *documentServer) client(allowLoopback bool) *http.Client {
	pool := x509.NewCertPool()
	pool.AddCert(s.Certificate())
	return faucet.NewHTTPClient(faucet.HTTPClientOptions{AllowInsecureLoopback: allowLoopback, RootCAs: pool})
}

func (s *documentServer) fetch(clientID string) (clientDocument, error) {
	return newDocumentFetcher(s.client(true), quartz.NewReal()).fetch(context.Background(), clientID)
}

// requestedURL is the URL that r asked for, which is what its document must
// name as client_id.
func requestedURL(r *http.Request) string { return "https://" + r.Host + r.URL.RequestURI() }

// documentFor returns a valid document for clientID, with members added or
// replaced. A member set to nil is left out.
func documentFor(clientID string, members map[string]any) string {
	doc := map[string]any{
		"client_id":     clientID,
		"client_name":   "Visual Studio Code",
		"redirect_uris": []string{"http://127.0.0.1/", "https://vscode.dev/redirect"},
	}
	for name, value := range members {
		if value == nil {
			delete(doc, name)
			continue
		}
		doc[name] = value
	}
	b, err := json.Marshal(doc)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// serve answers every request with status 200, contentType and the body that
// body returns for the request. An empty contentType sends no Content-Type.
func serve(contentType string, body func(r *http.Request) string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header()["Content-Type"] = nil
		if contentType != "" {
			w.Header().Set("Content-Type", contentType)
		}
		_, _ = io.WriteString(w, body(r))
	}
}

func validDocument(r *http.Request) string { return documentFor(requestedURL(r), nil) }

// VS Code serves its document as application/json; charset=utf-8 (review
// finding R15).
func TestADocumentWithACharsetParameterIsRead(t *testing.T) {
	t.Parallel()

	s := newDocumentServer(t, serve("application/json; charset=utf-8", validDocument))
	clientID := s.URL + "/oauth/client.json"
	doc, err := s.fetch(clientID)
	require.NoError(t, err)
	require.Equal(t, clientDocument{
		ClientID:     clientID,
		ClientName:   "Visual Studio Code",
		RedirectURIs: []string{"http://127.0.0.1/", "https://vscode.dev/redirect"},
	}, doc)
}

func requireRefused(t *testing.T, doc clientDocument, err error, name string) {
	t.Helper()
	require.ErrorIs(t, err, errClientDocument, name)
	require.Equal(t, clientDocument{}, doc, name)
}

// documentCase is a document that a test serves at /doc/<index>.json.
type documentCase struct {
	name string
	// members change the valid document, as documentFor does.
	members map[string]any
}

// serveCases serves each case at its own path, so one server answers a
// whole table.
func serveCases(t *testing.T, cases []documentCase) *documentServer {
	t.Helper()
	return newDocumentServer(t, serve("application/json", func(r *http.Request) string {
		var i int
		if _, err := fmt.Sscanf(r.URL.Path, "/doc/%d.json", &i); err != nil || i >= len(cases) {
			return "{}"
		}
		return documentFor(requestedURL(r), cases[i].members)
	}))
}

func caseURL(s *documentServer, i int) string { return fmt.Sprintf("%s/doc/%d.json", s.URL, i) }

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestFetchAsksForJSONWithAGet(t *testing.T) {
	t.Parallel()

	var method, accept atomic.Value
	s := newDocumentServer(t, func(w http.ResponseWriter, r *http.Request) {
		method.Store(r.Method)
		accept.Store(r.Header.Get("Accept"))
		serve("application/json", validDocument)(w, r)
	})
	_, err := s.fetch(s.URL + "/client.json")
	require.NoError(t, err)
	require.Equal(t, http.MethodGet, method.Load())
	require.Equal(t, "application/json", accept.Load())
}

func TestADocumentThatIsNotServedAsJSONIsRefused(t *testing.T) {
	t.Parallel()

	for _, contentType := range []string{"text/html", "", "text/plain; charset=utf-8", "application/jsonp", "application/json; charset"} {
		s := newDocumentServer(t, serve(contentType, validDocument))
		doc, err := s.fetch(s.URL + "/client.json")
		requireRefused(t, doc, err, contentType)
	}
	for _, contentType := range []string{"application/json", "Application/JSON", "application/json;charset=UTF-8", "application/json;"} {
		s := newDocumentServer(t, serve(contentType, validDocument))
		_, err := s.fetch(s.URL + "/client.json")
		require.NoError(t, err, contentType)
	}
}

func TestADocumentOfAnotherStatusIsRefused(t *testing.T) {
	t.Parallel()

	for _, status := range []int{http.StatusCreated, http.StatusNotFound, http.StatusInternalServerError} {
		s := newDocumentServer(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = io.WriteString(w, validDocument(r))
		})
		doc, err := s.fetch(s.URL + "/client.json")
		requireRefused(t, doc, err, http.StatusText(status))
	}
}

// padTo pads a JSON document with trailing white space, which JSON allows,
// to exactly n bytes.
func padTo(doc string, n int) string { return doc + strings.Repeat(" ", n-len(doc)) }

func TestADocumentOf5120BytesIsReadAndOneByteMoreIsRefused(t *testing.T) {
	t.Parallel()

	s := newDocumentServer(t, serve("application/json", func(r *http.Request) string {
		n := 5120
		if r.URL.Path == "/long.json" {
			n++
		}
		return padTo(validDocument(r), n)
	}))
	_, err := s.fetch(s.URL + "/client.json")
	require.NoError(t, err)
	doc, err := s.fetch(s.URL + "/long.json")
	requireRefused(t, doc, err, "5121 bytes")
}

func TestASlowDocumentIsCutAtTheDeadline(t *testing.T) {
	t.Parallel()

	s := newDocumentServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"client_id": `)
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-time.After(30 * time.Second):
		}
	})
	f := newDocumentFetcher(s.client(true), quartz.NewReal())
	f.timeout = 100 * time.Millisecond
	start := time.Now()
	doc, err := f.fetch(context.Background(), s.URL+"/client.json")
	requireRefused(t, doc, err, "a body that never ends")
	require.Less(t, time.Since(start), 2*time.Second, "the deadline covers the body, not only the headers")
}

func TestARedirectIsNotFollowed(t *testing.T) {
	t.Parallel()

	var moved atomic.Bool
	s := newDocumentServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/moved.json" {
			moved.Store(true)
			serve("application/json", validDocument)(w, r)
			return
		}
		http.Redirect(w, r, "/moved.json", http.StatusFound)
	})
	doc, err := s.fetch(s.URL + "/client.json")
	requireRefused(t, doc, err, "a redirect")
	require.False(t, moved.Load())
}

func TestAClientIDThatIsNotAPublicHTTPSAddressIsNeverFetched(t *testing.T) {
	t.Parallel()

	var plainRequests atomic.Int64
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		plainRequests.Add(1)
		serve("application/json", func(r *http.Request) string { return documentFor("http://"+r.Host+r.URL.RequestURI(), nil) })(w, r)
	}))
	t.Cleanup(plain.Close)
	s := newDocumentServer(t, serve("application/json", validDocument))

	doc, err := newDocumentFetcher(s.client(true), quartz.NewReal()).fetch(context.Background(), plain.URL+"/client.json")
	requireRefused(t, doc, err, "http, even through a client that may reach loopback over http")
	require.Zero(t, plainRequests.Load())

	doc, err = newDocumentFetcher(s.client(false), quartz.NewReal()).fetch(context.Background(), s.URL+"/client.json")
	requireRefused(t, doc, err, "a loopback host through the production client")
	require.Zero(t, s.requests.Load())
}

func TestAMalformedClientIDIsRefusedBeforeAnyRequest(t *testing.T) {
	t.Parallel()

	var requests atomic.Int64
	f := newDocumentFetcher(&http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		requests.Add(1)
		return nil, io.ErrUnexpectedEOF
	})}, quartz.NewReal())
	for _, clientID := range []string{
		"",
		"client.json",
		"http://app.example.com/client.json",
		"ftp://app.example.com/client.json",
		"https://app.example.com",
		"https://app.example.com/",
		"https:app.example.com/client.json",
		"https:///client.json",
		"https://:443/client.json",
		"https://app.example.com/client.json#main",
		"https://app.example.com/client.json#",
		"https://user@app.example.com/client.json",
		"https://user:pass@app.example.com/client.json",
		"https://@app.example.com/client.json",
		"https://app.example.com/./client.json",
		"https://app.example.com/oauth/../client.json",
		"https://app.example.com/oauth/..",
		"https://app.example.com/%2e%2e/client.json",
		"https://app.example.com/%zz",
		"https://app.example.com/" + strings.Repeat("a", 2048-len("https://app.example.com/")+1),
	} {
		doc, err := f.fetch(context.Background(), clientID)
		requireRefused(t, doc, err, clientID)
	}
	require.Zero(t, requests.Load())
}

func TestAClientIDOf2048BytesIsFetched(t *testing.T) {
	t.Parallel()

	s := newDocumentServer(t, serve("application/json", validDocument))
	clientID := s.URL + "/" + strings.Repeat("a", 2048-len(s.URL)-1)
	require.Len(t, clientID, 2048)
	doc, err := s.fetch(clientID)
	require.NoError(t, err)
	require.Equal(t, clientID, doc.ClientID)
}

func TestAClientIDWithAQueryIsFetched(t *testing.T) {
	t.Parallel()

	s := newDocumentServer(t, serve("application/json", validDocument))
	doc, err := s.fetch(s.URL + "/client.json?v=2")
	require.NoError(t, err)
	require.Equal(t, s.URL+"/client.json?v=2", doc.ClientID)
}

func TestADocumentThatNamesAnotherClientIsRefused(t *testing.T) {
	t.Parallel()

	s := newDocumentServer(t, serve("application/json", func(r *http.Request) string {
		id := requestedURL(r)
		served := map[string]string{
			"/slash.json": id + "/",
			"/upper.json": strings.Replace(id, "https://", "HTTPS://", 1),
			"/port.json":  strings.Replace(id, "127.0.0.1:", "127.0.0.1:1", 1),
			"/other.json": "https://evil.example/client.json",
			"/empty.json": "",
		}[r.URL.Path]
		return documentFor(served, nil)
	}))
	for _, path := range []string{"/slash.json", "/upper.json", "/port.json", "/other.json", "/empty.json"} {
		doc, err := s.fetch(s.URL + path)
		requireRefused(t, doc, err, path)
	}
	s = newDocumentServer(t, serve("application/json", func(r *http.Request) string {
		return documentFor(requestedURL(r), map[string]any{"client_id": nil})
	}))
	doc, err := s.fetch(s.URL + "/client.json")
	requireRefused(t, doc, err, "no client_id")
}

func TestADocumentThatIsNotOneJSONObjectIsRefusedWithoutQuotingIt(t *testing.T) {
	t.Parallel()

	const marker = "zq7-marker"
	bodies := []func(id string) string{
		func(string) string { return "not json " + marker },
		func(string) string { return `["` + marker + `"]` },
		func(string) string { return "null" },
		func(string) string { return `"` + marker + `"` },
		func(id string) string { return documentFor(id, nil) + ` {"` + marker + `": 1}` },
		func(id string) string { return documentFor(id, nil) + documentFor(id, nil) },
		func(id string) string { return documentFor(id, map[string]any{"client_name": 7}) },
		func(id string) string {
			return documentFor(id, map[string]any{"redirect_uris": "https://" + marker + "/cb"})
		},
		func(id string) string {
			return documentFor(id, map[string]any{"redirect_uris": []any{"https://app.example.com/cb", 7}})
		},
		func(id string) string {
			return documentFor(id, map[string]any{"token_endpoint_auth_method": []string{"none"}})
		},
		func(id string) string { return `{"client_id": "` + id + `", "redirect_uris": ["` + marker },
	}
	s := newDocumentServer(t, serve("application/json", func(r *http.Request) string {
		var i int
		_, _ = fmt.Sscanf(r.URL.Path, "/doc/%d.json", &i)
		return bodies[i](requestedURL(r))
	}))
	for i := range bodies {
		doc, err := s.fetch(caseURL(s, i))
		requireRefused(t, doc, err, fmt.Sprint(i))
		require.NotContains(t, err.Error(), marker, i)
	}
}

func TestADocumentWithoutGoodRedirectURIsIsRefused(t *testing.T) {
	t.Parallel()

	many := make([]string, 33)
	for i := range many {
		many[i] = fmt.Sprintf("https://app.example.com/cb/%d", i)
	}
	var cases []documentCase
	add := func(name string, uris any) {
		cases = append(cases, documentCase{name: name, members: map[string]any{"redirect_uris": uris}})
	}
	add("no redirect_uris", nil)
	add("an empty list", []string{})
	add("33 entries", many)
	for _, uri := range []string{
		"",
		"/callback",
		"app.example.com/cb",
		"https://app.example.com/cb#done",
		"https://app.example.com/cb#",
		"https://user@app.example.com/cb",
		"https:///cb",
		"https:cb",
		"https://app.example.com/%zz",
		"https://app.example.com/" + strings.Repeat("a", 2048-len("https://app.example.com/")+1),
		"http://app.example.com/cb",
		"http://127.0.0.1.evil.example/cb",
		"http://localhost.evil.example/cb",
		"http://127.0.0.2/cb",
		"http://LOCALHOST/cb",
		"http://[::1%25lo]/cb",
		"http:/cb",
		"javascript:alert(document.domain)",
		"JavaScript:alert(document.domain)",
		"data:text/html,<script>alert(1)</script>",
		"vbscript:msgbox",
		"file:///etc/passwd",
		"blob:https://app.example.com/0f6d6b4e",
		"about:blank",
		"ftp://app.example.com/cb",
		"ws://127.0.0.1/cb",
		"wss://app.example.com/cb",
	} {
		add(uri, []string{"https://app.example.com/good", uri})
	}
	s := serveCases(t, cases)
	for i, c := range cases {
		doc, err := s.fetch(caseURL(s, i))
		requireRefused(t, doc, err, c.name)
		if c.name != "" {
			require.NotContains(t, err.Error(), c.name, "the error names the entry by its index")
		}
	}
}

func TestEveryKindOfRedirectURIIsTaken(t *testing.T) {
	t.Parallel()

	uris := []string{
		"https://vscode.dev/redirect",
		"https://app.example.com:8443/cb?tenant=1",
		"http://127.0.0.1/",
		"http://127.0.0.1:33418/callback",
		"http://[::1]:33418/callback",
		"http://localhost:8080/cb",
		"vscode://vscode.github-authentication/did-authenticate",
		"com.example.app:/oauth2redirect/example-provider",
		"cursor://anysphere.cursor-retrieval/oauth/user/callback",
		"https://app.example.com/" + strings.Repeat("a", 2048-len("https://app.example.com/")),
	}
	full := make([]string, 32)
	for i := range full {
		full[i] = fmt.Sprintf("https://app.example.com/cb/%d", i)
	}
	cases := []documentCase{{name: "every kind", members: map[string]any{"redirect_uris": uris}}, {name: "32 entries", members: map[string]any{"redirect_uris": full}}}
	s := serveCases(t, cases)
	doc, err := s.fetch(caseURL(s, 0))
	require.NoError(t, err)
	require.Equal(t, uris, doc.RedirectURIs)
	doc, err = s.fetch(caseURL(s, 1))
	require.NoError(t, err)
	require.Len(t, doc.RedirectURIs, 32)
}

func TestTheClientNameIsShortAndHoldsNoControlCharacter(t *testing.T) {
	t.Parallel()

	refused := []documentCase{
		{name: "257 bytes", members: map[string]any{"client_name": strings.Repeat("a", 257)}},
		{name: "a newline", members: map[string]any{"client_name": "VS Code\nAllow"}},
		{name: "a NUL", members: map[string]any{"client_name": "VS Code\x00"}},
		{name: "DEL", members: map[string]any{"client_name": "VS Code\x7f"}},
		{name: "a C1 control", members: map[string]any{"client_name": "VS Code\u0085"}},
		{name: "a right-to-left override", members: map[string]any{"client_name": "VS Code \u202eedoC duolC"}},
		{name: "a zero-width joiner", members: map[string]any{"client_name": "VS\u200dCode"}},
	}
	taken := []documentCase{
		{name: "256 bytes", members: map[string]any{"client_name": strings.Repeat("a", 256)}},
		{name: "no name", members: map[string]any{"client_name": nil}},
		{name: "letters beyond ASCII", members: map[string]any{"client_name": "Visual Studio Code – Insiders ✓"}},
	}
	s := serveCases(t, append(refused, taken...))
	for i, c := range refused {
		doc, err := s.fetch(caseURL(s, i))
		requireRefused(t, doc, err, c.name)
	}
	for i, c := range taken {
		_, err := s.fetch(caseURL(s, len(refused)+i))
		require.NoError(t, err, c.name)
	}
}

func TestAClientThatAuthenticatesAtTheTokenEndpointIsRefused(t *testing.T) {
	t.Parallel()

	refused := []documentCase{
		{name: "client_secret_basic", members: map[string]any{"token_endpoint_auth_method": "client_secret_basic"}},
		{name: "private_key_jwt", members: map[string]any{"token_endpoint_auth_method": "private_key_jwt"}},
		{name: "an empty method", members: map[string]any{"token_endpoint_auth_method": ""}},
		{name: "a client secret", members: map[string]any{"client_secret": "s3cret"}},
		{name: "an expiry of a client secret", members: map[string]any{"client_secret_expires_at": 0}},
	}
	taken := []documentCase{
		{name: "none", members: map[string]any{"token_endpoint_auth_method": "none"}},
		{name: "no method", members: nil},
	}
	s := serveCases(t, append(refused, taken...))
	for i, c := range refused {
		doc, err := s.fetch(caseURL(s, i))
		requireRefused(t, doc, err, c.name)
	}
	for i, c := range taken {
		_, err := s.fetch(caseURL(s, len(refused)+i))
		require.NoError(t, err, c.name)
	}
}

func TestADocumentIgnoresMembersItDoesNotUse(t *testing.T) {
	t.Parallel()

	s := serveCases(t, []documentCase{{name: "extra members", members: map[string]any{
		"logo_uri": "https://app.example.com/logo.png", "grant_types": []string{"authorization_code", "refresh_token"},
		"response_types": []string{"code"}, "software_id": 7,
	}}})
	_, err := s.fetch(caseURL(s, 0))
	require.NoError(t, err)
}

func TestAllowsARedirectURIOfTheDocumentOrALoopbackOneOnAnyPort(t *testing.T) {
	t.Parallel()

	doc := clientDocument{RedirectURIs: []string{
		"https://vscode.dev/redirect",
		"http://127.0.0.1/callback",
		"http://localhost:8080/cb?flow=web",
		"http://[::1]/",
		"vscode://vscode.github-authentication/did-authenticate",
	}}
	for uri, want := range map[string]bool{
		"https://vscode.dev/redirect":                              true,
		"https://vscode.dev:443/redirect":                          false,
		"https://vscode.dev/redirect/":                             false,
		"https://VSCODE.dev/redirect":                              false,
		"vscode://vscode.github-authentication/did-authenticate":   true,
		"vscode://vscode.github-authentication/did-authenticate?x": false,
		"http://127.0.0.1/callback":                                true,
		"http://127.0.0.1:53111/callback":                          true,
		"http://127.0.0.1:1/callback":                              true,
		"http://127.0.0.1:65535/callback":                          true,
		"http://127.0.0.1:0/callback":                              false,
		"http://127.0.0.1:65536/callback":                          false,
		"http://127.0.0.1:/callback":                               true,
		"http://127.0.0.1:53111/callback/":                         false,
		"http://127.0.0.1:53111/Callback":                          false,
		"http://127.0.0.1:53111/%63allback":                        false,
		"http://127.0.0.1:53111/callback?x=1":                      false,
		"http://127.0.0.1:53111/callback?":                         false,
		"http://127.0.0.1:53111/callback#x":                        false,
		"http://user@127.0.0.1:53111/callback":                     false,
		"https://127.0.0.1:53111/callback":                         false,
		"HTTP://127.0.0.1:53111/callback":                          false,
		"http://localhost:53111/callback":                          false,
		"http://[::1]:53111/callback":                              false,
		"http://127.0.0.1.evil.example:53111/callback":             false,
		"http://127.0.0.1.evil.example/callback":                   false,
		"http://localhost:8080/cb?flow=web":                        true,
		"http://localhost/cb?flow=web":                             true,
		"http://localhost:9/cb?flow=web":                           true,
		"http://localhost:9/cb?flow=app":                           false,
		"http://127.0.0.1:9/cb?flow=web":                           false,
		"http://[::1]:9/":                                          true,
		"http://[::1]:9":                                           false,
		"http://[::1%25lo]:9/":                                     false,
		"":                                                         false,
	} {
		require.Equal(t, want, doc.allows(uri), uri)
	}
}
