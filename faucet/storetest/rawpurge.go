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
