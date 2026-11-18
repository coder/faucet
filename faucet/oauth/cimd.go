package oauth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"golang.org/x/xerrors"

	"github.com/coder/quartz"
)

const (
	maxURIBytes        = 2048
	maxDocumentBytes   = 5 << 10
	maxRedirectURIs    = 32
	maxClientNameBytes = 256
	documentTimeout    = 3 * time.Second
	// maxCachedDocuments bounds how many client documents faucet keeps.
	maxCachedDocuments = 64
	// maxDocumentAge bounds how long faucet keeps a client document,
	// whatever its headers allow, so that a change to the document shows
	// within this time.
	maxDocumentAge = 5 * time.Minute
)

// errClientDocument is the error of a client ID, or of the client ID
// metadata document at it, that faucet refuses. An error that wraps it says
// what the client must change and never quotes the document.
var errClientDocument = xerrors.New("the client ID metadata document is refused")

// errDocumentUnreachable is the error of a document that faucet could not
// fetch. An error that wraps it holds the cause, which is for the log only.
var errDocumentUnreachable = xerrors.New("faucet could not fetch the client's document")

var (
	// refusedSchemes are the schemes that a redirect URI must not use: a
	// browser runs them, or they name no place to come back to.
	refusedSchemes = []string{"javascript", "data", "vbscript", "file", "blob", "about", "ftp", "ws", "wss"}
	loopbackHosts  = []string{"127.0.0.1", "::1", "localhost"}
)

// clientDocument is what faucet takes from a client ID metadata document
// (draft-ietf-oauth-client-id-metadata-document): a public client whose ID
// is the URL of its document.
type clientDocument struct {
	ClientID     string
	ClientName   string
	RedirectURIs []string
}

// documentFetcher reads client ID metadata documents. Its copies share one
// cache.
type documentFetcher struct {
	// client is the guarded client of faucet.NewHTTPClient, which reaches
	// public https addresses only and follows no redirect.
	client *http.Client
	cache  *documentCache
	// timeout bounds a whole fetch. Zero is documentTimeout.
	timeout time.Duration
}

func newDocumentFetcher(client *http.Client, clock quartz.Clock) documentFetcher {
	return documentFetcher{client: client, cache: newDocumentCache(clock)}
}

// fetch returns the document of the client whose ID is clientID, from the
// cache while it is fresh. Every error wraps errClientDocument.
func (f documentFetcher) fetch(ctx context.Context, clientID string) (clientDocument, error) {
	if doc, ok := f.cache.get(clientID); ok {
		return doc, nil
	}
	doc, header, err := f.read(ctx, clientID)
	if err != nil {
		return clientDocument{}, fmt.Errorf("%w: %w", errClientDocument, err)
	}
	f.cache.put(clientID, doc, header)
	return doc, nil
}

// read fetches the document of clientID and checks it. It returns the
// header of the response with the document.
func (f documentFetcher) read(ctx context.Context, clientID string) (clientDocument, http.Header, error) {
	if err := checkClientID(clientID); err != nil {
		return clientDocument{}, nil, err
	}
	timeout := f.timeout
	if timeout == 0 {
		timeout = documentTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, clientID, nil)
	if err != nil {
		return clientDocument{}, nil, xerrors.New("the client ID must be a URL")
	}
	req.Header.Set("Accept", "application/json")
	resp, err := f.client.Do(req)
	if err != nil {
		return clientDocument{}, nil, fetchFailure(ctx, timeout, err)
	}
	defer resp.Body.Close()
	mediaType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	switch {
	case resp.StatusCode != http.StatusOK:
		return clientDocument{}, nil, xerrors.Errorf("the document must be served with status 200 and no redirect, not %d", resp.StatusCode)
	case err != nil || mediaType != "application/json":
		return clientDocument{}, nil, xerrors.New("the document must be served as application/json")
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxDocumentBytes+1))
	if err != nil {
		return clientDocument{}, nil, fetchFailure(ctx, timeout, err)
	}
	if len(body) > maxDocumentBytes {
		return clientDocument{}, nil, xerrors.Errorf("the document must be at most %d bytes", maxDocumentBytes)
	}
	doc, err := parseClientDocument(clientID, body)
	return doc, resp.Header, err
}

func fetchFailure(ctx context.Context, timeout time.Duration, err error) error {
	if xerrors.Is(ctx.Err(), context.DeadlineExceeded) {
		return xerrors.Errorf("the document must arrive within %s", timeout)
	}
	// A *url.Error repeats the client ID, which the caller has.
	var urlErr *url.Error
	if xerrors.As(err, &urlErr) {
		err = urlErr.Err
	}
	return fmt.Errorf("%w: %w", errDocumentUnreachable, err)
}

// checkClientID accepts the URLs that a client ID can be: https with a host
// and a path other than /, no user info, no fragment and no . or .. path
// segment.
func checkClientID(clientID string) error {
	if len(clientID) > maxURIBytes {
		return xerrors.Errorf("the client ID must be at most %d bytes", maxURIBytes)
	}
	u, err := url.Parse(clientID)
	switch {
	case err != nil:
		return xerrors.New("the client ID must be a URL")
	case u.Scheme != "https" || u.Hostname() == "":
		return xerrors.New("the client ID must be an https URL with a host")
	case u.User != nil:
		return xerrors.New("the client ID must hold no user info")
	case strings.Contains(clientID, "#"):
		return xerrors.New("the client ID must hold no fragment")
	case u.Path == "" || u.Path == "/":
		return xerrors.New("the client ID must have a path other than /")
	case slices.ContainsFunc(strings.Split(u.Path, "/"), func(s string) bool { return s == "." || s == ".." }):
		return xerrors.New("the client ID must hold no . or .. path segment")
	}
	return nil
}

// defaultPorts are the ports that a URL of each scheme names when it names
// none.
var defaultPorts = map[string]string{"http": "80", "https": "443"}

// hostKey names the host of rawURL so that two URLs of one host give the
// same key: the host name in lower case without a final dot, then the port
// unless it is the default of the scheme. It is empty for a URL with no
// host.
func hostKey(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	name := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if name == "" {
		return ""
	}
	port := u.Port()
	if n, err := strconv.Atoi(port); err == nil {
		port = strconv.Itoa(n)
	}
	if port == "" || port == defaultPorts[u.Scheme] {
		return name
	}
	return net.JoinHostPort(name, port)
}

func parseClientDocument(clientID string, body []byte) (clientDocument, error) {
	var raw struct {
		ClientID     string   `json:"client_id"`
		ClientName   string   `json:"client_name"`
		RedirectURIs []string `json:"redirect_uris"`
		AuthMethod   *string  `json:"token_endpoint_auth_method"`
		// Secret and SecretExpiresAt must be absent: a document is public, so
		// it can hold no shared secret.
		Secret          json.RawMessage `json:"client_secret"`
		SecretExpiresAt json.RawMessage `json:"client_secret_expires_at"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		var typeErr *json.UnmarshalTypeError
		if xerrors.As(err, &typeErr) && typeErr.Field != "" {
			return clientDocument{}, xerrors.Errorf("the document's %s has the wrong type", typeErr.Field)
		}
		return clientDocument{}, xerrors.New("the document must be one JSON object")
	}
	switch {
	case raw.ClientID != clientID:
		return clientDocument{}, xerrors.New("the document's client_id must equal its URL, byte for byte")
	case len(raw.RedirectURIs) == 0 || len(raw.RedirectURIs) > maxRedirectURIs:
		return clientDocument{}, xerrors.Errorf("the document must list 1 to %d redirect_uris", maxRedirectURIs)
	case len(raw.ClientName) > maxClientNameBytes || strings.IndexFunc(raw.ClientName, unprintable) >= 0:
		return clientDocument{}, xerrors.Errorf("the document's client_name must be at most %d bytes with no control or format character", maxClientNameBytes)
	case raw.AuthMethod != nil && *raw.AuthMethod != "none":
		return clientDocument{}, xerrors.New("the document's token_endpoint_auth_method must be none: faucet takes public clients only")
	case raw.Secret != nil || raw.SecretExpiresAt != nil:
		return clientDocument{}, xerrors.New("the document must hold no client_secret: anyone can read it")
	}
	for i, uri := range raw.RedirectURIs {
		if _, err := parseRedirectURI(uri); err != nil {
			return clientDocument{}, xerrors.Errorf("redirect_uris[%d] %v", i, err)
		}
	}
	return clientDocument{ClientID: raw.ClientID, ClientName: raw.ClientName, RedirectURIs: raw.RedirectURIs}, nil
}

// unprintable reports whether r is a control character or a format
// character, such as a bidirectional override, which can make the consent
// page show a name that the document does not hold.
func unprintable(r rune) bool {
	return unicode.IsControl(r) || unicode.Is(unicode.Cf, r)
}

// parseRedirectURI accepts an absolute URI of at most 2048 bytes with no
// fragment and no user info, whose scheme is https, or http on a loopback
// host, or a private-use scheme that a browser does not run. Its errors
// complete the sentence "redirect_uris[i] ...".
func parseRedirectURI(uri string) (*url.URL, error) {
	if len(uri) > maxURIBytes {
		return nil, xerrors.Errorf("must be at most %d bytes", maxURIBytes)
	}
	u, err := url.Parse(uri)
	switch {
	case err != nil || u.Scheme == "":
		return nil, xerrors.New("must be an absolute URI")
	case strings.Contains(uri, "#"):
		return nil, xerrors.New("must hold no fragment")
	case u.User != nil:
		return nil, xerrors.New("must hold no user info")
	case u.Scheme == "https" && u.Hostname() == "":
		return nil, xerrors.New("must name a host")
	case u.Scheme == "http" && !slices.Contains(loopbackHosts, u.Hostname()):
		return nil, xerrors.New("must use https, or http on 127.0.0.1, [::1] or localhost")
	case slices.Contains(refusedSchemes, u.Scheme):
		return nil, xerrors.Errorf("must not use the scheme %s", u.Scheme)
	}
	return u, nil
}

// allows reports whether a client may name redirectURI in a request: it is
// one of the document's redirect URIs byte for byte, or it differs from a
// loopback http one in the port only, which RFC 8252 section 7.3 lets a
// native app choose at the time of the request.
func (d clientDocument) allows(redirectURI string) bool {
	if slices.Contains(d.RedirectURIs, redirectURI) {
		return true
	}
	host, rest, ok := splitLoopback(redirectURI)
	return ok && slices.ContainsFunc(d.RedirectURIs, func(entry string) bool {
		entryHost, entryRest, ok := splitLoopback(entry)
		return ok && entryHost == host && entryRest == rest
	})
}

// splitLoopback splits a loopback http redirect URI with no port or a port
// from 1 to 65535 into its host and what follows its port, as written.
func splitLoopback(uri string) (host, rest string, ok bool) {
	u, err := parseRedirectURI(uri)
	if err != nil || u.Scheme != "http" {
		return "", "", false
	}
	if port := u.Port(); port != "" {
		if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
			return "", "", false
		}
	}
	rest, ok = strings.CutPrefix(uri, "http://"+u.Host)
	return u.Hostname(), rest, ok
}
