package storetest

import (
	"bytes"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/coder/faucet/faucet"
)

// rawLeaseSubtests are the contract cases of poll leases.
var rawLeaseSubtests = []rawSubtest{
	{"OpenRawLeaseStartsAtTheHead", openRawLeaseStartsAtTheHead},
	{"ReadRawLeaseFindsOnlyALiveLeaseOfItsKeyAndRepository", readRawLeaseFindsOnlyALiveLeaseOfItsKeyAndRepository},
	{"ReadRawLeaseReturnsUnreadEventsAboveAfterInOrder", readRawLeaseReturnsUnreadEventsAboveAfterInOrder},
}

// leasePending ingests a delivery that only lease matches, without giving it
// to the lease, and returns the lease's Pending in that ingest.
func (r *rawRig) leasePending(lease faucet.RawLease) int32 {
	r.t.Helper()
	var pending int32
	r.store(r.newDelivery(), func(interests []faucet.RawInterest) faucet.RawPlan {
		pending = leaseInterest(r.t, interests, lease).Pending
		return faucet.RawPlan{Body: rawBody}
	})
	return pending
}

// leaseFind polls a lease and reports whether it was found.
func (r *rawRig) leaseFind(q faucet.RawRead) (faucet.RawPage, bool) {
	r.t.Helper()
	page, found, err := r.s.ReadRawLease(r.ctx, q)
	require.NoError(r.t, err)
	return page, found
}

func openRawLeaseStartsAtTheHead(t *testing.T, s RawStore) {
	r := newRawRig(t, s)
	r.subscribe(1, rawSpec{})
	r.push()
	head := r.push()

	lease := newRawLease(1, rawSpec{}, r.now.Add(time.Hour))
	got, err := s.OpenRawLease(r.ctx, lease, r.now, rawMaxLive)
	require.NoError(t, err)
	require.Equal(t, head, got, "a new lease starts at the head")

	page := r.read(r.query(lease, 0))
	require.Empty(t, page.Events, "a new lease holds no event stored before it")
	require.Zero(t, page.Skipped)
	require.False(t, page.More)
	require.Equal(t, head, page.AckedThrough, "a new lease has acknowledged the head")
	require.Equal(t, head, page.Next)

	_, err = s.OpenRawLease(r.ctx, lease, r.now, rawMaxLive)
	require.Error(t, err, "a lease ID that is already stored is an error")
	next := r.push()
	require.Equal(t, []int64{next}, rawIDs(r.read(r.query(lease, head)).Events), "the refused open left the lease as it was")
}

func readRawLeaseFindsOnlyALiveLeaseOfItsKeyAndRepository(t *testing.T, s RawStore) {
	r := newRawRig(t, s)
	lease := r.open(rawSpec{})
	ids := []int64{r.push(), r.push()}

	other := newRawLease(1000, rawSpec{}, r.now.Add(time.Hour))
	misses := map[string]faucet.RawRead{}
	q := r.query(lease, ids[1])
	q.Lease = other.ID
	misses["an unknown lease ID"] = q
	q = r.query(lease, ids[1])
	q.Key = newRawLease(1, rawSpec{actions: []string{"opened"}}, r.now.Add(time.Hour)).Key
	misses["another key"] = q
	q = r.query(lease, ids[1])
	q.RepoID = rawRepo + 1
	misses["another repository"] = q
	q = r.query(lease, ids[1])
	q.Now, q.ExpiresAt = lease.ExpiresAt, lease.ExpiresAt.Add(time.Hour)
	misses["a lease that expired at now"] = q
	for name, q := range misses {
		page, found := r.leaseFind(q)
		require.False(t, found, "%s is not found", name)
		require.Equal(t, faucet.RawPage{}, page, "%s returns an empty page", name)
	}

	page := r.read(r.query(lease, 0))
	require.Equal(t, ids, rawIDs(page.Events), "a read that finds no lease deletes no event")
	require.Zero(t, page.AckedThrough, "a read that finds no lease does not raise the acknowledged position")
}

func readRawLeaseReturnsUnreadEventsAboveAfterInOrder(t *testing.T, s RawStore) {
	r := newRawRig(t, s)
	lease := r.open(rawSpec{fields: []string{"ref"}})
	deliveries := make([]faucet.RawDelivery, 3)
	ids := make([]int64, 3)
	for i := range deliveries {
		deliveries[i] = r.newDelivery()
		deliveries[i].Action = []string{"created", "", "deleted"}[i]
		ids[i] = r.store(deliveries[i], func(interests []faucet.RawInterest) faucet.RawPlan {
			plan := serveAll(rawBody)(interests)
			plan.Fields = []string{"ref"}
			return plan
		})
	}

	for range 2 {
		page := r.read(r.query(lease, 0))
		require.Equal(t, ids, rawIDs(page.Events), "the same position returns the unread events again, in ID order")
		for i, ev := range page.Events {
			require.Equal(t, deliveries[i].DeliveryID, ev.DeliveryID)
			require.Equal(t, rawEvent, ev.Event)
			require.Equal(t, deliveries[i].Action, ev.Action)
			require.Equal(t, rawRepo, ev.RepoID)
			require.True(t, bytes.Equal(rawBody, ev.Body), "the body is returned byte for byte")
			require.Equal(t, []string{"ref"}, ev.Fields)
			require.WithinDuration(t, time.Now(), ev.ReceivedAt, time.Minute, "the store's clock assigns the receive time")
		}
	}
	require.Equal(t, int32(3), r.leasePending(lease))

	require.Equal(t, ids[1:], rawIDs(r.read(r.query(lease, ids[0])).Events), "a read returns only the events above its position")
	require.Equal(t, ids[1:], rawIDs(r.read(r.query(lease, 0)).Events), "a read deletes the unread events at or below its position")
	require.Equal(t, int32(2), r.leasePending(lease), "a deleted unread event is no longer pending")
}
