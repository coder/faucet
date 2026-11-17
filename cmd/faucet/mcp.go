package main

import (
	"context"
	"net/http"
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
	// origin is faucet's public URL. The resource of an endpoint is the
	// origin and the endpoint's path.
	origin string
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
				unauthorized(w)
			case err != nil:
				logger.Error(r.Context(), "could not check the faucet token", slog.Error(err))
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
		return faucet.Principal{}, faucet.ErrInvalidToken
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
