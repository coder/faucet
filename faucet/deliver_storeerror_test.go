package faucet_test

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/xerrors"

	"github.com/coder/faucet/faucet"
)

var errStoreDown = xerrors.New("store down: connection reset by peer")

// settle is how long the first worker has to record its error before the other
// call returns. The store has no signal for it. A worker that is late only makes
// the test run the other order, and never makes it fail.
const settle = 100 * time.Millisecond

// signal is a one-shot event that a test waits for.
type signal struct {
	done chan struct{}
	once sync.Once
}

func newSignal() *signal { return &signal{done: make(chan struct{})} }

func (s *signal) fire() { s.once.Do(func() { close(s.done) }) }

// wait reports whether the signal fired before a hung test would give up.
func (s *signal) wait() bool {
	select {
	case <-s.done:
		return true
	case <-time.After(10 * time.Second):
		return false
	}
}

func (s *signal) await(t *testing.T, what string) {
	t.Helper()
	if !s.wait() {
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

	// inside is the second worker in its claim, cut when the claim returned,
	// and finishing the first worker in the recording of its result.
	inside, cut, finishing *signal
}

func newEndedClaimStore(claimFirst bool) *endedClaimStore {
	return &endedClaimStore{claimFirst: claimFirst, inside: newSignal(), cut: newSignal(), finishing: newSignal()}
}

func (s *endedClaimStore) ClaimDelivery(ctx context.Context, now time.Time, lease time.Duration, skip []string) (faucet.DeliveryClaim, bool, error) {
	if s.claims.Add(1) == 1 {
		return s.Store.ClaimDelivery(ctx, now, lease, skip)
	}
	s.inside.fire()
	<-ctx.Done()
	s.cut.fire()
	return faucet.DeliveryClaim{}, false, ctx.Err()
}

func (s *endedClaimStore) FinishDelivery(context.Context, faucet.DeliveryRef, time.Time, faucet.Finish) (faucet.FinishResult, error) {
	s.finishing.fire()
	if s.claimFirst {
		<-s.cut.done
		time.Sleep(settle)
	} else {
		<-s.inside.done
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

			store.inside.await(t, "the second worker's claim")
			store.finishing.await(t, "the first worker's result")
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

// finishTimesOutStore ends the run during the recording of a result, and the
// recording then times out, as a stalled query does under its own timeout.
type finishTimesOutStore struct {
	faucet.Store
	endRun func()
}

func (s *finishTimesOutStore) FinishDelivery(context.Context, faucet.DeliveryRef, time.Time, faucet.Finish) (faucet.FinishResult, error) {
	s.endRun()
	return faucet.FinishResult{}, xerrors.Errorf("update the delivery: %w", context.DeadlineExceeded)
}

// The result is recorded under a context that the run's end does not cancel,
// with a timeout of its own. A store that stalls past it is down, whatever the
// run's context did meanwhile.
func TestDeliverDueReturnsTheTimeoutOfARecordedResultAfterTheRunEnded(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d := newDeliverHarness(t, func(deps *faucet.Deps) {
		deps.Store = &finishTimesOutStore{Store: deps.Store, endRun: cancel}
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

// claimFailsStore fails one kind of claim with what fail returns.
type claimFailsStore struct {
	faucet.Store
	fail func(ctx context.Context) error
	gap  bool
}

func (s *claimFailsStore) ClaimGap(ctx context.Context, now time.Time, lease time.Duration, skip []string) (faucet.GapClaim, bool, error) {
	if !s.gap {
		return s.Store.ClaimGap(ctx, now, lease, skip)
	}
	return faucet.GapClaim{}, false, s.fail(ctx)
}

func (s *claimFailsStore) ClaimDelivery(ctx context.Context, now time.Time, lease time.Duration, skip []string) (faucet.DeliveryClaim, bool, error) {
	if s.gap {
		return s.Store.ClaimDelivery(ctx, now, lease, skip)
	}
	return faucet.DeliveryClaim{}, false, s.fail(ctx)
}

// endsRunInClaim ends the run inside the claim, as the cancel cuts a Postgres
// query short.
func endsRunInClaim(endRun func()) func(context.Context) error {
	return func(ctx context.Context) error {
		endRun()
		return ctx.Err()
	}
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
				deps.Store = &claimFailsStore{Store: deps.Store, fail: endsRunInClaim(cancel), gap: tc.gap}
			})

			res, err := d.svc.DeliverDue(ctx)
			require.NoError(t, err, "a claim that the run's own end cut short is no store failure")
			require.True(t, res.Incomplete)
		})
	}
}

// A store call can fail with an error that wraps a context error for a reason
// of its own, such as a timeout inside the store. The run's context is then
// alive, so the failure is real and stays the run's error.
func TestDeliverDueKeepsAContextLookingClaimErrorWhileTheRunIsAlive(t *testing.T) {
	t.Parallel()

	for _, claim := range []struct {
		name string
		gap  bool
	}{
		{"a gap claim", true},
		{"a delivery claim", false},
	} {
		for _, cause := range []struct {
			name string
			err  error
		}{
			{"a timeout of the store's own", context.DeadlineExceeded},
			{"a cancel of the store's own", context.Canceled},
		} {
			t.Run(claim.name+" with "+cause.name, func(t *testing.T) {
				t.Parallel()
				d := newDeliverHarness(t, func(deps *faucet.Deps) {
					deps.Store = &claimFailsStore{Store: deps.Store, gap: claim.gap, fail: func(context.Context) error {
						return xerrors.Errorf("claim: %w", cause.err)
					}}
				})

				_, err := d.svc.DeliverDue(d.ctx)
				require.ErrorIs(t, err, cause.err, "the run's own context is alive, so this is a store failure and not the end of the run")
			})
		}
	}
}

var (
	errFirstFailure = xerrors.New("the first store failure")
	errLaterFailure = xerrors.New("a later store failure")
)

// twoFailuresStore plays two workers of one run whose results the store fails
// to record. Both workers are inside the recording before either fails, so that
// the first failure cannot stop the run before the other worker starts. The
// first worker to arrive then fails at once. The other fails once every other
// goroutine of the bubble waits, and the first worker has recorded its error
// and returned by then.
type twoFailuresStore struct {
	faucet.Store
	arrivals atomic.Int32
	bothIn   *signal
}

func (s *twoFailuresStore) FinishDelivery(context.Context, faucet.DeliveryRef, time.Time, faucet.Finish) (faucet.FinishResult, error) {
	if s.arrivals.Add(1) == 1 {
		if !s.bothIn.wait() {
			return faucet.FinishResult{}, xerrors.New("the second worker never arrived")
		}
		return faucet.FinishResult{}, errFirstFailure
	}
	s.bothIn.fire()
	synctest.Wait()
	return faucet.FinishResult{}, errLaterFailure
}

// answersOK plays a callback endpoint that accepts every event.
func answersOK(req *http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: http.NoBody, Request: req}, nil
}

// The first failure is the cause that the operator needs. Later ones can be its
// echoes.
//
// A synctest bubble orders the two failures, with no margin of real time that a
// loaded machine can eat. The bubble has no socket, as a goroutine that waits
// on one does not count as waiting.
func TestDeliverDueReturnsTheFirstStoreFailureWhenWorkersFailInTurn(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		store := &twoFailuresStore{bothIn: newSignal()}
		d := newDeliverHarness(t, func(deps *faucet.Deps) {
			store.Store = deps.Store
			deps.Store = store
			deps.Webhook.Client = &http.Client{Transport: roundTripFunc(answersOK)}
		})
		d.rcv.server.Close()
		d.ledgerSkip = true
		d.subscribeEndpoint(d.endpoint("/hook"))
		d.advance(time.Second)
		d.newRunEvent()
		d.newRunEvent()

		// The bubble's clock starts in the year 2000, behind the harness's mock
		// clock, so the harness's deadline would end the run before it starts.
		_, err := d.svc.DeliverDue(context.WithoutCancel(d.ctx))
		require.ErrorIs(t, err, errFirstFailure, "the failure that came first is the run's error")
		require.NotErrorIs(t, err, errLaterFailure, "a later failure does not replace it")
	})
}
