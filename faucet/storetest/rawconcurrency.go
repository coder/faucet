package storetest

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/xerrors"

	"github.com/coder/faucet/faucet"
)

// rawConcurrencySubtests are the contract cases with concurrent callers.
var rawConcurrencySubtests = []rawSubtest{
	{"IngestRawSeesASubscriptionAsSoonAsItsUpsertReturns", ingestRawSeesASubscriptionAsSoonAsItsUpsertReturns},
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
