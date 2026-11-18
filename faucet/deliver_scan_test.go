package faucet_test

import (
	"context"
	"net/http"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/coder/faucet/faucet"
)

// someScansBroken fails or stalls the reads of the subscriptions that filter
// by sha, and passes the others to the store. A stalled read waits for its
// context, or an hour of the bubble's clock when nothing ends it, and then
// moves the service's clock by the time it took.
type someScansBroken struct {
	faucet.Store
	stall   bool
	elapsed func(time.Duration)
}

func (s *someScansBroken) PollEvents(ctx context.Context, q faucet.PollQuery) (faucet.PollPage, error) {
	if q.HeadSHA == "" {
		return s.Store.PollEvents(ctx, q)
	}
	if !s.stall {
		return faucet.PollPage{}, errStoreDown
	}
	start := time.Now()
	defer func() { s.elapsed(time.Since(start)) }()
	select {
	case <-ctx.Done():
		return faucet.PollPage{}, ctx.Err()
	case <-time.After(time.Hour):
		return faucet.PollPage{}, errStoreDown
	}
}

// Subscriptions whose reads stall or fail must not keep a run from the
// others. The broken ones are the furthest behind, so the run reads them
// first, under the deadline that /cron/deliver gives a run.
func TestSubscriptionsWhoseReadsFailDoNotStopTheRunFromTheOthers(t *testing.T) {
	t.Parallel()
	for name, stall := range map[string]bool{"the reads stall": true, "the reads fail": false} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				store := &someScansBroken{stall: stall}
				d := newDeliverHarness(t, func(deps *faucet.Deps) {
					store.Store = deps.Store
					deps.Store = store
					deps.Webhook.Client = &http.Client{Transport: roundTripFunc(answersOK)}
				})
				store.elapsed = d.advance
				d.rcv.server.Close()
				d.ledgerSkip = true
				d.subscribeEndpoint(d.endpoint("/broken", "sha", testShaA))
				d.subscribeEndpoint(d.endpoint("/broken-too", "sha", testShaA))
				d.advance(time.Second)
				d.newRunEvent()
				d.advance(time.Second)
				d.subscribeEndpoint(d.endpoint("/healthy"))
				d.newRunEvent()

				// The run's deadline is on the service's clock, which is ahead
				// of the bubble's, so only the service sees it come near.
				ctx, cancel := context.WithDeadline(context.WithoutCancel(d.ctx), d.clock.Now().Add(25*time.Second))
				defer cancel()
				res, err := d.svc.DeliverDue(ctx)
				require.Equal(t, 1, res.Enqueued, "the healthy subscription's event is queued")
				require.Equal(t, 1, res.Delivered, "and sent in the same run")
				if stall {
					require.NoError(t, err, "a read that runs out of time is no store failure")
					require.True(t, res.Incomplete, "the stalled subscriptions are left for the next run")
					require.True(t, d.logsContain("skipped a subscription whose events could not be read"), "the stalled reads are logged")
				} else {
					require.ErrorIs(t, err, errStoreDown, "the failure is still the run's error")
				}
			})
		})
	}
}
