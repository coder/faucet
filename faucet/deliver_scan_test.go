package faucet_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/coder/superblink-events/faucet"
)

// slowScans plays a store whose reads of the subscriptions that filter by
// sha are slow. A stalled read waits for its context, or an hour of the
// bubble's clock when nothing ends it. A costly read takes perEvent for each
// event that it looks at. A failing read fails at once. Each slow read moves
// the service's clock by the time it took, as the run would see it.
type slowScans struct {
	faucet.Store
	stall    bool
	perEvent time.Duration
	elapsed  func(time.Duration)
}

func (s *slowScans) ScanEvents(ctx context.Context, q faucet.ScanQuery) (faucet.ScanPage, error) {
	if q.HeadSHA == "" {
		return s.Store.ScanEvents(ctx, q)
	}
	start := time.Now()
	defer func() { s.elapsed(time.Since(start)) }()
	wait := time.Hour
	var page faucet.ScanPage
	switch {
	case s.perEvent > 0:
		var err error
		if page, err = s.Store.ScanEvents(ctx, q); err != nil {
			return faucet.ScanPage{}, err
		}
		wait = time.Duration(page.Next-q.After) * s.perEvent
	case !s.stall:
		return faucet.ScanPage{}, errStoreDown
	}
	select {
	case <-ctx.Done():
		return faucet.ScanPage{}, ctx.Err()
	case <-time.After(wait):
		if s.stall {
			return faucet.ScanPage{}, errStoreDown
		}
		return page, nil
	}
}

func newSlowScanHarness(t *testing.T, store *slowScans) *deliverHarness {
	t.Helper()
	d := newDeliverHarness(t, func(deps *faucet.Deps) {
		store.Store = deps.Store
		deps.Store = store
		deps.Webhook.Client = &http.Client{Transport: roundTripFunc(answersOK)}
	})
	store.elapsed = d.advance
	d.rcv.server.Close()
	d.ledgerSkip = true
	return d
}

// deliverWithCronDeadline runs DeliverDue with the deadline that /cron/deliver
// gives a run. The deadline is on the service's clock, which is ahead of the
// bubble's, so only the service sees it come near.
func deliverWithCronDeadline(d *deliverHarness) (faucet.DeliverResult, error) {
	ctx, cancel := context.WithDeadline(context.WithoutCancel(d.ctx), d.clock.Now().Add(25*time.Second))
	defer cancel()
	return d.svc.DeliverDue(ctx)
}

// Subscriptions whose reads stall or fail must not keep a run from the
// others. The broken ones are the furthest behind, so the run reads them
// first, and they are more than one principal's share of the run.
func TestSubscriptionsWhoseReadsFailDoNotStopTheRunFromTheOthers(t *testing.T) {
	t.Parallel()
	for name, stall := range map[string]bool{"the reads stall": true, "the reads fail": false} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				d := newSlowScanHarness(t, &slowScans{stall: stall})
				d.as("mallory")
				for i := range 6 {
					d.subscribeEndpoint(d.endpoint(fmt.Sprintf("/broken-%d", i), "sha", testShaA))
				}
				d.advance(time.Second)
				d.newRunEvent()
				d.advance(time.Second)
				d.as("octocat")
				d.subscribeEndpoint(d.endpoint("/healthy"))
				d.newRunEvent()

				res, err := deliverWithCronDeadline(d)
				require.Equal(t, 1, res.Enqueued, "the healthy subscription's event is queued")
				require.Equal(t, 1, res.Delivered, "and sent in the same run")
				if stall {
					require.NoError(t, err, "a read that runs out of time is no store failure")
					require.True(t, res.Incomplete, "the stalled subscriptions are left for the next run")
					require.True(t, d.logsContain("read ran out of time"), "the stalled reads are logged")
				} else {
					require.ErrorIs(t, err, errStoreDown, "the failure is still the run's error")
				}
			})
		})
	}
}

// Five principals replay a long history with a filter that matches nothing,
// and each replay takes longer than a run gives one principal. Each run must
// keep what each replay read, so the replays end and the subscription behind
// them is served, instead of the replays being read first, from the start,
// in every run.
func TestSlowReplaysMoveOnInEveryRunAndEnd(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		const backlog = 200_000
		d := newSlowScanHarness(t, &slowScans{perEvent: 10 * time.Microsecond})
		d.store.appendRunEvents(backlog)
		for i := range 5 {
			d.as(fmt.Sprintf("replayer-%d", i))
			ep := d.endpoint(fmt.Sprintf("/replay-%d", i), "sha", testShaA)
			d.subscribe(&rawParams{Name: "ci.run_completed", Arguments: raw(ep.args), Delivery: deliveryTo(d.rcv.url(ep.path), ep.secret), Cursor: cursorOf("v2.0")})
		}
		d.advance(time.Second)
		d.as("octocat")
		d.subscribeEndpoint(d.endpoint("/healthy"))
		d.newRunEvent()

		delivered := 0
		var last faucet.DeliverResult
		for range 3 {
			res, err := deliverWithCronDeadline(d)
			require.NoError(t, err)
			delivered += res.Delivered
			last = res
		}
		require.Equal(t, 1, delivered, "the subscription behind the replays gets its event within three runs")
		require.False(t, last.Incomplete, "and the replays have ended")
	})
}

// A subscription stored before the limits can hold a list past them. A run
// ends it instead of reading it, as it ends one whose arguments do not decode.
func TestARunEndsAStoredSubscriptionWhoseListsArePastTheLimits(t *testing.T) {
	t.Parallel()

	d := newDeliverHarness(t)
	d.subscribeEndpoint(d.endpoint("/hook"))
	pastTheLimit := make([]string, listLimit+1)
	for i := range pastTheLimit {
		pastTheLimit[i] = fmt.Sprintf("c%03d", i)
	}
	d.store.setArguments(d.rows()[0].KeyHash, raw(repoArgs("conclusion", pastTheLimit)))
	d.advance(time.Second)
	d.newRunEvent()

	require.Zero(t, d.deliverDue().Enqueued)
	require.Empty(t, d.rows(), "the subscription ended")
	for _, q := range d.store.scanQueries() {
		require.Empty(t, q.Fields, "the run never read its events")
	}
	require.True(t, d.logsContain("stored arguments do not decode or pass the limits"))
}

// Two principals' stalled subscriptions fill a whole batch of the
// subscriptions furthest behind. Once both have used their share, the next
// batch must pass over them to reach the rest.
func TestAFullBatchOfStalledSubscriptionsDoesNotHideTheRest(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		d := newSlowScanHarness(t, &slowScans{stall: true})
		for _, login := range []string{"mallory", "trudy"} {
			d.as(login)
			for i := range 100 {
				d.subscribeEndpoint(d.endpoint(fmt.Sprintf("/%s-%d", login, i), "sha", testShaA))
			}
		}
		d.advance(time.Second)
		d.newRunEvent()
		d.advance(time.Second)
		d.as("octocat")
		d.subscribeEndpoint(d.endpoint("/healthy"))
		d.newRunEvent()

		res, err := deliverWithCronDeadline(d)
		require.NoError(t, err)
		require.Equal(t, 1, res.Delivered, "the subscription behind the full batch is read and sent")
	})
}

// A purge that a subscription is behind can span several windows of its read.
// The run must count the gap once and send one gap envelope, as when the read
// was one statement.
func TestAGapThatSpansSeveralWindowsIsCountedAndSentOnce(t *testing.T) {
	t.Parallel()

	d := newDeliverHarness(t)
	ep := d.endpoint("/hook")
	d.subscribeEndpoint(ep)
	d.advance(time.Second)
	d.store.appendRunEvents(120_000)
	d.advance(5 * time.Minute)
	d.newRunEvent()
	_, err := d.store.Purge(d.ctx, d.clock.Now().Add(-time.Minute))
	require.NoError(t, err)

	res := d.deliverDue()
	require.Equal(t, 1, res.Gaps, "one subscription missed purged events")
	require.Equal(t, 1, res.Delivered, "and gets the event after them")
	require.Zero(t, d.deliverDue().Gaps)
	require.Len(t, d.rcv.envelopesOf("/hook", "gap"), 1)
}
