package faucet

import (
	"bytes"
	"context"
	"net/http"

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
	// ServiceGrants decides which owners, or whether every repository, a
	// service may read. Nil refuses every owner and all target.
	ServiceGrants ServiceGrants
	// PrincipalFrom returns the caller the host application authenticated for
	// this request. An error denies the request. It reads the context that
	// go-sdk gives the handler. In stateful mode that is the context of the
	// request that opened the session, not of each request, so a stateful host
	// must keep the principal in its session. Serve stateless as cmd/faucet does.
	PrincipalFrom func(context.Context) (Principal, error)
	Clock         quartz.Clock
	// Webhook turns on webhook delivery: events/subscribe, events/unsubscribe
	// and the webhook mode in events/list. Nil leaves it off, and
	// events/subscribe then answers Unsupported.
	Webhook *WebhookDeps
	// RawStore keeps the raw events, github.<event>: the GitHub events that
	// no typed family stores, kept only while a live webhook subscription or
	// poll lease wants them. It must be backed by the same database as Store.
	// Nil leaves raw events off.
	RawStore RawStore
	// RawEvents are the GitHub events, such as "push", that the service offers
	// raw. Each must be one of RawEventNames, and they need a RawStore. Empty
	// offers none: events/list then offers the typed names only, and the
	// webhook handler ignores the other events. A RawStore without RawEvents
	// still delivers and purges what it holds.
	RawEvents []string
	// RecordRepositories makes every signed delivery that names a repository
	// record it in the store (Store.RecordRepository), so a host that checks
	// services by repo grants can find a granted repository's ID without
	// GitHub. A failed write is logged, and the delivery goes on.
	RecordRepositories bool
	// Redelivered reports whether faucet's redelivery asked GitHub to send
	// the delivery of this ID again. Such a replay names accounts as they
	// were when it first came, so it moves no login. Nil knows no replay.
	Redelivered func(ctx context.Context, deliveryID string) (bool, error)
	// MaxWaitsPerLogin caps how many wait_for_github_events calls one login
	// holds open at once in this process, so a client that opens waits and
	// drops its connection cannot pile them up. Zero means 10. A negative value
	// removes the cap. The call over the cap fails at once.
	MaxWaitsPerLogin int
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
	// Client sends the events that DeliverDue delivers. Build it with
	// NewHTTPClient: a client without its checks lets a subscriber aim the
	// request at an address inside your network. Nil uses NewHTTPClient with
	// AllowInsecureLoopback as set here.
	Client *http.Client
	// MaxWorkersPerOrigin is how many of the 8 workers of one DeliverDue call
	// send to one callback origin at a time. Zero means 2, so that an endpoint
	// that stalls cannot hold up the others. A negative value removes the cap,
	// for a host whose subscribers all call back to one origin.
	MaxWorkersPerOrigin int
}

// Service turns GitHub webhooks into stored events and serves them to waiters.
type Service struct {
	store         Store
	raw           RawStore
	rawOffered    []*rawEntry
	secret        []byte
	logger        slog.Logger
	authorizer    Authorizer
	grants        ServiceGrants
	principalFrom func(context.Context) (Principal, error)
	clock         quartz.Clock
	webhook       *webhookState
	recordRepos   bool
	redelivered   func(ctx context.Context, deliveryID string) (bool, error)
	waits         *waitLimiter
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
	case len(d.RawEvents) > 0 && d.RawStore == nil:
		return nil, xerrors.New("raw events need a RawStore")
	}
	offered, err := offeredRawEntries(d.RawEvents)
	if err != nil {
		return nil, xerrors.Errorf("raw events: %w", err)
	}
	svc := &Service{
		store:         d.Store,
		raw:           d.RawStore,
		rawOffered:    offered,
		secret:        bytes.Clone(d.Secret),
		logger:        d.Logger,
		authorizer:    d.Authorizer,
		grants:        d.ServiceGrants,
		principalFrom: d.PrincipalFrom,
		clock:         d.Clock,
		recordRepos:   d.RecordRepositories,
		redelivered:   d.Redelivered,
		waits:         newWaitLimiter(d.MaxWaitsPerLogin),
	}
	delivery := []string{deliveryPoll}
	if d.Webhook != nil {
		if svc.webhook, err = newWebhookState(*d.Webhook, d.Clock, d.Logger); err != nil {
			return nil, xerrors.Errorf("webhook: %w", err)
		}
		delivery = []string{deliveryPoll, deliveryWebhook}
	}
	svc.listed = append(describeEvents(delivery), describeRawEvents(offered, delivery)...)
	return svc, nil
}
