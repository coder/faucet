package faucet

import (
	"context"
	"slices"

	"golang.org/x/xerrors"
)

type rawWantKey struct {
	repoID int64
	event  string
}

// RawWants reports what the live raw interests want now: the webhook
// subscriptions of a raw event name, and the poll leases. The returned func
// says whether one of them wants a delivery of that event, action and
// repository, for an event that the service offers. It reads the store once,
// so the func is a snapshot. Without a RawStore or offered events the func
// always says false.
//
// It is the value that a host gives redelivery.Deps.Wants, so that a delivery
// which failed is asked for again while an interest wants it.
func (s *Service) RawWants(ctx context.Context) (func(event, action string, repoID int64) bool, error) {
	if s.raw == nil || len(s.rawOffered) == 0 {
		return func(string, string, int64) bool { return false }, nil
	}
	wants, err := s.raw.RawWants(ctx, s.clock.Now())
	if err != nil {
		return nil, xerrors.Errorf("read what raw interests want: %w", err)
	}
	every := map[rawWantKey]bool{}
	actions := map[rawWantKey][]string{}
	for _, w := range wants {
		if _, offered := s.rawEntry(rawNamePrefix + w.Event); !offered {
			continue
		}
		key := rawWantKey{repoID: w.RepoID, event: w.Event}
		if len(w.Actions) == 0 {
			every[key] = true
			continue
		}
		actions[key] = append(actions[key], w.Actions...)
	}
	return func(event, action string, repoID int64) bool {
		key := rawWantKey{repoID: repoID, event: event}
		return every[key] || slices.Contains(actions[key], action)
	}, nil
}
