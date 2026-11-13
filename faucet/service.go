package faucet

import (
	"bytes"
	"context"

	"golang.org/x/xerrors"

	"cdr.dev/slog/v3"
	"github.com/coder/quartz"
)

const minSecretBytes = 16

// Deps are the collaborators a Service needs.
type Deps struct {
	Store Store
	// Secret is the GitHub webhook secret and the HMAC key. Never log or return it.
	Secret []byte
	Logger slog.Logger
	// Authorizer decides which repositories a caller may wait on.
	Authorizer Authorizer
	// PrincipalFrom returns the caller the host application authenticated for
	// this request. An error denies the request.
	PrincipalFrom func(context.Context) (Principal, error)
	Clock         quartz.Clock
}

// Service turns GitHub webhooks into stored events and serves them to waiters.
type Service struct {
	store         Store
	secret        []byte
	logger        slog.Logger
	authorizer    Authorizer
	principalFrom func(context.Context) (Principal, error)
	clock         quartz.Clock
}

// New returns a Service for d. It starts no goroutines.
func New(d Deps) (*Service, error) {
	switch {
	case d.Store == nil:
		return nil, xerrors.New("store is required")
	case len(d.Secret) < minSecretBytes:
		return nil, xerrors.Errorf("secret must be at least %d bytes", minSecretBytes)
	case d.Authorizer == nil:
		return nil, xerrors.New("authorizer is required")
	case d.PrincipalFrom == nil:
		return nil, xerrors.New("principal source is required")
	case d.Clock == nil:
		return nil, xerrors.New("clock is required")
	}
	return &Service{
		store:         d.Store,
		secret:        bytes.Clone(d.Secret),
		logger:        d.Logger,
		authorizer:    d.Authorizer,
		principalFrom: d.PrincipalFrom,
		clock:         d.Clock,
	}, nil
}
