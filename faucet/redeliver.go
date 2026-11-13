package faucet

import (
	"cmp"
	"context"
	"slices"
	"time"

	"golang.org/x/xerrors"

	"cdr.dev/slog/v3"
)

const (
	// MaxRedeliverPages is the most delivery pages one Redeliver call reads.
	MaxRedeliverPages = 10
	// MaxRedeliveries is the most redelivery requests one Redeliver call sends.
	MaxRedeliveries = 50
)

// ErrNoHookAdmin is what Redeliver returns when Deps had no HookAdmin.
var ErrNoHookAdmin = xerrors.New("no hook admin is configured")

// RedeliverResult reports one Redeliver call.
type RedeliverResult struct {
	// Examined is the number of delivery attempts inside the window.
	Examined int
	// Pending is the number of deliveries that failed, were not stored and had
	// no later successful attempt.
	Pending int
	// Requested is the number of redelivery requests GitHub accepted.
	Requested int
	// Incomplete is true when a bound stopped the call before it read the whole
	// window or asked for every pending delivery. The next call goes on.
	Incomplete bool
}

// Redeliver asks GitHub to deliver again each webhook delivery since the given
// time that failed, that faucet has not stored, and that has no later
// successful attempt. A failure is any answer outside 200 to 299, including no
// answer. Hosts call it on a schedule with a window longer than the schedule,
// for example the last hour every 30 minutes. GitHub keeps deliveries for three
// days, so an older window sees no more.
//
// One call reads at most MaxRedeliverPages pages and sends at most
// MaxRedeliveries requests, oldest delivery first. It stops at the first
// request that fails, because a bad token or a wrong hook would fail every
// request. Two calls at once can request the same delivery twice. The store
// keeps one event per delivery GUID, so the second delivery is a duplicate.
//
// A delivery that fails the same way every time, such as a malformed payload,
// is requested again on each call until it leaves the window.
//
// The method has no auth of its own. The host decides who may call it.
func (s *Service) Redeliver(ctx context.Context, since time.Time) (RedeliverResult, error) {
	if s.hooks == nil {
		return RedeliverResult{}, ErrNoHookAdmin
	}
	if since.IsZero() || since.After(s.clock.Now()) {
		return RedeliverResult{}, xerrors.New("since must be a time in the past")
	}

	attempts, examined, complete, err := s.readDeliveries(ctx, since)
	if err != nil {
		return RedeliverResult{}, err
	}
	res := RedeliverResult{Examined: examined, Incomplete: !complete}

	failed := failedWithoutLaterSuccess(attempts)
	stored, err := s.store.StoredDeliveries(ctx, guidsOf(failed))
	if err != nil {
		return res, xerrors.Errorf("look up stored deliveries: %w", err)
	}
	pending := slices.DeleteFunc(failed, func(d HookDelivery) bool { return slices.Contains(stored, d.GUID) })
	res.Pending = len(pending)

	for _, d := range pending {
		if res.Requested == MaxRedeliveries {
			res.Incomplete = true
			break
		}
		if err := s.hooks.Redeliver(ctx, d.ID); err != nil {
			s.logger.Warn(ctx, "redelivery request failed", slog.F("delivery_id", d.GUID), slog.Error(err))
			return res, xerrors.Errorf("request redelivery: %w", err)
		}
		res.Requested++
		s.logger.Info(ctx, "redelivery requested",
			slog.F("delivery_id", d.GUID), slog.F("attempt_id", d.ID), slog.F("status", d.StatusCode))
	}
	s.logger.Info(ctx, "redelivery sweep done",
		slog.F("examined", res.Examined), slog.F("pending", res.Pending),
		slog.F("requested", res.Requested), slog.F("incomplete", res.Incomplete))
	return res, nil
}

// readDeliveries returns the attempts since the given time, grouped by GUID.
// complete is false when MaxRedeliverPages stopped it first.
func (s *Service) readDeliveries(ctx context.Context, since time.Time) (attempts map[string][]HookDelivery, examined int, complete bool, err error) {
	attempts = map[string][]HookDelivery{}
	seen := map[int64]bool{}
	cursor := ""
	for range MaxRedeliverPages {
		page, err := s.hooks.ListDeliveries(ctx, cursor)
		if err != nil {
			return nil, 0, false, xerrors.Errorf("list deliveries: %w", err)
		}
		windowEnded := false
		for _, d := range page.Deliveries {
			switch {
			case d.DeliveredAt.Before(since):
				windowEnded = true
			case !seen[d.ID]:
				seen[d.ID] = true
				attempts[d.GUID] = append(attempts[d.GUID], d)
				examined++
			}
		}
		if windowEnded || page.Next == "" {
			return attempts, examined, true, nil
		}
		cursor = page.Next
	}
	return attempts, examined, false, nil
}

// failedWithoutLaterSuccess returns, for each GUID, the newest failed attempt
// when no successful attempt came after it, oldest first.
func failedWithoutLaterSuccess(attempts map[string][]HookDelivery) []HookDelivery {
	var out []HookDelivery
	for _, group := range attempts {
		var failed, succeeded *HookDelivery
		for i := range group {
			d := &group[i]
			if d.StatusCode >= 200 && d.StatusCode < 300 {
				if succeeded == nil || compareAttempts(*d, *succeeded) > 0 {
					succeeded = d
				}
			} else if failed == nil || compareAttempts(*d, *failed) > 0 {
				failed = d
			}
		}
		if failed != nil && (succeeded == nil || compareAttempts(*failed, *succeeded) > 0) {
			out = append(out, *failed)
		}
	}
	slices.SortFunc(out, compareAttempts)
	return out
}

func compareAttempts(a, b HookDelivery) int {
	return cmp.Or(a.DeliveredAt.Compare(b.DeliveredAt), cmp.Compare(a.ID, b.ID))
}

func guidsOf(deliveries []HookDelivery) []string {
	guids := make([]string, len(deliveries))
	for i, d := range deliveries {
		guids[i] = d.GUID
	}
	return guids
}
