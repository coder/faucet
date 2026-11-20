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

	"github.com/coder/superblink-events/faucet"
)

// rawConcurrencySubtests are the contract cases with concurrent callers.
var rawConcurrencySubtests = []rawSubtest{
	{"IngestRawSeesASubscriptionAsSoonAsItsUpsertReturns", ingestRawSeesASubscriptionAsSoonAsItsUpsertReturns},
	{"ReadRawLeaseCursorMissesNoConcurrentIngest", readRawLeaseCursorMissesNoConcurrentIngest},
	{"ClaimRawDeliveryTakesOnlyTheLiveIncarnationUnderChurn", claimRawDeliveryTakesOnlyTheLiveIncarnationUnderChurn},
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
			d.RepoID = sub.Target.RepoID()
			var got []faucet.RawInterest
			res, err := s.IngestRaw(r.ctx, d, r.now, func(interests []faucet.RawInterest) faucet.RawPlan {
				got = interests
				return serveAll(rawBody)(interests)
			})
			switch {
			case err != nil:
				return xerrors.Errorf("ingest for repository %d: %w", sub.Target.RepoID(), err)
			case !res.Matched:
				return xerrors.Errorf("repository %d: an ingest after the upsert returned matched nothing", sub.Target.RepoID())
			case len(got) != 1 || got[0].IsLease || got[0].Subscription != sub.KeyHash || !got[0].Incarnation.Equal(sub.CreatedAt):
				return xerrors.Errorf("repository %d: the plan got %+v, not only the subscription upserted for it", sub.Target.RepoID(), got)
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
			if _, err := s.OpenRawLease(r.ctx, lease, r.now, rawLimits(maxLive)); err != nil {
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

// concurrentIngester ingests pushes from one goroutine at a time and records,
// per stored raw ID, the incarnation that its plan served.
type concurrentIngester struct {
	r      *rawRig
	now    time.Time
	remade *atomic.Int64
	served map[int64]time.Time
}

// awaitRemake waits until remade moves, so that a plan that calls it is
// written after the subscription it planned for is made again.
func (c *concurrentIngester) awaitRemake() {
	seen, deadline := c.remade.Load(), time.Now().Add(20*time.Millisecond)
	for c.remade.Load() == seen && time.Now().Before(deadline) {
		time.Sleep(50 * time.Microsecond)
	}
}

// ingest stores one push; a non-nil stall runs inside its plan.
func (c *concurrentIngester) ingest(stall func()) error {
	var incarnations []time.Time
	res, err := c.r.s.IngestRaw(c.r.ctx, c.r.newDelivery(), c.now, func(interests []faucet.RawInterest) faucet.RawPlan {
		for _, in := range interests {
			if !in.IsLease {
				incarnations = append(incarnations, in.Incarnation)
			}
		}
		if stall != nil {
			stall()
		}
		return serveAll(rawBody)(interests)
	})
	switch {
	case err != nil:
		return xerrors.Errorf("ingest: %w", err)
	case len(incarnations) > 1:
		return xerrors.Errorf("the plan got one key as %d interests", len(incarnations))
	case !res.Matched || res.Duplicate || len(incarnations) == 0:
		return nil
	}
	c.served[res.ID] = incarnations[0]
	return nil
}

func claimRawDeliveryTakesOnlyTheLiveIncarnationUnderChurn(t *testing.T, s RawStore) {
	const (
		ingests  = 200
		remakes  = 2000
		pollsPer = 3
	)
	r := newRawRig(t, s)
	ingestNow := r.now.Add(time.Minute)
	var remade atomic.Int64
	ing := &concurrentIngester{r: r, now: ingestNow, remade: &remade, served: map[int64]time.Time{}}
	live := r.subscribe(1, rawSpec{})
	remake := func() error {
		deleted, err := s.DeleteSubscription(r.ctx, live.Principal, live.KeyHash)
		switch {
		case err != nil:
			return xerrors.Errorf("delete subscription: %w", err)
		case !deleted:
			return xerrors.New("the subscription was gone before its delete")
		}
		n := remade.Load() + 1
		at := r.now.Add(time.Duration(n) * time.Millisecond)
		row, err := s.UpsertSubscription(r.ctx, newRawSubscription(1, rawSpec{}, at.Add(time.Hour)), at, rawMaxLive)
		if err != nil {
			return xerrors.Errorf("make the subscription again: %w", err)
		}
		live = row
		remade.Store(n)
		return nil
	}
	var ingesting atomic.Bool
	ingesting.Store(true)
	err := concurrentRun(func() error {
		defer ingesting.Store(false)
		for n := range ingests {
			var stall func()
			if n%2 == 1 {
				stall = ing.awaitRemake
			}
			if err := ing.ingest(stall); err != nil {
				return err
			}
		}
		return nil
	}, func() error {
		for i := 1; i <= remakes && ingesting.Load(); i++ {
			if err := remake(); err != nil {
				return err
			}
		}
		return nil
	}, func() error {
		for ingesting.Load() {
			lease := newRawLease(r.leases.Add(1), rawSpec{}, ingestNow.Add(time.Hour))
			head, err := s.OpenRawLease(r.ctx, lease, ingestNow, rawLimits(rawMaxLive))
			if err != nil {
				return xerrors.Errorf("open lease: %w", err)
			}
			q := r.query(lease, head)
			q.Now, q.ExpiresAt = ingestNow, ingestNow.Add(time.Hour)
			for range pollsPer {
				page, found, err := s.ReadRawLease(r.ctx, q)
				switch {
				case err != nil:
					return xerrors.Errorf("read lease: %w", err)
				case !found:
					return xerrors.New("a lease that its principal holds below the limit ended")
				}
				q.After = page.Next
			}
		}
		return nil
	})
	require.NoError(t, err, "ingests, remakes and polls of one repository run side by side")
	stale := r.ingestAt(r.newDelivery(), ingestNow, func(interests []faucet.RawInterest) faucet.RawPlan {
		require.NoError(t, remake())
		return serveAll(rawBody)(interests)
	})
	require.True(t, stale.Matched)
	require.NoError(t, ing.ingest(nil))

	var want []int64
	for id, incarnation := range ing.served {
		if incarnation.Equal(live.CreatedAt) {
			want = append(want, id)
		}
	}
	slices.Sort(want)
	require.NotEmpty(t, want, "the ingest after the churn serves the live subscription")
	var claimed []int64
	for _, claim := range r.claimAll(ingestNow.Add(time.Minute)) {
		require.Equal(t, live.KeyHash, claim.Ref.Key)
		require.True(t, claim.Ref.Incarnation.Equal(live.CreatedAt), "a claim names the incarnation that is live, not %s", claim.Ref.Incarnation)
		claimed = append(claimed, claim.Ref.EventID)
	}
	slices.Sort(claimed)
	require.Equal(t, want, claimed, "only the deliveries planned for the live incarnation are claimed, each once")
}
