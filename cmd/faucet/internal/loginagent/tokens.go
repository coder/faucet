package main

import (
	"context"
	"errors"
	"slices"
	"sync"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/xerrors"
)

// refresher is the agent's token source, which go-sdk builds through
// NewTokenSource after the login. It hands out the tokens of the oauth2
// source it wraps, so it refreshes as that source does, and it can make the
// next refresh happen now instead of when the access token expires 15
// minutes later.
type refresher struct {
	mu     sync.Mutex
	ctx    context.Context
	config *oauth2.Config
	source oauth2.TokenSource
	tokens []string
}

func (r *refresher) start(ctx context.Context, config *oauth2.Config, token *oauth2.Token) (oauth2.TokenSource, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ctx, r.config, r.source = ctx, config, config.TokenSource(ctx, token)
	r.keep(token)
	return r, nil
}

func (r *refresher) Token() (*oauth2.Token, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	token, err := r.source.Token()
	if err != nil {
		return nil, err
	}
	r.keep(token)
	return token, nil
}

// expire makes the next Token refresh, as the expiry of the access token
// would, and returns the token that the refresh replaces.
func (r *refresher) expire() (*oauth2.Token, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	current, err := r.source.Token()
	if err != nil {
		return nil, xerrors.Errorf("read the token before the refresh: %w", err)
	}
	expired := *current
	expired.Expiry = time.Now().Add(-time.Minute)
	r.source = r.config.TokenSource(r.ctx, &expired)
	return current, nil
}

// present asks the token endpoint for a token with the refresh token of
// replaced, and returns the OAuth error code that it answers with.
func (r *refresher) present(replaced *oauth2.Token) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	stale := *replaced
	stale.Expiry = time.Now().Add(-time.Minute)
	token, err := r.config.TokenSource(r.ctx, &stale).Token()
	var refused *oauth2.RetrieveError
	switch {
	case err == nil:
		r.keep(token)
		return "", xerrors.New("the token endpoint took the refresh token that the refresh replaced")
	case !errors.As(err, &refused):
		return "", xerrors.Errorf("present the replaced refresh token: %w", err)
	}
	return refused.ErrorCode, nil
}

func (r *refresher) keep(token *oauth2.Token) {
	for _, value := range []string{token.AccessToken, token.RefreshToken} {
		if value != "" && !slices.Contains(r.tokens, value) {
			r.tokens = append(r.tokens, value)
		}
	}
}

func (r *refresher) seen() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.tokens)
}
