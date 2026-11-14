package storetest

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/coder/faucet/faucet"
)

// RedeliveryStore is a store that passes both contracts: events, which
// redelivery checks for stored deliveries, and redelivery state.
type RedeliveryStore interface {
	faucet.Store
	faucet.RedeliveryStore
}

// RunRedelivery runs the redelivery contract against stores returned by
// newStore. Every call must return an empty store that does not share state
// with the others, because the subtests run in parallel.
func RunRedelivery(t *testing.T, newStore func(t *testing.T) RedeliveryStore) {
	subtests := []struct {
		name string
		run  func(t *testing.T, s RedeliveryStore)
	}{
		{"LeaseIsExclusiveAndFencesAStaleHolder", leaseIsExclusiveAndFencesAStaleHolder},
		{"FailedAttemptsTrackOneDeliveryOnce", failedAttemptsTrackOneDeliveryOnce},
		{"AnEarlierAttemptSetsTheFirstTimeAndALaterOneSetsTheAskTarget", anEarlierAttemptSetsTheFirstTimeAndALaterOneSetsTheAskTarget},
		{"ASuccessfulAttemptRecoversAPendingDeliveryOnce", aSuccessfulAttemptRecoversAPendingDeliveryOnce},
		{"SettleRecoversAStoredDeliveryAndClaimSkipsIt", settleRecoversAStoredDeliveryAndClaimSkipsIt},
		{"SettleGivesUpAnExpiredDeliveryOnce", settleGivesUpAnExpiredDeliveryOnce},
		{"ClaimFollowsTheScheduleThenSettleGivesUpOnTheLimit", claimFollowsTheScheduleThenSettleGivesUpOnTheLimit},
		{"ClaimPicksTheDeliveryDueLongest", claimPicksTheDeliveryDueLongest},
		{"ConcurrentClaimsNeverReturnOneAskTwice", concurrentClaimsNeverReturnOneAskTwice},
		{"ScanPositionSurvivesBetweenRuns", scanPositionSurvivesBetweenRuns},
		{"AFailedScanStepRecordsNothing", aFailedScanStepRecordsNothing},
		{"ChangingTheHookClosesOpenRangesAsGaps", changingTheHookClosesOpenRangesAsGaps},
		{"ClaimSkipsTheDeliveriesOfAnotherHook", claimSkipsTheDeliveriesOfAnotherHook},
		{"PurgeKeepsPendingDeliveriesAndOpenRanges", purgeKeepsPendingDeliveriesAndOpenRanges},
		{"RedeliveryInputsAreValidated", redeliveryInputsAreValidated},
	}
	for _, st := range subtests {
		t.Run(st.name, func(t *testing.T) {
			t.Parallel()
			st.run(t, newStore(t))
		})
	}
}

const (
	testHook     = "/orgs/acme/hooks/7"
	testMaxAsks  = 3
	testLeaseTTL = 2 * time.Minute
)

var (
	t0           = time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	testSchedule = []time.Duration{10 * time.Minute, 20 * time.Minute, 5 * time.Minute}
)

func failedAt(guid string, attemptID int64, at time.Time) faucet.FailedAttempt {
	return faucet.FailedAttempt{GUID: guid, AttemptID: attemptID, DeliveredAt: at}
}

func settleRequest(now time.Time) faucet.SettleRequest {
	return faucet.SettleRequest{Now: now, Retention: faucet.GitHubDeliveryRetention, MaxAsks: testMaxAsks}
}

func claimRequest(now time.Time) faucet.ClaimRequest {
	return faucet.ClaimRequest{Now: now, Retention: faucet.GitHubDeliveryRetention, MaxAsks: testMaxAsks, Schedule: testSchedule}
}

func acquire(t *testing.T, s RedeliveryStore, hook string, now time.Time) faucet.RedeliveryScan {
	t.Helper()
	scan, ok, err := s.AcquireRedeliveryLease(context.Background(), hook, now, testLeaseTTL)
	require.NoError(t, err)
	require.True(t, ok, "the lease should be free")
	require.Positive(t, scan.Epoch)
	return scan
}

func apply(t *testing.T, s RedeliveryStore, step faucet.ScanStep) faucet.ScanResult {
	t.Helper()
	res, err := s.ApplyRedeliveryScan(context.Background(), step)
	require.NoError(t, err)
	return res
}

func settle(t *testing.T, s RedeliveryStore, now time.Time) faucet.SettleResult {
	t.Helper()
	res, err := s.SettleRedeliveries(context.Background(), settleRequest(now))
	require.NoError(t, err)
	return res
}

func claim(t *testing.T, s RedeliveryStore, now time.Time) (faucet.RedeliveryClaim, bool) {
	t.Helper()
	c, ok, err := s.ClaimRedelivery(context.Background(), claimRequest(now))
	require.NoError(t, err)
	return c, ok
}

func store(t *testing.T, s RedeliveryStore, guid string) {
	t.Helper()
	_, inserted, err := s.InsertEvent(context.Background(), newEvent(guid))
	require.NoError(t, err)
	require.True(t, inserted)
}

func leaseIsExclusiveAndFencesAStaleHolder(t *testing.T, s RedeliveryStore) {
	ctx := context.Background()

	first := acquire(t, s, testHook, t0)
	require.True(t, first.Top.IsZero())
	require.Empty(t, first.Ranges)
	require.False(t, first.HookChanged)

	_, ok, err := s.AcquireRedeliveryLease(ctx, testHook, t0.Add(time.Minute), testLeaseTTL)
	require.NoError(t, err)
	require.False(t, ok, "a second run must not start while the lease is held")

	apply(t, s, faucet.ScanStep{Epoch: first.Epoch, Now: t0, Failed: []faucet.FailedAttempt{failedAt("guid-held", 1, t0)}})

	second := acquire(t, s, testHook, t0.Add(3*time.Minute))
	require.Greater(t, second.Epoch, first.Epoch, "a lease that ran out is replaced")

	_, err = s.ApplyRedeliveryScan(ctx, faucet.ScanStep{Epoch: first.Epoch, Now: t0, Failed: []faucet.FailedAttempt{failedAt("guid-stale", 2, t0)}})
	require.ErrorIs(t, err, faucet.ErrRedeliveryLeaseLost)
	require.Equal(t, 1, settle(t, s, t0.Add(4*time.Minute)).Pending, "a stale run must record nothing")

	require.NoError(t, s.ReleaseRedeliveryLease(ctx, first.Epoch))
	_, ok, err = s.AcquireRedeliveryLease(ctx, testHook, t0.Add(4*time.Minute), testLeaseTTL)
	require.NoError(t, err)
	require.False(t, ok, "a stale release must not free the lease of the run that replaced it")

	require.NoError(t, s.ReleaseRedeliveryLease(ctx, second.Epoch))
	acquire(t, s, testHook, t0.Add(4*time.Minute))
}

func failedAttemptsTrackOneDeliveryOnce(t *testing.T, s RedeliveryStore) {
	lease := acquire(t, s, testHook, t0)
	step := faucet.ScanStep{Epoch: lease.Epoch, Now: t0, Failed: []faucet.FailedAttempt{
		failedAt("guid-a", 10, t0), failedAt("guid-b", 11, t0), failedAt("guid-a", 10, t0),
	}}

	require.Equal(t, 2, apply(t, s, step).Tracked, "a repeated attempt in one step counts once")
	require.Zero(t, apply(t, s, step).Tracked, "reading the same page again tracks nothing new")
	require.Equal(t, 2, settle(t, s, t0.Add(time.Minute)).Pending)
}

func anEarlierAttemptSetsTheFirstTimeAndALaterOneSetsTheAskTarget(t *testing.T, s RedeliveryStore) {
	lease := acquire(t, s, testHook, t0)
	redelivery := t0.Add(-10 * time.Hour)
	original := t0.Add(-60 * time.Hour)

	apply(t, s, faucet.ScanStep{Epoch: lease.Epoch, Now: t0, Failed: []faucet.FailedAttempt{failedAt("guid-a", 50, redelivery)}})
	apply(t, s, faucet.ScanStep{Epoch: lease.Epoch, Now: t0, Failed: []faucet.FailedAttempt{failedAt("guid-a", 30, original)}})

	picked, ok := claim(t, s, t0)
	require.True(t, ok)
	require.Equal(t, faucet.RedeliveryClaim{GUID: "guid-a", AttemptID: 50, Ask: 1}, picked,
		"the ask names the newest failed attempt, whatever order the attempts were read in")

	expiresAt := original.Add(faucet.GitHubDeliveryRetention)
	require.Empty(t, settle(t, s, expiresAt.Add(-time.Second)).GaveUp)
	res := settle(t, s, expiresAt)
	require.Len(t, res.GaveUp, 1, "the deadline comes from the earliest attempt, not from the redelivery that was read first")
	require.Equal(t, faucet.GiveUpExpired, res.GaveUp[0].Reason)
	require.True(t, original.Equal(res.GaveUp[0].FirstDeliveredAt))
}

func aSuccessfulAttemptRecoversAPendingDeliveryOnce(t *testing.T, s RedeliveryStore) {
	lease := acquire(t, s, testHook, t0)
	apply(t, s, faucet.ScanStep{Epoch: lease.Epoch, Now: t0, Failed: []faucet.FailedAttempt{failedAt("guid-a", 1, t0), failedAt("guid-b", 2, t0)}})

	res := apply(t, s, faucet.ScanStep{Epoch: lease.Epoch, Now: t0.Add(time.Minute), Succeeded: []string{"guid-a", "guid-unknown"}})
	require.Equal(t, []string{"guid-a"}, res.Recovered, "only a pending delivery is recovered")
	res = apply(t, s, faucet.ScanStep{Epoch: lease.Epoch, Now: t0.Add(2 * time.Minute), Succeeded: []string{"guid-a"}})
	require.Empty(t, res.Recovered, "a delivery recovers once")

	res = apply(t, s, faucet.ScanStep{Epoch: lease.Epoch, Now: t0.Add(3 * time.Minute), Failed: []faucet.FailedAttempt{failedAt("guid-a", 3, t0.Add(2*time.Minute))}})
	require.Zero(t, res.Tracked, "a failed attempt does not revive a final delivery")
	require.Equal(t, 1, settle(t, s, t0.Add(4*time.Minute)).Pending)
}

func settleRecoversAStoredDeliveryAndClaimSkipsIt(t *testing.T, s RedeliveryStore) {
	lease := acquire(t, s, testHook, t0)
	apply(t, s, faucet.ScanStep{Epoch: lease.Epoch, Now: t0, Failed: []faucet.FailedAttempt{failedAt("guid-a", 1, t0), failedAt("guid-b", 2, t0)}})
	store(t, s, "guid-a")

	picked, ok := claim(t, s, t0.Add(time.Minute))
	require.True(t, ok)
	require.Equal(t, "guid-b", picked.GUID, "a delivery that faucet has stored needs no ask, even before a settle")
	_, ok = claim(t, s, t0.Add(time.Minute))
	require.False(t, ok)

	res := settle(t, s, t0.Add(2*time.Minute))
	require.Equal(t, []string{"guid-a"}, res.Recovered)
	require.Equal(t, 1, res.Pending)
	require.Empty(t, settle(t, s, t0.Add(3*time.Minute)).Recovered, "a delivery recovers once")
}

func settleGivesUpAnExpiredDeliveryOnce(t *testing.T, s RedeliveryStore) {
	lease := acquire(t, s, testHook, t0)
	apply(t, s, faucet.ScanStep{Epoch: lease.Epoch, Now: t0.Add(time.Hour), Failed: []faucet.FailedAttempt{failedAt("guid-a", 1, t0)}})

	deadline := t0.Add(faucet.GitHubDeliveryRetention)
	require.Empty(t, settle(t, s, deadline.Add(-time.Second)).GaveUp)
	res := settle(t, s, deadline)
	require.Equal(t, []faucet.GaveUp{{GUID: "guid-a", Reason: faucet.GiveUpExpired, Asks: 0, FirstDeliveredAt: t0}}, res.GaveUp)
	require.Zero(t, res.Pending)
	require.Empty(t, settle(t, s, deadline.Add(time.Minute)).GaveUp, "a give-up is final and happens once")
	_, ok := claim(t, s, deadline.Add(time.Minute))
	require.False(t, ok)
}

func claimFollowsTheScheduleThenSettleGivesUpOnTheLimit(t *testing.T, s RedeliveryStore) {
	lease := acquire(t, s, testHook, t0)
	apply(t, s, faucet.ScanStep{Epoch: lease.Epoch, Now: t0, Failed: []faucet.FailedAttempt{failedAt("guid-a", 1, t0)}})

	first, ok := claim(t, s, t0)
	require.True(t, ok)
	require.Equal(t, 1, first.Ask)
	_, ok = claim(t, s, t0.Add(10*time.Minute-time.Second))
	require.False(t, ok, "the second ask waits the first schedule entry")
	second, ok := claim(t, s, t0.Add(10*time.Minute))
	require.True(t, ok)
	require.Equal(t, 2, second.Ask)
	_, ok = claim(t, s, t0.Add(30*time.Minute-time.Second))
	require.False(t, ok)
	third, ok := claim(t, s, t0.Add(30*time.Minute))
	require.True(t, ok)
	require.Equal(t, 3, third.Ask)

	_, ok = claim(t, s, t0.Add(24*time.Hour))
	require.False(t, ok, "no ask is left")
	require.Empty(t, settle(t, s, t0.Add(35*time.Minute-time.Second)).GaveUp, "the last ask gets its wait before the give-up")
	res := settle(t, s, t0.Add(35*time.Minute))
	require.Equal(t, []faucet.GaveUp{{GUID: "guid-a", Reason: faucet.GiveUpLimit, Asks: 3, FirstDeliveredAt: t0}}, res.GaveUp)
	require.Empty(t, settle(t, s, t0.Add(time.Hour)).GaveUp)
}

func claimPicksTheDeliveryDueLongest(t *testing.T, s RedeliveryStore) {
	lease := acquire(t, s, testHook, t0)
	apply(t, s, faucet.ScanStep{Epoch: lease.Epoch, Now: t0.Add(2 * time.Minute), Failed: []faucet.FailedAttempt{failedAt("guid-late", 2, t0)}})
	apply(t, s, faucet.ScanStep{Epoch: lease.Epoch, Now: t0.Add(time.Minute), Failed: []faucet.FailedAttempt{failedAt("guid-early", 1, t0)}})

	now := t0.Add(5 * time.Minute)
	var order []string
	for {
		c, ok := claim(t, s, now)
		if !ok {
			break
		}
		order = append(order, c.GUID)
	}
	require.Equal(t, []string{"guid-early", "guid-late"}, order, "a delivery that waited longer goes first, and a delivery in its wait goes after")
}

func concurrentClaimsNeverReturnOneAskTwice(t *testing.T, s RedeliveryStore) {
	ctx := context.Background()
	const deliveries, claimers = 40, 8

	lease := acquire(t, s, testHook, t0)
	failed := make([]faucet.FailedAttempt, deliveries)
	for i := range failed {
		failed[i] = failedAt("guid-"+string(rune('a'+i/26))+string(rune('a'+i%26)), int64(i+1), t0)
	}
	apply(t, s, faucet.ScanStep{Epoch: lease.Epoch, Now: t0, Failed: failed})

	var (
		mu     sync.Mutex
		claims = map[string]int{}
		wg     sync.WaitGroup
	)
	for range claimers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				c, ok, err := s.ClaimRedelivery(ctx, claimRequest(t0))
				if err != nil {
					t.Error(err)
					return
				}
				if !ok {
					return
				}
				mu.Lock()
				claims[c.GUID]++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	require.Len(t, claims, deliveries, "every delivery is claimed")
	for guid, n := range claims {
		require.Equal(t, 1, n, "%s was claimed %d times by runs that overlap", guid, n)
	}
}

func scanPositionSurvivesBetweenRuns(t *testing.T, s RedeliveryStore) {
	ctx := context.Background()
	lease := acquire(t, s, testHook, t0)
	opened := apply(t, s, faucet.ScanStep{
		Epoch: lease.Epoch, Now: t0, Top: t0,
		Open: &faucet.RedeliveryRange{NewerAt: t0.Add(-time.Hour), OlderAt: t0.Add(-48 * time.Hour), Cursor: "cursor-1"},
	})
	require.Positive(t, opened.OpenedID)
	apply(t, s, faucet.ScanStep{Epoch: lease.Epoch, Now: t0, Top: t0.Add(-time.Hour)})
	require.NoError(t, s.ReleaseRedeliveryLease(ctx, lease.Epoch))

	next := acquire(t, s, testHook, t0.Add(time.Minute))
	require.True(t, t0.Equal(next.Top), "the top never moves down")
	require.Equal(t, []faucet.RedeliveryRange{{
		ID: opened.OpenedID, NewerAt: t0.Add(-time.Hour), OlderAt: t0.Add(-48 * time.Hour), Cursor: "cursor-1",
	}}, normalizeRanges(next.Ranges), "a run that stopped early finds its place")

	newer := apply(t, s, faucet.ScanStep{
		Epoch: next.Epoch, Now: t0, Top: t0.Add(time.Hour),
		Open: &faucet.RedeliveryRange{NewerAt: t0.Add(30 * time.Minute), OlderAt: t0, Cursor: "cursor-2"},
	})
	apply(t, s, faucet.ScanStep{Epoch: next.Epoch, Now: t0, Advance: &faucet.RangeAdvance{ID: opened.OpenedID, NewerAt: t0.Add(-3 * time.Hour), Cursor: "cursor-1b"}})
	require.NoError(t, s.ReleaseRedeliveryLease(ctx, next.Epoch))

	third := acquire(t, s, testHook, t0.Add(2*time.Minute))
	require.True(t, t0.Add(time.Hour).Equal(third.Top))
	require.Equal(t, []faucet.RedeliveryRange{
		{ID: newer.OpenedID, NewerAt: t0.Add(30 * time.Minute), OlderAt: t0, Cursor: "cursor-2"},
		{ID: opened.OpenedID, NewerAt: t0.Add(-3 * time.Hour), OlderAt: t0.Add(-48 * time.Hour), Cursor: "cursor-1b"},
	}, normalizeRanges(third.Ranges), "ranges come newest first, and an advance moved one cursor")

	apply(t, s, faucet.ScanStep{Epoch: third.Epoch, Now: t0, CloseID: newer.OpenedID})
	gapFrom, gapTo := t0.Add(-48*time.Hour), t0.Add(-40*time.Hour)
	apply(t, s, faucet.ScanStep{Epoch: third.Epoch, Now: t0.Add(5 * time.Minute), CloseID: opened.OpenedID, Gap: &faucet.RedeliveryGap{From: gapFrom, To: gapTo}})
	apply(t, s, faucet.ScanStep{Epoch: third.Epoch, Now: t0.Add(5 * time.Minute), Gap: &faucet.RedeliveryGap{From: gapFrom.Add(-time.Hour), To: gapFrom}})
	require.NoError(t, s.ReleaseRedeliveryLease(ctx, third.Epoch))

	last := acquire(t, s, testHook, t0.Add(10*time.Minute))
	require.Empty(t, last.Ranges, "a closed range, finished or a gap, is no longer open")
	deleted, err := s.PurgeRedeliveries(ctx, t0.Add(6*time.Minute))
	require.NoError(t, err)
	require.EqualValues(t, 2, deleted, "a gap is kept as a final state until it is purged, whether or not a range held it, and a finished range leaves no row")
}

func normalizeRanges(ranges []faucet.RedeliveryRange) []faucet.RedeliveryRange {
	out := make([]faucet.RedeliveryRange, len(ranges))
	for i, r := range ranges {
		out[i] = faucet.RedeliveryRange{ID: r.ID, NewerAt: r.NewerAt.UTC(), OlderAt: r.OlderAt.UTC(), Cursor: r.Cursor}
	}
	return out
}

func aFailedScanStepRecordsNothing(t *testing.T, s RedeliveryStore) {
	lease := acquire(t, s, testHook, t0)

	_, err := s.ApplyRedeliveryScan(context.Background(), faucet.ScanStep{
		Epoch: lease.Epoch, Now: t0, Failed: []faucet.FailedAttempt{failedAt("guid-a", 1, t0)}, Top: t0,
		Advance: &faucet.RangeAdvance{ID: 9999, NewerAt: t0, Cursor: "cursor"},
	})
	require.Error(t, err, "a range that does not exist cannot move")
	require.NotErrorIs(t, err, faucet.ErrRedeliveryLeaseLost)

	require.Zero(t, settle(t, s, t0).Pending, "the attempts of a failed step are not recorded")
	require.NoError(t, s.ReleaseRedeliveryLease(context.Background(), lease.Epoch))
	require.True(t, acquire(t, s, testHook, t0).Top.IsZero(), "and neither is its top")
}

func changingTheHookClosesOpenRangesAsGaps(t *testing.T, s RedeliveryStore) {
	ctx := context.Background()
	lease := acquire(t, s, testHook, t0)
	apply(t, s, faucet.ScanStep{
		Epoch: lease.Epoch, Now: t0, Top: t0,
		Open: &faucet.RedeliveryRange{NewerAt: t0.Add(-time.Hour), OlderAt: t0.Add(-48 * time.Hour), Cursor: "cursor-1"},
	})
	require.NoError(t, s.ReleaseRedeliveryLease(ctx, lease.Epoch))

	same := acquire(t, s, testHook, t0.Add(time.Minute))
	require.False(t, same.HookChanged)
	require.Len(t, same.Ranges, 1)
	require.NoError(t, s.ReleaseRedeliveryLease(ctx, same.Epoch))

	other := acquire(t, s, "/orgs/acme/hooks/8", t0.Add(2*time.Minute))
	require.True(t, other.HookChanged)
	require.True(t, other.Top.IsZero(), "a new hook starts from scratch")
	require.Empty(t, other.Ranges)
	require.Len(t, other.ClosedGaps, 1)
	require.True(t, t0.Add(-48*time.Hour).Equal(other.ClosedGaps[0].From))
	require.True(t, t0.Add(-time.Hour).Equal(other.ClosedGaps[0].To))
}

func claimSkipsTheDeliveriesOfAnotherHook(t *testing.T, s RedeliveryStore) {
	ctx := context.Background()
	const otherHook = "/orgs/acme/hooks/8"
	old := acquire(t, s, testHook, t0)
	apply(t, s, faucet.ScanStep{Epoch: old.Epoch, Now: t0, Failed: []faucet.FailedAttempt{failedAt("guid-old", 1, t0)}})
	require.NoError(t, s.ReleaseRedeliveryLease(ctx, old.Epoch))

	replaced := acquire(t, s, otherHook, t0.Add(time.Minute))
	apply(t, s, faucet.ScanStep{Epoch: replaced.Epoch, Now: t0.Add(time.Minute), Failed: []faucet.FailedAttempt{failedAt("guid-new", 7, t0.Add(time.Minute))}})

	c, ok := claim(t, s, t0.Add(2*time.Minute))
	require.True(t, ok)
	require.Equal(t, "guid-new", c.GUID, "an attempt ID of the old hook means nothing to the new one")
	_, ok = claim(t, s, t0.Add(2*time.Minute))
	require.False(t, ok, "the delivery of the old hook is not due for anyone")
	require.Equal(t, 2, settle(t, s, t0.Add(2*time.Minute)).Pending, "it stays pending until it is stored or expires")

	require.NoError(t, s.ReleaseRedeliveryLease(ctx, replaced.Epoch))
	back := acquire(t, s, testHook, t0.Add(3*time.Minute))
	require.NoError(t, s.ReleaseRedeliveryLease(ctx, back.Epoch))
	c, ok = claim(t, s, t0.Add(4*time.Minute))
	require.True(t, ok, "the old hook's delivery is its own again when the old hook returns")
	require.Equal(t, "guid-old", c.GUID)
}

func purgeKeepsPendingDeliveriesAndOpenRanges(t *testing.T, s RedeliveryStore) {
	ctx := context.Background()
	lease := acquire(t, s, testHook, t0)
	apply(t, s, faucet.ScanStep{
		Epoch: lease.Epoch, Now: t0, Top: t0,
		Failed: []faucet.FailedAttempt{failedAt("guid-pending", 1, t0), failedAt("guid-recovered", 2, t0), failedAt("guid-fresh", 3, t0.Add(80*time.Hour))},
		Open:   &faucet.RedeliveryRange{NewerAt: t0.Add(-time.Hour), OlderAt: t0.Add(-48 * time.Hour), Cursor: "cursor-1"},
	})
	apply(t, s, faucet.ScanStep{Epoch: lease.Epoch, Now: t0.Add(time.Minute), Succeeded: []string{"guid-recovered", "guid-fresh"}})

	before := t0.Add(faucet.GitHubDeliveryRetention)
	deleted, err := s.PurgeRedeliveries(ctx, before)
	require.NoError(t, err)
	require.EqualValues(t, 1, deleted, "only guid-recovered has a final state that ended before the cutoff")
	require.Equal(t, 1, settle(t, s, t0.Add(time.Hour)).Pending, "a pending delivery is never purged")
	require.NoError(t, s.ReleaseRedeliveryLease(ctx, lease.Epoch))
	require.Len(t, acquire(t, s, testHook, t0.Add(2*time.Hour)).Ranges, 1, "an open range is never purged")

	deleted, err = s.PurgeRedeliveries(ctx, before.Add(72*time.Hour))
	require.NoError(t, err)
	require.EqualValues(t, 1, deleted, "guid-fresh is purged once the cutoff passes its last attempt")
}

func redeliveryInputsAreValidated(t *testing.T, s RedeliveryStore) {
	ctx := context.Background()
	lease := acquire(t, s, testHook, t0)

	for name, step := range map[string]faucet.ScanStep{
		"no epoch":                       {Now: t0},
		"no time":                        {Epoch: lease.Epoch},
		"an attempt without a GUID":      {Epoch: lease.Epoch, Now: t0, Failed: []faucet.FailedAttempt{failedAt("", 1, t0)}},
		"an attempt ID of zero":          {Epoch: lease.Epoch, Now: t0, Failed: []faucet.FailedAttempt{failedAt("guid", 0, t0)}},
		"two range moves":                {Epoch: lease.Epoch, Now: t0, CloseID: 1, Advance: &faucet.RangeAdvance{ID: 1, NewerAt: t0, Cursor: "c"}},
		"an open range without a cursor": {Epoch: lease.Epoch, Now: t0, Open: &faucet.RedeliveryRange{NewerAt: t0, OlderAt: t0}},
	} {
		_, err := s.ApplyRedeliveryScan(ctx, step)
		require.Error(t, err, name)
		require.NotErrorIs(t, err, faucet.ErrRedeliveryLeaseLost, name)
	}

	_, _, err := s.ClaimRedelivery(ctx, faucet.ClaimRequest{Now: t0, Retention: faucet.GitHubDeliveryRetention, MaxAsks: 3, Schedule: testSchedule[:2]})
	require.Error(t, err, "the schedule needs one entry per ask")
	_, err = s.SettleRedeliveries(ctx, faucet.SettleRequest{Now: t0, Retention: faucet.GitHubDeliveryRetention})
	require.Error(t, err, "settle needs the ask limit")
	_, _, err = s.AcquireRedeliveryLease(ctx, "", t0, testLeaseTTL)
	require.Error(t, err, "a hook needs a name")
}
