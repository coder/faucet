package faucet

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"time"

	"golang.org/x/xerrors"

	"cdr.dev/slog/v3"
)

// rawSubscriptionEntry resolves the name of an events/subscribe or
// events/unsubscribe call on a service that offers raw names. ok is false for
// a typed name, and for params that the typed path refuses in its own words.
func (s *Service) rawSubscriptionEntry(p *subscriptionParams) (entry *rawEntry, ok bool, err error) {
	if s.raw == nil || p == nil || p.malformed {
		return nil, false, nil
	}
	var name string
	if isAbsent(p.fields.Name) || json.Unmarshal(p.fields.Name, &name) != nil || name == "" {
		return nil, false, nil
	}
	if entry, ok := s.rawEntry(name); ok {
		return entry, true, nil
	}
	if _, typed := lookupEntry(name); !typed {
		return nil, false, s.unknownEvent(name)
	}
	return nil, false, nil
}

type rawSubscribeRequest struct {
	entry       *rawEntry
	args        rawArguments
	callbackURL string
	secret      subscriberSecret
	// hasCursor says that the caller named a position. No event is kept for
	// a raw subscription before it exists, so a new one that names a
	// position has missed what came after it.
	hasCursor bool
	ttl       time.Duration
}

func parseRawSubscribe(entry *rawEntry, p *subscriptionParams, allowInsecureLoopback bool) (rawSubscribeRequest, error) {
	args, err := parseRawArguments(entry, p.fields.Arguments)
	if err != nil {
		return rawSubscribeRequest{}, err
	}
	d, err := decodeDelivery(p.fields.Delivery)
	if err != nil {
		return rawSubscribeRequest{}, err
	}
	callbackURL, err := d.callbackURL(allowInsecureLoopback)
	if err != nil {
		return rawSubscribeRequest{}, err
	}
	secret, err := d.subscriberSecret()
	if err != nil {
		return rawSubscribeRequest{}, err
	}
	req := rawSubscribeRequest{entry: entry, args: args.normalized(), callbackURL: callbackURL, secret: secret}
	if req.ttl, err = parseTTL(p.fields.TTLMs); err != nil {
		return rawSubscribeRequest{}, err
	}
	if !isAbsent(p.fields.Cursor) {
		var cursor string
		if json.Unmarshal(p.fields.Cursor, &cursor) != nil {
			return rawSubscribeRequest{}, invalidParams("cursor must be a string")
		}
		if _, _, _, ok := decodeRawCursor(cursor); !ok {
			return rawSubscribeRequest{}, invalidParams("cursor is not one this server returned")
		}
		req.hasCursor = true
	}
	// maxAgeMs is checked as for a typed name and has no further effect: a
	// raw subscription never replays.
	if _, err := parseMaxAge(p.fields.MaxAgeMs); err != nil {
		return rawSubscribeRequest{}, err
	}
	return req, nil
}

// subscribeRaw serves events/subscribe for a raw name with the checks of a
// typed name, in the same order. The subscription is a row like a typed one,
// at a position that the delivery queue of the event log never reaches: its
// deliveries are written when a webhook delivery of its event arrives.
func (s *Service) subscribeRaw(ctx context.Context, wh *webhookState, entry *rawEntry, p *subscriptionParams) (*eventsSubscribeResult, error) {
	req, err := parseRawSubscribe(entry, p, wh.allowInsecureLoopback)
	if err != nil {
		return nil, err
	}
	logger := s.logger.With(slog.F("repo", req.args.Repo), slog.F("event", entry.name))

	principal, err := s.subscriber(ctx, logger, "subscribe")
	if err != nil {
		return nil, err
	}
	canonical := req.args.canonical()
	key := NewSubscriptionKeyHash(principal.Login, req.callbackURL, entry.name, canonical)
	repoID, err := s.authorizeFreshRepo(ctx, logger, principal, "subscribe", req.args.Repo)
	if err != nil {
		if xerrors.Is(err, ErrNoAccess) {
			s.endRevokedSubscription(ctx, logger, wh, principal, key)
		}
		return nil, accessError(err)
	}
	secret := req.secret
	verification := CallbackRequest{
		Login: principal.Login, URL: req.callbackURL, SubscriptionID: key.ID(),
		sign: func(id string, timestamp time.Time, body []byte) (http.Header, error) {
			return SignWebhook(string(secret), id, timestamp, body)
		},
	}
	if err := wh.verifyCallback(ctx, logger, verification); err != nil {
		return nil, err
	}

	// The head is read before the subscription is stored, so every event that
	// the subscription gets has a higher raw ID.
	head, err := s.raw.RawHead(ctx)
	if err != nil {
		return nil, subscriptionStoreError(ctx, logger, err)
	}
	now := s.clock.Now()
	expiresAt := now.Add(req.ttl).Truncate(time.Second)
	row, err := s.store.UpsertSubscription(ctx, Subscription{
		KeyHash:       key,
		Principal:     principal.Login,
		CallbackURL:   req.callbackURL,
		EventType:     entry.name,
		EventTypes:    []string{entry.name},
		Arguments:     canonical,
		RepoID:        repoID,
		SecretEnc:     sealSecret(wh.aead, wh.nonceKey, req.secret, key[:]),
		Cursor:        math.MaxInt64,
		ExpiresAt:     expiresAt,
		RotationGrace: min(rotationGrace, expiresAt.Sub(now)),
	}, now, maxLiveSubscriptions)
	switch {
	case xerrors.Is(err, ErrSubscriptionLimit):
		logger.Info(ctx, "subscribe refused, limit reached", slog.F("login", principal.Login))
		return nil, rpcError(codeResourceExhausted, "subscription limit reached",
			map[string]any{"limit": "subscriptions", "max": maxLiveSubscriptions})
	case err != nil:
		return nil, subscriptionStoreError(ctx, logger, err)
	}
	logger.Info(ctx, "subscribed",
		slog.F("login", principal.Login), slog.F("subscription", key.ID()), slog.F("expires_at", row.ExpiresAt))
	renewed := row.UpdatedAt.After(row.CreatedAt)
	result := &eventsSubscribeResult{
		ResultType:    resultComplete,
		ID:            key.ID(),
		RefreshBefore: row.ExpiresAt.UTC().Format(time.RFC3339),
		Cursor:        encodeRawCursor(head),
		Truncated:     req.hasCursor && !renewed,
	}
	if renewed {
		result.DeliveryStatus = newDeliveryStatusResult(row.Status)
	}
	return result, nil
}

// unsubscribeRaw serves events/unsubscribe for a raw name. The deliveries
// that the subscription still held are never claimed again, and the next
// delivery run or purge deletes them.
func (s *Service) unsubscribeRaw(ctx context.Context, wh *webhookState, entry *rawEntry, p *subscriptionParams) (*eventsUnsubscribeResult, error) {
	args, err := parseRawArguments(entry, p.fields.Arguments)
	if err != nil {
		return nil, err
	}
	d, err := decodeDelivery(p.fields.Delivery)
	if err != nil {
		return nil, err
	}
	callbackURL, err := d.callbackURL(wh.allowInsecureLoopback)
	if err != nil {
		return nil, err
	}
	logger := s.logger.With(slog.F("repo", args.Repo), slog.F("event", entry.name))

	principal, err := s.subscriber(ctx, logger, "unsubscribe")
	if err != nil {
		return nil, err
	}
	key := NewSubscriptionKeyHash(principal.Login, callbackURL, entry.name, args.canonical())
	return s.endSubscription(ctx, logger, principal, key)
}
