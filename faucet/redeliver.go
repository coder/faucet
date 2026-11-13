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
	// MaxRedeliveriesPerDelivery is the number of redeliveries after which
	// Redeliver stops asking for a delivery. Three covers an outage that
	// outlasts one retry, and a delivery that faucet rejects for its content
	// costs three requests and then no more.
	MaxRedeliveriesPerDelivery = 3
)

// ErrNoHookAdmin is what Redeliver returns when Deps had no HookAdmin.
var ErrNoHookAdmin = xerrors.New("no hook admin is configured")

// RedeliverResult reports one Redeliver call.
type RedeliverResult struct {
	// Examined is the number of delivery attempts inside the window.
	Examined int
	// Pending is the number of deliveries that failed, were not stored, had no
	// later successful attempt, and that Redeliver may still ask for: their
	// original attempt is inside the window and they have had fewer than
	// MaxRedeliveriesPerDelivery redeliveries.
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
// GitHub records a redelivery as a new attempt with its own, later time, so a
// delivery that faucet rejects every time, such as a malformed payload, would
// stay inside any window. Redeliver therefore asks for a delivery only while
// its original attempt is inside the window and at most
// MaxRedeliveriesPerDelivery redeliveries of it exist. The deliveries with
// the fewest redeliveries go first, so a delivery that keeps failing does not
// use up the requests that a new failure needs. A delivery that Redeliver gave
// up on is logged and stays lost until someone redelivers it by hand.
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
	pending := s.redeliverable(ctx, slices.DeleteFunc(failed, func(d HookDelivery) bool { return slices.Contains(stored, d.GUID) }), attempts)
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

// redeliverable drops the failed deliveries that Redeliver must not ask for
// and puts the rest in the order it asks: the fewest redeliveries first, then
// the oldest first. Each redelivery is a new attempt dated when it happened, so
// only an attempt that is not a redelivery says how long ago the delivery
// began. The group of attempts is complete from that attempt on, so the count
// of redeliveries in it is exact.
func (s *Service) redeliverable(ctx context.Context, failed []HookDelivery, attempts map[string][]HookDelivery) []HookDelivery {
	redeliveries := make(map[string]int, len(failed))
	keep := failed[:0]
	for _, d := range failed {
		original := false
		for _, a := range attempts[d.GUID] {
			if a.Redelivery {
				redeliveries[d.GUID]++
			} else {
				original = true
			}
		}
		switch {
		case !original:
		case redeliveries[d.GUID] >= MaxRedeliveriesPerDelivery:
			s.logger.Warn(ctx, "redelivery limit reached", slog.F("delivery_id", d.GUID), slog.F("redeliveries", redeliveries[d.GUID]))
		default:
			keep = append(keep, d)
		}
	}
	slices.SortStableFunc(keep, func(a, b HookDelivery) int {
		return cmp.Or(cmp.Compare(redeliveries[a.GUID], redeliveries[b.GUID]), compareAttempts(a, b))
	})
	return keep
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
