package faucet

import (
	"bytes"

	"golang.org/x/xerrors"

	"cdr.dev/slog/v3"
)

// minSecretBytes is the shortest webhook secret New accepts.
const minSecretBytes = 16

// Deps are the collaborators a Service needs. New checks them once, so nothing
// else reads the environment or starts work behind the caller's back.
type Deps struct {
	Store Store
	// Secret is the GitHub webhook secret. It is the HMAC key that proves a
	// request came from GitHub, so it must never be logged or returned.
	Secret []byte
	Logger slog.Logger
}

// Service turns GitHub webhooks into stored events.
type Service struct {
	store  Store
	secret []byte
	logger slog.Logger
}

// New returns a Service for d. It starts no goroutines.
func New(d Deps) (*Service, error) {
	if d.Store == nil {
		return nil, xerrors.New("store is required")
	}
	if len(d.Secret) < minSecretBytes {
		return nil, xerrors.Errorf("secret must be at least %d bytes", minSecretBytes)
	}
	return &Service{
		store:  d.Store,
		secret: bytes.Clone(d.Secret),
		logger: d.Logger,
	}, nil
}
