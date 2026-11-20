package storetest

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/coder/superblink-events/faucet"
	"github.com/coder/superblink-events/faucet/redelivery"
)

// RedeliveryStore is a store that passes both contracts: events, which
// redelivery checks for stored deliveries, and redelivery state.
type RedeliveryStore interface {
	faucet.Store
	redelivery.Store
}

// GapRecords is what a RedeliveryStore can also implement so that
// RunRedelivery checks the gaps that it keeps as final states: each one as a
// step gave it, or as the store reported it when the hook changed. Without it
// the suite checks only the gaps that the store reports.
type GapRecords interface {
	// RecordedRedeliveryGaps returns every gap that the store keeps, in any
	// order.
	RecordedRedeliveryGaps(ctx context.Context) ([]redelivery.Gap, error)
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
		{"TheClearIsTheLatestTimeAStepSetNotTheHighest", theClearIsTheLatestTimeAStepSetNotTheHighest},
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

func failedAt(guid string, attemptID int64, at time.Time) redelivery.FailedAttempt {
	return redelivery.FailedAttempt{GUID: guid, AttemptID: attemptID, DeliveredAt: at}
}

func settleRequest(now time.Time) redelivery.SettleRequest {
	return redelivery.SettleRequest{Now: now, Retention: redelivery.GitHubDeliveryRetention, MaxAsks: testMaxAsks}
}

func claimRequest(now time.Time) redelivery.ClaimRequest {
	return redelivery.ClaimRequest{Now: now, Retention: redelivery.GitHubDeliveryRetention, MaxAsks: testMaxAsks, Schedule: testSchedule}
}

func acquire(t *testing.T, s RedeliveryStore, hook string, now time.Time) redelivery.Scan {
	t.Helper()
	scan, ok, err := s.AcquireRedeliveryLease(context.Background(), hook, now, testLeaseTTL)
	require.NoError(t, err)
	require.True(t, ok, "the lease should be free")
	require.Positive(t, scan.Epoch)
	return scan
}

func apply(t *testing.T, s RedeliveryStore, step redelivery.ScanStep) redelivery.ScanResult {
	t.Helper()
	res, err := s.ApplyRedeliveryScan(context.Background(), step)
	require.NoError(t, err)
	return res
}

func settle(t *testing.T, s RedeliveryStore, now time.Time) redelivery.SettleResult {
	t.Helper()
	res, err := s.SettleRedeliveries(context.Background(), settleRequest(now))
	require.NoError(t, err)
	return res
}

func claim(t *testing.T, s RedeliveryStore, now time.Time) (redelivery.Claim, bool) {
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
	require.True(t, first.Clear.IsZero())
	require.Empty(t, first.Ranges)
	require.False(t, first.HookChanged)

	_, ok, err := s.AcquireRedeliveryLease(ctx, testHook, t0.Add(time.Minute), testLeaseTTL)
	require.NoError(t, err)
	require.False(t, ok, "a second run must not start while the lease is held")

	apply(t, s, redelivery.ScanStep{Epoch: first.Epoch, Now: t0, Failed: []redelivery.FailedAttempt{failedAt("guid-held", 1, t0)}})

	second := acquire(t, s, testHook, t0.Add(3*time.Minute))
	require.Greater(t, second.Epoch, first.Epoch, "a lease that ran out is replaced")

	_, err = s.ApplyRedeliveryScan(ctx, redelivery.ScanStep{Epoch: first.Epoch, Now: t0, Failed: []redelivery.FailedAttempt{failedAt("guid-stale", 2, t0)}})
	require.ErrorIs(t, err, redelivery.ErrLeaseLost)
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
	step := redelivery.ScanStep{Epoch: lease.Epoch, Now: t0, Failed: []redelivery.FailedAttempt{
		failedAt("guid-a", 10, t0), failedAt("guid-b", 11, t0), failedAt("guid-a", 10, t0),
	}}

	require.Equal(t, 2, apply(t, s, step).Tracked, "a repeated attempt in one step counts once")
	require.Zero(t, apply(t, s, step).Tracked, "reading the same page again tracks nothing new")
	require.Equal(t, 2, settle(t, s, t0.Add(time.Minute)).Pending)
}

func anEarlierAttemptSetsTheFirstTimeAndALaterOneSetsTheAskTarget(t *testing.T, s RedeliveryStore) {
	lease := acquire(t, s, testHook, t0)
	redelivered := t0.Add(-10 * time.Hour)
	original := t0.Add(-60 * time.Hour)

	apply(t, s, redelivery.ScanStep{Epoch: lease.Epoch, Now: t0, Failed: []redelivery.FailedAttempt{failedAt("guid-a", 50, redelivered)}})
	apply(t, s, redelivery.ScanStep{Epoch: lease.Epoch, Now: t0, Failed: []redelivery.FailedAttempt{failedAt("guid-a", 30, original)}})

	picked, ok := claim(t, s, t0)
	require.True(t, ok)
	require.Equal(t, redelivery.Claim{GUID: "guid-a", AttemptID: 50, Ask: 1}, picked,
		"the ask names the newest failed attempt, whatever order the attempts were read in")

	expiresAt := original.Add(redelivery.GitHubDeliveryRetention)
	require.Empty(t, settle(t, s, expiresAt.Add(-time.Second)).GaveUp)
	res := settle(t, s, expiresAt)
	require.Len(t, res.GaveUp, 1, "the deadline comes from the earliest attempt, not from the redelivery that was read first")
	require.Equal(t, redelivery.GiveUpExpired, res.GaveUp[0].Reason)
	require.True(t, original.Equal(res.GaveUp[0].FirstDeliveredAt))
}

func aSuccessfulAttemptRecoversAPendingDeliveryOnce(t *testing.T, s RedeliveryStore) {
	lease := acquire(t, s, testHook, t0)
	apply(t, s, redelivery.ScanStep{Epoch: lease.Epoch, Now: t0, Failed: []redelivery.FailedAttempt{failedAt("guid-a", 1, t0), failedAt("guid-b", 2, t0)}})

	res := apply(t, s, redelivery.ScanStep{Epoch: lease.Epoch, Now: t0.Add(time.Minute), Succeeded: []string{"guid-a", "guid-unknown"}})
	require.Equal(t, []string{"guid-a"}, res.Recovered, "only a pending delivery is recovered")
	res = apply(t, s, redelivery.ScanStep{Epoch: lease.Epoch, Now: t0.Add(2 * time.Minute), Succeeded: []string{"guid-a"}})
	require.Empty(t, res.Recovered, "a delivery recovers once")

	res = apply(t, s, redelivery.ScanStep{Epoch: lease.Epoch, Now: t0.Add(3 * time.Minute), Failed: []redelivery.FailedAttempt{failedAt("guid-a", 3, t0.Add(2*time.Minute))}})
	require.Zero(t, res.Tracked, "a failed attempt does not revive a final delivery")
	require.Equal(t, 1, settle(t, s, t0.Add(4*time.Minute)).Pending)
}

func settleRecoversAStoredDeliveryAndClaimSkipsIt(t *testing.T, s RedeliveryStore) {
	lease := acquire(t, s, testHook, t0)
	apply(t, s, redelivery.ScanStep{Epoch: lease.Epoch, Now: t0, Failed: []redelivery.FailedAttempt{failedAt("guid-a", 1, t0), failedAt("guid-b", 2, t0)}})
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
	apply(t, s, redelivery.ScanStep{Epoch: lease.Epoch, Now: t0.Add(time.Hour), Failed: []redelivery.FailedAttempt{failedAt("guid-a", 1, t0)}})

	deadline := t0.Add(redelivery.GitHubDeliveryRetention)
	require.Empty(t, settle(t, s, deadline.Add(-time.Second)).GaveUp)
	res := settle(t, s, deadline)
	require.Equal(t, []redelivery.GaveUp{{GUID: "guid-a", Reason: redelivery.GiveUpExpired, Asks: 0, FirstDeliveredAt: t0}}, res.GaveUp)
	require.Zero(t, res.Pending)
	require.Empty(t, settle(t, s, deadline.Add(time.Minute)).GaveUp, "a give-up is final and happens once")
	_, ok := claim(t, s, deadline.Add(time.Minute))
	require.False(t, ok)
}

func claimFollowsTheScheduleThenSettleGivesUpOnTheLimit(t *testing.T, s RedeliveryStore) {
	lease := acquire(t, s, testHook, t0)
	apply(t, s, redelivery.ScanStep{Epoch: lease.Epoch, Now: t0, Failed: []redelivery.FailedAttempt{failedAt("guid-a", 1, t0)}})

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
	require.Equal(t, []redelivery.GaveUp{{GUID: "guid-a", Reason: redelivery.GiveUpLimit, Asks: 3, FirstDeliveredAt: t0}}, res.GaveUp)
	require.Empty(t, settle(t, s, t0.Add(time.Hour)).GaveUp)
}

func claimPicksTheDeliveryDueLongest(t *testing.T, s RedeliveryStore) {
	lease := acquire(t, s, testHook, t0)
	apply(t, s, redelivery.ScanStep{Epoch: lease.Epoch, Now: t0.Add(2 * time.Minute), Failed: []redelivery.FailedAttempt{failedAt("guid-late", 2, t0)}})
	apply(t, s, redelivery.ScanStep{Epoch: lease.Epoch, Now: t0.Add(time.Minute), Failed: []redelivery.FailedAttempt{failedAt("guid-early", 1, t0)}})

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
	failed := make([]redelivery.FailedAttempt, deliveries)
	for i := range failed {
		failed[i] = failedAt("guid-"+string(rune('a'+i/26))+string(rune('a'+i%26)), int64(i+1), t0)
	}
	apply(t, s, redelivery.ScanStep{Epoch: lease.Epoch, Now: t0, Failed: failed})

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
	opened := apply(t, s, redelivery.ScanStep{
		Epoch: lease.Epoch, Now: t0, Top: t0,
		Open: &redelivery.Range{NewerAt: t0.Add(-time.Hour), OlderAt: t0.Add(-48 * time.Hour), Cursor: "cursor-1"},
	})
	require.Positive(t, opened.OpenedID)
	apply(t, s, redelivery.ScanStep{Epoch: lease.Epoch, Now: t0, Top: t0.Add(-time.Hour)})
	require.NoError(t, s.ReleaseRedeliveryLease(ctx, lease.Epoch))

	next := acquire(t, s, testHook, t0.Add(time.Minute))
	require.True(t, t0.Equal(next.Top), "the top never moves down")
	require.Equal(t, []redelivery.Range{{
		ID: opened.OpenedID, NewerAt: t0.Add(-time.Hour), OlderAt: t0.Add(-48 * time.Hour), Cursor: "cursor-1",
	}}, normalizeRanges(next.Ranges), "a run that stopped early finds its place")

	newer := apply(t, s, redelivery.ScanStep{
		Epoch: next.Epoch, Now: t0, Top: t0.Add(time.Hour),
		Open: &redelivery.Range{NewerAt: t0.Add(30 * time.Minute), OlderAt: t0, Cursor: "cursor-2"},
	})
	apply(t, s, redelivery.ScanStep{Epoch: next.Epoch, Now: t0, Advance: &redelivery.RangeAdvance{ID: opened.OpenedID, NewerAt: t0.Add(-3 * time.Hour), Cursor: "cursor-1b"}})
	require.NoError(t, s.ReleaseRedeliveryLease(ctx, next.Epoch))

	third := acquire(t, s, testHook, t0.Add(2*time.Minute))
	require.True(t, t0.Add(time.Hour).Equal(third.Top))
	require.Equal(t, []redelivery.Range{
		{ID: newer.OpenedID, NewerAt: t0.Add(30 * time.Minute), OlderAt: t0, Cursor: "cursor-2"},
		{ID: opened.OpenedID, NewerAt: t0.Add(-3 * time.Hour), OlderAt: t0.Add(-48 * time.Hour), Cursor: "cursor-1b"},
	}, normalizeRanges(third.Ranges), "ranges come newest first, and an advance moved one cursor")

	apply(t, s, redelivery.ScanStep{Epoch: third.Epoch, Now: t0, CloseID: newer.OpenedID})
	gapFrom, gapTo := t0.Add(-48*time.Hour), t0.Add(-40*time.Hour)
	apply(t, s, redelivery.ScanStep{Epoch: third.Epoch, Now: t0.Add(5 * time.Minute), CloseID: opened.OpenedID, Gap: &redelivery.Gap{From: gapFrom, To: gapTo}})
	apply(t, s, redelivery.ScanStep{Epoch: third.Epoch, Now: t0.Add(5 * time.Minute), Gap: &redelivery.Gap{From: gapFrom.Add(-time.Hour), To: gapFrom}})
	require.NoError(t, s.ReleaseRedeliveryLease(ctx, third.Epoch))
	requireRecordedGaps(t, s, []redelivery.Gap{{From: gapFrom, To: gapTo}, {From: gapFrom.Add(-time.Hour), To: gapFrom}}, "a store keeps each gap as its step gave it")

	last := acquire(t, s, testHook, t0.Add(10*time.Minute))
	require.Empty(t, last.Ranges, "a closed range, finished or a gap, is no longer open")
	deleted, err := s.PurgeRedeliveries(ctx, t0.Add(6*time.Minute))
	require.NoError(t, err)
	require.EqualValues(t, 2, deleted, "a gap is kept as a final state until it is purged, whether or not a range held it, and a finished range leaves no row")
}

func theClearIsTheLatestTimeAStepSetNotTheHighest(t *testing.T, s RedeliveryStore) {
	ctx := context.Background()
	ahead, later := t0.Add(100*time.Hour), t0.Add(10*time.Minute)

	lease := acquire(t, s, testHook, t0)
	apply(t, s, redelivery.ScanStep{Epoch: lease.Epoch, Now: t0, Top: t0.Add(-time.Minute), Clear: ahead})
	require.NoError(t, s.ReleaseRedeliveryLease(ctx, lease.Epoch))

	next := acquire(t, s, testHook, t0.Add(time.Minute))
	require.True(t, ahead.Equal(next.Clear), "a step's clear is stored: %s", next.Clear)
	require.True(t, t0.Add(-time.Minute).Equal(next.Top), "and it is not the top")
	apply(t, s, redelivery.ScanStep{Epoch: next.Epoch, Now: later, Clear: later})
	apply(t, s, redelivery.ScanStep{Epoch: next.Epoch, Now: later, Failed: []redelivery.FailedAttempt{failedAt("guid-a", 1, t0)}})
	require.NoError(t, s.ReleaseRedeliveryLease(ctx, next.Epoch))

	last := acquire(t, s, testHook, later.Add(time.Minute))
	require.True(t, later.Equal(last.Clear), "an earlier clear replaces a later one, and a step without a clear leaves it: %s", last.Clear)
	require.True(t, t0.Add(-time.Minute).Equal(last.Top), "a clear never moves the top")
}

func requireRecordedGaps(t *testing.T, s RedeliveryStore, want []redelivery.Gap, msg string) {
	t.Helper()
	records, ok := s.(GapRecords)
	if !ok {
		return
	}
	got, err := records.RecordedRedeliveryGaps(context.Background())
	require.NoError(t, err)
	require.ElementsMatch(t, normalizeGaps(want), normalizeGaps(got), msg)
}

func normalizeGaps(gaps []redelivery.Gap) []redelivery.Gap {
	out := make([]redelivery.Gap, len(gaps))
	for i, g := range gaps {
		out[i] = redelivery.Gap{From: g.From.UTC(), To: g.To.UTC()}
	}
	return out
}

func normalizeRanges(ranges []redelivery.Range) []redelivery.Range {
	out := make([]redelivery.Range, len(ranges))
	for i, r := range ranges {
		out[i] = redelivery.Range{ID: r.ID, NewerAt: r.NewerAt.UTC(), OlderAt: r.OlderAt.UTC(), Cursor: r.Cursor}
	}
	return out
}

func aFailedScanStepRecordsNothing(t *testing.T, s RedeliveryStore) {
	lease := acquire(t, s, testHook, t0)

	_, err := s.ApplyRedeliveryScan(context.Background(), redelivery.ScanStep{
		Epoch: lease.Epoch, Now: t0, Failed: []redelivery.FailedAttempt{failedAt("guid-a", 1, t0)}, Top: t0, Clear: t0,
		Advance: &redelivery.RangeAdvance{ID: 9999, NewerAt: t0, Cursor: "cursor"},
	})
	require.Error(t, err, "a range that does not exist cannot move")
	require.NotErrorIs(t, err, redelivery.ErrLeaseLost)

	require.Zero(t, settle(t, s, t0).Pending, "the attempts of a failed step are not recorded")
	require.NoError(t, s.ReleaseRedeliveryLease(context.Background(), lease.Epoch))
	next := acquire(t, s, testHook, t0)
	require.True(t, next.Top.IsZero(), "and neither is its top")
	require.True(t, next.Clear.IsZero(), "nor its clear")
}

func changingTheHookClosesOpenRangesAsGaps(t *testing.T, s RedeliveryStore) {
	ctx := context.Background()
	long := redelivery.Range{NewerAt: t0.Add(-time.Hour), OlderAt: t0.Add(-48 * time.Hour), Cursor: "cursor-1"}
	insideTheOverlap := redelivery.Range{NewerAt: t0.Add(-50*time.Hour - 2*time.Minute), OlderAt: t0.Add(-50 * time.Hour), Cursor: "cursor-2"}
	lease := acquire(t, s, testHook, t0)
	apply(t, s, redelivery.ScanStep{Epoch: lease.Epoch, Now: t0, Top: t0, Clear: t0.Add(time.Second), Open: &long})
	apply(t, s, redelivery.ScanStep{Epoch: lease.Epoch, Now: t0, Open: &insideTheOverlap})
	require.NoError(t, s.ReleaseRedeliveryLease(ctx, lease.Epoch))

	same := acquire(t, s, testHook, t0.Add(time.Minute))
	require.False(t, same.HookChanged)
	require.Len(t, same.Ranges, 2)
	require.False(t, same.Clear.IsZero())
	require.NoError(t, s.ReleaseRedeliveryLease(ctx, same.Epoch))

	other := acquire(t, s, "/orgs/acme/hooks/8", t0.Add(2*time.Minute))
	require.True(t, other.HookChanged)
	require.True(t, other.Top.IsZero(), "a new hook starts from scratch")
	require.True(t, other.Clear.IsZero(), "the clear is a time of the old hook's list, and the new hook has not been read: %s", other.Clear)
	require.Empty(t, other.Ranges)
	require.ElementsMatch(t, []redelivery.Gap{
		{From: long.OlderAt.Add(-redelivery.ReadOverlap), To: long.NewerAt},
		{From: insideTheOverlap.OlderAt.Add(-redelivery.ReadOverlap), To: insideTheOverlap.NewerAt},
	}, normalizeGaps(other.ClosedGaps), "each gap starts where the read of its range would have stopped, the overlap below its old end, and ends at the oldest attempt read above it")
	requireRecordedGaps(t, s, other.ClosedGaps, "a store keeps each gap of a changed hook as it reports it")
	require.NoError(t, s.ReleaseRedeliveryLease(ctx, other.Epoch))

	again := acquire(t, s, "/orgs/acme/hooks/8", t0.Add(3*time.Minute))
	require.False(t, again.HookChanged)
	require.True(t, again.Clear.IsZero(), "the reset is stored")
	require.Empty(t, again.ClosedGaps, "the closed ranges are reported once")
}

func claimSkipsTheDeliveriesOfAnotherHook(t *testing.T, s RedeliveryStore) {
	ctx := context.Background()
	const otherHook = "/orgs/acme/hooks/8"
	old := acquire(t, s, testHook, t0)
	apply(t, s, redelivery.ScanStep{Epoch: old.Epoch, Now: t0, Failed: []redelivery.FailedAttempt{failedAt("guid-old", 1, t0)}})
	require.NoError(t, s.ReleaseRedeliveryLease(ctx, old.Epoch))

	replaced := acquire(t, s, otherHook, t0.Add(time.Minute))
	apply(t, s, redelivery.ScanStep{Epoch: replaced.Epoch, Now: t0.Add(time.Minute), Failed: []redelivery.FailedAttempt{failedAt("guid-new", 7, t0.Add(time.Minute))}})

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
	apply(t, s, redelivery.ScanStep{
		Epoch: lease.Epoch, Now: t0, Top: t0,
		Failed: []redelivery.FailedAttempt{failedAt("guid-pending", 1, t0), failedAt("guid-recovered", 2, t0), failedAt("guid-fresh", 3, t0.Add(80*time.Hour))},
		Open:   &redelivery.Range{NewerAt: t0.Add(-time.Hour), OlderAt: t0.Add(-48 * time.Hour), Cursor: "cursor-1"},
	})
	apply(t, s, redelivery.ScanStep{Epoch: lease.Epoch, Now: t0.Add(time.Minute), Succeeded: []string{"guid-recovered", "guid-fresh"}})

	before := t0.Add(redelivery.GitHubDeliveryRetention)
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

	for name, step := range map[string]redelivery.ScanStep{
		"no epoch":                       {Now: t0},
		"no time":                        {Epoch: lease.Epoch},
		"an attempt without a GUID":      {Epoch: lease.Epoch, Now: t0, Failed: []redelivery.FailedAttempt{failedAt("", 1, t0)}},
		"an attempt ID of zero":          {Epoch: lease.Epoch, Now: t0, Failed: []redelivery.FailedAttempt{failedAt("guid", 0, t0)}},
		"two range moves":                {Epoch: lease.Epoch, Now: t0, CloseID: 1, Advance: &redelivery.RangeAdvance{ID: 1, NewerAt: t0, Cursor: "c"}},
		"an open range without a cursor": {Epoch: lease.Epoch, Now: t0, Open: &redelivery.Range{NewerAt: t0, OlderAt: t0}},
	} {
		_, err := s.ApplyRedeliveryScan(ctx, step)
		require.Error(t, err, name)
		require.NotErrorIs(t, err, redelivery.ErrLeaseLost, name)
	}

	_, _, err := s.ClaimRedelivery(ctx, redelivery.ClaimRequest{Now: t0, Retention: redelivery.GitHubDeliveryRetention, MaxAsks: 3, Schedule: testSchedule[:2]})
	require.Error(t, err, "the schedule needs one entry per ask")
	_, err = s.SettleRedeliveries(ctx, redelivery.SettleRequest{Now: t0, Retention: redelivery.GitHubDeliveryRetention})
	require.Error(t, err, "settle needs the ask limit")
	_, _, err = s.AcquireRedeliveryLease(ctx, "", t0, testLeaseTTL)
	require.Error(t, err, "a hook needs a name")
}
