package faucet_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/xerrors"

	"github.com/coder/faucet/faucet"
)

var errStoreDown = xerrors.New("store down: connection reset by peer")

// settle is how long the first worker has to record its error before the other
// call returns. The store has no signal for it. A worker that is late makes the
// test pass for the other order and never fail.
const settle = 100 * time.Millisecond

func awaitSignal(t *testing.T, signal <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(10 * time.Second):
		t.Fatalf("%s never happened", what)
	}
}

// endedClaimStore plays two workers of one run. The first worker sends its
// delivery, and the store then fails to record the result. The second worker
// is inside a claim that the end of the run cuts short, as the cancel cuts a
// Postgres query.
type endedClaimStore struct {
	faucet.Store
	claims atomic.Int32
	// claimFirst makes the cut claim return before the result fails.
	claimFirst bool

	claimInside, claimCut, finishInside func()
	inside, cut, finishing              chan struct{}
}

func newEndedClaimStore(claimFirst bool) *endedClaimStore {
	s := &endedClaimStore{claimFirst: claimFirst, inside: make(chan struct{}), cut: make(chan struct{}), finishing: make(chan struct{})}
	s.claimInside = sync.OnceFunc(func() { close(s.inside) })
	s.claimCut = sync.OnceFunc(func() { close(s.cut) })
	s.finishInside = sync.OnceFunc(func() { close(s.finishing) })
	return s
}

func (s *endedClaimStore) ClaimDelivery(ctx context.Context, now time.Time, lease time.Duration, skip []string) (faucet.DeliveryClaim, bool, error) {
	if s.claims.Add(1) == 1 {
		return s.Store.ClaimDelivery(ctx, now, lease, skip)
	}
	s.claimInside()
	<-ctx.Done()
	s.claimCut()
	return faucet.DeliveryClaim{}, false, ctx.Err()
}

func (s *endedClaimStore) FinishDelivery(context.Context, faucet.DeliveryRef, time.Time, faucet.Finish) (faucet.FinishResult, error) {
	s.finishInside()
	if s.claimFirst {
		<-s.cut
		time.Sleep(settle)
	} else {
		<-s.inside
	}
	return faucet.FinishResult{}, errStoreDown
}

func TestDeliverDueKeepsAStoreErrorWhenTheEndOfTheRunCutsAClaimShort(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name       string
		claimFirst bool
	}{
		{"the cut claim returns first", true},
		{"the store error returns first", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			store := newEndedClaimStore(tc.claimFirst)
			d := newDeliverHarness(t, func(deps *faucet.Deps) {
				store.Store = deps.Store
				deps.Store = store
			})
			d.ledgerSkip = true
			d.subscribeEndpoint(d.endpoint("/hook"))
			d.advance(time.Second)
			d.newRunEvent()

			ctx, cancel := context.WithCancel(d.ctx)
			defer cancel()
			done := make(chan error, 1)
			go func() {
				_, err := d.svc.DeliverDue(ctx)
				done <- err
			}()

			awaitSignal(t, store.inside, "the second worker's claim")
			awaitSignal(t, store.finishing, "the first worker's result")
			if !tc.claimFirst {
				time.Sleep(settle)
			}
			cancel()
			select {
			case err := <-done:
				require.ErrorIs(t, err, errStoreDown, "a store failure is the run's error, whichever of the two calls returns first")
			case <-time.After(10 * time.Second):
				t.Fatal("the run did not end")
			}
		})
	}
}

// finishEndsStore ends the run during the recording of a result and fails it
// with err.
type finishEndsStore struct {
	faucet.Store
	endRun func()
	err    error
}

func (s *finishEndsStore) FinishDelivery(context.Context, faucet.DeliveryRef, time.Time, faucet.Finish) (faucet.FinishResult, error) {
	s.endRun()
	return faucet.FinishResult{}, s.err
}

// The result is recorded under a context that the run's end does not cancel,
// with a timeout of its own. A store that stalls past it is down, whatever the
// run's context did meanwhile.
func TestDeliverDueReturnsTheTimeoutOfARecordedResultAfterTheRunEnded(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d := newDeliverHarness(t, func(deps *faucet.Deps) {
		deps.Store = &finishEndsStore{Store: deps.Store, endRun: cancel, err: xerrors.Errorf("update the delivery: %w", context.DeadlineExceeded)}
	})
	d.ledgerSkip = true
	d.subscribeEndpoint(d.endpoint("/hook"))
	d.advance(time.Second)
	d.newRunEvent()

	_, err := d.svc.DeliverDue(ctx)
	require.ErrorIs(t, err, context.DeadlineExceeded, "the store timed out on its own, and the run's end is no reason to hide it")
}

// sweepEndsStore ends the run during the sweep of previous secrets. The sweep
// fails with the cut query's context error when cut is set, and with
// errStoreDown otherwise.
type sweepEndsStore struct {
	faucet.Store
	endRun func()
	cut    bool
}

func (s *sweepEndsStore) ClearExpiredPreviousSecrets(ctx context.Context, _ time.Time) (int64, error) {
	s.endRun()
	if s.cut {
		return 0, ctx.Err()
	}
	return 0, errStoreDown
}

func TestDeliverDueLogsAFailedSweepOfPreviousSecretsUnlessTheEndOfTheRunCausedIt(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		cut    bool
		logged bool
	}{
		{"the store failed for its own reason", false, true},
		{"the end of the run cut the sweep short", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			d := newDeliverHarness(t, func(deps *faucet.Deps) {
				deps.Store = &sweepEndsStore{Store: deps.Store, endRun: cancel, cut: tc.cut}
			})
			d.ledgerSkip = true

			res, err := d.svc.DeliverDue(ctx)
			require.NoError(t, err, "a failed sweep ends nothing")
			require.True(t, res.Incomplete)
			require.Equal(t, tc.logged, d.logsContain("clear expired previous secrets"))
		})
	}
}

// claimEndsStore ends the run inside one kind of claim, as the cancel cuts a
// Postgres query short.
type claimEndsStore struct {
	faucet.Store
	endRun func()
	gap    bool
}

func (s *claimEndsStore) ClaimGap(ctx context.Context, now time.Time, lease time.Duration, skip []string) (faucet.GapClaim, bool, error) {
	if !s.gap {
		return s.Store.ClaimGap(ctx, now, lease, skip)
	}
	s.endRun()
	return faucet.GapClaim{}, false, ctx.Err()
}

func (s *claimEndsStore) ClaimDelivery(ctx context.Context, now time.Time, lease time.Duration, skip []string) (faucet.DeliveryClaim, bool, error) {
	if s.gap {
		return s.Store.ClaimDelivery(ctx, now, lease, skip)
	}
	s.endRun()
	return faucet.DeliveryClaim{}, false, ctx.Err()
}

func TestDeliverDueIsIncompleteNotFailedWhenTheEndOfTheRunCutsAClaimShort(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		gap  bool
	}{
		{"a gap claim", true},
		{"a delivery claim", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			d := newDeliverHarness(t, func(deps *faucet.Deps) {
				deps.Store = &claimEndsStore{Store: deps.Store, endRun: cancel, gap: tc.gap}
			})

			res, err := d.svc.DeliverDue(ctx)
			require.NoError(t, err, "a claim that the run's own end cut short is no store failure")
			require.True(t, res.Incomplete)
		})
	}
}
