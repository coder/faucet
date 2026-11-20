package storetest

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/coder/superblink-events/faucet"
)

var lineSubtests = []subtest{
	{"APurgeOrTheEventsOfOtherTargetsChangeNoAnswerOfALine", aPurgeOrTheEventsOfOtherTargetsChangeNoAnswerOfALine},
	{"PurgeRaisesTheMarkOfEveryLineThatLostEvents", purgeRaisesTheMarkOfEveryLineThatLostEvents},
	{"StartSubscriptionReadsTheEventsAboveTheCursorOnTheTargetsLine", startSubscriptionReadsTheEventsAboveTheCursorOnTheTargetsLine},
	{"StartSubscriptionMovesACursorBelowTheFloorUpAsTruncated", startSubscriptionMovesACursorBelowTheFloorUpAsTruncated},
	{"ClaimDeliveryGivesTheWatermarkOnTheTargetsLine", claimDeliveryGivesTheWatermarkOnTheTargetsLine},
	{"ClaimGapCarriesThePurgeMarkOfTheTargetsLine", claimGapCarriesThePurgeMarkOfTheTargetsLine},
	{"UpsertSubscriptionKeepsThePositionOfALiveRow", upsertSubscriptionKeepsThePositionOfALiveRow},
	{"UpsertSubscriptionThatMovesTheTargetCountsThePositionOnTheNewLine", upsertSubscriptionThatMovesTheTargetCountsThePositionOnTheNewLine},
	{"ClaimDeliveryWatermarkCountsOnlyTheEventsOfTheCurrentTarget", claimDeliveryWatermarkCountsOnlyTheEventsOfTheCurrentTarget},
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

func (r *lineRig) start(q faucet.StartQuery) faucet.Start {
	r.t.Helper()
	start, err := r.s.StartSubscription(r.ctx, q)
	require.NoError(r.t, err)
	return start
}

func (r *lineRig) scan(target faucet.Target, after int64) faucet.ScanPage {
	r.t.Helper()
	page, err := r.s.ScanEvents(r.ctx, faucet.ScanQuery{EventFilter: faucet.EventFilter{Target: target}, After: after, Limit: 100})
	require.NoError(r.t, err)
	return page
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
		start := r.start(faucet.StartQuery{Target: target, Cursor: &after})
		got = append(got, start.Position, start.Truncated)
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

func startSubscriptionReadsTheEventsAboveTheCursorOnTheTargetsLine(t *testing.T, s faucet.Store) {
	r := newLineRig(t, s)
	repo := faucet.RepoTarget(42)
	five := faucet.Position(5)
	empty := r.start(faucet.StartQuery{Target: repo, Cursor: &five})
	require.Equal(t, faucet.Start{}, empty, "an empty line starts at its head, 0")

	r.others(2)
	var own []faucet.Event
	for range 3 {
		own = append(own, r.own())
		r.others(2)
	}
	for p := faucet.Position(0); p < 3; p++ {
		start := r.start(faucet.StartQuery{Target: repo, Cursor: &p})
		require.Equal(t, p, start.Position)
		require.False(t, start.Truncated)
		got := r.scan(repo, start.After).Events
		require.Len(t, got, len(own)-int(p), "position %d", p)
		for i, ev := range got {
			requireSameEvent(t, own[int(p)+i], ev)
		}
	}
	head := own[2].RepoPosition
	for _, p := range []faucet.Position{head, head + 10} {
		start := r.start(faucet.StartQuery{Target: repo, Cursor: &p})
		require.Equal(t, head, start.Position, "position %d: a start above the head is the head", p)
		require.False(t, start.Truncated, "position %d", p)
		page := r.scan(faucet.AllTarget(), start.After)
		require.Empty(t, page.Events, "position %d: a start at the head reads past every event", p)
		require.Equal(t, start.After, page.Next)
	}
	now := r.start(faucet.StartQuery{Target: repo, Since: time.Now().Add(time.Hour)})
	require.Equal(t, head, now.Position, "with no cursor, the start is the position at Since")
	require.False(t, now.Truncated)
	between := r.start(faucet.StartQuery{Target: repo, Since: own[1].ReceivedAt})
	require.Equal(t, own[0].RepoPosition, between.Position, "the newest position received before Since")
	require.Len(t, r.scan(repo, between.After).Events, 2)
}

// startSubscriptionMovesACursorBelowTheFloorUpAsTruncated: the floor is the
// line's purge mark, and with Since, its position at Since. A start is never
// below the purge mark of the all target, so a subscription that lost nothing
// gets no gap.
func startSubscriptionMovesACursorBelowTheFloorUpAsTruncated(t *testing.T, s faucet.Store) {
	r := newLineRig(t, s)
	repo := faucet.RepoTarget(42)
	var own []faucet.Event
	for range 4 {
		own = append(own, r.own())
		r.others(2)
	}
	_, err := s.Purge(r.ctx, own[1].ReceivedAt)
	require.NoError(t, err)
	mark := own[0].RepoPosition

	zero := faucet.Position(0)
	below := r.start(faucet.StartQuery{Target: repo, Cursor: &zero})
	require.Equal(t, mark, below.Position, "a cursor below the line's purge mark starts at the mark")
	require.True(t, below.Truncated, "and reports that the target lost events above the cursor")
	page := r.scan(repo, below.After)
	require.GreaterOrEqual(t, below.After, page.PurgedThrough)
	require.Len(t, page.Events, 3)

	at := r.start(faucet.StartQuery{Target: repo, Cursor: &mark})
	require.Equal(t, faucet.Start{Position: mark, After: below.After}, at, "a cursor at the mark lost nothing")

	floor := r.start(faucet.StartQuery{Target: repo, Cursor: &mark, Since: own[2].ReceivedAt})
	require.Equal(t, own[1].RepoPosition, floor.Position, "a cursor below the position at Since starts there")
	require.True(t, floor.Truncated)
	require.Len(t, r.scan(repo, floor.After).Events, 2)

	above := own[2].RepoPosition
	kept := r.start(faucet.StartQuery{Target: repo, Cursor: &above, Since: own[2].ReceivedAt})
	require.Equal(t, above, kept.Position, "a cursor above the floor stays")
	require.False(t, kept.Truncated)

	negative := faucet.Position(-1)
	_, err = s.StartSubscription(r.ctx, faucet.StartQuery{Target: repo, Cursor: &negative})
	require.Error(t, err)
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

// upsertSubscriptionThatMovesTheTargetCountsThePositionOnTheNewLine: a
// renewal whose repository or owner ID changed keeps the subscription's
// cursor, an event ID, and names the position on the new target's line at
// that cursor, which a resubscribe can read on from.
func upsertSubscriptionThatMovesTheTargetCountsThePositionOnTheNewLine(t *testing.T, s faucet.Store) {
	r := newLineRig(t, s)
	r.own()
	r.insert(77, 9)
	r.own()
	at := r.own()
	r.insert(77, 9)
	sub := subscriptionAt(1, subscriptionBase.Add(time.Hour))
	sub.Cursor, sub.Position = at.ID, at.RepoPosition
	stored, err := s.UpsertSubscription(r.ctx, sub, subscriptionBase, 10)
	require.NoError(t, err)
	require.Equal(t, at.RepoPosition, stored.Position)

	for _, tt := range []struct {
		target faucet.Target
		want   faucet.Position
	}{
		{faucet.RepoTarget(77), 1},
		{faucet.RepoTarget(77), 1},
		{faucet.OwnerTarget(9), 1},
		{faucet.OwnerTarget(7), at.OwnerPosition},
		{faucet.RepoTarget(78), 0},
	} {
		moved := sub
		moved.Target, moved.Position = tt.target, 99
		got, err := s.UpsertSubscription(r.ctx, moved, subscriptionBase.Add(time.Minute), 10)
		require.NoError(t, err)
		require.Equal(t, at.ID, got.Cursor, "a renewal keeps the cursor")
		require.Equal(t, tt.want, got.Position, "%s %d %d: the position of the new line at the cursor", tt.target.Kind(), tt.target.RepoID(), tt.target.OwnerID())
	}
}

// claimDeliveryWatermarkCountsOnlyTheEventsOfTheCurrentTarget: after a
// renewal moved the subscription to another repository, a delivery of the
// old repository's event that is still queued does not hold the watermark of
// the new line down.
func claimDeliveryWatermarkCountsOnlyTheEventsOfTheCurrentTarget(t *testing.T, s faucet.Store) {
	r := newRig(t, s, 0)
	lr := newLineRig(t, s)
	lr.insert(77, 9)
	old := r.addEvent()
	r.enqueue(old)
	moved := r.sub
	moved.Target = faucet.RepoTarget(77)
	_, err := s.UpsertSubscription(r.ctx, moved, r.now, 10)
	require.NoError(t, err)
	first := lr.insert(77, 9)
	second := lr.insert(77, 9)
	r.enqueue(first, second)

	watermarks := map[int64]faucet.Position{}
	for range 3 {
		claim := r.mustClaim(r.now)
		watermarks[claim.Ref.EventID] = claim.Watermark
	}
	require.Equal(t, first.RepoPosition-1, watermarks[first.ID], "below the new line's lowest unsent event")
	require.Equal(t, first.RepoPosition-1, watermarks[second.ID], "below the new line's lowest unsent event")
}
