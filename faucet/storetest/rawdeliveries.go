package storetest

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/coder/superblink-events/faucet"
)

// rawDeliverySubtests are the contract cases of raw deliveries and gaps.
var rawDeliverySubtests = []rawSubtest{
	{"ClaimRawDeliveryTakesTheDueTimeThenTheRawIDAndHoldsItForTheLease", claimRawDeliveryTakesTheDueTimeThenTheRawIDAndHoldsItForTheLease},
	{"ClaimRawDeliveryReturnsTheSubscriptionAndTheStoredEvent", claimRawDeliveryReturnsTheSubscriptionAndTheStoredEvent},
	{"ClaimRawDeliveryWatermarkStaysBelowTheLowestUnsentDelivery", claimRawDeliveryWatermarkStaysBelowTheLowestUnsentDelivery},
	{"FinishRawDeliveryRetriesOrEndsAndRecordsTheStatus", finishRawDeliveryRetriesOrEndsAndRecordsTheStatus},
	{"FinishRawDeliveryRefusesAnAttemptThatIsNotTheCurrentOne", finishRawDeliveryRefusesAnAttemptThatIsNotTheCurrentOne},
	{"FinishRawDeliveryThatOwesAGapWritesItWithTheEnd", finishRawDeliveryThatOwesAGapWritesItWithTheEnd},
	{"ClaimRawDeliveryPassesOverTheOriginsItIsToldTo", claimRawDeliveryPassesOverTheOriginsItIsToldTo},
	{"FinishRawDeliveryDeadDeletesTheSubscriptionOnlyForTheSameSealedSecret", finishRawDeliveryDeadDeletesTheSubscriptionOnlyForTheSameSealedSecret},
	{"ClaimRawDeliveryNeedsALiveSubscription", claimRawDeliveryNeedsALiveSubscription},
	{"ClaimRawDeliveryGivesASubscriptionMadeAgainOnlyItsOwnDeliveries", claimRawDeliveryGivesASubscriptionMadeAgainOnlyItsOwnDeliveries},
	{"ClaimRawDeliveryReturnsNoEventAfterAPurge", claimRawDeliveryReturnsNoEventAfterAPurge},
	{"ClaimRawGapHandsOutAPlannedGapOncePerLeaseUntilItIsFinished", claimRawGapHandsOutAPlannedGapOncePerLeaseUntilItIsFinished},
	{"FinishRawGapKeepsAGapThatGrewWhileItWasClaimed", finishRawGapKeepsAGapThatGrewWhileItWasClaimed},
	{"ClaimRawGapNeedsTheLiveIncarnation", claimRawGapNeedsTheLiveIncarnation},
	{"ExpireRawDeliveriesDeletesWhatNoLiveSubscriptionHolds", expireRawDeliveriesDeletesWhatNoLiveSubscriptionHolds},
}

// deliveryOf returns the claim of the subscription's delivery of the raw ID.
func deliveryOf(t *testing.T, claims []faucet.RawDeliveryClaim, sub faucet.Subscription, id int64) faucet.RawDeliveryClaim {
	t.Helper()
	for _, c := range claims {
		if c.Ref.Key == sub.KeyHash && c.Ref.EventID == id {
			return c
		}
	}
	require.Failf(t, "no claim", "no claim of raw event %d for the subscription", id)
	return faucet.RawDeliveryClaim{}
}

func deliveryRefIDs(claims []faucet.RawDeliveryClaim) []int64 {
	ids := make([]int64, 0, len(claims))
	for _, c := range claims {
		ids = append(ids, c.Ref.EventID)
	}
	return ids
}

func claimRawDeliveryTakesTheDueTimeThenTheRawIDAndHoldsItForTheLease(t *testing.T, s RawStore) {
	r := newRawRig(t, s)
	r.subscribe(1, rawSpec{})
	later := r.now.Add(time.Second)
	late := r.ingestAt(r.newDelivery(), later, serveAll(rawBody)).ID
	first, second := r.push(), r.push()

	claims := r.claimAll(later)
	require.Equal(t, []int64{first, second, late}, deliveryRefIDs(claims), "the earliest due time first, then the lowest raw ID")
	for _, c := range claims {
		require.EqualValues(t, 1, c.Ref.Attempt, "the first claim is attempt 1")
	}
	_, ok := r.claim(later.Add(testLease - time.Second))
	require.False(t, ok, "a claim holds a delivery until its lease runs out")

	claims = r.claimAll(later.Add(testLease))
	require.Equal(t, []int64{first, second, late}, deliveryRefIDs(claims), "a delivery whose lease ran out is claimable again")
	for _, c := range claims {
		require.EqualValues(t, 2, c.Ref.Attempt, "every claim is one more attempt")
	}
}

func claimRawDeliveryReturnsTheSubscriptionAndTheStoredEvent(t *testing.T, s RawStore) {
	r := newRawRig(t, s)
	w := rawSpec{fields: []string{"ref"}}
	sub := r.subscribe(1, w)
	rotated := newRawSubscription(1, w, r.now.Add(time.Hour))
	rotated.SecretEnc = []byte("sealed-2")
	rotated.RotationGrace = 5 * time.Minute
	_, err := s.UpsertSubscription(r.ctx, rotated, r.now, rawMaxLive)
	require.NoError(t, err)

	d := r.newDelivery()
	d.Action = "created"
	id := r.store(d, func(interests []faucet.RawInterest) faucet.RawPlan {
		plan := serveAll(rawBody)(interests)
		plan.Fields = []string{"ref"}
		return plan
	})
	claim := r.mustClaim(r.now)

	require.Equal(t, sub.KeyHash, claim.Ref.Key)
	require.Equal(t, id, claim.Ref.EventID, "the event ID of a raw delivery is the raw ID")
	require.Empty(t, claim.Ref.MessageID, "the service derives the webhook-id of a raw delivery")
	require.EqualValues(t, 1, claim.Ref.Attempt)
	require.True(t, sub.CreatedAt.Equal(claim.Ref.Incarnation), "the incarnation is the subscription's creation time, got %s", claim.Ref.Incarnation)
	require.True(t, r.now.Equal(claim.EnqueuedAt), "a delivery is enqueued at the ingest's time, got %s", claim.EnqueuedAt)
	require.Equal(t, "github."+rawEvent, claim.EventType)
	require.JSONEq(t, string(w.arguments()), string(claim.Arguments), "the claim carries the subscription's canonical arguments")
	require.Equal(t, sub.CallbackURL, claim.CallbackURL)
	require.Equal(t, []byte("sealed-2"), claim.SecretEnc, "the claim carries the secret as of the claim")
	require.Equal(t, []byte("sealed-1"), claim.PreviousSecretEnc, "and the secret that the delivery is also signed with")
	require.True(t, r.now.Add(5*time.Minute).Equal(claim.PreviousSecretUntil), "got %s", claim.PreviousSecretUntil)
	require.Equal(t, faucet.RepoTarget(rawRepo), claim.Target)

	require.NotNil(t, claim.Event, "the event and its body are stored")
	require.Equal(t, id, claim.Event.ID)
	require.Equal(t, d.DeliveryID, claim.Event.DeliveryID)
	require.Equal(t, rawEvent, claim.Event.Event)
	require.Equal(t, "created", claim.Event.Action)
	require.Equal(t, rawRepo, claim.Event.RepoID)
	require.JSONEq(t, string(rawBody), string(claim.Event.Body))
	require.Equal(t, []string{"ref"}, claim.Event.Fields)
	require.False(t, claim.Event.ReceivedAt.IsZero(), "the store assigns the received time")
}

func claimRawDeliveryWatermarkStaysBelowTheLowestUnsentDelivery(t *testing.T, s RawStore) {
	r := newRawRig(t, s)
	delivered, behind := r.subscribe(1, rawSpec{}), r.subscribe(2, rawSpec{})
	first, second, third := r.push(), r.push(), r.push()

	claims := r.claimAll(r.now)
	require.Len(t, claims, 6)
	for _, c := range claims {
		require.Equal(t, first-1, c.Watermark, "no delivery has been sent yet")
	}
	res := r.finish(deliveryOf(t, claims, delivered, first).Ref, r.now, faucet.Finish{Kind: faucet.FinishDelivered})
	require.True(t, res.Accepted)

	claims = r.claimAll(r.now.Add(testLease))
	require.Len(t, claims, 5)
	for _, id := range []int64{second, third} {
		require.Equal(t, second-1, deliveryOf(t, claims, delivered, id).Watermark, "the first delivery was sent, so the second is the lowest unsent one")
	}
	for _, id := range []int64{first, second, third} {
		require.Equal(t, first-1, deliveryOf(t, claims, behind, id).Watermark, "another subscription's sent delivery does not move this one's watermark")
	}
}

// deliveryStatus refreshes the nth raw subscription and returns what it has
// recorded about its deliveries.
func (r *rawRig) deliveryStatus(n int, w rawSpec) faucet.DeliveryStatus {
	r.t.Helper()
	return r.subscribe(n, w).Status
}

// deliverySubscribeTo stores the nth raw subscription with another callback
// URL, and so another key.
func (r *rawRig) deliverySubscribeTo(n int, callbackURL string) faucet.Subscription {
	r.t.Helper()
	sub := newRawSubscription(n, rawSpec{}, r.now.Add(time.Hour))
	sub.CallbackURL = callbackURL
	sub.KeyHash = faucet.NewSubscriptionKeyHash(sub.Principal, callbackURL, sub.EventType, sub.Arguments)
	row, err := r.s.UpsertSubscription(r.ctx, sub, r.now, rawMaxLive)
	require.NoError(r.t, err)
	return row
}

func (r *rawRig) deliveryClaimSkipping(now time.Time, skip []string) (faucet.RawDeliveryClaim, bool) {
	r.t.Helper()
	claim, ok, err := r.s.ClaimRawDelivery(r.ctx, now, testLease, skip)
	require.NoError(r.t, err)
	return claim, ok
}

func finishRawDeliveryRetriesOrEndsAndRecordsTheStatus(t *testing.T, s RawStore) {
	r := newRawRig(t, s)
	r.subscribe(1, rawSpec{})
	first, second := r.push(), r.push()
	claims := r.claimAll(r.now)
	require.Equal(t, []int64{first, second}, deliveryRefIDs(claims))

	retryAt := r.now.Add(time.Minute)
	res := r.finish(claims[0].Ref, r.now, faucet.Finish{Kind: faucet.FinishRetry, RetryAt: retryAt, Failure: faucet.CallbackTimeout})
	require.Equal(t, faucet.FinishResult{Accepted: true}, res, "a retry keeps the delivery")
	status := r.deliveryStatus(1, rawSpec{})
	require.Equal(t, faucet.CallbackTimeout, status.LastError, "a retry with a failure records it")
	require.True(t, r.now.Equal(status.FailedSince), "got %s", status.FailedSince)
	require.True(t, status.LastDeliveryAt.IsZero(), "got %s", status.LastDeliveryAt)
	res = r.finish(claims[1].Ref, r.now.Add(time.Second), faucet.Finish{Kind: faucet.FinishRetry, RetryAt: retryAt, Failure: faucet.CallbackConnectionRefused})
	require.Equal(t, faucet.FinishResult{Accepted: true}, res)
	status = r.deliveryStatus(1, rawSpec{})
	require.Equal(t, faucet.CallbackConnectionRefused, status.LastError, "the last failure is recorded")
	require.True(t, r.now.Equal(status.FailedSince), "the first failure's time stays, got %s", status.FailedSince)

	_, ok := r.claim(retryAt.Add(-time.Second))
	require.False(t, ok, "a retried delivery is due at its retry time")
	claims = r.claimAll(retryAt)
	require.Equal(t, []int64{first, second}, deliveryRefIDs(claims))
	require.EqualValues(t, 2, claims[0].Ref.Attempt)

	res = r.finish(claims[0].Ref, retryAt, faucet.Finish{Kind: faucet.FinishDelivered})
	require.Equal(t, faucet.FinishResult{Accepted: true, Removed: 1}, res, "a delivered attempt ends the delivery")
	status = r.deliveryStatus(1, rawSpec{})
	require.True(t, retryAt.Equal(status.LastDeliveryAt), "got %s", status.LastDeliveryAt)
	require.Empty(t, status.LastError, "a delivery clears the failure")
	require.True(t, status.FailedSince.IsZero(), "got %s", status.FailedSince)

	gaveUpAt := retryAt.Add(time.Second)
	res = r.finish(claims[1].Ref, gaveUpAt, faucet.Finish{Kind: faucet.FinishGiveUp, Failure: faucet.CallbackTimeout})
	require.Equal(t, faucet.FinishResult{Accepted: true, Removed: 1}, res, "a give-up ends the delivery")
	status = r.deliveryStatus(1, rawSpec{})
	require.True(t, retryAt.Equal(status.LastDeliveryAt), "got %s", status.LastDeliveryAt)
	require.Equal(t, faucet.CallbackTimeout, status.LastError, "a give-up with a failure records it")
	require.True(t, gaveUpAt.Equal(status.FailedSince), "got %s", status.FailedSince)

	_, ok = r.claim(gaveUpAt.Add(10 * testLease))
	require.False(t, ok, "both deliveries ended")
}

func finishRawDeliveryRefusesAnAttemptThatIsNotTheCurrentOne(t *testing.T, s RawStore) {
	r := newRawRig(t, s)
	r.subscribe(1, rawSpec{})
	r.push()
	stale := r.mustClaim(r.now)
	current := r.mustClaim(r.now.Add(testLease))
	require.EqualValues(t, 2, current.Ref.Attempt)

	otherIncarnation := current.Ref
	otherIncarnation.Incarnation = current.Ref.Incarnation.Add(time.Second)
	unknown := current.Ref
	unknown.EventID = current.Ref.EventID + 1000
	for _, refused := range []struct {
		why string
		ref faucet.DeliveryRef
	}{
		{"the delivery was claimed again", stale.Ref},
		{"the ref names another incarnation", otherIncarnation},
		{"no such delivery", unknown},
	} {
		for _, f := range []faucet.Finish{
			{Kind: faucet.FinishDelivered},
			{Kind: faucet.FinishRetry, RetryAt: r.now.Add(time.Hour), Failure: faucet.CallbackTimeout},
			{Kind: faucet.FinishGiveUp, Failure: faucet.CallbackTimeout},
			{Kind: faucet.FinishDead, Unreadable: current.SecretEnc},
		} {
			require.Equal(t, faucet.FinishResult{}, r.finish(refused.ref, r.now.Add(testLease), f), "%s, so the finish is refused", refused.why)
		}
	}
	require.Equal(t, faucet.DeliveryStatus{}, r.deliveryStatus(1, rawSpec{}), "a refused finish writes no status")
	again := r.mustClaim(r.now.Add(2 * testLease))
	require.EqualValues(t, 3, again.Ref.Attempt, "the delivery is still queued, with its lease as it was")
}

func finishRawDeliveryThatOwesAGapWritesItWithTheEnd(t *testing.T, s RawStore) {
	r := newRawRig(t, s)
	sub := r.subscribe(1, rawSpec{})
	r.subscribe(2, rawSpec{})
	first, second := r.push(), r.push()
	claims := r.claimAll(r.now)
	missed := deliveryOf(t, claims, sub, first).Ref

	_, err := s.FinishRawDelivery(r.ctx, missed, r.now, faucet.Finish{Kind: faucet.FinishRetry, RetryAt: r.now.Add(time.Minute), Failure: faucet.CallbackTimeout, Gap: true})
	require.Error(t, err, "only a give-up owes a gap")
	notCurrent := missed
	notCurrent.Attempt++
	require.Equal(t, faucet.FinishResult{}, r.finish(notCurrent, r.now, faucet.Finish{Kind: faucet.FinishGiveUp, Gap: true}))
	_, ok := r.claimGap(r.now)
	require.False(t, ok, "a finish that is refused or not accepted owes no gap")

	require.Equal(t, faucet.FinishResult{Accepted: true, Removed: 1}, r.finish(missed, r.now, faucet.Finish{Kind: faucet.FinishGiveUp, Gap: true}))
	gap := r.deliveryMustClaimGap(r.now)
	require.Equal(t, sub.KeyHash, gap.Ref.Key)
	require.True(t, sub.CreatedAt.Equal(gap.Ref.Incarnation), "got %s", gap.Ref.Incarnation)
	require.Equal(t, first, gap.Ref.Through, "the gap runs through the event of the delivery")
	_, ok = r.claimGap(r.now)
	require.False(t, ok, "the other subscription, whose delivery of the event waits, is owed no gap")
	require.Equal(t, faucet.DeliveryStatus{}, r.deliveryStatus(1, rawSpec{}), "a give-up without a failure writes no status")

	require.True(t, r.finish(deliveryOf(t, claims, sub, second).Ref, r.now, faucet.Finish{Kind: faucet.FinishGiveUp, Gap: true}).Accepted)
	require.NoError(t, s.FinishRawGap(r.ctx, gap.Ref))
	grown := r.deliveryMustClaimGap(r.now)
	require.Equal(t, second, grown.Ref.Through, "a gap owed while another was claimed stays owed, through the newer event")
}

func claimRawDeliveryPassesOverTheOriginsItIsToldTo(t *testing.T, s RawStore) {
	r := newRawRig(t, s)
	skipped := r.subscribe(1, rawSpec{})
	other := r.deliverySubscribeTo(2, "https://other.example.test/raw/2")
	r.push()
	skip := []string{faucet.CallbackOrigin(skipped.CallbackURL)}

	claim, ok := r.deliveryClaimSkipping(r.now, skip)
	require.True(t, ok)
	require.Equal(t, other.KeyHash, claim.Ref.Key, "a delivery to another origin is claimed")
	_, ok = r.deliveryClaimSkipping(r.now, skip)
	require.False(t, ok, "a delivery to a skipped origin is not claimed")
	claim, ok = r.deliveryClaimSkipping(r.now, nil)
	require.True(t, ok)
	require.Equal(t, skipped.KeyHash, claim.Ref.Key)
}

func finishRawDeliveryDeadDeletesTheSubscriptionOnlyForTheSameSealedSecret(t *testing.T, s RawStore) {
	r := newRawRig(t, s)
	dead, kept := r.subscribe(1, rawSpec{}), r.subscribe(2, rawSpec{})
	first, second, third := r.push(), r.push(), r.push()
	claims := r.claimAll(r.now)
	require.Len(t, claims, 6)

	refreshed := newRawSubscription(1, rawSpec{}, r.now.Add(time.Hour))
	refreshed.SecretEnc = []byte("sealed-2")
	_, err := s.UpsertSubscription(r.ctx, refreshed, r.now, rawMaxLive)
	require.NoError(t, err)

	res := r.finish(deliveryOf(t, claims, dead, first).Ref, r.now, faucet.Finish{Kind: faucet.FinishDead, Unreadable: []byte("sealed-1")})
	require.Equal(t, faucet.FinishResult{Accepted: true, Removed: 1}, res, "a refresh sealed a new secret, so only this delivery ends")
	res = r.finish(deliveryOf(t, claims, dead, second).Ref, r.now, faucet.Finish{Kind: faucet.FinishDead, Unreadable: []byte("sealed-2")})
	require.Equal(t, faucet.FinishResult{Accepted: true, Removed: 2}, res, "the subscription is deleted, and with it its delivery that was still queued")
	require.Equal(t, faucet.FinishResult{}, r.finish(deliveryOf(t, claims, dead, third).Ref, r.now, faucet.Finish{Kind: faucet.FinishDelivered}),
		"the delivery that went with the subscription is gone")
	deleted, err := s.DeleteSubscription(r.ctx, dead.Principal, dead.KeyHash)
	require.NoError(t, err)
	require.False(t, deleted, "the subscription was already deleted")

	claims = r.claimAll(r.now.Add(testLease))
	require.Equal(t, []int64{first, second, third}, deliveryRefIDs(claims), "the other subscription keeps its deliveries")
	for _, c := range claims {
		require.Equal(t, kept.KeyHash, c.Ref.Key)
	}
}

// deliveryRemake deletes the nth raw subscription, moves the rig a second on
// and makes the subscription again, which is a new incarnation.
func (r *rawRig) deliveryRemake(n int, w rawSpec) faucet.Subscription {
	r.t.Helper()
	old := newRawSubscription(n, w, r.now)
	deleted, err := r.s.DeleteSubscription(r.ctx, old.Principal, old.KeyHash)
	require.NoError(r.t, err)
	require.True(r.t, deleted)
	r.now = r.now.Add(time.Second)
	return r.subscribe(n, w)
}

func claimRawDeliveryNeedsALiveSubscription(t *testing.T, s RawStore) {
	r := newRawRig(t, s)
	gone, expiring := r.subscribe(1, rawSpec{}), r.subscribe(2, rawSpec{})
	r.push()
	deleted, err := s.DeleteSubscription(r.ctx, gone.Principal, gone.KeyHash)
	require.NoError(t, err)
	require.True(t, deleted)

	_, ok := r.claim(expiring.ExpiresAt)
	require.False(t, ok, "a subscription that expired at now gets no delivery, and a deleted one none at all")
	claim := r.mustClaim(expiring.ExpiresAt.Add(-time.Second))
	require.Equal(t, expiring.KeyHash, claim.Ref.Key, "a subscription that is live at now gets its delivery")
	_, ok = r.claim(expiring.ExpiresAt.Add(-time.Second))
	require.False(t, ok, "the deleted subscription's delivery is never claimed")
}

func claimRawDeliveryGivesASubscriptionMadeAgainOnlyItsOwnDeliveries(t *testing.T, s RawStore) {
	r := newRawRig(t, s)
	r.subscribe(1, rawSpec{})
	r.push()
	remade := r.deliveryRemake(1, rawSpec{})
	id := r.push()

	claims := r.claimAll(r.now)
	require.Len(t, claims, 1, "the old incarnation's delivery is never claimed")
	require.Equal(t, id, claims[0].Ref.EventID, "the new incarnation gets the events ingested after it was made")
	require.True(t, remade.CreatedAt.Equal(claims[0].Ref.Incarnation), "got %s", claims[0].Ref.Incarnation)
	require.Equal(t, id-1, claims[0].Watermark, "the old incarnation's delivery does not hold the new one's watermark back")
	_, ok := r.claim(r.now.Add(testLease))
	require.True(t, ok, "the new incarnation's delivery is claimable again after the lease")
	_, ok = r.claim(r.now.Add(testLease))
	require.False(t, ok, "and nothing else is")
}

func claimRawDeliveryReturnsNoEventAfterAPurge(t *testing.T, s RawStore) {
	r := newRawRig(t, s)
	r.subscribe(1, rawSpec{})
	cleared := r.push()
	time.Sleep(2 * time.Millisecond)
	deleted := r.push()
	claims := r.claimAll(r.now)
	require.Equal(t, []int64{cleared, deleted}, deliveryRefIDs(claims))
	require.NotNil(t, claims[0].Event)
	require.NotNil(t, claims[1].Event)
	clearedAt, deletedAt := claims[0].Event.ReceivedAt, claims[1].Event.ReceivedAt
	require.True(t, clearedAt.Before(deletedAt), "the events were received apart")

	res := r.purge(faucet.RawPurge{Now: r.now, ClearBefore: deletedAt})
	require.EqualValues(t, 1, res.Cleared)
	claims = r.claimAll(r.now.Add(testLease))
	require.Equal(t, []int64{cleared, deleted}, deliveryRefIDs(claims), "a purge ends no delivery")
	require.Nil(t, claims[0].Event, "the claim of an event whose body is cleared carries no event")
	require.NotNil(t, claims[1].Event, "the later event keeps its body")

	res = r.purge(faucet.RawPurge{Now: r.now, DeleteBefore: deletedAt.Add(time.Millisecond)})
	require.EqualValues(t, 2, res.Deleted)
	claims = r.claimAll(r.now.Add(2 * testLease))
	require.Equal(t, []int64{cleared, deleted}, deliveryRefIDs(claims))
	require.Nil(t, claims[1].Event, "the claim of a deleted event carries no event")
}

// deliveryGapAll plans a gap for every subscription and a mark for every
// lease: no interest gets the event.
func deliveryGapAll(interests []faucet.RawInterest) faucet.RawPlan {
	var plan faucet.RawPlan
	for _, in := range interests {
		if in.IsLease {
			plan.Lost = append(plan.Lost, in.Lease)
			continue
		}
		plan.Gaps = append(plan.Gaps, faucet.RawGap{Key: in.Subscription, Incarnation: in.Incarnation})
	}
	return plan
}

// deliveryMiss stores a new push that every interest misses.
func (r *rawRig) deliveryMiss() int64 {
	r.t.Helper()
	return r.store(r.newDelivery(), deliveryGapAll)
}

func (r *rawRig) deliveryGapSkipping(now time.Time, skip []string) (faucet.GapClaim, bool) {
	r.t.Helper()
	claim, ok, err := r.s.ClaimRawGap(r.ctx, now, testLease, skip)
	require.NoError(r.t, err)
	return claim, ok
}

func (r *rawRig) deliveryMustClaimGap(now time.Time) faucet.GapClaim {
	r.t.Helper()
	claim, ok := r.claimGap(now)
	require.True(r.t, ok, "a gap is owed at %s", now)
	return claim
}

func claimRawGapHandsOutAPlannedGapOncePerLeaseUntilItIsFinished(t *testing.T, s RawStore) {
	r := newRawRig(t, s)
	sub := r.subscribe(1, rawSpec{})
	rotated := newRawSubscription(1, rawSpec{}, r.now.Add(time.Hour))
	rotated.SecretEnc = []byte("sealed-2")
	rotated.RotationGrace = 5 * time.Minute
	_, err := s.UpsertSubscription(r.ctx, rotated, r.now, rawMaxLive)
	require.NoError(t, err)
	r.push()
	_, ok := r.claimGap(r.now)
	require.False(t, ok, "a served subscription is owed no gap")

	r.deliveryMiss()
	through := r.deliveryMiss()
	r.push()
	claim := r.deliveryMustClaimGap(r.now)
	require.Equal(t, sub.KeyHash, claim.Ref.Key)
	require.True(t, sub.CreatedAt.Equal(claim.Ref.Incarnation), "got %s", claim.Ref.Incarnation)
	require.Equal(t, through, claim.Ref.Through, "the gap runs through the highest raw ID that the subscription missed")
	require.Equal(t, sub.CallbackURL, claim.CallbackURL, "the claim carries where the envelope goes")
	require.Equal(t, []byte("sealed-2"), claim.SecretEnc, "and the sealed secret that signs it, as of the claim")
	require.Equal(t, []byte("sealed-1"), claim.PreviousSecretEnc, "and the secret that also signs it")
	require.True(t, r.now.Add(5*time.Minute).Equal(claim.PreviousSecretUntil), "got %s", claim.PreviousSecretUntil)

	_, ok = r.claimGap(r.now.Add(testLease - time.Second))
	require.False(t, ok, "a claimed gap is held for its lease")
	later := r.now.Add(testLease)
	_, ok = r.deliveryGapSkipping(later, []string{faucet.CallbackOrigin(sub.CallbackURL)})
	require.False(t, ok, "a skipped origin gets no gap")
	again := r.deliveryMustClaimGap(later)
	require.Equal(t, claim.Ref.Through, again.Ref.Through, "a run that died leaves the gap to the next run after the lease")

	require.NoError(t, s.FinishRawGap(r.ctx, again.Ref))
	_, ok = r.claimGap(later.Add(10 * testLease))
	require.False(t, ok, "a finished gap is not owed any more")
}

func finishRawGapKeepsAGapThatGrewWhileItWasClaimed(t *testing.T, s RawStore) {
	r := newRawRig(t, s)
	r.subscribe(1, rawSpec{})
	first := r.deliveryMiss()
	claim := r.deliveryMustClaimGap(r.now)
	require.Equal(t, first, claim.Ref.Through)

	second := r.deliveryMiss()
	require.NoError(t, s.FinishRawGap(r.ctx, claim.Ref))
	grown := r.deliveryMustClaimGap(r.now)
	require.Equal(t, second, grown.Ref.Through, "a gap that grew while it was claimed stays owed, at its new mark, with no lease")

	require.NoError(t, s.FinishRawGap(r.ctx, grown.Ref))
	_, ok := r.claimGap(r.now.Add(10 * testLease))
	require.False(t, ok, "the finish of the current claim ends the gap")
}

func claimRawGapNeedsTheLiveIncarnation(t *testing.T, s RawStore) {
	r := newRawRig(t, s)
	sub := r.subscribe(1, rawSpec{})
	r.deliveryMiss()
	_, ok := r.claimGap(sub.ExpiresAt)
	require.False(t, ok, "a subscription that expired at now is owed no gap")

	remade := r.deliveryRemake(1, rawSpec{})
	_, ok = r.claimGap(r.now)
	require.False(t, ok, "a subscription made again is not owed the old incarnation's gap")

	through := r.deliveryMiss()
	old := faucet.GapRef{Key: sub.KeyHash, Incarnation: sub.CreatedAt, Through: through}
	require.NoError(t, s.FinishRawGap(r.ctx, old))
	claim := r.deliveryMustClaimGap(r.now)
	require.True(t, remade.CreatedAt.Equal(claim.Ref.Incarnation), "the end of the old incarnation's gap leaves the new one's owed, got %s", claim.Ref.Incarnation)
	require.Equal(t, through, claim.Ref.Through)

	deleted, err := s.DeleteSubscription(r.ctx, remade.Principal, remade.KeyHash)
	require.NoError(t, err)
	require.True(t, deleted)
	_, ok = r.claimGap(r.now.Add(testLease))
	require.False(t, ok, "a deleted subscription is owed no gap")
}

func expireRawDeliveriesDeletesWhatNoLiveSubscriptionHolds(t *testing.T, s RawStore) {
	r := newRawRig(t, s)
	held := r.subscribe(1, rawSpec{})
	heldID := r.push()
	live := r.subscribe(2, rawSpec{})
	expiresAt := r.now.Add(20 * time.Second)
	_, err := s.UpsertSubscription(r.ctx, newRawSubscription(3, rawSpec{}, expiresAt), r.now, rawMaxLive)
	require.NoError(t, err)
	gone := r.subscribe(4, rawSpec{})
	r.subscribe(5, rawSpec{})
	r.push()
	r.deliveryMiss()
	claim := r.mustClaim(r.now)
	require.Equal(t, held.KeyHash, claim.Ref.Key)
	require.Equal(t, heldID, claim.Ref.EventID)

	for _, sub := range []faucet.Subscription{held, gone} {
		deleted, err := s.DeleteSubscription(r.ctx, sub.Principal, sub.KeyHash)
		require.NoError(t, err)
		require.True(t, deleted)
	}
	start := r.now
	r.deliveryRemake(5, rawSpec{})
	expire := func(now time.Time) int64 {
		t.Helper()
		n, err := s.ExpireRawDeliveries(r.ctx, now)
		require.NoError(t, err)
		return n
	}
	require.EqualValues(t, 3, expire(expiresAt.Add(-time.Second)),
		"the unclaimed deliveries of the deleted subscriptions and of the old incarnation, and not the one a claim holds")
	require.EqualValues(t, 1, expire(expiresAt), "the delivery of the subscription that expired at now")
	require.EqualValues(t, 1, expire(start.Add(testLease)), "the delivery whose claim ran out")
	require.Zero(t, expire(start.Add(testLease)))

	claims := r.claimAll(start.Add(testLease))
	require.Len(t, claims, 1, "the live subscription keeps its delivery")
	require.Equal(t, live.KeyHash, claims[0].Ref.Key)
	gap := r.deliveryMustClaimGap(start.Add(testLease))
	require.Equal(t, live.KeyHash, gap.Ref.Key, "the live subscription keeps its gap")
	_, ok := r.claimGap(start.Add(testLease))
	require.False(t, ok, "the subscription made again is owed no old gap")
}
