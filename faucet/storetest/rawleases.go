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
	{"ReadRawLeaseReportsTheHighestPositionAcknowledgedBefore", readRawLeaseReportsTheHighestPositionAcknowledgedBefore},
	{"ReadRawLeaseMovesTheExpiry", readRawLeaseMovesTheExpiry},
	{"ReadRawLeaseEndsAPageAtTheLimit", readRawLeaseEndsAPageAtTheLimit},
	{"ReadRawLeaseEndsAPageAtMaxBytes", readRawLeaseEndsAPageAtMaxBytes},
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

func readRawLeaseReportsTheHighestPositionAcknowledgedBefore(t *testing.T, s RawStore) {
	r := newRawRig(t, s)
	r.subscribe(1, rawSpec{})
	opened := r.push()
	lease := r.open(rawSpec{})
	first, second := r.push(), r.push()
	r.push()

	steps := []struct {
		after, acked int64
		rule         string
	}{
		{first, opened, "before any read the lease has acknowledged the head at the open"},
		{second, first, "a read reports the position of the read before it"},
		{first, second, "a read reports the highest position of the reads before it"},
		{0, second, "a read at a lower position does not lower the acknowledged position"},
	}
	for _, step := range steps {
		require.Equal(t, step.acked, r.read(r.query(lease, step.after)).AckedThrough, step.rule)
	}
}

func readRawLeaseMovesTheExpiry(t *testing.T, s RawStore) {
	r := newRawRig(t, s)
	moved := r.open(rawSpec{})
	r.open(rawSpec{})
	q := r.query(moved, 0)
	q.ExpiresAt = r.now.Add(2 * time.Hour)
	r.read(q)

	later := r.now.Add(90 * time.Minute)
	interests := r.interestsAt(r.newDelivery(), later)
	leaseInterest(t, interests, moved)
	require.Len(t, interests, 1, "the lease that was not read expired at its old expiry")

	q = r.query(moved, 0)
	q.Now, q.ExpiresAt = later, later.Add(time.Hour)
	require.Len(t, r.read(q).Events, 1, "the lease is live until the expiry of its last read")
}

func readRawLeaseEndsAPageAtTheLimit(t *testing.T, s RawStore) {
	r := newRawRig(t, s)
	lease := r.open(rawSpec{})
	ids := []int64{r.push(), r.push(), r.push(), r.push(), r.push()}

	after := int64(0)
	for _, want := range [][]int64{ids[0:2], ids[2:4], ids[4:]} {
		q := r.query(lease, after)
		q.Limit = 2
		page := r.read(q)
		require.Equal(t, want, rawIDs(page.Events), "a page continues after the position and holds at most the limit")
		last := want[len(want)-1] == ids[len(ids)-1]
		require.Equal(t, !last, page.More, "only the last page has no more events")
		require.Equal(t, want[len(want)-1], page.Next, "a page at its limit ends at its last event")
		after = page.Next
	}
}

func readRawLeaseEndsAPageAtMaxBytes(t *testing.T, s RawStore) {
	r := newRawRig(t, s)
	lease := r.open(rawSpec{})
	ids := []int64{r.push(), r.push(), r.push(), r.push()}
	size := int64(len(rawBody))

	cases := []struct {
		maxBytes int64
		want     []int64
		rule     string
	}{
		{1, ids[:1], "the first event always fits, also when it alone is larger"},
		{size, ids[:1], "the page ends after the event that takes the bodies to the limit"},
		{size + 1, ids[:2], "the page ends after the event that takes the bodies beyond the limit"},
		{2 * size, ids[:2], "the page ends after the event that takes the bodies to the limit"},
		{3*size + 1, ids, "the page ends at the last event when the bodies stay below the limit"},
	}
	for _, tc := range cases {
		q := r.query(lease, 0)
		q.MaxBytes = tc.maxBytes
		page := r.read(q)
		require.Equal(t, tc.want, rawIDs(page.Events), "max bytes %d: %s", tc.maxBytes, tc.rule)
		require.Equal(t, len(tc.want) < len(ids), page.More, "max bytes %d: a page that ended early has more", tc.maxBytes)
		if page.More {
			require.Equal(t, tc.want[len(tc.want)-1], page.Next, "max bytes %d: a page that ended early ends at its last event", tc.maxBytes)
		}
	}
}
