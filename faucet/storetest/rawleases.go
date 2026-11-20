package storetest

import (
	"bytes"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/coder/superblink-events/faucet"
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
	{"ReadRawLeasePassesOverEventsReceivedBeforeNotBefore", readRawLeasePassesOverEventsReceivedBeforeNotBefore},
	{"ReadRawLeasePassesOverDeletedClearedAndLostEvents", readRawLeasePassesOverDeletedClearedAndLostEvents},
	{"ReadRawLeaseNextIsTheHeadWhenNothingIsUnread", readRawLeaseNextIsTheHeadWhenNothingIsUnread},
	{"ReadRawLeaseCountsALostMarkOnceWhenItPassesIt", readRawLeaseCountsALostMarkOnceWhenItPassesIt},
	{"ReadRawLeaseNeverReturnsALostEventThatAnotherLeaseHolds", readRawLeaseNeverReturnsALostEventThatAnotherLeaseHolds},
	{"IngestRawKeepsOneLostMarkPerLeaseAtTheHighestID", ingestRawKeepsOneLostMarkPerLeaseAtTheHighestID},
	{"RawLeasesOfOneKeyAreIndependent", rawLeasesOfOneKeyAreIndependent},
	{"OpenRawLeaseEndsOnlyTheLeasePolledLongestAgoAtTheLimit", openRawLeaseEndsOnlyTheLeasePolledLongestAgoAtTheLimit},
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

// leaseSpaced pushes n events for every interest that matches them, with
// time between their receive times, and returns them as lease reads them.
func (r *rawRig) leaseSpaced(lease faucet.RawLease, n int32) []faucet.RawEvent {
	r.t.Helper()
	ids := make([]int64, 0, n)
	for range n {
		ids = append(ids, r.push())
		time.Sleep(2 * time.Millisecond)
	}
	q := r.query(lease, 0)
	q.Limit = n
	events := r.read(q).Events
	require.Equal(r.t, ids, rawIDs(events))
	return events
}

// leaseLose stores a delivery that lease misses and returns its raw ID.
func (r *rawRig) leaseLose(lease faucet.RawLease) int64 {
	r.t.Helper()
	return r.store(r.newDelivery(), func([]faucet.RawInterest) faucet.RawPlan {
		return faucet.RawPlan{Lost: []faucet.RawLeaseID{lease.ID}}
	})
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
	got, err := s.OpenRawLease(r.ctx, lease, r.now, rawLimits(rawMaxLive))
	require.NoError(t, err)
	require.Equal(t, head, got, "a new lease starts at the head")

	page := r.read(r.query(lease, 0))
	require.Empty(t, page.Events, "a new lease holds no event stored before it")
	require.Zero(t, page.Skipped)
	require.False(t, page.More)
	require.Equal(t, head, page.AckedThrough, "a new lease has acknowledged the head")
	require.Equal(t, head, page.Next)

	_, err = s.OpenRawLease(r.ctx, lease, r.now, rawLimits(rawMaxLive))
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
	q.Target = faucet.RepoTarget(rawRepo + 1)
	misses["another repository"] = q
	q = r.query(lease, ids[1])
	q.Target = faucet.OwnerTarget(rawOwner)
	misses["the owner of its repository"] = q
	q = r.query(lease, ids[1])
	q.Target = faucet.AllTarget()
	misses["every repository"] = q
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

func readRawLeasePassesOverEventsReceivedBeforeNotBefore(t *testing.T, s RawStore) {
	r := newRawRig(t, s)
	lease := r.open(rawSpec{})
	events := r.leaseSpaced(lease, 4)
	ids := rawIDs(events)

	q := r.query(lease, 0)
	q.NotBefore = events[2].ReceivedAt
	page := r.read(q)
	require.Equal(t, ids[2:], rawIDs(page.Events), "a read passes over the events received before its floor")
	require.Equal(t, 2, page.Skipped, "a read counts the events that it passed over")

	q.Limit = 1
	page = r.read(q)
	require.Equal(t, ids[2:3], rawIDs(page.Events))
	require.True(t, page.More)
	require.Equal(t, 2, page.Skipped, "events passed over do not count toward the limit")
	require.Equal(t, ids[2], page.Next, "the next position is past the events passed over")

	q.Limit, q.NotBefore = 100, events[3].ReceivedAt.Add(time.Hour)
	page = r.read(q)
	require.Empty(t, page.Events)
	require.Equal(t, 4, page.Skipped)
	require.False(t, page.More)
	require.Equal(t, ids[3], page.Next, "a page that passes over every event moves past them")
}

func readRawLeasePassesOverDeletedClearedAndLostEvents(t *testing.T, s RawStore) {
	r := newRawRig(t, s)
	lease := r.open(rawSpec{})
	events := r.leaseSpaced(lease, 3)
	kept := events[2].ID
	r.leaseLose(lease)
	last := r.push()
	res := r.purge(faucet.RawPurge{Now: r.now, DeleteBefore: events[1].ReceivedAt, ClearBefore: events[2].ReceivedAt})
	require.Equal(t, int64(1), res.Deleted, "the purge deletes the oldest event")

	q := r.query(lease, 0)
	q.Limit = 2
	page := r.read(q)
	require.Equal(t, []int64{kept, last}, rawIDs(page.Events), "rows passed over do not count toward the limit")
	require.Equal(t, 3, page.Skipped, "a deleted event, a cleared event and a lost mark are passed over and counted")
	require.False(t, page.More)
	require.Equal(t, last, page.Next)

	q.Limit = 1
	page = r.read(q)
	require.Equal(t, []int64{kept}, rawIDs(page.Events))
	require.Equal(t, 2, page.Skipped, "the page counts only the rows that it passed")
	require.True(t, page.More)
	require.Equal(t, kept, page.Next, "the next position is past the rows passed over")
}

func readRawLeaseNextIsTheHeadWhenNothingIsUnread(t *testing.T, s RawStore) {
	r := newRawRig(t, s)
	lease := r.open(rawSpec{})
	r.open(rawSpec{event: "issues"})
	page := r.read(r.query(lease, 0))
	require.Empty(t, page.Events)
	require.Zero(t, page.Next, "an empty store has head 0")

	own := r.push()
	issues := faucet.RawDelivery{DeliveryID: "raw-issues-1", Event: "issues", RepoID: rawRepo}
	r.store(issues, serveAll(rawBody))
	issues.DeliveryID = "raw-issues-2"
	head := r.store(issues, serveAll(rawBody))

	page = r.read(r.query(lease, 0))
	require.Equal(t, []int64{own}, rawIDs(page.Events))
	require.Equal(t, head, page.Next, "the next position is the head when nothing beyond the page is unread")
	page = r.read(r.query(lease, own))
	require.Empty(t, page.Events)
	require.Equal(t, head, page.Next, "the head rose through events of other interests")
	page = r.read(r.query(lease, head+5))
	require.Equal(t, head+5, page.Next, "the next position is never below the read's position")
}

func readRawLeaseCountsALostMarkOnceWhenItPassesIt(t *testing.T, s RawStore) {
	r := newRawRig(t, s)
	lease := r.open(rawSpec{})
	event := r.push()
	mark := r.leaseLose(lease)

	q := r.query(lease, 0)
	q.Limit = 1
	page := r.read(q)
	require.Equal(t, []int64{event}, rawIDs(page.Events))
	require.True(t, page.More, "the lease holds a mark beyond the page")
	require.Zero(t, page.Skipped, "a page that ends before a mark does not count it")
	require.Equal(t, event, page.Next)

	page = r.read(r.query(lease, page.Next))
	require.Empty(t, page.Events)
	require.Equal(t, 1, page.Skipped, "the page that passes a mark counts it")
	require.False(t, page.More)
	require.Equal(t, mark, page.Next)

	page = r.read(r.query(lease, page.Next))
	require.Zero(t, page.Skipped, "a mark at or below the read's position is counted once")
	page = r.read(r.query(lease, 0))
	require.Zero(t, page.Skipped, "a read deletes the mark at or below its position")
	require.Empty(t, page.Events)
}

func ingestRawKeepsOneLostMarkPerLeaseAtTheHighestID(t *testing.T, s RawStore) {
	r := newRawRig(t, s)
	lease := r.open(rawSpec{})
	first := r.leaseLose(lease)
	r.leaseLose(lease)
	event := r.push()
	require.Equal(t, int32(1), r.leasePending(lease), "a lease's pending count never counts its mark")

	page := r.read(r.query(lease, 0))
	require.Equal(t, []int64{event}, rawIDs(page.Events))
	require.Equal(t, 1, page.Skipped, "two ingests that a lease misses leave one mark")
	page = r.read(r.query(lease, first))
	require.Equal(t, []int64{event}, rawIDs(page.Events))
	require.Equal(t, 1, page.Skipped, "the mark is at the higher raw ID")
}

func rawLeasesOfOneKeyAreIndependent(t *testing.T, s RawStore) {
	r := newRawRig(t, s)
	one, two := r.open(rawSpec{}), r.open(rawSpec{})
	require.Equal(t, one.Key, two.Key)
	require.NotEqual(t, one.ID, two.ID)
	ids := []int64{r.push(), r.push()}

	require.Equal(t, ids, rawIDs(r.read(r.query(one, 0)).Events), "each lease gets every event")
	require.Empty(t, r.read(r.query(one, ids[1])).Events)
	require.Equal(t, ids, rawIDs(r.read(r.query(two, 0)).Events), "a read of one lease deletes nothing of the other")
	require.Zero(t, r.read(r.query(two, 0)).AckedThrough, "a read of one lease acknowledges nothing for the other")
}

func openRawLeaseEndsOnlyTheLeasePolledLongestAgoAtTheLimit(t *testing.T, s RawStore) {
	r := newRawRig(t, s)
	const maxLive = 3
	var n uint64 = 100
	openAt := func(w rawSpec, at time.Time, ttl time.Duration) faucet.RawLease {
		t.Helper()
		n++
		lease := newRawLease(n, w, at.Add(ttl))
		_, err := s.OpenRawLease(r.ctx, lease, at, rawLimits(maxLive))
		require.NoError(t, err)
		return lease
	}
	readAt := func(lease faucet.RawLease, at time.Time) (faucet.RawPage, bool) {
		t.Helper()
		q := r.query(lease, 0)
		q.Now, q.ExpiresAt = at, at.Add(time.Hour)
		return r.leaseFind(q)
	}
	minute := func(m int) time.Time { return r.now.Add(time.Duration(m) * time.Minute) }

	openAt(rawSpec{}, minute(0), time.Minute)
	hubot := []faucet.RawLease{
		openAt(rawSpec{principal: "hubot"}, minute(0), time.Hour),
		openAt(rawSpec{principal: "hubot"}, minute(0), time.Hour),
		openAt(rawSpec{principal: "hubot"}, minute(0), time.Hour),
	}
	first := openAt(rawSpec{}, minute(2), time.Hour)
	second := openAt(rawSpec{}, minute(3), time.Hour)
	third := openAt(rawSpec{}, minute(4), time.Hour)
	for _, lease := range []faucet.RawLease{first, second, third} {
		_, found := readAt(lease, minute(5))
		require.True(t, found, "an expired lease and another principal's leases do not count toward the limit")
	}

	_, found := readAt(first, minute(6))
	require.True(t, found)
	_, found = readAt(third, minute(6))
	require.True(t, found)
	event := r.store(r.newDelivery(), serveAll(rawBody))
	fourth := openAt(rawSpec{}, minute(7), time.Hour)

	_, found = readAt(second, minute(8))
	require.False(t, found, "a new lease at the limit ends the lease polled longest ago")
	for _, lease := range append([]faucet.RawLease{first, third}, hubot...) {
		page, found := readAt(lease, minute(8))
		require.True(t, found, "a new lease ends only one lease")
		require.Equal(t, []int64{event}, rawIDs(page.Events), "the leases that stay keep their events")
	}
	_, found = readAt(fourth, minute(8))
	require.True(t, found)
}

func readRawLeaseNeverReturnsALostEventThatAnotherLeaseHolds(t *testing.T, s RawStore) {
	r := newRawRig(t, s)
	missed, served := r.open(rawSpec{}), r.open(rawSpec{principal: "bob"})
	id := r.store(r.newDelivery(), func(interests []faucet.RawInterest) faucet.RawPlan {
		return faucet.RawPlan{
			Body:   rawBody,
			Unread: []faucet.RawLeaseID{leaseInterest(t, interests, served).Lease},
			Lost:   []faucet.RawLeaseID{leaseInterest(t, interests, missed).Lease},
		}
	})

	page := r.read(r.query(missed, 0))
	require.Empty(t, page.Events, "the lease that missed the event never reads it, though its body is stored")
	require.Equal(t, 1, page.Skipped)
	require.Equal(t, id, page.Next)
	require.Equal(t, []int64{id}, rawIDs(r.read(r.query(served, 0)).Events), "the lease that the event was for reads it")
}
