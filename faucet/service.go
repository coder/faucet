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
	// HookAdmin lets Redeliver ask GitHub for failed deliveries again. It is
	// optional. Without it Redeliver returns ErrNoHookAdmin.
	HookAdmin HookAdmin
	// Webhook turns on webhook delivery: events/subscribe, events/unsubscribe
	// and the webhook mode in events/list. Nil leaves it off, and
	// events/subscribe then answers Unsupported.
	Webhook *WebhookDeps
}

type WebhookDeps struct {
	// Key seals subscriber secrets in the store.
	Key WebhookKey
	// Callbacks verifies a callback URL inside events/subscribe, before
	// anything is stored. NewCallbackAllowlist is the one that makes no
	// request.
	Callbacks CallbackVerifier
	// AllowInsecureLoopback accepts callback URLs on loopback hosts, over http
	// too. It exists for tests. Production hosts leave it false.
	AllowInsecureLoopback bool
}

// Service turns GitHub webhooks into stored events and serves them to waiters.
type Service struct {
	store         Store
	secret        []byte
	logger        slog.Logger
	authorizer    Authorizer
	principalFrom func(context.Context) (Principal, error)
	clock         quartz.Clock
	hooks         HookAdmin
	webhook       *webhookState
	listed        []eventDescriptor
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
	svc := &Service{
		store:         d.Store,
		secret:        bytes.Clone(d.Secret),
		logger:        d.Logger,
		authorizer:    d.Authorizer,
		principalFrom: d.PrincipalFrom,
		clock:         d.Clock,
		hooks:         d.HookAdmin,
		listed:        describeEvents([]string{deliveryPoll}),
	}
	if d.Webhook != nil {
		var err error
		if svc.webhook, err = newWebhookState(*d.Webhook, d.Clock); err != nil {
			return nil, xerrors.Errorf("webhook: %w", err)
		}
		svc.listed = describeEvents([]string{deliveryPoll, deliveryWebhook})
	}
	return svc, nil
}
