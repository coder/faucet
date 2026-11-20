package storetest

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/coder/superblink-events/faucet"
)

// rawPurgeSubtests are the contract cases of the purge, of what interests want, and of the event log next to raw events.
var rawPurgeSubtests = []rawSubtest{
	{"PurgeRawEventsClearsABodyThatNothingNames", purgeRawEventsClearsABodyThatNothingNames},
	{"PurgeRawEventsClearsTheBodyOfAMissedEvent", purgeRawEventsClearsTheBodyOfAMissedEvent},
	{"PurgeRawEventsClearsEveryBodyReceivedBeforeClearBefore", purgeRawEventsClearsEveryBodyReceivedBeforeClearBefore},
	{"PurgeRawEventsOwesAGapForEveryPayloadThatADeliveryStillNamed", purgeRawEventsOwesAGapForEveryPayloadThatADeliveryStillNamed},
	{"PurgeRawEventsDeletesExpiredLeasesWithTheirUnreadEvents", purgeRawEventsDeletesExpiredLeasesWithTheirUnreadEvents},
	{"PurgeRawEventsDeletesTheEventsReceivedBeforeDeleteBefore", purgeRawEventsDeletesTheEventsReceivedBeforeDeleteBefore},
	{"PurgeRawEventsKeepsTheDeliveryIDOfAClearedEvent", purgeRawEventsKeepsTheDeliveryIDOfAClearedEvent},
	{"PurgeRawEventsLeavesTheEventLogAlone", purgeRawEventsLeavesTheEventLogAlone},
	{"RawWantsListsWhatLiveRawInterestsWantOnce", rawWantsListsWhatLiveRawInterestsWantOnce},
	{"TheEventLogNeverScansARawSubscription", theEventLogNeverScansARawSubscription},
	{"IngestRawRefusesAnInvalidDelivery", ingestRawRefusesAnInvalidDelivery},
	{"OpenRawLeaseRefusesAnInvalidLease", openRawLeaseRefusesAnInvalidLease},
	{"OpenRawLeaseRefusesAMaxLiveOfZero", openRawLeaseRefusesAMaxLiveOfZero},
	{"ReadRawLeaseRefusesAnInvalidRead", readRawLeaseRefusesAnInvalidRead},
	{"PurgeRawEventsRefusesAZeroNow", purgeRawEventsRefusesAZeroNow},
	{"ClaimRawDeliveryAndGapRefuseALeaseOfZero", claimRawDeliveryAndGapRefuseALeaseOfZero},
}

// purgeServeOnly plans a delivery for every subscription when subscriptions
// is set, and an unread event for every lease otherwise.
func purgeServeOnly(subscriptions bool) rawPlanner {
	return func(interests []faucet.RawInterest) faucet.RawPlan {
		plan := faucet.RawPlan{Body: rawBody}
		for _, in := range interests {
			switch {
			case in.IsLease && !subscriptions:
				plan.Unread = append(plan.Unread, in.Lease)
			case !in.IsLease && subscriptions:
				plan.Deliveries = append(plan.Deliveries, faucet.RawOutbox{Key: in.Subscription, Incarnation: in.Incarnation})
			}
		}
		return plan
	}
}

// purgeDeliver claims the raw delivery that is due first and finishes it as
// delivered.
func (r *rawRig) purgeDeliver(wantID int64) faucet.RawDeliveryClaim {
	r.t.Helper()
	claim := r.mustClaim(r.now)
	require.Equal(r.t, wantID, claim.Ref.EventID)
	require.True(r.t, r.finish(claim.Ref, r.now, faucet.Finish{Kind: faucet.FinishDelivered}).Accepted)
	return claim
}

// purgeSpaced stores n pushes for every interest, apart in real time, so that
// the store receives each one later than the one before.
func (r *rawRig) purgeSpaced(n int) []int64 {
	r.t.Helper()
	ids := make([]int64, 0, n)
	for range n {
		ids = append(ids, r.push())
		time.Sleep(2 * time.Millisecond)
	}
	return ids
}

func (r *rawRig) purgeAt(now time.Time) faucet.RawPurgeResult {
	r.t.Helper()
	return r.purge(faucet.RawPurge{Now: now})
}

func purgeRawEventsClearsABodyThatNothingNames(t *testing.T, s RawStore) {
	r := newRawRig(t, s)
	r.subscribe(1, rawSpec{})
	lease := r.open(rawSpec{})
	delivered := r.store(r.newDelivery(), purgeServeOnly(true))
	unread := r.store(r.newDelivery(), purgeServeOnly(false))
	both := r.push()
	require.Equal(t, faucet.RawPurgeResult{}, r.purgeAt(r.now), "a delivery or an unread event names every body")

	r.purgeDeliver(delivered)
	require.Equal(t, faucet.RawPurgeResult{Cleared: 1}, r.purgeAt(r.now), "the finished delivery named the only event that nothing names now")
	page := r.read(r.query(lease, 0))
	require.Equal(t, []int64{unread, both}, rawIDs(page.Events), "the lease still reads the bodies that its unread events name")
	for _, ev := range page.Events {
		require.Equal(t, rawBody, []byte(ev.Body))
	}

	r.read(r.query(lease, both))
	require.Equal(t, faucet.RawPurgeResult{Cleared: 1}, r.purgeAt(r.now), "the acknowledged event that no delivery names is cleared")
	claim := r.purgeDeliver(both)
	require.NotNil(t, claim.Event, "a delivery keeps the body of the event it names")
	require.Equal(t, rawBody, []byte(claim.Event.Body))

	require.Equal(t, faucet.RawPurgeResult{Cleared: 1}, r.purgeAt(r.now), "the last finished delivery named the last body")
	require.Equal(t, faucet.RawPurgeResult{}, r.purgeAt(r.now), "a cleared body is not counted again")
	require.Equal(t, both, r.head(), "clearing a body keeps the event")
}

func purgeRawEventsClearsTheBodyOfAMissedEvent(t *testing.T, s RawStore) {
	r := newRawRig(t, s)
	sub := r.subscribe(1, rawSpec{})
	lease := r.open(rawSpec{})
	id := r.store(r.newDelivery(), func(interests []faucet.RawInterest) faucet.RawPlan {
		in := subscriptionInterest(t, interests, sub)
		return faucet.RawPlan{
			Body:       rawBody,
			Deliveries: []faucet.RawOutbox{{Key: in.Subscription, Incarnation: in.Incarnation}},
			Lost:       []faucet.RawLeaseID{leaseInterest(t, interests, lease).Lease},
		}
	})
	require.Equal(t, faucet.RawPurgeResult{}, r.purgeAt(r.now), "the delivery names the event")

	r.purgeDeliver(id)
	require.Equal(t, faucet.RawPurgeResult{Cleared: 1}, r.purgeAt(r.now), "the mark of the lease that missed the event does not name it")
	page := r.read(r.query(lease, 0))
	require.Empty(t, page.Events)
	require.Equal(t, 1, page.Skipped, "the lease still learns that it missed the event")
}

func purgeRawEventsClearsEveryBodyReceivedBeforeClearBefore(t *testing.T, s RawStore) {
	r := newRawRig(t, s)
	r.subscribe(1, rawSpec{})
	lease := r.open(rawSpec{})
	ids := r.purgeSpaced(3)
	page := r.read(r.query(lease, 0))
	require.Equal(t, ids, rawIDs(page.Events))
	require.True(t, page.Events[0].ReceivedAt.Before(page.Events[1].ReceivedAt), "the pushes are received apart")

	require.Equal(t, faucet.RawPurgeResult{}, r.purgeAt(r.now), "a zero ClearBefore clears nothing by age")
	cut := page.Events[1].ReceivedAt
	require.Equal(t, faucet.RawPurgeResult{Cleared: 1}, r.purge(faucet.RawPurge{Now: r.now, ClearBefore: cut}),
		"ClearBefore clears the body received before it, and none received at or after it")

	page = r.read(r.query(lease, 0))
	require.Equal(t, ids[1:], rawIDs(page.Events), "the lease reads only the bodies that are kept")
	require.Equal(t, 1, page.Skipped, "the lease passes over the cleared event")
	claim := r.mustClaim(r.now)
	require.Equal(t, ids[0], claim.Ref.EventID)
	require.Nil(t, claim.Event, "a delivery that names a cleared event gets no body")
	require.Equal(t, ids[2], r.head(), "clearing a body keeps the event")
}

func purgeRawEventsOwesAGapForEveryPayloadThatADeliveryStillNamed(t *testing.T, s RawStore) {
	r := newRawRig(t, s)
	waiting := r.subscribe(1, rawSpec{})
	sent := r.subscribe(2, rawSpec{})
	ids := r.purgeSpaced(3)
	claims := r.claimAll(r.now)
	require.Len(t, claims, 6, "each subscription has a delivery of each push")
	for _, id := range ids[:2] {
		require.True(t, r.finish(deliveryOf(t, claims, sent, id).Ref, r.now, faucet.Finish{Kind: faucet.FinishDelivered}).Accepted)
	}
	cut := deliveryOf(t, claims, sent, ids[2]).Event.ReceivedAt

	require.Equal(t, faucet.RawPurgeResult{Cleared: 2}, r.purge(faucet.RawPurge{Now: r.now, ClearBefore: cut}))
	gap := r.deliveryMustClaimGap(r.now)
	require.Equal(t, waiting.KeyHash, gap.Ref.Key, "the subscription whose deliveries still named the cleared payloads is owed a gap, also while a claim holds them")
	require.True(t, waiting.CreatedAt.Equal(gap.Ref.Incarnation), "got %s", gap.Ref.Incarnation)
	require.Equal(t, ids[1], gap.Ref.Through, "the gap runs through the newest event whose payload the subscription lost")
	_, ok := r.claimGap(r.now)
	require.False(t, ok, "a subscription whose deliveries of the cleared events ended is owed no gap")
	require.NoError(t, s.FinishRawGap(r.ctx, gap.Ref))

	require.Equal(t, faucet.RawPurgeResult{Deleted: 2}, r.purge(faucet.RawPurge{Now: r.now, ClearBefore: cut, DeleteBefore: cut}))
	_, ok = r.claimGap(r.now)
	require.False(t, ok, "deleting an event whose payload a purge already cleared owes no second gap")

	require.Equal(t, faucet.RawPurgeResult{Deleted: 1}, r.purge(faucet.RawPurge{Now: r.now, DeleteBefore: cut.Add(time.Hour)}))
	var owed []faucet.SubscriptionKeyHash
	for range 2 {
		gap := r.deliveryMustClaimGap(r.now)
		require.Equal(t, ids[2], gap.Ref.Through, "deleting an event with its payload owes a gap through it")
		owed = append(owed, gap.Ref.Key)
	}
	require.ElementsMatch(t, []faucet.SubscriptionKeyHash{waiting.KeyHash, sent.KeyHash}, owed, "each subscription whose delivery named the deleted event")
	_, ok = r.claimGap(r.now)
	require.False(t, ok)
}

func purgeRawEventsDeletesExpiredLeasesWithTheirUnreadEvents(t *testing.T, s RawStore) {
	r := newRawRig(t, s)
	live := r.open(rawSpec{})
	expiresAt := r.now.Add(time.Minute)
	expiring := newRawLease(r.leases.Add(1), rawSpec{}, expiresAt)
	_, err := s.OpenRawLease(r.ctx, expiring, r.now, rawLimits(rawMaxLive))
	require.NoError(t, err)
	shared := r.push()
	only := r.store(r.newDelivery(), func(interests []faucet.RawInterest) faucet.RawPlan {
		return faucet.RawPlan{Body: rawBody, Unread: []faucet.RawLeaseID{leaseInterest(t, interests, expiring).Lease}}
	})

	require.Equal(t, faucet.RawPurgeResult{}, r.purgeAt(expiresAt.Add(-time.Microsecond)), "a lease is live before its expiry")
	require.Equal(t, faucet.RawPurgeResult{ExpiredLeases: 1, Cleared: 1}, r.purgeAt(expiresAt),
		"a lease expires at its expiry, and the same call clears the event that only its unread events named")
	require.Equal(t, faucet.RawPurgeResult{}, r.purgeAt(expiresAt), "an expired lease is deleted once")

	q := r.query(live, 0)
	q.Now = expiresAt
	page := r.read(q)
	require.Equal(t, []int64{shared}, rawIDs(page.Events), "the live lease keeps its unread event")
	require.Equal(t, rawBody, []byte(page.Events[0].Body))
	require.Zero(t, page.Skipped, "the event that only the expired lease got was never the live lease's")
	require.Equal(t, only, page.Next)
}

func purgeRawEventsDeletesTheEventsReceivedBeforeDeleteBefore(t *testing.T, s RawStore) {
	r := newRawRig(t, s)
	lease := r.open(rawSpec{})
	first := r.newDelivery()
	ids := []int64{r.store(first, serveAll(rawBody))}
	time.Sleep(2 * time.Millisecond)
	ids = append(ids, r.purgeSpaced(2)...)
	page := r.read(r.query(lease, 0))
	require.Equal(t, ids, rawIDs(page.Events))

	require.Equal(t, faucet.RawPurgeResult{}, r.purgeAt(r.now), "a zero DeleteBefore deletes nothing")
	cut := page.Events[1].ReceivedAt
	require.Equal(t, faucet.RawPurgeResult{Deleted: 1}, r.purge(faucet.RawPurge{Now: r.now, DeleteBefore: cut}),
		"DeleteBefore deletes the event received before it, and none received at or after it")
	require.Equal(t, faucet.RawPurgeResult{}, r.purge(faucet.RawPurge{Now: r.now, DeleteBefore: cut}), "a deleted event is not counted again")

	page = r.read(r.query(lease, 0))
	require.Equal(t, ids[1:], rawIDs(page.Events), "the lease reads the events that are kept")
	require.Equal(t, 1, page.Skipped, "the lease passes over the deleted event")

	again := r.store(first, serveAll(rawBody))
	require.Greater(t, again, ids[2], "a deleted delivery ID is stored again as a new event")
}

func purgeRawEventsKeepsTheDeliveryIDOfAClearedEvent(t *testing.T, s RawStore) {
	r := newRawRig(t, s)
	r.subscribe(1, rawSpec{})
	d := r.newDelivery()
	id := r.store(d, serveAll(rawBody))
	r.purgeDeliver(id)
	require.Equal(t, faucet.RawPurgeResult{Cleared: 1}, r.purgeAt(r.now))

	res := r.ingestAt(d, r.now, serveAll(rawBody))
	require.True(t, res.Matched)
	require.True(t, res.Duplicate, "a cleared event keeps its delivery ID, so GitHub's second send is a duplicate")
	require.Equal(t, id, r.head(), "a duplicate is not stored")
	_, ok := r.claim(r.now)
	require.False(t, ok, "a duplicate plans no delivery")
}

func purgeRawEventsLeavesTheEventLogAlone(t *testing.T, s RawStore) {
	r := newRawRig(t, s)
	r.open(rawSpec{})
	logged := insertSpaced(t, s, "log", 2)
	r.push()
	future := time.Now().Add(time.Hour)
	require.Equal(t, faucet.RawPurgeResult{Cleared: 1, Deleted: 1}, r.purge(faucet.RawPurge{Now: r.now, ClearBefore: future, DeleteBefore: future}))
	requireRemaining(t, s, logged...)
}

// purgeWantsAt returns the wants at now, with no actions as nil.
func (r *rawRig) purgeWantsAt(now time.Time) []faucet.RawWant {
	r.t.Helper()
	wants, err := r.s.RawWants(r.ctx, now)
	require.NoError(r.t, err)
	for i := range wants {
		if len(wants[i].Actions) == 0 {
			wants[i].Actions = nil
		}
	}
	return wants
}

func rawWantsListsWhatLiveRawInterestsWantOnce(t *testing.T, s RawStore) {
	r := newRawRig(t, s)
	require.Empty(t, r.purgeWantsAt(r.now), "an empty store wants nothing")
	at := r.now.Add(30 * time.Minute)

	typed := newSubscription("octocat", "https://hooks.example.test/typed", r.now.Add(time.Hour))
	_, err := s.UpsertSubscription(r.ctx, typed, r.now, rawMaxLive)
	require.NoError(t, err)
	r.subscribe(1, rawSpec{actions: []string{"opened", "closed"}})
	r.open(rawSpec{actions: []string{"closed", "opened"}})
	r.subscribe(2, rawSpec{})
	r.open(rawSpec{})
	r.open(rawSpec{event: "issues", repoID: rawRepo + 1, actions: []string{"labeled"}})
	r.subscribeAt(3, rawSpec{event: "release"}, at.Add(-time.Hour))
	_, err = s.OpenRawLease(r.ctx, newRawLease(r.leases.Add(1), rawSpec{event: "fork"}, at), r.now, rawLimits(rawMaxLive))
	require.NoError(t, err)

	require.ElementsMatch(t, []faucet.RawWant{
		{Target: faucet.RepoTarget(rawRepo), Event: rawEvent, Actions: []string{"closed", "opened"}},
		{Target: faucet.RepoTarget(rawRepo), Event: rawEvent},
		{Target: faucet.RepoTarget(rawRepo + 1), Event: "issues", Actions: []string{"labeled"}},
	}, r.purgeWantsAt(at), "one entry per want of a live raw interest, with its actions in rising order, and none for a typed subscription")
	require.Len(t, r.purgeWantsAt(r.now), 5, "the subscription and the lease that expire at the later time are live before it")
}

func theEventLogNeverScansARawSubscription(t *testing.T, s RawStore) {
	r := newRawRig(t, s)
	sub := r.subscribe(1, rawSpec{})
	typed := newSubscription("hubot", "https://hooks.example.test/typed", r.now.Add(time.Hour))
	typed.Cursor = 0
	_, err := s.UpsertSubscription(r.ctx, typed, r.now, rawMaxLive)
	require.NoError(t, err)
	scanned := func(typedReads bool, when string) {
		t.Helper()
		targets, err := s.ScanTargets(r.ctx, r.now, 10, 100)
		require.NoError(t, err)
		keys := make([]faucet.SubscriptionKeyHash, 0, len(targets))
		for _, target := range targets {
			keys = append(keys, target.KeyHash)
		}
		require.NotContains(t, keys, sub.KeyHash, "the delivery queue never reads a raw subscription %s", when)
		if typedReads {
			require.Contains(t, keys, typed.KeyHash, "the typed subscription has something to read %s", when)
		}
	}
	scanned(false, "at first")
	insertSpaced(t, s, "typed", 2)
	scanned(true, "after events of its repository")
	_, err = s.Purge(context.Background(), time.Now().Add(time.Hour))
	require.NoError(t, err)
	scanned(true, "after a purge of the event log")
	_, ok, err := s.ClaimGap(r.ctx, r.now, testLease, nil)
	require.NoError(t, err)
	require.False(t, ok, "a purge of the event log records no gap for a raw subscription")

	_, err = s.UpsertSubscription(r.ctx, subscriptionAt(2, r.now.Add(time.Hour)), r.now, 1)
	require.ErrorIs(t, err, faucet.ErrSubscriptionLimit, "a raw subscription counts toward the principal's live subscriptions")
}

func ingestRawRefusesAnInvalidDelivery(t *testing.T, s RawStore) {
	r := newRawRig(t, s)
	r.subscribe(1, rawSpec{})
	for name, d := range map[string]faucet.RawDelivery{
		"an empty delivery ID":       {Event: rawEvent, RepoID: rawRepo},
		"an empty event":             {DeliveryID: "raw-delivery-bad", RepoID: rawRepo},
		"no repository and no owner": {DeliveryID: "raw-delivery-bad", Event: rawEvent},
		"a negative repository":      {DeliveryID: "raw-delivery-bad", Event: rawEvent, RepoID: -1, OwnerID: rawOwner},
		"a negative owner":           {DeliveryID: "raw-delivery-bad", Event: rawEvent, RepoID: rawRepo, OwnerID: -1},
	} {
		_, err := s.IngestRaw(r.ctx, d, r.now, func([]faucet.RawInterest) faucet.RawPlan {
			t.Errorf("the store planned a delivery with %s", name)
			return faucet.RawPlan{}
		})
		require.Error(t, err, "a delivery with %s is refused", name)
	}
	require.Zero(t, r.head(), "a refused delivery is not stored")
	_, ok := r.claim(r.now)
	require.False(t, ok, "a refused delivery queues nothing")
}

func openRawLeaseRefusesAnInvalidLease(t *testing.T, s RawStore) {
	r := newRawRig(t, s)
	bad := map[string]func(*faucet.RawLease){
		"a zero ID":               func(l *faucet.RawLease) { l.ID = faucet.RawLeaseID{} },
		"an empty principal":      func(l *faucet.RawLease) { l.Principal = "" },
		"an empty event":          func(l *faucet.RawLease) { l.Event = "" },
		"repository 0":            func(l *faucet.RawLease) { l.Target = faucet.RepoTarget(0) },
		"a zero target":           func(l *faucet.RawLease) { l.Target = faucet.Target{} },
		"an expiry not after now": func(l *faucet.RawLease) { l.ExpiresAt = r.now },
	}
	for name, spoil := range bad {
		lease := newRawLease(r.leases.Add(1), rawSpec{}, r.now.Add(time.Hour))
		spoil(&lease)
		_, err := s.OpenRawLease(r.ctx, lease, r.now, rawLimits(rawMaxLive))
		require.Error(t, err, "a lease with %s is refused", name)
	}
	require.Empty(t, r.purgeWantsAt(r.now.Add(-time.Minute)), "a refused lease is not stored")
	require.Nil(t, r.interestsAt(r.newDelivery(), r.now), "a refused lease wants nothing")
}

func openRawLeaseRefusesAMaxLiveOfZero(t *testing.T, s RawStore) {
	r := newRawRig(t, s)
	_, err := s.OpenRawLease(r.ctx, newRawLease(r.leases.Add(1), rawSpec{}, r.now.Add(time.Hour)), r.now, rawLimits(0))
	require.Error(t, err, "maxLive must be positive")
	require.Empty(t, r.purgeWantsAt(r.now), "a refused lease is not stored")
	require.Nil(t, r.interestsAt(r.newDelivery(), r.now), "a refused lease wants nothing")
}

func readRawLeaseRefusesAnInvalidRead(t *testing.T, s RawStore) {
	r := newRawRig(t, s)
	lease := r.open(rawSpec{})
	id := r.push()
	bad := map[string]func(*faucet.RawRead){
		"a negative After":        func(q *faucet.RawRead) { q.After = -1 },
		"a Limit of 0":            func(q *faucet.RawRead) { q.Limit = 0 },
		"a MaxBytes of 0":         func(q *faucet.RawRead) { q.MaxBytes = 0 },
		"an expiry not after Now": func(q *faucet.RawRead) { q.ExpiresAt = q.Now },
	}
	for name, spoil := range bad {
		q := r.query(lease, id)
		q.ExpiresAt = r.now.Add(time.Minute)
		spoil(&q)
		_, found, err := s.ReadRawLease(r.ctx, q)
		require.Error(t, err, "a read with %s is refused", name)
		require.False(t, found)
	}
	require.NotEmpty(t, r.purgeWantsAt(r.now.Add(30*time.Minute)), "a refused read does not move the expiry")
	page := r.read(r.query(lease, 0))
	require.Zero(t, page.AckedThrough, "a refused read acknowledges nothing")
	require.Equal(t, []int64{id}, rawIDs(page.Events), "a refused read deletes no unread event")
}

func purgeRawEventsRefusesAZeroNow(t *testing.T, s RawStore) {
	r := newRawRig(t, s)
	_, err := s.OpenRawLease(r.ctx, newRawLease(r.leases.Add(1), rawSpec{}, r.now.Add(time.Minute)), r.now, rawLimits(rawMaxLive))
	require.NoError(t, err)
	id := r.push()
	future := time.Now().Add(time.Hour)
	_, err = s.PurgeRawEvents(r.ctx, faucet.RawPurge{ClearBefore: future, DeleteBefore: future})
	require.Error(t, err, "a purge needs the time that decides which leases expired")
	require.Equal(t, id, r.head(), "a refused purge deletes no event")
	require.Equal(t, faucet.RawPurgeResult{ExpiredLeases: 1, Cleared: 1}, r.purgeAt(r.now.Add(time.Minute)),
		"a refused purge deleted no lease and cleared no body")
}

func claimRawDeliveryAndGapRefuseALeaseOfZero(t *testing.T, s RawStore) {
	r := newRawRig(t, s)
	r.subscribe(1, rawSpec{})
	missed := r.subscribe(2, rawSpec{})
	r.store(r.newDelivery(), func(interests []faucet.RawInterest) faucet.RawPlan {
		plan := serveAll(rawBody)(interests)
		in := subscriptionInterest(t, interests, missed)
		plan.Deliveries = slices.DeleteFunc(plan.Deliveries, func(o faucet.RawOutbox) bool { return o.Key == missed.KeyHash })
		plan.Gaps = []faucet.RawGap{{Key: in.Subscription, Incarnation: in.Incarnation}}
		return plan
	})

	_, ok, err := s.ClaimRawDelivery(r.ctx, r.now, 0, nil)
	require.Error(t, err, "a claim needs a positive lease")
	require.False(t, ok)
	require.EqualValues(t, 1, r.mustClaim(r.now).Ref.Attempt, "a refused claim counts no attempt")

	_, ok, err = s.ClaimRawGap(r.ctx, r.now, 0, nil)
	require.Error(t, err, "a gap claim needs a positive lease")
	require.False(t, ok)
	gap, ok := r.claimGap(r.now)
	require.True(t, ok, "a refused gap claim leaves the gap owed")
	require.Equal(t, missed.KeyHash, gap.Ref.Key)
}
