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

// requireBearer admits a request only when its bearer token is a valid GitHub
// token. The token is handed to authn and goes no further: it is not logged
// and not stored. Every rejected token gets the same 401, so the response
// does not say why GitHub refused it.
func requireBearer(authn authenticator, logger slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := bearerToken(r.Header.Get("Authorization"))
		if !ok {
			unauthorized(w)
			return
		}
		p, err := authn.Authenticate(r.Context(), token)
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
