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
// Purge records the highest ID it deleted as the purge mark. A wait or poll
// whose cursor is below the mark learns that events were lost, through
// events/poll's truncated field and the wait tool's truncated field.
//
// The method has no auth of its own. The host decides who may call it.
func (s *Service) Purge(ctx context.Context, before time.Time) (PurgeResult, error) {
	if before.IsZero() {
		return PurgeResult{}, xerrors.New("purge time is required")
	}
	if now := s.clock.Now(); before.After(now) {
		return PurgeResult{}, xerrors.New("purge time must not be in the future")
	}
	res, err := s.store.Purge(ctx, before)
	if err != nil {
		return PurgeResult{}, xerrors.Errorf("purge events: %w", err)
	}
	s.logger.Info(ctx, "events purged",
		slog.F("deleted", res.Deleted), slog.F("purged_through", res.PurgedThrough), slog.F("before", before))
	return res, nil
}
