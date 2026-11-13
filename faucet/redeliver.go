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
	// Redeliver stops asking for a delivery. A host that calls Redeliver every
	// 30 minutes makes the third request 60 to 90 minutes after the first
	// failure, so three recovers a delivery that failed in an outage of up to an
	// hour, and a delivery that faucet rejects for its content costs three
	// requests and then no more.
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
	// first attempt is inside the window and they have had fewer than
	// MaxRedeliveriesPerDelivery redeliveries.
	Pending int
	// GaveUp is the number of deliveries that failed, were not stored and had
	// no later successful attempt, and that Redeliver will not ask for again:
	// they have had MaxRedeliveriesPerDelivery redeliveries, or their first
	// attempt is older than the window. Each one is logged.
	GaveUp int
	// Requested is the number of redelivery requests GitHub accepted.
	Requested int
	// Incomplete is true when a bound stopped the call before it read the whole
	// window or asked for every pending delivery. The next call goes on.
	Incomplete bool
}

// Redeliver asks GitHub to deliver again each webhook delivery since the given
// time that failed, that faucet has not stored, and that has no later
// successful attempt. A failure is any answer outside 200 to 299, including no
// answer. GitHub keeps deliveries for three days, so an older window sees no
// more.
//
// One call reads at most MaxRedeliverPages pages and sends at most
// MaxRedeliveries requests, the deliveries with the fewest redeliveries first
// and then the oldest. It stops at the first
// request that fails, because a bad token or a wrong hook would fail every
// request. Two calls at once can request the same delivery twice. The store
// keeps one event per delivery GUID, so the second delivery is a duplicate.
//
// GitHub records a redelivery as a new attempt with its own, later time, so a
// delivery that faucet rejects every time, such as a malformed payload, would
// stay inside any window. Redeliver therefore asks for a delivery only while
// its first attempt (the one that is not a redelivery) is inside the window
// and fewer than MaxRedeliveriesPerDelivery redeliveries of it exist. The
// fewest-redeliveries-first order keeps a delivery that keeps failing from
// using up the requests that a new failure needs.
//
// A delivery that Redeliver will not ask for again is counted in GaveUp and
// logged as "redelivery gave up" on every call that still sees one of its
// attempts, with the reason "limit" or "window". It stays lost until someone
// redelivers it by hand. The window must therefore reach back
// MaxRedeliveriesPerDelivery+1 times the interval at which the host calls
// Redeliver. A shorter window lets the first attempt leave it before the
// limit is reached, and a delivery that a later redelivery would have
// recovered is given up. A host that calls every 30 minutes passes a since
// that is two hours back.
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
	pending, gaveUp := s.redeliverable(ctx, slices.DeleteFunc(failed, func(d HookDelivery) bool { return slices.Contains(stored, d.GUID) }), attempts)
	res.Pending, res.GaveUp = len(pending), gaveUp

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
		slog.F("examined", res.Examined), slog.F("pending", res.Pending), slog.F("gave_up", res.GaveUp),
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

// redeliverable splits the failed deliveries into the ones Redeliver asks for,
// in the order it asks (the fewest redeliveries first, then the oldest first),
// and the number it gave up on, each of which it logs. Each redelivery is a new
// attempt dated when it happened, so only an attempt that is not a redelivery
// says how long ago the delivery began. The group of attempts is complete from
// that attempt on, so the count of redeliveries in it is exact.
func (s *Service) redeliverable(ctx context.Context, failed []HookDelivery, attempts map[string][]HookDelivery) (askable []HookDelivery, gaveUp int) {
	redeliveries := make(map[string]int, len(failed))
	for _, d := range failed {
		first := false
		for _, a := range attempts[d.GUID] {
			if a.Redelivery {
				redeliveries[d.GUID]++
			} else {
				first = true
			}
		}
		reason := ""
		switch {
		case redeliveries[d.GUID] >= MaxRedeliveriesPerDelivery:
			reason = "limit"
		case !first:
			reason = "window"
		default:
			askable = append(askable, d)
			continue
		}
		gaveUp++
		s.logger.Warn(ctx, "redelivery gave up",
			slog.F("delivery_id", d.GUID), slog.F("redeliveries", redeliveries[d.GUID]), slog.F("reason", reason))
	}
	slices.SortStableFunc(askable, func(a, b HookDelivery) int {
		return cmp.Or(cmp.Compare(redeliveries[a.GUID], redeliveries[b.GUID]), compareAttempts(a, b))
	})
	return askable, gaveUp
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
