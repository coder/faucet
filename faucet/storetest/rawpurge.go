package storetest

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/coder/faucet/faucet"
)

// rawPurgeSubtests are the contract cases of the purge, of what interests want, and of the event log next to raw events.
var rawPurgeSubtests = []rawSubtest{
	{"PurgeRawEventsClearsABodyThatNothingNames", purgeRawEventsClearsABodyThatNothingNames},
	{"PurgeRawEventsClearsTheBodyOfAMissedEvent", purgeRawEventsClearsTheBodyOfAMissedEvent},
	{"PurgeRawEventsClearsEveryBodyReceivedBeforeClearBefore", purgeRawEventsClearsEveryBodyReceivedBeforeClearBefore},
	{"PurgeRawEventsDeletesExpiredLeasesWithTheirUnreadEvents", purgeRawEventsDeletesExpiredLeasesWithTheirUnreadEvents},
	{"PurgeRawEventsDeletesTheEventsReceivedBeforeDeleteBefore", purgeRawEventsDeletesTheEventsReceivedBeforeDeleteBefore},
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

func purgeRawEventsDeletesExpiredLeasesWithTheirUnreadEvents(t *testing.T, s RawStore) {
	r := newRawRig(t, s)
	live := r.open(rawSpec{})
	expiresAt := r.now.Add(time.Minute)
	expiring := newRawLease(r.leases.Add(1), rawSpec{}, expiresAt)
	_, err := s.OpenRawLease(r.ctx, expiring, r.now, rawMaxLive)
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
