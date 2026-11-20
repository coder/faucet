package faucet

import (
	"context"
	"slices"

	"golang.org/x/xerrors"
)

// rawWantKey is an event of one repository, or with repoID 0 of every
// repository.
type rawWantKey struct {
	repoID int64
	event  string
}

// RawWants reports what the live raw interests want now: the webhook
// subscriptions of a raw event name, and the poll leases. The returned func
// says whether one of them wants a delivery of that event, action and
// repository, for an event that the service offers. An owner or all interest
// wants every delivery of its event and actions, whatever the repository:
// GitHub's list of deliveries names no owner, and a delivery that no interest
// wants is dropped when it arrives again. It reads the store once, so the
// func is a snapshot. Without a RawStore or offered events the func always
// says false. When the store fails, the func that it returns with the error
// wants every event that the service offers, of any repository, so that a
// host that cannot read the interests passes no delivery that one wanted.
//
// RedeliveryWants gives it to redelivery, so that a delivery which failed is
// asked for again while an interest wants it.
func (s *Service) RawWants(ctx context.Context) (func(event, action string, repoID int64) bool, error) {
	if s.raw == nil || len(s.rawOffered) == 0 {
		return func(string, string, int64) bool { return false }, nil
	}
	wants, err := s.raw.RawWants(ctx, s.clock.Now())
	if err != nil {
		offered := func(event, _ string, _ int64) bool {
			_, ok := s.rawEntry(rawNamePrefix + event)
			return ok
		}
		return offered, xerrors.Errorf("read what raw interests want: %w", err)
	}
	every := map[rawWantKey]bool{}
	actions := map[rawWantKey][]string{}
	for _, w := range wants {
		if _, offered := s.rawEntry(rawNamePrefix + w.Event); !offered {
			continue
		}
		key := rawWantKey{event: w.Event}
		if w.Target.Kind() == TargetRepo {
			key.repoID = w.Target.RepoID()
		}
		if len(w.Actions) == 0 {
			every[key] = true
			continue
		}
		actions[key] = append(actions[key], w.Actions...)
	}
	wanted := func(key rawWantKey, action string) bool {
		return every[key] || slices.Contains(actions[key], action)
	}
	return func(event, action string, repoID int64) bool {
		return repoID > 0 && wanted(rawWantKey{repoID: repoID, event: event}, action) || wanted(rawWantKey{event: event}, action)
	}, nil
}
