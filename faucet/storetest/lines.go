package storetest

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/coder/faucet/faucet"
)

var lineSubtests = []subtest{
	{"APurgeOrTheEventsOfOtherTargetsChangeNoAnswerOfALine", aPurgeOrTheEventsOfOtherTargetsChangeNoAnswerOfALine},
	{"PurgeRaisesTheMarkOfEveryLineThatLostEvents", purgeRaisesTheMarkOfEveryLineThatLostEvents},
	{"StartAfterReadsTheEventsAboveThePositionOnTheTargetsLine", startAfterReadsTheEventsAboveThePositionOnTheTargetsLine},
	{"ClaimDeliveryGivesTheWatermarkOnTheTargetsLine", claimDeliveryGivesTheWatermarkOnTheTargetsLine},
	{"ClaimGapCarriesThePurgeMarkOfTheTargetsLine", claimGapCarriesThePurgeMarkOfTheTargetsLine},
	{"UpsertSubscriptionKeepsThePositionOfALiveRow", upsertSubscriptionKeepsThePositionOfALiveRow},
}

// lineRig stores events of the line under test, repository 42 of owner 7,
// and of other targets.
type lineRig struct {
	t      *testing.T
	s      faucet.Store
	ctx    context.Context
	stored int
}

func newLineRig(t *testing.T, s faucet.Store) *lineRig {
	return &lineRig{t: t, s: s, ctx: context.Background()}
}

func (r *lineRig) insert(repoID, ownerID int64) faucet.Event {
	r.t.Helper()
	r.stored++
	ev := newEvent(fmt.Sprintf("line-%d", r.stored))
	ev.RepoID, ev.OwnerID = repoID, ownerID
	stored, inserted, err := r.s.InsertEvent(r.ctx, ev)
	require.NoError(r.t, err)
	require.True(r.t, inserted)
	time.Sleep(2 * time.Millisecond)
	return stored
}

func (r *lineRig) own() faucet.Event { return r.insert(42, 7) }

// others stores n events of another repository of another owner, and of a
// repository that names no owner.
func (r *lineRig) others(n int) {
	r.t.Helper()
	for i := range n {
		if i%2 == 0 {
			r.insert(50, 8)
		} else {
			r.insert(51, 0)
		}
	}
}

func (r *lineRig) poll(target faucet.Target, after faucet.Position) faucet.PollPage {
	r.t.Helper()
	page, err := r.s.PollEvents(r.ctx, faucet.PollQuery{EventFilter: faucet.EventFilter{Target: target}, After: after, Limit: 100})
	require.NoError(r.t, err)
	return page
}

// answers is everything that a reader of target can get from the store.
func (r *lineRig) answers(target faucet.Target, until time.Time) []any {
	r.t.Helper()
	var got []any
	for _, after := range []faucet.Position{0, 1, 2, 100} {
		page := r.poll(target, after)
		ids := make([]string, len(page.Events))
		for i, ev := range page.Events {
			ids[i] = fmt.Sprintf("%s@%d", ev.DeliveryID, target.PositionOf(ev))
		}
		got = append(got, ids, page.Next, page.PurgedThrough)
		_, start, err := r.s.StartAfter(r.ctx, target, after)
		require.NoError(r.t, err)
		got = append(got, start)
	}
	for _, at := range []time.Time{{}, until} {
		p, err := r.s.PositionAt(r.ctx, target, at)
		require.NoError(r.t, err)
		got = append(got, p)
	}
	return got
}

func aPurgeOrTheEventsOfOtherTargetsChangeNoAnswerOfALine(t *testing.T, s faucet.Store) {
	r := newLineRig(t, s)
	r.others(3)
	first := r.own()
	r.others(2)
	r.own()
	later := time.Now().Add(time.Hour)
	repo, owner := faucet.RepoTarget(42), faucet.OwnerTarget(7)
	wantRepo, wantOwner := r.answers(repo, later), r.answers(owner, later)

	r.others(4)
	require.Equal(t, wantRepo, r.answers(repo, later), "other targets' events")
	require.Equal(t, wantOwner, r.answers(owner, later), "other targets' events")

	res, err := s.Purge(r.ctx, first.ReceivedAt)
	require.NoError(t, err)
	require.EqualValues(t, 3, res.Deleted)
	require.Equal(t, wantRepo, r.answers(repo, later), "a purge of other targets' events")
	require.Equal(t, wantOwner, r.answers(owner, later), "a purge of other targets' events")
}

func purgeRaisesTheMarkOfEveryLineThatLostEvents(t *testing.T, s faucet.Store) {
	r := newLineRig(t, s)
	a := r.insert(42, 7)
	b := r.insert(43, 7)
	c := r.insert(42, 7)
	kept := r.insert(42, 7)
	quiet := r.insert(44, 9)
	_, err := s.Purge(r.ctx, kept.ReceivedAt)
	require.NoError(t, err)

	for _, tt := range []struct {
		target faucet.Target
		mark   faucet.Position
		head   faucet.Position
	}{
		{faucet.RepoTarget(42), c.RepoPosition, kept.RepoPosition},
		{faucet.RepoTarget(43), b.RepoPosition, b.RepoPosition},
		{faucet.OwnerTarget(7), c.OwnerPosition, kept.OwnerPosition},
		{faucet.RepoTarget(44), 0, quiet.RepoPosition},
		{faucet.OwnerTarget(9), 0, quiet.OwnerPosition},
		{faucet.AllTarget(), faucet.Position(c.ID), faucet.Position(quiet.ID)},
	} {
		page := r.poll(tt.target, 0)
		require.Equal(t, tt.mark, page.PurgedThrough, "the mark of the line of %s %d %d", tt.target.Kind(), tt.target.RepoID(), tt.target.OwnerID())
		require.Equal(t, tt.head, page.Next, "the head of the line of %s %d %d", tt.target.Kind(), tt.target.RepoID(), tt.target.OwnerID())
	}
	require.Equal(t, []faucet.Position{1, 2, 3}, []faucet.Position{a.RepoPosition, c.RepoPosition, kept.RepoPosition})

	next := r.insert(43, 7)
	require.Equal(t, b.RepoPosition+1, next.RepoPosition, "a line whose events were all purged goes on from its head")
	require.Equal(t, kept.OwnerPosition+1, next.OwnerPosition)
}

func startAfterReadsTheEventsAboveThePositionOnTheTargetsLine(t *testing.T, s faucet.Store) {
	r := newLineRig(t, s)
	repo := faucet.RepoTarget(42)
	empty, start, err := s.StartAfter(r.ctx, repo, 5)
	require.NoError(t, err)
	require.Zero(t, start, "an empty line starts at its head, 0")
	require.Zero(t, empty)

	r.others(2)
	var own []faucet.Event
	for range 3 {
		own = append(own, r.own())
		r.others(2)
	}
	scanFrom := func(after int64) []faucet.Event {
		t.Helper()
		page, err := s.ScanEvents(r.ctx, faucet.ScanQuery{EventFilter: faucet.EventFilter{Target: repo}, After: after, Limit: 100})
		require.NoError(t, err)
		return page.Events
	}
	for p := faucet.Position(0); p < 3; p++ {
		after, start, err := s.StartAfter(r.ctx, repo, p)
		require.NoError(t, err)
		require.Equal(t, p, start)
		got := scanFrom(after)
		require.Len(t, got, len(own)-int(p), "position %d", p)
		for i, ev := range got {
			requireSameEvent(t, own[int(p)+i], ev)
		}
	}
	head := own[2].RepoPosition
	for _, p := range []faucet.Position{head, head + 10} {
		after, start, err := s.StartAfter(r.ctx, repo, p)
		require.NoError(t, err)
		require.Equal(t, head, start, "position %d: a start above the head is the head", p)
		page, err := s.ScanEvents(r.ctx, faucet.ScanQuery{EventFilter: faucet.EventFilter{Target: faucet.AllTarget()}, After: after, Limit: 100})
		require.NoError(t, err)
		require.Empty(t, page.Events, "position %d: a start at the head reads past every event", p)
		require.Equal(t, after, page.Next)
	}

	_, err = s.Purge(r.ctx, own[1].ReceivedAt)
	require.NoError(t, err)
	mark, err := s.PositionAt(r.ctx, repo, time.Time{})
	require.NoError(t, err)
	require.Equal(t, own[0].RepoPosition, mark)
	after, _, err := s.StartAfter(r.ctx, repo, mark)
	require.NoError(t, err)
	page, err := s.ScanEvents(r.ctx, faucet.ScanQuery{EventFilter: faucet.EventFilter{Target: repo}, After: after, Limit: 100})
	require.NoError(t, err)
	require.GreaterOrEqual(t, after, page.PurgedThrough, "a start is never below the purge mark, so a subscription that lost nothing gets no gap")
	require.Len(t, page.Events, 2)
}

func claimDeliveryGivesTheWatermarkOnTheTargetsLine(t *testing.T, s faucet.Store) {
	r := newRig(t, s, 0)
	lr := newLineRig(t, s)
	lr.others(3)
	first := r.addEvent()
	lr.others(3)
	second := r.addEvent()
	lr.others(3)
	third := r.addEvent()
	r.enqueue(first, second, third)

	claim := r.mustClaim(r.now)
	require.Equal(t, first.ID, claim.Ref.EventID)
	require.Equal(t, first.RepoPosition-1, claim.Watermark)
	r.finish(claim.Ref, r.now, faucet.Finish{Kind: faucet.FinishDelivered})
	claim = r.mustClaim(r.now)
	require.Equal(t, second.RepoPosition-1, claim.Watermark, "the position below the lowest unsent event, which other targets' events do not move")
	r.finish(claim.Ref, r.now, faucet.Finish{Kind: faucet.FinishRetry, RetryAt: r.now.Add(time.Hour), Failure: faucet.CallbackHTTP5xx})

	_, err := s.Purge(r.ctx, third.ReceivedAt)
	require.NoError(t, err)
	claim = r.mustClaim(r.now)
	require.Equal(t, third.ID, claim.Ref.EventID)
	require.Equal(t, second.RepoPosition-1, claim.Watermark,
		"the lowest unsent event is gone, so the watermark is below the line's purge mark and a resume from it reports the loss")
}

func claimGapCarriesThePurgeMarkOfTheTargetsLine(t *testing.T, s faucet.Store) {
	r := newRig(t, s, 0)
	lr := newLineRig(t, s)
	lr.others(3)
	r.addEvent()
	lost := r.addEvent()
	lr.others(3)
	kept := r.addEvent()
	res, err := s.Purge(r.ctx, kept.ReceivedAt)
	require.NoError(t, err)
	r.recordGap(res.PurgedThrough)

	claim, ok, err := s.ClaimGap(r.ctx, r.now, testLease, nil)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, res.PurgedThrough, claim.Ref.Through, "the store keeps the gap by event ID")
	require.Equal(t, lost.RepoPosition, claim.Cursor, "the cursor is the purge mark of the line of repository 42")
}

func upsertSubscriptionKeepsThePositionOfALiveRow(t *testing.T, s faucet.Store) {
	ctx := context.Background()
	sub := subscriptionAt(1, subscriptionBase.Add(time.Hour))
	sub.Position = 3
	stored, err := s.UpsertSubscription(ctx, sub, subscriptionBase, 10)
	require.NoError(t, err)
	require.EqualValues(t, 3, stored.Position)

	sub.Position = 9
	again, err := s.UpsertSubscription(ctx, sub, subscriptionBase.Add(time.Minute), 10)
	require.NoError(t, err)
	require.EqualValues(t, 3, again.Position, "a refresh of a live subscription keeps its position and ignores the supplied one")

	sub.ExpiresAt = subscriptionBase.Add(3 * time.Hour)
	replaced, err := s.UpsertSubscription(ctx, sub, subscriptionBase.Add(2*time.Hour), 10)
	require.NoError(t, err)
	require.EqualValues(t, 9, replaced.Position, "an expired row is replaced whole")

	sub.Position = -1
	_, err = s.UpsertSubscription(ctx, sub, subscriptionBase.Add(2*time.Hour), 10)
	require.Error(t, err)
}
