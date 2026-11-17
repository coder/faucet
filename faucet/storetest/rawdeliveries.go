package storetest

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/coder/faucet/faucet"
)

// rawDeliverySubtests are the contract cases of raw deliveries and gaps.
var rawDeliverySubtests = []rawSubtest{
	{"ClaimRawDeliveryTakesTheDueTimeThenTheRawIDAndHoldsItForTheLease", claimRawDeliveryTakesTheDueTimeThenTheRawIDAndHoldsItForTheLease},
	{"ClaimRawDeliveryReturnsTheSubscriptionAndTheStoredEvent", claimRawDeliveryReturnsTheSubscriptionAndTheStoredEvent},
	{"ClaimRawDeliveryWatermarkStaysBelowTheLowestUnsentDelivery", claimRawDeliveryWatermarkStaysBelowTheLowestUnsentDelivery},
	{"ClaimRawDeliveryCarriesThePlannedGiveUp", claimRawDeliveryCarriesThePlannedGiveUp},
	{"FinishRawDeliveryRetriesOrEndsAndRecordsTheStatus", finishRawDeliveryRetriesOrEndsAndRecordsTheStatus},
	{"FinishRawDeliveryRefusesAnAttemptThatIsNotTheCurrentOne", finishRawDeliveryRefusesAnAttemptThatIsNotTheCurrentOne},
	{"ClaimRawDeliveryPassesOverTheOriginsItIsToldTo", claimRawDeliveryPassesOverTheOriginsItIsToldTo},
	{"FinishRawDeliveryDeadDeletesTheSubscriptionOnlyForTheSameSealedSecret", finishRawDeliveryDeadDeletesTheSubscriptionOnlyForTheSameSealedSecret},
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
	require.Equal(t, rawRepo, claim.RepoID)
	require.Empty(t, claim.GiveUp)

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

func claimRawDeliveryCarriesThePlannedGiveUp(t *testing.T, s RawStore) {
	r := newRawRig(t, s)
	sub := r.subscribe(1, rawSpec{})
	tooLarge := r.store(r.newDelivery(), func(interests []faucet.RawInterest) faucet.RawPlan {
		in := subscriptionInterest(t, interests, sub)
		return faucet.RawPlan{Deliveries: []faucet.RawOutbox{{Key: in.Subscription, Incarnation: in.Incarnation, GiveUp: faucet.GiveUpEventTooLarge}}}
	})
	served := r.push()

	claims := r.claimAll(r.now)
	require.Equal(t, []int64{tooLarge, served}, deliveryRefIDs(claims))
	require.Equal(t, faucet.GiveUpEventTooLarge, claims[0].GiveUp, "the claim ends the delivery with the reason the plan set")
	require.Nil(t, claims[0].Event, "a plan without a body stores no body to send")
	require.Empty(t, claims[1].GiveUp)
	require.NotNil(t, claims[1].Event)
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
	require.Equal(t, faucet.DeliveryStatus{LastError: faucet.CallbackTimeout, FailedSince: r.now}, r.deliveryStatus(1, rawSpec{}))
	res = r.finish(claims[1].Ref, r.now.Add(time.Second), faucet.Finish{Kind: faucet.FinishRetry, RetryAt: retryAt, Failure: faucet.CallbackConnectionRefused})
	require.Equal(t, faucet.FinishResult{Accepted: true}, res)
	status := r.deliveryStatus(1, rawSpec{})
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
