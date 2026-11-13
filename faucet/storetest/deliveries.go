package storetest

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/coder/faucet/faucet"
)

const testLease = faucet.DeliveryLease

var deliverySubtests = []subtest{
	{"ScanTargetsListsLiveSubscriptionsBehindTheHead", scanTargetsListsLiveSubscriptionsBehindTheHead},
	{"ScanTargetsSkipsSubscriptionsAtThePendingCap", scanTargetsSkipsSubscriptionsAtThePendingCap},
	{"ScanTargetsNeedsAnEventOfTheSubscriptionsRepositoryAndType", scanTargetsNeedsAnEventOfTheSubscriptionsRepositoryAndType},
	{"ScanTargetsIncludesASubscriptionThatAPurgeOvertook", scanTargetsIncludesASubscriptionThatAPurgeOvertook},
	{"EnqueueDeliveriesMovesThePositionOnlyForTheCaller", enqueueDeliveriesMovesThePositionOnlyForTheCaller},
	{"EnqueueDeliveriesWinsOnceUnderConcurrency", enqueueDeliveriesWinsOnceUnderConcurrency},
	{"EnqueueDeliveriesValidatesInput", enqueueDeliveriesValidatesInput},
	{"ClaimDeliveryReturnsTheDeliveryWithItsEventSecretAndWatermark", claimDeliveryReturnsTheDeliveryWithItsEventSecretAndWatermark},
	{"ScanTargetsAndClaimsCarryThePreviousSecret", scanTargetsAndClaimsCarryThePreviousSecret},
	{"ClaimDeliveryGivesOneAttemptToOneCaller", claimDeliveryGivesOneAttemptToOneCaller},
	{"ClaimDeliveryLeaseRunsOutAndTheNextAttemptCounts", claimDeliveryLeaseRunsOutAndTheNextAttemptCounts},
	{"ClaimDeliverySkipsExpiredSubscriptions", claimDeliverySkipsExpiredSubscriptions},
	{"ClaimDeliveryReturnsNoEventAfterAPurge", claimDeliveryReturnsNoEventAfterAPurge},
	{"FinishDeliveryNeedsTheCurrentAttempt", finishDeliveryNeedsTheCurrentAttempt},
	{"FinishDeliveryRetryReschedulesAndRecordsTheFailure", finishDeliveryRetryReschedulesAndRecordsTheFailure},
	{"FinishDeliveryDeliveredEndsTheDeliveryAndClearsTheFailure", finishDeliveryDeliveredEndsTheDeliveryAndClearsTheFailure},
	{"FinishDeliveryGiveUpRecordsOnlyWhatTheEndpointSaid", finishDeliveryGiveUpRecordsOnlyWhatTheEndpointSaid},
	{"FinishDeliveryDeadDeletesTheSubscriptionOnlyForTheSameSealedSecret", finishDeliveryDeadDeletesTheSubscriptionOnlyForTheSameSealedSecret},
	{"FinishDeliveryValidatesInput", finishDeliveryValidatesInput},
	{"WatermarkStaysBelowTheLowestUnsentEvent", watermarkStaysBelowTheLowestUnsentEvent},
	{"ExpireDeliveriesLeavesLiveSubscriptionsAndHeldClaims", expireDeliveriesLeavesLiveSubscriptionsAndHeldClaims},
	{"DeletingASubscriptionEndsItsDeliveries", deletingASubscriptionEndsItsDeliveries},
	{"ScanPositionStartsAtTheCursorAndSurvivesARefresh", scanPositionStartsAtTheCursorAndSurvivesARefresh},
}

// rig is one subscription of repository 42 at cursor 0, and a log of events.
type rig struct {
	t       *testing.T
	s       faucet.Store
	ctx     context.Context
	now     time.Time
	sub     faucet.Subscription
	events  []faucet.Event
	scanned int64
}

func newRig(t *testing.T, s faucet.Store, events int) *rig {
	t.Helper()
	r := &rig{t: t, s: s, ctx: context.Background(), now: subscriptionBase}
	r.sub = subscriptionAt(1, r.now.Add(time.Hour))
	r.sub.Cursor = 0
	_, err := s.UpsertSubscription(r.ctx, r.sub, r.now, 10)
	require.NoError(t, err)
	for range events {
		r.addEvent()
	}
	return r
}

func (r *rig) addEvent() faucet.Event {
	r.t.Helper()
	ev, _, err := r.s.InsertEvent(r.ctx, newEvent(fmt.Sprintf("delivery-%d", len(r.events)+1)))
	require.NoError(r.t, err)
	r.events = append(r.events, ev)
	return ev
}

func messageIDOf(ev faucet.Event) string { return fmt.Sprintf("msg_%d", ev.ID) }

func (r *rig) enqueue(events ...faucet.Event) {
	r.t.Helper()
	e := faucet.Enqueue{Key: r.sub.KeyHash, ExpectedScannedThrough: r.scanned, NewScannedThrough: events[len(events)-1].ID}
	for _, ev := range events {
		e.Deliveries = append(e.Deliveries, faucet.NewDelivery{EventID: ev.ID, MessageID: messageIDOf(ev)})
	}
	won, err := r.s.EnqueueDeliveries(r.ctx, r.now, e)
	require.NoError(r.t, err)
	require.True(r.t, won)
	r.scanned = e.NewScannedThrough
}

func (r *rig) claim(now time.Time) (faucet.DeliveryClaim, bool) {
	r.t.Helper()
	claim, ok, err := r.s.ClaimDelivery(r.ctx, now, testLease)
	require.NoError(r.t, err)
	return claim, ok
}

func (r *rig) mustClaim(now time.Time) faucet.DeliveryClaim {
	r.t.Helper()
	claim, ok := r.claim(now)
	require.True(r.t, ok, "a delivery is due at %s", now)
	return claim
}

func (r *rig) finish(ref faucet.DeliveryRef, now time.Time, f faucet.Finish) faucet.FinishResult {
	r.t.Helper()
	res, err := r.s.FinishDelivery(r.ctx, ref, now, f)
	require.NoError(r.t, err)
	return res
}

func (r *rig) targets(now time.Time, maxPending int32) []faucet.ScanTarget {
	r.t.Helper()
	targets, err := r.s.ScanTargets(r.ctx, now, maxPending, 100)
	require.NoError(r.t, err)
	return targets
}

// status reads the subscription's recorded delivery status through a refresh,
// which keeps it.
func (r *rig) status() faucet.DeliveryStatus {
	r.t.Helper()
	row, err := r.s.UpsertSubscription(r.ctx, r.sub, r.now, 10)
	require.NoError(r.t, err)
	return row.Status
}

func (r *rig) pending() int32 {
	r.t.Helper()
	r.addEvent()
	for _, target := range r.targets(r.now, faucet.MaxPendingDeliveries) {
		if target.KeyHash == r.sub.KeyHash {
			return target.Pending
		}
	}
	r.t.Fatal("the subscription is not a scan target")
	return 0
}

func scanTargetsListsLiveSubscriptionsBehindTheHead(t *testing.T, s faucet.Store) {
	r := newRig(t, s, 3)
	head := r.events[2].ID

	caughtUp := subscriptionAt(2, r.now.Add(time.Hour))
	caughtUp.Cursor = head
	lapsed := subscriptionAt(3, r.now.Add(time.Minute))
	lapsed.Cursor = 0
	ahead := subscriptionAt(4, r.now.Add(time.Hour))
	ahead.Cursor = r.events[0].ID
	for _, sub := range []faucet.Subscription{caughtUp, lapsed, ahead} {
		_, err := s.UpsertSubscription(r.ctx, sub, r.now, 10)
		require.NoError(t, err)
	}

	later := r.now.Add(2 * time.Minute)
	targets := r.targets(later, 10)
	require.Len(t, targets, 2, "the caught up subscription and the lapsed one are not targets")
	require.Equal(t, r.sub.KeyHash, targets[0].KeyHash, "the subscription furthest behind comes first")
	require.Equal(t, ahead.KeyHash, targets[1].KeyHash)
	first := targets[0]
	require.Equal(t, "ci.run_completed", first.EventType)
	require.JSONEq(t, `{"repo":"octo/repo"}`, string(first.Arguments))
	require.EqualValues(t, 42, first.RepoID)
	require.Equal(t, r.sub.CallbackURL, first.CallbackURL, "the target carries where a gap envelope goes")
	require.Equal(t, r.sub.SecretEnc, first.SecretEnc, "and the sealed secret that signs it")
	require.EqualValues(t, 0, first.ScannedThrough)
	require.Zero(t, first.Pending)

	limited, err := s.ScanTargets(r.ctx, later, 10, 1)
	require.NoError(t, err)
	require.Len(t, limited, 1, "the limit applies")
	require.Equal(t, r.sub.KeyHash, limited[0].KeyHash)

	r.enqueue(r.events...)
	targets = r.targets(later, 10)
	require.Len(t, targets, 1, "a subscription that has read to the head is not a target")
	require.Equal(t, ahead.KeyHash, targets[0].KeyHash)
}

func scanTargetsSkipsSubscriptionsAtThePendingCap(t *testing.T, s faucet.Store) {
	r := newRig(t, s, 3)
	r.enqueue(r.events[:2]...)
	r.addEvent()

	require.Len(t, r.targets(r.now, 3), 1, "two unsent deliveries are under a cap of 3")
	targets := r.targets(r.now, 2)
	require.Empty(t, targets, "a subscription with as many unsent deliveries as the cap is not a target")
	targets = r.targets(r.now, 3)
	require.Len(t, targets, 1)
	require.EqualValues(t, 2, targets[0].Pending)
}

func scanTargetsNeedsAnEventOfTheSubscriptionsRepositoryAndType(t *testing.T, s faucet.Store) {
	r := newRig(t, s, 0)
	other := func(repoID int64, eventType string) {
		t.Helper()
		ev := newEvent(fmt.Sprintf("other-%d-%s", repoID, eventType))
		ev.RepoID, ev.Type = repoID, eventType
		_, _, err := s.InsertEvent(r.ctx, ev)
		require.NoError(t, err)
	}

	other(43, "ci.run_completed")
	other(42, "pr.closed")
	require.Empty(t, r.targets(r.now, 10), "events of another repository or another type are nothing to read")

	other(42, "ci.run_completed")
	targets := r.targets(r.now, 10)
	require.Len(t, targets, 1, "an event of the subscription's repository and type is")
	require.EqualValues(t, 0, targets[0].ScannedThrough)
}

func scanTargetsIncludesASubscriptionThatAPurgeOvertook(t *testing.T, s faucet.Store) {
	r := newRig(t, s, 3)
	_, err := s.Purge(r.ctx, time.Now().Add(time.Hour))
	require.NoError(t, err)

	targets := r.targets(r.now, 10)
	require.Len(t, targets, 1, "the purge mark is the head, and the subscription is below it")
	require.EqualValues(t, 0, targets[0].ScannedThrough)
}

func enqueueDeliveriesMovesThePositionOnlyForTheCaller(t *testing.T, s faucet.Store) {
	r := newRig(t, s, 4)
	e := faucet.Enqueue{
		Key:                    r.sub.KeyHash,
		ExpectedScannedThrough: 0,
		NewScannedThrough:      r.events[2].ID,
		Deliveries: []faucet.NewDelivery{
			{EventID: r.events[0].ID, MessageID: "msg_a"},
			{EventID: r.events[2].ID, MessageID: "msg_b"},
		},
	}

	won, err := s.EnqueueDeliveries(r.ctx, r.now, e)
	require.NoError(t, err)
	require.True(t, won)
	targets := r.targets(r.now, 10)
	require.Len(t, targets, 1)
	require.Equal(t, r.events[2].ID, targets[0].ScannedThrough, "the position moved to the last event read, matching or not")
	require.EqualValues(t, 2, targets[0].Pending)

	again := e
	again.Deliveries = []faucet.NewDelivery{{EventID: r.events[1].ID, MessageID: "msg_c"}}
	won, err = s.EnqueueDeliveries(r.ctx, r.now, again)
	require.NoError(t, err)
	require.False(t, won, "a caller that read from a position the subscription has left queues nothing")
	require.EqualValues(t, 2, r.targets(r.now, 10)[0].Pending)

	empty := faucet.Enqueue{Key: r.sub.KeyHash, ExpectedScannedThrough: r.events[2].ID, NewScannedThrough: r.events[3].ID}
	won, err = s.EnqueueDeliveries(r.ctx, r.now, empty)
	require.NoError(t, err)
	require.True(t, won, "a read that found nothing still moves the position")
	require.Empty(t, r.targets(r.now, 10), "the subscription has read to the head")

	r.addEvent()
	lapsed := faucet.Enqueue{Key: r.sub.KeyHash, ExpectedScannedThrough: r.events[3].ID, NewScannedThrough: r.events[4].ID}
	won, err = s.EnqueueDeliveries(r.ctx, r.now.Add(2*time.Hour), lapsed)
	require.NoError(t, err)
	require.False(t, won, "an expired subscription queues nothing")

	unknown := faucet.Enqueue{Key: faucet.SubscriptionKeyHash{1}, ExpectedScannedThrough: 0, NewScannedThrough: 1}
	won, err = s.EnqueueDeliveries(r.ctx, r.now, unknown)
	require.NoError(t, err)
	require.False(t, won, "an unknown subscription queues nothing")
}

func enqueueDeliveriesWinsOnceUnderConcurrency(t *testing.T, s faucet.Store) {
	const callers = 8
	r := newRig(t, s, 3)

	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		wins int
	)
	start := make(chan struct{})
	for n := range callers {
		wg.Go(func() {
			e := faucet.Enqueue{Key: r.sub.KeyHash, ExpectedScannedThrough: 0, NewScannedThrough: r.events[2].ID}
			for _, ev := range r.events {
				e.Deliveries = append(e.Deliveries, faucet.NewDelivery{EventID: ev.ID, MessageID: fmt.Sprintf("msg_%d_%d", n, ev.ID)})
			}
			<-start
			won, err := s.EnqueueDeliveries(r.ctx, r.now, e)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				t.Errorf("caller %d: %v", n, err)
			}
			if won {
				wins++
			}
		})
	}
	close(start)
	wg.Wait()
	require.Equal(t, 1, wins, "one caller moves the position and queues the deliveries")
	require.EqualValues(t, 3, r.pending(), "the deliveries are queued once")
}

func enqueueDeliveriesValidatesInput(t *testing.T, s faucet.Store) {
	r := newRig(t, s, 2)
	a, b := r.events[0].ID, r.events[1].ID
	bad := map[string]faucet.Enqueue{
		"position moves back":     {Key: r.sub.KeyHash, ExpectedScannedThrough: b, NewScannedThrough: a},
		"negative position":       {Key: r.sub.KeyHash, ExpectedScannedThrough: -1, NewScannedThrough: a},
		"event below the old":     {Key: r.sub.KeyHash, ExpectedScannedThrough: a, NewScannedThrough: b, Deliveries: []faucet.NewDelivery{{EventID: a, MessageID: "msg_a"}}},
		"event above the new":     {Key: r.sub.KeyHash, ExpectedScannedThrough: 0, NewScannedThrough: a, Deliveries: []faucet.NewDelivery{{EventID: b, MessageID: "msg_a"}}},
		"message ID not a header": {Key: r.sub.KeyHash, ExpectedScannedThrough: 0, NewScannedThrough: a, Deliveries: []faucet.NewDelivery{{EventID: a, MessageID: "msg a"}}},
		"empty message ID":        {Key: r.sub.KeyHash, ExpectedScannedThrough: 0, NewScannedThrough: a, Deliveries: []faucet.NewDelivery{{EventID: a}}},
	}
	for name, e := range bad {
		won, err := s.EnqueueDeliveries(r.ctx, r.now, e)
		require.Error(t, err, name)
		require.False(t, won, name)
	}
	require.Zero(t, r.pending(), "a refused request queues nothing")
}

func claimDeliveryReturnsTheDeliveryWithItsEventSecretAndWatermark(t *testing.T, s faucet.Store) {
	r := newRig(t, s, 2)
	r.enqueue(r.events...)

	first := r.mustClaim(r.now)
	require.Equal(t, r.sub.KeyHash, first.Ref.Key)
	require.Equal(t, r.events[0].ID, first.Ref.EventID, "the earliest event is due first")
	require.EqualValues(t, 1, first.Ref.Attempt)
	require.Equal(t, messageIDOf(r.events[0]), first.MessageID)
	require.True(t, r.now.Equal(first.EnqueuedAt))
	require.Equal(t, r.sub.CallbackURL, first.CallbackURL)
	require.Equal(t, r.sub.SecretEnc, first.SecretEnc)
	require.EqualValues(t, 42, first.RepoID)
	require.Equal(t, r.events[0].ID-1, first.Watermark)
	require.NotNil(t, first.Event)
	requireSameEvent(t, r.events[0], *first.Event)

	second := r.mustClaim(r.now)
	require.Equal(t, r.events[1].ID, second.Ref.EventID)
	require.Equal(t, r.events[0].ID-1, second.Watermark, "the first event is unsent, so the watermark stays below it")

	_, ok := r.claim(r.now)
	require.False(t, ok, "both deliveries are held")
}

func claimDeliveryGivesOneAttemptToOneCaller(t *testing.T, s faucet.Store) {
	const callers = 8
	r := newRig(t, s, 1)
	r.enqueue(r.events...)

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		claimed []faucet.DeliveryClaim
	)
	start := make(chan struct{})
	for n := range callers {
		wg.Go(func() {
			<-start
			claim, ok, err := s.ClaimDelivery(r.ctx, r.now, testLease)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				t.Errorf("caller %d: %v", n, err)
			}
			if ok {
				claimed = append(claimed, claim)
			}
		})
	}
	close(start)
	wg.Wait()
	require.Len(t, claimed, 1, "one caller holds the attempt")
	require.EqualValues(t, 1, claimed[0].Ref.Attempt)
}

func claimDeliveryLeaseRunsOutAndTheNextAttemptCounts(t *testing.T, s faucet.Store) {
	r := newRig(t, s, 1)
	r.enqueue(r.events...)
	first := r.mustClaim(r.now)

	_, ok := r.claim(r.now.Add(testLease - time.Second))
	require.False(t, ok, "the lease still holds")

	retaken := r.mustClaim(r.now.Add(testLease))
	require.EqualValues(t, 2, retaken.Ref.Attempt, "a claim whose run never finished used its attempt")
	require.Equal(t, first.MessageID, retaken.MessageID, "the message ID is the same on every attempt")

	res := r.finish(first.Ref, r.now.Add(testLease), faucet.Finish{Kind: faucet.FinishDelivered})
	require.False(t, res.Accepted, "the first run is late, and the second owns the delivery")
	res = r.finish(retaken.Ref, r.now.Add(testLease), faucet.Finish{Kind: faucet.FinishDelivered})
	require.True(t, res.Accepted)
}

func claimDeliverySkipsExpiredSubscriptions(t *testing.T, s faucet.Store) {
	r := newRig(t, s, 1)
	r.enqueue(r.events...)

	_, ok := r.claim(r.sub.ExpiresAt)
	require.False(t, ok, "an expired subscription gets no attempt")
	claim, ok := r.claim(r.sub.ExpiresAt.Add(-time.Second))
	require.True(t, ok, "one second before its expiry it does")
	require.EqualValues(t, 1, claim.Ref.Attempt, "the refused claim took no attempt")
}

func claimDeliveryReturnsNoEventAfterAPurge(t *testing.T, s faucet.Store) {
	r := newRig(t, s, 2)
	r.enqueue(r.events...)
	_, err := s.Purge(r.ctx, time.Now().Add(time.Hour))
	require.NoError(t, err)

	claim := r.mustClaim(r.now)
	require.Nil(t, claim.Event, "the purge deleted the event, and the claim says so")
	require.Equal(t, r.events[0].ID, claim.Ref.EventID)
}

func finishDeliveryNeedsTheCurrentAttempt(t *testing.T, s faucet.Store) {
	r := newRig(t, s, 1)
	r.enqueue(r.events...)
	claim := r.mustClaim(r.now)

	for _, attempt := range []int32{2, claim.Ref.Attempt + 5} {
		stale := claim.Ref
		stale.Attempt = attempt
		res := r.finish(stale, r.now, faucet.Finish{Kind: faucet.FinishDelivered})
		require.False(t, res.Accepted, "attempt %d is not the current one", attempt)
		require.Zero(t, res.Removed)
		res = r.finish(stale, r.now, faucet.Finish{Kind: faucet.FinishRetry, RetryAt: r.now.Add(time.Hour), Failure: faucet.CallbackTimeout})
		require.False(t, res.Accepted, "a late retry of attempt %d does not move the lease or the next attempt", attempt)
	}
	unclaimed := claim.Ref
	unclaimed.Attempt = 0
	_, err := s.FinishDelivery(r.ctx, unclaimed, r.now, faucet.Finish{Kind: faucet.FinishDelivered})
	require.Error(t, err, "attempt 0 is no attempt, and a delivery that no one has claimed is at 0")
	require.Equal(t, faucet.DeliveryStatus{}, r.status(), "a refused finish writes no status")
	reborn := claim.Ref
	reborn.MessageID = "msg_from_an_older_subscription"
	require.False(t, r.finish(reborn, r.now, faucet.Finish{Kind: faucet.FinishDelivered}).Accepted,
		"a row with another message ID belongs to another incarnation of the subscription")
	again := r.mustClaim(r.now.Add(testLease))
	require.EqualValues(t, 2, again.Ref.Attempt, "the delivery is still queued")

	gone := claim.Ref
	gone.EventID = claim.Ref.EventID + 1000
	require.False(t, r.finish(gone, r.now, faucet.Finish{Kind: faucet.FinishDelivered}).Accepted, "an unknown delivery")
}

func finishDeliveryRetryReschedulesAndRecordsTheFailure(t *testing.T, s faucet.Store) {
	r := newRig(t, s, 1)
	r.enqueue(r.events...)
	first := r.mustClaim(r.now)

	retryAt := r.now.Add(15 * time.Second)
	res := r.finish(first.Ref, r.now, faucet.Finish{Kind: faucet.FinishRetry, RetryAt: retryAt, Failure: faucet.CallbackHTTP5xx})
	require.True(t, res.Accepted)
	require.Zero(t, res.Removed, "a retry ends nothing")
	status := r.status()
	require.Equal(t, faucet.CallbackHTTP5xx, status.LastError)
	require.True(t, r.now.Equal(status.FailedSince), "failedSince is the first failure, got %s", status.FailedSince)
	require.True(t, status.LastDeliveryAt.IsZero())

	_, ok := r.claim(retryAt.Add(-time.Second))
	require.False(t, ok, "the retry is not due yet, and the lease was released")
	second := r.mustClaim(retryAt)
	require.EqualValues(t, 2, second.Ref.Attempt)

	later := retryAt.Add(time.Minute)
	r.finish(second.Ref, retryAt, faucet.Finish{Kind: faucet.FinishRetry, RetryAt: later, Failure: faucet.CallbackTimeout})
	status = r.status()
	require.Equal(t, faucet.CallbackTimeout, status.LastError, "lastError is the latest failure")
	require.True(t, r.now.Equal(status.FailedSince), "failedSince keeps the first failure")
}

func finishDeliveryDeliveredEndsTheDeliveryAndClearsTheFailure(t *testing.T, s faucet.Store) {
	r := newRig(t, s, 2)
	r.enqueue(r.events...)
	first := r.mustClaim(r.now)
	r.finish(first.Ref, r.now, faucet.Finish{Kind: faucet.FinishRetry, RetryAt: r.now.Add(time.Second), Failure: faucet.CallbackHTTP4xx})
	require.Equal(t, faucet.CallbackHTTP4xx, r.status().LastError)

	deliveredAt := r.now.Add(time.Minute)
	second := r.mustClaim(deliveredAt)
	require.Equal(t, r.events[1].ID, second.Ref.EventID, "the first event waits for its retry")
	res := r.finish(second.Ref, deliveredAt, faucet.Finish{Kind: faucet.FinishDelivered})
	require.True(t, res.Accepted)
	require.EqualValues(t, 1, res.Removed)
	status := r.status()
	require.True(t, deliveredAt.Equal(status.LastDeliveryAt), "lastDeliveryAt is the time of the 2xx, got %s", status.LastDeliveryAt)
	require.Empty(t, status.LastError, "a 2xx clears the failure")
	require.True(t, status.FailedSince.IsZero())

	res = r.finish(second.Ref, deliveredAt, faucet.Finish{Kind: faucet.FinishDelivered})
	require.False(t, res.Accepted, "a delivery ends once")
	next := r.mustClaim(deliveredAt)
	require.Equal(t, r.events[0].ID, next.Ref.EventID, "the delivery that ended is not claimed again")
	require.EqualValues(t, 2, next.Ref.Attempt)
}

func finishDeliveryGiveUpRecordsOnlyWhatTheEndpointSaid(t *testing.T, s faucet.Store) {
	r := newRig(t, s, 2)
	r.enqueue(r.events...)

	ours := r.mustClaim(r.now)
	res := r.finish(ours.Ref, r.now, faucet.Finish{Kind: faucet.FinishGiveUp})
	require.True(t, res.Accepted)
	require.EqualValues(t, 1, res.Removed)
	require.Equal(t, faucet.DeliveryStatus{}, r.status(), "a give-up that faucet chose says nothing about the endpoint")

	theirs := r.mustClaim(r.now)
	require.Equal(t, r.events[1].ID, theirs.Ref.EventID)
	res = r.finish(theirs.Ref, r.now, faucet.Finish{Kind: faucet.FinishGiveUp, Failure: faucet.CallbackHTTP4xx})
	require.True(t, res.Accepted)
	status := r.status()
	require.Equal(t, faucet.CallbackHTTP4xx, status.LastError)
	require.False(t, status.FailedSince.IsZero())
	_, ok := r.claim(r.now.Add(time.Hour))
	require.False(t, ok, "both deliveries ended")
}

func finishDeliveryDeadDeletesTheSubscriptionOnlyForTheSameSealedSecret(t *testing.T, s faucet.Store) {
	r := newRig(t, s, 3)
	r.enqueue(r.events...)
	claim := r.mustClaim(r.now)

	refreshed := r.sub
	refreshed.SecretEnc = []byte("sealed-2")
	_, err := s.UpsertSubscription(r.ctx, refreshed, r.now, 10)
	require.NoError(t, err)

	res := r.finish(claim.Ref, r.now, faucet.Finish{Kind: faucet.FinishDead, Unreadable: claim.SecretEnc})
	require.True(t, res.Accepted)
	require.EqualValues(t, 1, res.Removed, "a refresh sealed a new secret, so only this delivery ends")
	other := r.mustClaim(r.now)
	require.Equal(t, []byte("sealed-2"), other.SecretEnc, "the subscription is still there with its new secret")

	res = r.finish(other.Ref, r.now, faucet.Finish{Kind: faucet.FinishDead, Unreadable: other.SecretEnc})
	require.True(t, res.Accepted)
	require.EqualValues(t, 2, res.Removed, "the subscription is deleted, and with it the delivery that was still queued")
	deleted, err := s.DeleteSubscription(r.ctx, r.sub.Principal, r.sub.KeyHash)
	require.NoError(t, err)
	require.False(t, deleted, "the subscription was already deleted")
	_, ok := r.claim(r.now.Add(time.Hour))
	require.False(t, ok, "nothing is left to claim")
}

func finishDeliveryValidatesInput(t *testing.T, s faucet.Store) {
	r := newRig(t, s, 1)
	r.enqueue(r.events...)
	claim := r.mustClaim(r.now)

	bad := map[string]faucet.Finish{
		"no kind":                        {},
		"unknown kind":                   {Kind: 99},
		"retry without a time":           {Kind: faucet.FinishRetry, Failure: faucet.CallbackTimeout},
		"retry without failure":          {Kind: faucet.FinishRetry, RetryAt: r.now},
		"retry with a made-up failure":   {Kind: faucet.FinishRetry, RetryAt: r.now, Failure: "boom"},
		"delivered with a failure":       {Kind: faucet.FinishDelivered, Failure: faucet.CallbackTimeout},
		"give-up with a made-up failure": {Kind: faucet.FinishGiveUp, Failure: "boom"},
		"dead without the secret":        {Kind: faucet.FinishDead},
	}
	for name, f := range bad {
		res, err := s.FinishDelivery(r.ctx, claim.Ref, r.now, f)
		require.Error(t, err, name)
		require.False(t, res.Accepted, name)
	}
	res := r.finish(claim.Ref, r.now, faucet.Finish{Kind: faucet.FinishDelivered})
	require.True(t, res.Accepted, "the refused calls left the attempt unfinished")
}

func watermarkStaysBelowTheLowestUnsentEvent(t *testing.T, s faucet.Store) {
	r := newRig(t, s, 3)
	r.enqueue(r.events...)

	first := r.mustClaim(r.now)
	r.finish(first.Ref, r.now, faucet.Finish{Kind: faucet.FinishRetry, RetryAt: r.now.Add(10 * time.Minute), Failure: faucet.CallbackHTTP5xx})

	second := r.mustClaim(r.now)
	require.Equal(t, r.events[1].ID, second.Ref.EventID)
	require.Equal(t, r.events[0].ID-1, second.Watermark, "an earlier event is still unsent")
	r.finish(second.Ref, r.now, faucet.Finish{Kind: faucet.FinishDelivered})

	third := r.mustClaim(r.now)
	require.Equal(t, r.events[2].ID, third.Ref.EventID)
	require.Equal(t, r.events[0].ID-1, third.Watermark)
	r.finish(third.Ref, r.now, faucet.Finish{Kind: faucet.FinishDelivered})

	again := r.mustClaim(r.now.Add(11 * time.Minute))
	require.Equal(t, r.events[0].ID-1, again.Watermark, "the lowest unsent event is the one that waits")
	require.EqualValues(t, 2, again.Ref.Attempt)
	r.finish(again.Ref, r.now.Add(11*time.Minute), faucet.Finish{Kind: faucet.FinishDelivered})

	next := r.addEvent()
	r.enqueue(next)
	last := r.mustClaim(r.now.Add(11 * time.Minute))
	require.Equal(t, next.ID-1, last.Watermark, "with nothing older unsent, the watermark reaches the event before this one")
}

func expireDeliveriesLeavesLiveSubscriptionsAndHeldClaims(t *testing.T, s faucet.Store) {
	r := newRig(t, s, 2)
	expiresAt := r.now.Add(20 * time.Second)
	short := subscriptionAt(2, expiresAt)
	short.Cursor = 0
	_, err := s.UpsertSubscription(r.ctx, short, r.now, 10)
	require.NoError(t, err)
	for i, queued := range []struct {
		sub faucet.Subscription
		at  time.Time
	}{{short, r.now}, {r.sub, r.now.Add(time.Second)}} {
		won, err := s.EnqueueDeliveries(r.ctx, queued.at, faucet.Enqueue{
			Key: queued.sub.KeyHash, ExpectedScannedThrough: 0, NewScannedThrough: r.events[1].ID,
			Deliveries: []faucet.NewDelivery{
				{EventID: r.events[0].ID, MessageID: fmt.Sprintf("msg_a%d", i)},
				{EventID: r.events[1].ID, MessageID: fmt.Sprintf("msg_b%d", i)},
			},
		})
		require.NoError(t, err)
		require.True(t, won)
	}
	held := r.mustClaim(r.now)
	require.Equal(t, short.KeyHash, held.Ref.Key)

	deleted, err := s.ExpireDeliveries(r.ctx, expiresAt.Add(-time.Second))
	require.NoError(t, err)
	require.Zero(t, deleted, "nothing has expired yet")
	deleted, err = s.ExpireDeliveries(r.ctx, expiresAt)
	require.NoError(t, err)
	require.EqualValues(t, 1, deleted, "the expired subscription's delivery that no claim holds")

	res := r.finish(held.Ref, expiresAt, faucet.Finish{Kind: faucet.FinishDelivered})
	require.True(t, res.Accepted, "the held claim finishes as usual")
	deleted, err = s.ExpireDeliveries(r.ctx, expiresAt)
	require.NoError(t, err)
	require.Zero(t, deleted)
	claim := r.mustClaim(expiresAt.Add(testLease))
	require.Equal(t, r.sub.KeyHash, claim.Ref.Key, "the live subscription's deliveries are still queued")
}

func deletingASubscriptionEndsItsDeliveries(t *testing.T, s faucet.Store) {
	r := newRig(t, s, 2)
	r.enqueue(r.events...)

	deleted, err := s.DeleteSubscription(r.ctx, r.sub.Principal, r.sub.KeyHash)
	require.NoError(t, err)
	require.True(t, deleted)
	_, ok := r.claim(r.now)
	require.False(t, ok, "an unsubscribed subscription's deliveries are gone")

	_, err = s.UpsertSubscription(r.ctx, r.sub, r.now, 10)
	require.NoError(t, err)
	_, ok = r.claim(r.now)
	require.False(t, ok, "a new subscription with the same key does not inherit them")

	r.scanned = 0
	r.enqueue(r.events...)
	expired, err := s.DeleteExpiredSubscriptions(r.ctx, r.sub.ExpiresAt)
	require.NoError(t, err)
	require.EqualValues(t, 1, expired)
	_, ok = r.claim(r.now)
	require.False(t, ok, "an expired subscription's deliveries go with it")
}

func scanPositionStartsAtTheCursorAndSurvivesARefresh(t *testing.T, s faucet.Store) {
	r := newRig(t, s, 3)
	started := subscriptionAt(2, r.now.Add(time.Hour))
	started.Cursor = r.events[0].ID
	_, err := s.UpsertSubscription(r.ctx, started, r.now, 10)
	require.NoError(t, err)

	positions := func() map[faucet.SubscriptionKeyHash]int64 {
		got := map[faucet.SubscriptionKeyHash]int64{}
		for _, target := range r.targets(r.now, 10) {
			got[target.KeyHash] = target.ScannedThrough
		}
		return got
	}
	require.Equal(t, map[faucet.SubscriptionKeyHash]int64{r.sub.KeyHash: 0, started.KeyHash: r.events[0].ID}, positions(),
		"a new subscription reads the log after its cursor")

	won, err := s.EnqueueDeliveries(r.ctx, r.now, faucet.Enqueue{Key: started.KeyHash, ExpectedScannedThrough: r.events[0].ID, NewScannedThrough: r.events[1].ID})
	require.NoError(t, err)
	require.True(t, won)
	refresh := started
	refresh.Cursor = 0
	_, err = s.UpsertSubscription(r.ctx, refresh, r.now, 10)
	require.NoError(t, err)
	require.Equal(t, r.events[1].ID, positions()[started.KeyHash], "a refresh of a live subscription keeps its read position")

	lapsedAt := r.now.Add(2 * time.Hour)
	replaced := started
	replaced.Cursor = r.events[2].ID
	replaced.ExpiresAt = lapsedAt.Add(time.Hour)
	_, err = s.UpsertSubscription(r.ctx, replaced, lapsedAt, 10)
	require.NoError(t, err)
	for _, target := range r.targets(lapsedAt, 10) {
		require.NotEqual(t, started.KeyHash, target.KeyHash, "a replaced row starts at its new cursor, which is the head")
	}
}

func scanTargetsAndClaimsCarryThePreviousSecret(t *testing.T, s faucet.Store) {
	r := newRig(t, s, 1)
	rotated := r.sub
	rotated.SecretEnc = []byte("sealed-2")
	rotated.RotationGrace = 5 * time.Minute
	_, err := s.UpsertSubscription(r.ctx, rotated, r.now, 10)
	require.NoError(t, err)
	until := r.now.Add(5 * time.Minute)

	targets := r.targets(r.now, 10)
	require.Len(t, targets, 1)
	require.Equal(t, []byte("sealed-2"), targets[0].SecretEnc)
	require.Equal(t, []byte("sealed-1"), targets[0].PreviousSecretEnc, "the scan target carries the secret that the gap envelope is also signed with")
	require.True(t, until.Equal(targets[0].PreviousSecretUntil), "got %s", targets[0].PreviousSecretUntil)

	r.enqueue(r.events...)
	claim := r.mustClaim(r.now)
	require.Equal(t, []byte("sealed-2"), claim.SecretEnc)
	require.Equal(t, []byte("sealed-1"), claim.PreviousSecretEnc, "the claim carries the secret that the delivery is also signed with")
	require.True(t, until.Equal(claim.PreviousSecretUntil), "got %s", claim.PreviousSecretUntil)
}
