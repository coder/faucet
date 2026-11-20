package faucet_test

import (
	"context"
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/xerrors"

	"github.com/coder/superblink-events/faucet"
)

var errRawStoreDown = xerrors.New("raw store is down")

// rawPurgeDown is a RawStore whose purge fails after the event log's has
// passed.
type rawPurgeDown struct{ faucet.RawStore }

func (rawPurgeDown) PurgeRawEvents(context.Context, faucet.RawPurge) (faucet.RawPurgeResult, error) {
	return faucet.RawPurgeResult{}, errRawStoreDown
}

// purge purges with a retention of 30 days, as a host does.
func (h *ingestHarness) purge() faucet.PurgeResult {
	h.t.Helper()
	res, err := h.svc.Purge(h.ctx, h.clock.Now().Add(-30*24*time.Hour))
	require.NoError(h.t, err)
	return res
}

// push stores the nth push of the repository through the webhook handler.
func (h *ingestHarness) push(n int) {
	h.t.Helper()
	code, body := h.post("push", "raw-purge-"+strconv.Itoa(n), rawPayload(repoID, "", map[string]any{"n": n}))
	require.Equal(h.t, 200, code, body)
	require.Equal(h.t, "stored", body)
}

func withoutRawStore(d *faucet.Deps) { d.RawStore, d.RawEvents = nil, nil }

func TestPurgeClearsARawEventOnceItsLeaseHasReadIt(t *testing.T) {
	t.Parallel()

	h := newIngestHarness(t)
	lease := h.openLease(ingestInterest{})
	h.push(1)

	require.Zero(t, h.purge().Raw, "an unread event keeps its payload")
	require.Len(t, lease.poll().Events, 1)
	require.Zero(t, h.purge().Raw, "an event that the poller has not acknowledged keeps its payload")

	require.Empty(t, lease.poll().Events, "the second poll acknowledges the first")
	require.Equal(t, faucet.RawPurgeResult{Cleared: 1}, h.purge().Raw)
	require.Zero(t, h.purge().Raw, "a cleared event is not counted again")
	require.Contains(t, logLine(t, h.logs.String(), "raw_events_cleared=1"), "[info]")
}

func TestPurgeClearsARawEventThatAPollerKeptUnreadForOverAnHour(t *testing.T) {
	t.Parallel()

	h := newIngestHarness(t)
	lease := h.openLease(ingestInterest{})
	start := lease.after
	h.push(1)

	h.clock.Advance(40 * time.Minute)
	require.Len(t, lease.poll().Events, 1)
	h.clock.Advance(25 * time.Minute)
	require.Equal(t, faucet.RawPurgeResult{Cleared: 1}, h.purge().Raw, "the lease is live, and its event is over an hour old")

	lease.after = start
	page := lease.poll()
	require.Empty(t, page.Events)
	require.Equal(t, 1, page.Skipped, "the poller learns that it lost the event")
}

func TestPurgeDeletesExpiredLeasesAndClearsWhatOnlyTheyHeld(t *testing.T) {
	t.Parallel()

	h := newIngestHarness(t)
	h.openLease(ingestInterest{})
	h.push(1)

	h.clock.Advance(time.Hour - time.Second)
	require.Zero(t, h.purge().Raw, "the lease is live for a second more")
	h.clock.Advance(time.Second)
	require.Equal(t, faucet.RawPurgeResult{ExpiredLeases: 1, Cleared: 1}, h.purge().Raw)

	code, body := h.post("push", "raw-purge-2", rawPayload(repoID, "", nil))
	require.Equal(t, 202, code, body)
	require.Equal(t, "ignored", body, "no lease is left to want a push")
}

func TestPurgeDeletesRawEventsAtTheRetention(t *testing.T) {
	t.Parallel()

	h := newIngestHarness(t)
	h.openLease(ingestInterest{})
	h.push(1)
	h.clock.Advance(29 * 24 * time.Hour)
	h.openLease(ingestInterest{})
	h.push(2)
	h.clock.Advance(24*time.Hour + time.Second)

	res := h.purge()
	require.EqualValues(t, 1, res.Raw.Deleted, "only the first push is older than the retention")

	lease := h.openLease(ingestInterest{})
	code, body := h.post("push", "raw-purge-1", rawPayload(repoID, "", map[string]any{"n": 1}))
	require.Equal(t, 200, code, body)
	require.Equal(t, "stored", body, "a deleted delivery ID is stored again")
	code, body = h.post("push", "raw-purge-2", rawPayload(repoID, "", map[string]any{"n": 2}))
	require.Equal(t, 200, code, body)
	require.Equal(t, "duplicate", body, "a cleared event keeps its delivery ID")
	require.Len(t, lease.poll().Events, 1)
}

func TestPurgeEndsTheRawDeliveriesOfAnExpiredSubscription(t *testing.T) {
	t.Parallel()

	h := newIngestHarness(t)
	h.subscribe(ingestInterest{})
	h.push(1)
	h.push(2)

	require.Zero(t, h.purge(), "the subscription is live and its deliveries are unsent")
	h.logs.Reset()
	h.clock.Advance(time.Hour)
	res := h.purge()
	require.Equal(t, faucet.PurgeResult{
		ExpiredSubscriptions: 1,
		EndedDeliveries:      2,
		Raw:                  faucet.RawPurgeResult{Cleared: 2},
	}, res)
	require.Empty(t, h.claims())

	line := logLine(t, h.logs.String(), "events purged")
	require.Contains(t, line, "[info]")
	for _, field := range []string{"deliveries_ended=2", "raw_events_cleared=2", "raw_events_deleted=0", "expired_leases=0"} {
		require.Contains(t, line, field)
	}
}

func TestPurgeWithoutARawStoreReportsAndLogsNothingRaw(t *testing.T) {
	t.Parallel()

	h := newIngestHarness(t, withoutRawStore)
	lease := h.openLease(ingestInterest{})
	_, err := h.store.IngestRaw(h.ctx, faucet.RawDelivery{DeliveryID: "d", Event: "push", RepoID: repoID}, h.clock.Now(),
		func([]faucet.RawInterest) faucet.RawPlan {
			return faucet.RawPlan{Body: []byte(`{}`), Unread: []faucet.RawLeaseID{lease.read.Lease}}
		})
	require.NoError(t, err)
	h.clock.Advance(2 * time.Hour)

	require.Zero(t, h.purge(), "the service has no RawStore, so it purges nothing raw")
	line := logLine(t, h.logs.String(), "events purged")
	require.Contains(t, line, "deliveries_ended=0")
	require.NotContains(t, line, "raw_events")
	require.NotContains(t, line, "expired_leases")
	require.EqualValues(t, 1, h.head(), "the raw event is still stored")
}

// rawExpireDown is a RawStore whose purge passes and whose end of the
// deliveries of expired subscriptions fails.
type rawExpireDown struct{ faucet.RawStore }

func (rawExpireDown) ExpireRawDeliveries(context.Context, time.Time) (int64, error) {
	return 0, errRawStoreDown
}

func TestARawPurgeThatFailsIsCountedAndThePurgeGoesOn(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		down    func(faucet.RawStore) faucet.RawStore
		expired int64
		logged  string
	}{
		"the purge of raw events": {func(s faucet.RawStore) faucet.RawStore { return rawPurgeDown{s} }, 0, "purge raw events"},
		"the end of raw deliveries": {
			func(s faucet.RawStore) faucet.RawStore { return rawExpireDown{s} }, 1, "end raw deliveries of expired subscriptions",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newIngestHarness(t, func(d *faucet.Deps) { d.RawStore = tc.down(d.RawStore) })
			h.openLease(ingestInterest{})
			h.push(1)
			_, _, err := h.store.InsertEvent(h.ctx, faucet.Event{
				Source: "github", DeliveryID: "typed", Type: "ci.run_completed", RepoID: repoID, HeadSHA: "abc",
				Data: json.RawMessage(sampleData["ci.run_completed"]),
			})
			require.NoError(t, err)
			h.clock.Advance(31 * 24 * time.Hour)

			res := h.purge()
			require.EqualValues(t, 1, res.Deleted, "the typed events past the retention are deleted")
			require.EqualValues(t, 1, res.RawFailures)
			require.Equal(t, tc.expired, res.Raw.ExpiredLeases, "a raw step that fails leaves the other to run")
			require.Contains(t, logLine(t, h.logs.String(), "a raw purge step failed, the purge goes on"), tc.logged)
			require.Contains(t, h.logs.String(), errRawStoreDown.Error())
		})
	}
}
