package main

import (
	"context"
	"net"
	"net/http"
	"net/url"
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

// parseOrigin parses faucet's public URL: https with a host and nothing after
// it, or http on a loopback host. A path, even a trailing slash, would give
// each endpoint a resource that no client computes.
func parseOrigin(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	switch {
	case err != nil || u.Host == "" || u.Hostname() == "":
		return nil, xerrors.New("faucet's public URL must be an absolute URL with a host")
	case u.User != nil || u.Opaque != "" || u.Path != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "":
		return nil, xerrors.New("faucet's public URL must have no user info, path, trailing slash, query or fragment")
	case u.Scheme != "https" && (u.Scheme != "http" || !isLoopback(u.Hostname())):
		return nil, xerrors.New("faucet's public URL must use https, or http on a loopback host")
	}
	return u, nil
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// requireBearer admits a request only when its bearer token is valid. A token
// with a faucet prefix is faucet's own: it goes to the faucet verifier, for
// the endpoint's resource, and never to GitHub. Every other token goes to
// GitHub. A token is handed to its checker and goes no further: it is not
// logged and not stored. Every rejected token gets the same 401, so the
// response does not say why it was refused.
func requireBearer(b bearers, resource string, logger slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := bearerToken(r.Header.Get("Authorization"))
		if !ok {
			unauthorized(w)
			return
		}
		if oauth.Minted(token) {
			p, err := b.verifyFaucetToken(r.Context(), token, resource)
			switch {
			case xerrors.Is(err, faucet.ErrInvalidToken):
				logger.Debug(r.Context(), "faucet token rejected", slog.Error(err))
				unauthorized(w)
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
			unauthorized(w)
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

func unauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", "Bearer")
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
