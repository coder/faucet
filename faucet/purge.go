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
// With a RawStore, Purge also ends the raw deliveries that no live
// subscription holds, deletes the poll leases that have expired, drops the
// payload of every raw event that no delivery and no lease still waits for or
// that is older than the life of a lease, and deletes the raw events received
// before the given time. The result counts them in Raw, and the raw
// deliveries in EndedDeliveries.
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
	fields := []slog.Field{
		slog.F("deleted", res.Deleted), slog.F("purged_through", res.PurgedThrough), slog.F("before", before),
		slog.F("expired_subscriptions_deleted", subscriptions),
	}
	if s.raw != nil {
		if res, err = s.purgeRaw(ctx, res, now, before); err != nil {
			return PurgeResult{}, err
		}
		fields = append(fields,
			slog.F("raw_events_cleared", res.Raw.Cleared), slog.F("raw_events_deleted", res.Raw.Deleted),
			slog.F("expired_leases", res.Raw.ExpiredLeases))
	}
	fields = append(fields, slog.F("deliveries_ended", res.EndedDeliveries))
	logger := s.logger.Debug
	if res.Deleted > 0 || subscriptions > 0 || res.EndedDeliveries > 0 || res.Raw != (RawPurgeResult{}) {
		logger = s.logger.Info
	}
	logger(ctx, "events purged", fields...)
	return res, nil
}

// purgeRaw runs after the expired subscriptions are deleted, so that the raw
// deliveries they held end first and their events are cleared by the same
// call. A body older than a lease lives is cleared whatever still names it,
// which bounds what a poller that never moves its cursor can keep.
func (s *Service) purgeRaw(ctx context.Context, res PurgeResult, now, before time.Time) (PurgeResult, error) {
	ended, err := s.raw.ExpireRawDeliveries(ctx, now)
	if err != nil {
		return PurgeResult{}, xerrors.Errorf("end raw deliveries of expired subscriptions: %w", err)
	}
	res.EndedDeliveries += ended
	res.Raw, err = s.raw.PurgeRawEvents(ctx, RawPurge{Now: now, ClearBefore: now.Add(-rawLeaseTTL), DeleteBefore: before})
	if err != nil {
		return PurgeResult{}, xerrors.Errorf("purge raw events: %w", err)
	}
	return res, nil
}
