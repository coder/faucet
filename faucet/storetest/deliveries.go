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
	{"ClaimsCarryThePreviousSecret", claimsCarryThePreviousSecret},
	{"ClaimDeliveryPassesOverTheOriginsItIsToldTo", claimDeliveryPassesOverTheOriginsItIsToldTo},
	{"ClaimsSkipTheOriginThatCallbackOriginNamesForAnyWayOfWritingTheURL", claimsSkipTheOriginThatCallbackOriginNamesForAnyWayOfWritingTheURL},
	{"ClaimGapHandsOutARecordedGapToOneCallerUntilItIsFinished", claimGapHandsOutARecordedGapToOneCallerUntilItIsFinished},
	{"FinishGapIgnoresAnOlderIncarnationAndKeepsANewerGap", finishGapIgnoresAnOlderIncarnationAndKeepsANewerGap},
	{"EnqueueDeliveriesKeepsAnOwedGapAndItsHigherMark", enqueueDeliveriesKeepsAnOwedGapAndItsHigherMark},
	{"ClaimGapSkipsExpiredSubscriptions", claimGapSkipsExpiredSubscriptions},
	{"ClaimDeliveryGivesOneAttemptToOneCaller", claimDeliveryGivesOneAttemptToOneCaller},
	{"ClaimDeliveryLeaseRunsOutAndTheNextAttemptCounts", claimDeliveryLeaseRunsOutAndTheNextAttemptCounts},
	{"ClaimDeliverySkipsExpiredSubscriptions", claimDeliverySkipsExpiredSubscriptions},
	{"ClaimDeliveryReturnsNoEventAfterAPurge", claimDeliveryReturnsNoEventAfterAPurge},
	{"FinishDeliveryNeedsTheCurrentAttempt", finishDeliveryNeedsTheCurrentAttempt},
	{"FinishDeliveryIgnoresAnAttemptOfAnOlderIncarnation", finishDeliveryIgnoresAnAttemptOfAnOlderIncarnation},
	{"FinishDeliveryRetryReschedulesAndRecordsTheFailure", finishDeliveryRetryReschedulesAndRecordsTheFailure},
	{"FinishDeliveryDeliveredEndsTheDeliveryAndClearsTheFailure", finishDeliveryDeliveredEndsTheDeliveryAndClearsTheFailure},
	{"FinishDeliveryGiveUpRecordsOnlyWhatTheEndpointSaid", finishDeliveryGiveUpRecordsOnlyWhatTheEndpointSaid},
	{"FinishDeliveryDeadDeletesTheSubscriptionOnlyForTheSameSealedSecret", finishDeliveryDeadDeletesTheSubscriptionOnlyForTheSameSealedSecret},
	{"FinishDeliveryValidatesInput", finishDeliveryValidatesInput},
	{"WatermarkStaysBelowTheLowestUnsentEvent", watermarkStaysBelowTheLowestUnsentEvent},
	{"ExpireDeliveriesLeavesLiveSubscriptionsAndHeldClaims", expireDeliveriesLeavesLiveSubscriptionsAndHeldClaims},
	{"DeletingASubscriptionEndsItsDeliveries", deletingASubscriptionEndsItsDeliveries},
	{"ScanPositionStartsAtTheCursorAndSurvivesARefresh", scanPositionStartsAtTheCursorAndSurvivesARefresh},
	{"PurgeLeavesBelowTheMarkOnlyTheSubscriptionsThatLostEvents", purgeLeavesBelowTheMarkOnlyTheSubscriptionsThatLostEvents},
	{"PurgeMovesASubscriptionPastEventsOfAnotherTypeOfItsRepository", purgeMovesASubscriptionPastEventsOfAnotherTypeOfItsRepository},
	{"ClaimDeliveryCarriesTheRepositoryTheSubscriptionIsGrantedNow", claimDeliveryCarriesTheRepositoryTheSubscriptionIsGrantedNow},
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

func (r *rig) recordGap(through int64) {
	r.t.Helper()
	e := faucet.Enqueue{Key: r.sub.KeyHash, ExpectedScannedThrough: r.scanned, NewScannedThrough: through, GapThrough: through}
	won, err := r.s.EnqueueDeliveries(r.ctx, r.now, e)
	require.NoError(r.t, err)
	require.True(r.t, won)
	r.scanned = through
}

func (r *rig) claim(now time.Time) (faucet.DeliveryClaim, bool) {
	r.t.Helper()
	claim, ok, err := r.s.ClaimDelivery(r.ctx, now, testLease, nil)
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
	require.Equal(t, r.sub.Principal, targets[0].Principal, "a target names the principal that owns it")
	first := targets[0]
	require.Equal(t, "ci.run_completed", first.EventType)
	require.JSONEq(t, `{"repo":"octo/repo"}`, string(first.Arguments))
	require.EqualValues(t, 42, first.RepoID)
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
			claim, ok, err := s.ClaimDelivery(r.ctx, r.now, testLease, nil)
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
	reborn.MessageID = "msg_of_another_delivery"
	require.False(t, r.finish(reborn, r.now, faucet.Finish{Kind: faucet.FinishDelivered}).Accepted,
		"a row with another message ID is not the delivery that the claim saw")
	again := r.mustClaim(r.now.Add(testLease))
	require.EqualValues(t, 2, again.Ref.Attempt, "the delivery is still queued")

	gone := claim.Ref
	gone.EventID = claim.Ref.EventID + 1000
	require.False(t, r.finish(gone, r.now, faucet.Finish{Kind: faucet.FinishDelivered}).Accepted, "an unknown delivery")
}

func finishDeliveryIgnoresAnAttemptOfAnOlderIncarnation(t *testing.T, s faucet.Store) {
	r := newRig(t, s, 1)
	r.enqueue(r.events...)
	older := r.mustClaim(r.now)

	deleted, err := s.DeleteSubscription(r.ctx, r.sub.Principal, r.sub.KeyHash)
	require.NoError(t, err)
	require.True(t, deleted)
	r.now = r.now.Add(time.Second)
	_, err = s.UpsertSubscription(r.ctx, r.sub, r.now, 10)
	require.NoError(t, err)
	r.scanned = 0
	r.enqueue(r.events...)
	newer := r.mustClaim(r.now)
	require.Equal(t, older.Ref.MessageID, newer.Ref.MessageID)
	require.Equal(t, older.Ref.Attempt, newer.Ref.Attempt)

	res := r.finish(older.Ref, r.now, faucet.Finish{Kind: faucet.FinishGiveUp})
	require.False(t, res.Accepted, "the result of the older incarnation's attempt ends nothing of the new one")
	require.Zero(t, res.Removed)
	require.True(t, r.finish(newer.Ref, r.now, faucet.Finish{Kind: faucet.FinishDelivered}).Accepted,
		"the delivery was still queued for the new incarnation's own attempt")
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

func claimsCarryThePreviousSecret(t *testing.T, s faucet.Store) {
	r := newRig(t, s, 1)
	rotated := r.sub
	rotated.SecretEnc = []byte("sealed-2")
	rotated.RotationGrace = 5 * time.Minute
	_, err := s.UpsertSubscription(r.ctx, rotated, r.now, 10)
	require.NoError(t, err)
	until := r.now.Add(5 * time.Minute)

	r.enqueue(r.events...)
	claim := r.mustClaim(r.now)
	require.Equal(t, []byte("sealed-2"), claim.SecretEnc)
	require.Equal(t, []byte("sealed-1"), claim.PreviousSecretEnc, "the claim carries the secret that the delivery is also signed with")
	require.True(t, until.Equal(claim.PreviousSecretUntil), "got %s", claim.PreviousSecretUntil)

	r.recordGap(r.scanned)
	gap, ok, err := s.ClaimGap(r.ctx, r.now, testLease, nil)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, []byte("sealed-2"), gap.SecretEnc)
	require.Equal(t, []byte("sealed-1"), gap.PreviousSecretEnc, "the gap claim carries the secret that the envelope is also signed with")
	require.True(t, until.Equal(gap.PreviousSecretUntil), "got %s", gap.PreviousSecretUntil)
}

// purgeLeavesBelowTheMarkOnlyTheSubscriptionsThatLostEvents pins what a gap
// means: a subscription is left below the purge mark only if the purge deleted
// an event of its repository and event type that it had not read. A
// subscription that lost none is moved to the mark, and one that was already
// below the mark stays there however many later purges pass it.
func purgeLeavesBelowTheMarkOnlyTheSubscriptionsThatLostEvents(t *testing.T, s faucet.Store) {
	r := newRig(t, s, 0)
	quiet := r.sub
	lossy := subscriptionAt(2, r.now.Add(time.Hour))
	lossy.RepoID = 77
	lossy.Cursor = 0
	_, err := s.UpsertSubscription(r.ctx, lossy, r.now, 10)
	require.NoError(t, err)

	insert := func(deliveryID string, repoID int64) faucet.Event {
		t.Helper()
		ev := newEvent(deliveryID)
		ev.RepoID = repoID
		stored, inserted, err := s.InsertEvent(r.ctx, ev)
		require.NoError(t, err)
		require.True(t, inserted)
		time.Sleep(2 * time.Millisecond)
		return stored
	}
	positions := func() map[faucet.SubscriptionKeyHash]int64 {
		t.Helper()
		got := map[faucet.SubscriptionKeyHash]int64{}
		for _, target := range r.targets(r.now, 10) {
			got[target.KeyHash] = target.ScannedThrough
		}
		return got
	}
	other1, other2 := insert("other-1", 77), insert("other-2", 77)
	survivor := insert("survivor", 42)
	later := insert("later", 99)

	res, err := s.Purge(r.ctx, survivor.ReceivedAt)
	require.NoError(t, err)
	require.Equal(t, faucet.PurgeResult{Deleted: 2, PurgedThrough: other2.ID}, res)
	require.Equal(t, map[faucet.SubscriptionKeyHash]int64{quiet.KeyHash: other2.ID, lossy.KeyHash: 0}, positions(),
		"the subscription of repository 42 lost nothing and moves to the mark, the one of repository 77 lost events and stays below it")
	require.Less(t, other1.ID, other2.ID)

	res, err = s.Purge(r.ctx, later.ReceivedAt.Add(time.Hour))
	require.NoError(t, err)
	require.Equal(t, faucet.PurgeResult{Deleted: 2, PurgedThrough: later.ID}, res)
	require.Equal(t, map[faucet.SubscriptionKeyHash]int64{quiet.KeyHash: other2.ID, lossy.KeyHash: 0}, positions(),
		"the second purge deleted the survivor of repository 42, so that subscription now lost an event, and the first loss of the other is not forgotten")
}

func claimDeliveryPassesOverTheOriginsItIsToldTo(t *testing.T, s faucet.Store) {
	r := newRig(t, s, 1)
	r.enqueue(r.events...)
	other := newSubscription("octocat", "https://other.example.test/hook", r.now.Add(time.Hour))
	other.Cursor = 0
	_, err := s.UpsertSubscription(r.ctx, other, r.now, 10)
	require.NoError(t, err)
	won, err := s.EnqueueDeliveries(r.ctx, r.now, faucet.Enqueue{
		Key: other.KeyHash, NewScannedThrough: r.events[0].ID,
		Deliveries: []faucet.NewDelivery{{EventID: r.events[0].ID, MessageID: "msg_other"}},
	})
	require.NoError(t, err)
	require.True(t, won)

	skip := []string{faucet.CallbackOrigin(r.sub.CallbackURL)}
	claim, ok, err := s.ClaimDelivery(r.ctx, r.now, testLease, skip)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, other.KeyHash, claim.Ref.Key, "the delivery of the skipped origin is passed over, although it is due first")
	_, ok, err = s.ClaimDelivery(r.ctx, r.now, testLease, skip)
	require.NoError(t, err)
	require.False(t, ok, "only a delivery of the skipped origin is left")
	claim, ok, err = s.ClaimDelivery(r.ctx, r.now, testLease, []string{})
	require.NoError(t, err)
	require.True(t, ok, "with nothing to skip it is claimed")
	require.Equal(t, r.sub.KeyHash, claim.Ref.Key)
}

// The workers' cap counts a callback by faucet.CallbackOrigin, so a store must
// match the skip list by the same rule. A stored URL that is not in canonical
// form (an older row, or a Store of the host's) must still be held to the cap.
func claimsSkipTheOriginThatCallbackOriginNamesForAnyWayOfWritingTheURL(t *testing.T, s faucet.Store) {
	ctx := context.Background()
	now := subscriptionBase
	for _, c := range []struct{ name, authority, origin string }{
		{"Canonical", "https://example.com", "https://example.com"},
		{"MixedCaseHost", "https://Example.COM", "https://example.com"},
		{"UpperCaseScheme", "HTTPS://example.com", "https://example.com"},
		{"UserInfo", "https://user:pass@example.com:8443", "https://example.com:8443"},
		{"EscapedAtInUserInfo", "https://Us%40er@EXAMPLE.com", "https://example.com"},
		{"AtInUserInfo", "https://a@b@example.com", "https://example.com"},
		{"IPv6", "https://[2001:DB8::1]:8443", "https://[2001:db8::1]:8443"},
	} {
		require.Equal(t, c.origin, faucet.CallbackOrigin(c.authority+"/hook"), c.name)
		skip := []string{c.origin}

		deliverySub := newSubscription(c.name+"-delivery", c.authority+"/delivery", now.Add(time.Hour))
		deliverySub.Cursor = 0
		_, err := s.UpsertSubscription(ctx, deliverySub, now, 10)
		require.NoError(t, err, c.name)
		ev, _, err := s.InsertEvent(ctx, newEvent("origin-"+c.name))
		require.NoError(t, err, c.name)
		won, err := s.EnqueueDeliveries(ctx, now, faucet.Enqueue{
			Key: deliverySub.KeyHash, NewScannedThrough: ev.ID,
			Deliveries: []faucet.NewDelivery{{EventID: ev.ID, MessageID: "msg_" + c.name}},
		})
		require.NoError(t, err, c.name)
		require.True(t, won, c.name)
		_, ok, err := s.ClaimDelivery(ctx, now, testLease, skip)
		require.NoError(t, err, c.name)
		require.False(t, ok, "%s: a delivery whose callback has the skipped origin is passed over", c.name)
		claim, ok, err := s.ClaimDelivery(ctx, now, testLease, []string{})
		require.NoError(t, err, c.name)
		require.True(t, ok, "%s: the delivery is due, so the skip list was the only thing that held it back", c.name)
		require.Equal(t, deliverySub.KeyHash, claim.Ref.Key, c.name)

		gapSub := newSubscription(c.name+"-gap", c.authority+"/gap", now.Add(time.Hour))
		gapSub.Cursor = 0
		_, err = s.UpsertSubscription(ctx, gapSub, now, 10)
		require.NoError(t, err, c.name)
		won, err = s.EnqueueDeliveries(ctx, now, faucet.Enqueue{Key: gapSub.KeyHash, NewScannedThrough: 5, GapThrough: 5})
		require.NoError(t, err, c.name)
		require.True(t, won, c.name)
		_, ok, err = s.ClaimGap(ctx, now, testLease, skip)
		require.NoError(t, err, c.name)
		require.False(t, ok, "%s: a gap whose callback has the skipped origin is passed over", c.name)
		gap, ok, err := s.ClaimGap(ctx, now, testLease, []string{})
		require.NoError(t, err, c.name)
		require.True(t, ok, "%s: the gap is owed, so the skip list was the only thing that held it back", c.name)
		require.Equal(t, gapSub.KeyHash, gap.Ref.Key, c.name)
	}
}

func claimGapHandsOutARecordedGapToOneCallerUntilItIsFinished(t *testing.T, s faucet.Store) {
	r := newRig(t, s, 0)
	claimAt := func(now time.Time, skip []string) (faucet.GapClaim, bool) {
		t.Helper()
		claim, ok, err := s.ClaimGap(r.ctx, now, testLease, skip)
		require.NoError(t, err)
		return claim, ok
	}
	_, ok := claimAt(r.now, nil)
	require.False(t, ok, "no gap is owed")

	r.recordGap(5)
	claim, ok := claimAt(r.now, nil)
	require.True(t, ok)
	require.Equal(t, r.sub.KeyHash, claim.Ref.Key)
	require.EqualValues(t, 5, claim.Ref.Through)
	require.True(t, r.now.Equal(claim.Ref.Incarnation), "got %s", claim.Ref.Incarnation)
	require.Equal(t, r.sub.CallbackURL, claim.CallbackURL, "the claim carries where the envelope goes")
	require.Equal(t, r.sub.SecretEnc, claim.SecretEnc, "and the sealed secret that signs it, as of the claim")

	_, ok = claimAt(r.now, nil)
	require.False(t, ok, "a claimed gap is held for its lease")
	later := r.now.Add(testLease)
	_, ok = claimAt(later, []string{faucet.CallbackOrigin(r.sub.CallbackURL)})
	require.False(t, ok, "a skipped origin gets no gap")
	_, ok = claimAt(later, nil)
	require.True(t, ok, "a run that died leaves the gap to the next run after the lease")

	require.NoError(t, s.FinishGap(r.ctx, claim.Ref))
	_, ok = claimAt(later.Add(10*testLease), nil)
	require.False(t, ok, "a finished gap is not owed any more")
}

func finishGapIgnoresAnOlderIncarnationAndKeepsANewerGap(t *testing.T, s faucet.Store) {
	r := newRig(t, s, 0)
	claim := func() faucet.GapClaim {
		t.Helper()
		claim, ok, err := s.ClaimGap(r.ctx, r.now, testLease, nil)
		require.NoError(t, err)
		require.True(t, ok, "a gap is owed")
		return claim
	}
	r.recordGap(5)
	first := claim()
	r.recordGap(9)
	require.NoError(t, s.FinishGap(r.ctx, first.Ref))
	second := claim()
	require.EqualValues(t, 9, second.Ref.Through, "a gap that grew while it was in flight stays owed, at its new mark, with no lease")

	deleted, err := s.DeleteSubscription(r.ctx, r.sub.Principal, r.sub.KeyHash)
	require.NoError(t, err)
	require.True(t, deleted)
	r.now = r.now.Add(time.Second)
	_, err = s.UpsertSubscription(r.ctx, r.sub, r.now, 10)
	require.NoError(t, err)
	r.scanned = 0
	r.recordGap(second.Ref.Through)
	require.NoError(t, s.FinishGap(r.ctx, second.Ref))
	third := claim()
	require.EqualValues(t, second.Ref.Through, third.Ref.Through, "the end of an older incarnation's gap leaves the new subscription's gap owed, also at the same mark")
}

func enqueueDeliveriesKeepsAnOwedGapAndItsHigherMark(t *testing.T, s faucet.Store) {
	r := newRig(t, s, 2)
	r.enqueue(r.events[0])
	r.recordGap(r.scanned)
	r.enqueue(r.events[1])
	first, ok, err := s.ClaimGap(r.ctx, r.now, testLease, nil)
	require.NoError(t, err)
	require.True(t, ok, "a later enqueue with no gap does not clear the gap that is owed")
	require.EqualValues(t, r.events[0].ID, first.Ref.Through)
	require.NoError(t, s.FinishGap(r.ctx, first.Ref))

	r.recordGap(r.scanned)
	lower := faucet.Enqueue{Key: r.sub.KeyHash, ExpectedScannedThrough: r.scanned, NewScannedThrough: r.scanned, GapThrough: r.events[0].ID}
	won, err := s.EnqueueDeliveries(r.ctx, r.now, lower)
	require.NoError(t, err)
	require.True(t, won)
	second, ok, err := s.ClaimGap(r.ctx, r.now, testLease, nil)
	require.NoError(t, err)
	require.True(t, ok)
	require.EqualValues(t, r.events[1].ID, second.Ref.Through, "a lower mark does not lower the one that is owed")
}

func claimGapSkipsExpiredSubscriptions(t *testing.T, s faucet.Store) {
	r := newRig(t, s, 0)
	r.recordGap(5)

	_, ok, err := s.ClaimGap(r.ctx, r.sub.ExpiresAt, testLease, nil)
	require.NoError(t, err)
	require.False(t, ok, "an expired subscription is owed no gap: it is gone, and a purge deletes it")
	claim, ok, err := s.ClaimGap(r.ctx, r.sub.ExpiresAt.Add(-time.Second), testLease, nil)
	require.NoError(t, err)
	require.True(t, ok, "one second before its expiry it is")
	require.EqualValues(t, 5, claim.Ref.Through, "the refused claim took no lease")
}

func purgeMovesASubscriptionPastEventsOfAnotherTypeOfItsRepository(t *testing.T, s faucet.Store) {
	r := newRig(t, s, 0)
	insert := func(deliveryID, eventType string) faucet.Event {
		t.Helper()
		ev := newEvent(deliveryID)
		ev.Type = eventType
		stored, inserted, err := s.InsertEvent(r.ctx, ev)
		require.NoError(t, err)
		require.True(t, inserted)
		time.Sleep(2 * time.Millisecond)
		return stored
	}
	otherType := insert("other-type", "pr.closed")
	survivor := insert("survivor", "ci.run_completed")

	_, err := s.Purge(r.ctx, survivor.ReceivedAt)
	require.NoError(t, err)
	targets := r.targets(r.now, 10)
	require.Len(t, targets, 1)
	require.Equal(t, otherType.ID, targets[0].ScannedThrough,
		"the purge deleted no event of the subscription's type, so it moves to the mark and no gap is owed")

	lost := insert("lost", "ci.run_completed")
	_, err = s.Purge(r.ctx, lost.ReceivedAt.Add(time.Hour))
	require.NoError(t, err)
	for _, target := range r.targets(r.now, 10) {
		require.Equal(t, otherType.ID, target.ScannedThrough, "a purge of an event of its own type leaves it below the mark: that is a gap")
	}
}

func claimDeliveryCarriesTheRepositoryTheSubscriptionIsGrantedNow(t *testing.T, s faucet.Store) {
	r := newRig(t, s, 1)
	r.enqueue(r.events...)
	moved := r.sub
	moved.RepoID = 77
	_, err := s.UpsertSubscription(r.ctx, moved, r.now, 10)
	require.NoError(t, err)

	claim := r.mustClaim(r.now)
	require.EqualValues(t, 77, claim.RepoID, "the claim carries the repository that the subscription is granted now")
	require.NotNil(t, claim.Event)
	require.EqualValues(t, 42, claim.Event.RepoID, "and the event keeps its own, so that the run can tell they differ")
}
