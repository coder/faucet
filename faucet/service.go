package faucet

import (
	"bytes"

	"golang.org/x/xerrors"

	"cdr.dev/slog/v3"
)

const minSecretBytes = 16

// Deps are the collaborators a Service needs.
type Deps struct {
	Store Store
	// Secret is the GitHub webhook secret and the HMAC key. Never log or return it.
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
