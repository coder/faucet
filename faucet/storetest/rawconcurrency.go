package storetest

import (
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/xerrors"

	"github.com/coder/faucet/faucet"
)

// rawConcurrencySubtests are the contract cases with concurrent callers.
var rawConcurrencySubtests = []rawSubtest{
	{"IngestRawSeesASubscriptionAsSoonAsItsUpsertReturns", ingestRawSeesASubscriptionAsSoonAsItsUpsertReturns},
	{"ReadRawLeaseCursorMissesNoConcurrentIngest", readRawLeaseCursorMissesNoConcurrentIngest},
	{"OpenRawLeaseKeepsTheLimitUnderConcurrentOpens", openRawLeaseKeepsTheLimitUnderConcurrentOpens},
}

// concurrentRun runs each fn in a goroutine of its own and returns every
// error they returned, so that only the test's goroutine asserts.
func concurrentRun(fns ...func() error) error {
	errs := make(chan error, len(fns))
	var wg sync.WaitGroup
	for _, fn := range fns {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- fn()
		}()
	}
	wg.Wait()
	close(errs)
	var all []error
	for err := range errs {
		all = append(all, err)
	}
	return errors.Join(all...)
}

func ingestRawSeesASubscriptionAsSoonAsItsUpsertReturns(t *testing.T, s RawStore) {
	const writers = 32
	r := newRawRig(t, s)
	upserted := make(chan faucet.Subscription, writers)
	var fns []func() error
	for n := range writers {
		fns = append(fns, func() error {
			row, err := s.UpsertSubscription(r.ctx, newRawSubscription(n, rawSpec{repoID: rawRepo + 1 + int64(n)}, r.now.Add(time.Hour)), r.now, rawMaxLive)
			if err != nil {
				return xerrors.Errorf("upsert subscription %d: %w", n, err)
			}
			upserted <- row
			return nil
		}, func() error {
			sub := <-upserted
			d := r.newDelivery()
			d.RepoID = sub.RepoID
			var got []faucet.RawInterest
			res, err := s.IngestRaw(r.ctx, d, r.now, func(interests []faucet.RawInterest) faucet.RawPlan {
				got = interests
				return serveAll(rawBody)(interests)
			})
			switch {
			case err != nil:
				return xerrors.Errorf("ingest for repository %d: %w", sub.RepoID, err)
			case !res.Matched:
				return xerrors.Errorf("repository %d: an ingest after the upsert returned matched nothing", sub.RepoID)
			case len(got) != 1 || got[0].IsLease || got[0].Subscription != sub.KeyHash || !got[0].Incarnation.Equal(sub.CreatedAt):
				return xerrors.Errorf("repository %d: the plan got %+v, not only the subscription upserted for it", sub.RepoID, got)
			}
			return nil
		})
	}
	require.NoError(t, concurrentRun(fns...), "an ingest sees every subscription whose upsert returned before it started")
}

func readRawLeaseCursorMissesNoConcurrentIngest(t *testing.T, s RawStore) {
	const (
		writers = 4
		each    = 50
	)
	r := newRawRig(t, s)
	lease := r.open(rawSpec{})
	stored := make(chan int64, writers*each)
	var (
		fns     []func() error
		writing sync.WaitGroup
		done    atomic.Bool
	)
	writing.Add(writers)
	for range writers {
		fns = append(fns, func() error {
			defer writing.Done()
			for range each {
				res, err := s.IngestRaw(r.ctx, r.newDelivery(), r.now, serveAll(rawBody))
				switch {
				case err != nil:
					return xerrors.Errorf("ingest: %w", err)
				case !res.Matched || res.Duplicate:
					return xerrors.Errorf("the lease wants every push, got %+v", res)
				}
				stored <- res.ID
			}
			return nil
		})
	}
	go func() {
		writing.Wait()
		done.Store(true)
	}()
	var read []int64
	fns = append(fns, func() error {
		q := r.query(lease, 0)
		q.Limit = 7
		for {
			finished := done.Load()
			page, found, err := s.ReadRawLease(r.ctx, q)
			switch {
			case err != nil:
				return xerrors.Errorf("read after %d: %w", q.After, err)
			case !found:
				return xerrors.Errorf("the lease ended while it was read")
			case page.Skipped != 0:
				return xerrors.Errorf("a page after %d skipped %d events that the lease got", q.After, page.Skipped)
			case page.Next < q.After:
				return xerrors.Errorf("the cursor went back from %d to %d", q.After, page.Next)
			}
			read = append(read, rawIDs(page.Events)...)
			q.After = page.Next
			if finished && len(page.Events) == 0 && !page.More {
				return nil
			}
		}
	})
	require.NoError(t, concurrentRun(fns...))
	close(stored)
	var want []int64
	for id := range stored {
		want = append(want, id)
	}
	slices.Sort(want)
	require.Len(t, want, writers*each)
	require.Equal(t, want, read, "a reader that follows Next gets every event once, in ID order, however the ingests interleave")
}

func openRawLeaseKeepsTheLimitUnderConcurrentOpens(t *testing.T, s RawStore) {
	const (
		openers = 8
		maxLive = 4
	)
	r := newRawRig(t, s)
	opened := make(map[faucet.RawLeaseID]bool, openers)
	var fns []func() error
	for range openers {
		lease := newRawLease(r.leases.Add(1), rawSpec{}, r.now.Add(time.Hour))
		opened[lease.ID] = true
		fns = append(fns, func() error {
			if _, err := s.OpenRawLease(r.ctx, lease, r.now, maxLive); err != nil {
				return xerrors.Errorf("open lease: %w", err)
			}
			return nil
		})
	}
	require.NoError(t, concurrentRun(fns...))
	interests := r.interestsAt(r.newDelivery(), r.now)
	require.NotEmpty(t, interests, "the last lease to open is live")
	require.LessOrEqual(t, len(interests), maxLive, "concurrent opens keep a principal to its live leases")
	for _, in := range interests {
		require.True(t, in.IsLease && opened[in.Lease], "only the opened leases are interests")
	}
}
