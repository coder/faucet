package faucet

import (
	"context"
	"time"

	"golang.org/x/xerrors"

	"cdr.dev/slog/v3"
)

// Purge deletes the events received before the given time. Hosts call it on a
// schedule with the time that their retention rule gives, for example 30 days
// ago. It refuses the zero time and a time in the future, because either would
// delete every event.
//
// Purge also deletes the webhook subscriptions that have expired, because
// each holds a sealed secret that no longer has a use. It does that whether or
// not webhook delivery is on. The deliveries that such a subscription still
// held end with it, and the result counts them in EndedDeliveries.
//
// Purge records the highest ID it deleted as the purge mark. A wait or poll
// whose cursor is below the mark learns that events were lost, through
// events/poll's truncated field and the wait tool's truncated field.
//
// The method has no auth of its own. The host decides who may call it.
func (s *Service) Purge(ctx context.Context, before time.Time) (PurgeResult, error) {
	if before.IsZero() {
		return PurgeResult{}, xerrors.New("purge time is required")
	}
	now := s.clock.Now()
	if before.After(now) {
		return PurgeResult{}, xerrors.New("purge time must not be in the future")
	}
	ended, err := s.store.ExpireDeliveries(ctx, now)
	if err != nil {
		return PurgeResult{}, xerrors.Errorf("end deliveries of expired subscriptions: %w", err)
	}
	subscriptions, err := s.store.DeleteExpiredSubscriptions(ctx, now)
	if err != nil {
		return PurgeResult{}, xerrors.Errorf("purge subscriptions: %w", err)
	}
	res, err := s.store.Purge(ctx, before)
	if err != nil {
		return PurgeResult{}, xerrors.Errorf("purge events: %w", err)
	}
	res.ExpiredSubscriptions, res.EndedDeliveries = subscriptions, ended
	s.logger.Info(ctx, "events purged",
		slog.F("deleted", res.Deleted), slog.F("purged_through", res.PurgedThrough), slog.F("before", before),
		slog.F("expired_subscriptions_deleted", subscriptions), slog.F("deliveries_ended", ended))
	return res, nil
}
