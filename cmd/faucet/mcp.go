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
	"github.com/coder/faucet/faucet"
	"github.com/coder/faucet/faucet/oauth"
)

const (
	mcpServerName    = "faucet"
	mcpServerVersion = "0.1.0"
	maxTokenBytes    = 1024
)

type principalKey struct{}

type authenticator interface {
	Authenticate(ctx context.Context, token string) (faucet.Principal, error)
}

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

// bearers sends each bearer token to the one that can check it.
type bearers struct {
	// github resolves a GitHub token, such as a pasted personal access token.
	github authenticator
	// faucet verifies the tokens that faucet mints. Nil, the default, rejects
	// every one of them.
	faucet faucetVerifier
	// origin is faucet's public URL when faucet is set. The resource of an
	// endpoint is the origin and the endpoint's path.
	origin *url.URL
}

// newBearers returns the bearers for github and, when verifier is not nil,
// for faucet's own tokens. A verifier needs origin, faucet's public URL,
// because a token is valid only on the resource it was issued for.
func newBearers(github authenticator, verifier faucetVerifier, origin string) (bearers, error) {
	b := bearers{github: github}
	if verifier == nil {
		return b, nil
	}
	u, err := parseOrigin(origin)
	if err != nil {
		return bearers{}, err
	}
	b.faucet, b.origin = verifier, u
	return b, nil
}

// resource is the resource of the endpoint at path, empty when faucet tokens
// are off.
func (b bearers) resource(path string) string {
	if b.origin == nil {
		return ""
	}
	return b.origin.JoinPath(path).String()
}

// protectedResourcePath is where the metadata (RFC 9728) of the endpoint at
// path lives: the well-known prefix with the endpoint's path after it, which
// is where a client looks first.
func protectedResourcePath(path string) string {
	return "/.well-known/oauth-protected-resource" + path
}

// challenge is the WWW-Authenticate of a 401 from the endpoint at path when
// faucet's login server is on. It names the endpoint's metadata and every
// scope, so a client that sent no token can find the login server (RFC 9728
// section 5.1).
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
// token is valid, and answers any other with a 401 that carries challenge. A token with a faucet prefix is faucet's own: it goes to
// the faucet verifier, for the endpoint's resource, and never to GitHub.
// Every other token goes to GitHub. A token is handed to its checker and goes
// no further: it is not logged and not stored. Every request without a valid
// token gets the same 401, so the response does not say why it was refused.
func requireBearer(b bearers, path, challenge string, logger slog.Logger, next http.Handler) http.Handler {
	resource := b.resource(path)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := bearerToken(r.Header.Get("Authorization"))
		if !ok {
			unauthorized(w, challenge)
			return
		}
		if oauth.Minted(token) {
			p, err := b.verifyFaucetToken(r.Context(), token, resource)
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
			return
		}
		p, err := b.github.Authenticate(r.Context(), token)
		switch {
		case xerrors.Is(err, faucet.ErrInvalidToken):
			unauthorized(w, challenge)
			return
		case err != nil:
			logger.Warn(r.Context(), "could not check the GitHub token", slog.Error(err))
			http.Error(w, "could not check the token with GitHub", http.StatusBadGateway)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalKey{}, p)))
	})
}

func (b bearers) verifyFaucetToken(ctx context.Context, token, resource string) (faucet.Principal, error) {
	if b.faucet == nil {
		return faucet.Principal{}, xerrors.Errorf("this server accepts no faucet token yet: %w", faucet.ErrInvalidToken)
	}
	return b.faucet.Verify(ctx, token, resource)
}

func unauthorized(w http.ResponseWriter, challenge string) {
	w.Header().Set("WWW-Authenticate", challenge)
	http.Error(w, "unauthorized", http.StatusUnauthorized)
}

// bearerToken extracts the token from "Bearer <token>". It accepts only
// printable ASCII without spaces, because the token goes into a header of the
// request to GitHub.
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
