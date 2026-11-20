package main

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/xerrors"

	"cdr.dev/slog/v3"
	"github.com/coder/superblink-events/faucet"
	"github.com/coder/superblink-events/faucet/oauth"
)

const (
	mcpServerName    = "faucet"
	mcpServerVersion = "0.1.0"
	maxTokenBytes    = 1024
)

type principalKey struct{}

func principalFrom(ctx context.Context) (faucet.Principal, error) {
	p, ok := ctx.Value(principalKey{}).(faucet.Principal)
	if !ok {
		return faucet.Principal{}, xerrors.New("request has no authenticated principal")
	}
	return p, nil
}

func newMCPHandler(svc *faucet.Service) http.Handler {
	srv := mcp.NewServer(&mcp.Implementation{Name: mcpServerName, Version: mcpServerVersion}, &mcp.ServerOptions{
		Capabilities: faucet.ServerCapabilities(),
	})
	svc.Register(srv)
	return faucet.RequireEventName(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, &mcp.StreamableHTTPOptions{
		Stateless:                    true,
		JSONResponse:                 true,
		PropagateRequestCancellation: true,
		MaxRequestBodyBytes:          faucet.MaxRequestBodyBytes,
		// The SDK refuses a request that reaches a loopback listener under any
		// other Host, against DNS rebinding of a local server that trusts the
		// browser. Vercel hands every request to the function on a loopback
		// address under faucet's public Host, so that check refused every MCP
		// request in production. Faucet trusts no browser: each request carries
		// a bearer token that requireBearer checked before it got here.
		DisableLocalhostProtection: true,
	}))
}

// longResponse lifts the server's write deadline for requests that can wait
// up to 50 seconds before they answer.
func longResponse(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Not every ResponseWriter supports deadlines, and the server's own
		// deadline then applies, so the error is not fatal.
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(mcpWriteTimeout))
		next.ServeHTTP(w, r)
	})
}

// faucetVerifier checks the access tokens that faucet mints. *oauth.Verifier
// is one.
type faucetVerifier interface {
	Verify(ctx context.Context, token, resource string) (faucet.Principal, error)
}

// bearers checks the tokens that faucet mints, for the endpoints of origin.
type bearers struct {
	faucet faucetVerifier
	// origin is faucet's public URL. The resource of an endpoint is the
	// origin and the endpoint's path.
	origin *url.URL
}

// newBearers returns the bearers that verifier checks. It needs origin,
// faucet's public URL, because a token is valid only on the resource it was
// issued for.
func newBearers(verifier faucetVerifier, origin string) (bearers, error) {
	if verifier == nil {
		return bearers{}, xerrors.New("the token verifier is required")
	}
	u, err := parseOrigin(origin)
	if err != nil {
		return bearers{}, err
	}
	return bearers{faucet: verifier, origin: u}, nil
}

// resource is the resource of the endpoint at path.
func (b bearers) resource(path string) string {
	return b.origin.JoinPath(path).String()
}

// protectedResourcePath is where the metadata (RFC 9728) of the endpoint at
// path lives: the well-known prefix with the endpoint's path after it, which
// is where a client looks first.
func protectedResourcePath(path string) string {
	return "/.well-known/oauth-protected-resource" + path
}

// challenge is the WWW-Authenticate of a 401 from the endpoint at path. It
// names the endpoint's metadata and every scope, so a client that sent no
// faucet token can find the login server (RFC 9728 section 5.1).
func (b bearers) challenge(path string) string {
	return `Bearer resource_metadata="` + b.origin.JoinPath(protectedResourcePath(path)).String() + `", scope="` + strings.Join(scopeNames(faucet.AllScopes()), " ") + `"`
}

// parseOrigin parses faucet's public URL: https with a host and nothing after
// it, or http on a loopback host. The URL must already be in the form that a
// client computes, because a token is valid only on a resource that matches
// it byte for byte: a lower-case host with no trailing dot, no default or
// empty port, and no path, not even a trailing slash.
func parseOrigin(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	switch {
	case err != nil || u.Host == "" || u.Hostname() == "":
		return nil, xerrors.New("faucet's public URL must be an absolute URL with a host")
	case u.User != nil || u.Opaque != "" || u.Path != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "":
		return nil, xerrors.New("faucet's public URL must have no user info, path, trailing slash, query or fragment")
	case u.Scheme != "https" && (u.Scheme != "http" || !isLoopback(u.Hostname())):
		return nil, xerrors.New("faucet's public URL must use https, or http on a loopback host")
	case u.Hostname() != strings.ToLower(u.Hostname()) || strings.HasSuffix(u.Hostname(), "."):
		return nil, xerrors.New("faucet's public URL must have a lower-case host with no trailing dot")
	}
	if err := canonicalPort(u); err != nil {
		return nil, err
	}
	return u, nil
}

// canonicalPort accepts no port, or a port from 1 to 65535 that is not the
// scheme's default and has no leading zero.
func canonicalPort(u *url.URL) error {
	port := u.Port()
	if port == "" {
		if strings.HasSuffix(u.Host, ":") {
			return xerrors.New("faucet's public URL must not end its host with an empty port")
		}
		return nil
	}
	n, err := strconv.Atoi(port)
	switch {
	case err != nil || n < 1 || n > 65535 || strconv.Itoa(n) != port:
		return xerrors.New("faucet's public URL must have a port from 1 to 65535, written without a leading zero")
	case (u.Scheme == "https" && n == 443) || (u.Scheme == "http" && n == 80):
		return xerrors.Errorf("faucet's public URL must leave out the default port %d", n)
	}
	return nil
}

func isLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// requireBearer admits a request to the endpoint at path only when its bearer
// token is a valid faucet token for the endpoint's resource, and answers any
// other with a 401 that names the endpoint's metadata. Faucet takes no GitHub
// token: GitHub would answer that a token with only metadata access can see a
// private repository, and no GitHub API says what a fine-grained token may
// read. A token goes no further than the verifier: it is not logged and not
// stored, and never reaches GitHub. Every request without a valid token gets
// the same 401, so the response does not say why it was refused.
func requireBearer(b bearers, path string, logger slog.Logger, next http.Handler) http.Handler {
	resource, challenge := b.resource(path), b.challenge(path)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := bearerToken(r.Header.Get("Authorization"))
		if !ok || !oauth.Minted(token) {
			unauthorized(w, challenge)
			return
		}
		p, err := b.faucet.Verify(r.Context(), token, resource)
		switch {
		case xerrors.Is(err, faucet.ErrInvalidToken):
			// One line of text: slog.Error would add the error's stack frames.
			logger.Debug(r.Context(), "faucet token rejected", slog.F("reason", err.Error()))
			unauthorized(w, challenge)
		case err != nil:
			logger.Warn(r.Context(), "could not check the faucet token", slog.Error(err))
			http.Error(w, "could not check the token", http.StatusInternalServerError)
		default:
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalKey{}, p)))
		}
	})
}

func unauthorized(w http.ResponseWriter, challenge string) {
	w.Header().Set("WWW-Authenticate", challenge)
	http.Error(w, "unauthorized", http.StatusUnauthorized)
}

// bearerToken extracts the token from "Bearer <token>". It accepts at most
// maxTokenBytes of printable ASCII without spaces.
func bearerToken(header string) (string, bool) {
	scheme, token, ok := strings.Cut(header, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || !validBearerToken(token) {
		return "", false
	}
	return token, true
}

// validBearerToken reports whether bearerToken can return token. A secret that
// callers must send as a bearer token has to pass it, or no caller can send it.
func validBearerToken(token string) bool {
	if token == "" || len(token) > maxTokenBytes {
		return false
	}
	for i := range len(token) {
		if token[i] <= ' ' || token[i] > '~' {
			return false
		}
	}
	return true
}
