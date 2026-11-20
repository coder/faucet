package storetest

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/coder/superblink-events/faucet"
)

var targetSubtests = []subtest{
	{"UpsertSubscriptionStoresAndReplacesTheTarget", upsertSubscriptionStoresAndReplacesTheTarget},
	{"PollEventsReadsEveryRepositoryThatTheTargetCovers", pollEventsReadsEveryRepositoryThatTheTargetCovers},
	{"PollEventsKeepsOnlyTheRepositoryNamesAsked", pollEventsKeepsOnlyTheRepositoryNamesAsked},
	{"ScanTargetsListsAnOwnerOrAllSubscriptionWithAnUnreadEventItCovers", scanTargetsListsAnOwnerOrAllSubscriptionWithAnUnreadEventItCovers},
	{"PurgeLeavesAnOwnerOrAllSubscriptionThatLostAnEventBelowTheMark", purgeLeavesAnOwnerOrAllSubscriptionThatLostAnEventBelowTheMark},
	{"ClaimDeliveryCarriesTheTargetAndTheEventsOwner", claimDeliveryCarriesTheTargetAndTheEventsOwner},
}

const (
	ownerOcto  int64 = 7
	ownerOther int64 = 8
)

// targetRig stores subscriptions of any target and events of any repository
// and owner.
type targetRig struct {
	t      *testing.T
	s      faucet.Store
	ctx    context.Context
	now    time.Time
	events int
}

func newTargetRig(t *testing.T, s faucet.Store) *targetRig {
	return &targetRig{t: t, s: s, ctx: context.Background(), now: subscriptionBase}
}

// subscribe stores the nth subscription of ci.run_completed, at cursor 0,
// with this target.
func (r *targetRig) subscribe(n int, target faucet.Target) faucet.Subscription {
	r.t.Helper()
	sub := subscriptionAt(n, r.now.Add(time.Hour))
	sub.Target, sub.Cursor = target, 0
	_, err := r.s.UpsertSubscription(r.ctx, sub, r.now, 10)
	require.NoError(r.t, err)
	return sub
}

// insert stores an event of ci.run_completed of this repository and owner, a
// little later than the one before.
func (r *targetRig) insert(repoID, ownerID int64, repoName string) faucet.Event {
	r.t.Helper()
	r.events++
	ev := newEvent(fmt.Sprintf("target-%d", r.events))
	ev.RepoID, ev.OwnerID, ev.RepoName = repoID, ownerID, repoName
	stored, inserted, err := r.s.InsertEvent(r.ctx, ev)
	require.NoError(r.t, err)
	require.True(r.t, inserted)
	time.Sleep(2 * time.Millisecond)
	return stored
}

func (r *targetRig) positions() map[faucet.SubscriptionKeyHash]int64 {
	r.t.Helper()
	targets, err := r.s.ScanTargets(r.ctx, r.now, 10, 100)
	require.NoError(r.t, err)
	got := map[faucet.SubscriptionKeyHash]int64{}
	for _, target := range targets {
		got[target.KeyHash] = target.ScannedThrough
	}
	return got
}

func upsertSubscriptionStoresAndReplacesTheTarget(t *testing.T, s faucet.Store) {
	r := newTargetRig(t, s)
	sub := subscriptionAt(1, r.now.Add(time.Hour))
	sub.Target = faucet.OwnerTarget(ownerOcto)
	stored, err := s.UpsertSubscription(r.ctx, sub, r.now, 10)
	require.NoError(t, err)
	require.Equal(t, faucet.OwnerTarget(ownerOcto), stored.Target)

	sub.Target = faucet.AllTarget()
	again, err := s.UpsertSubscription(r.ctx, sub, r.now.Add(time.Minute), 10)
	require.NoError(t, err)
	require.Equal(t, faucet.AllTarget(), again.Target, "a refresh replaces the target")
	require.True(t, stored.CreatedAt.Equal(again.CreatedAt), "and keeps the row")

	sub.Target = faucet.RepoTarget(42)
	again, err = s.UpsertSubscription(r.ctx, sub, r.now.Add(2*time.Minute), 10)
	require.NoError(t, err)
	require.Equal(t, faucet.RepoTarget(42), again.Target, "back to one repository, with no owner left over")
}

// seedTargetEvents stores events of two repositories of ownerOcto, one of
// ownerOther, and one stored before owners were recorded.
func seedTargetEvents(r *targetRig) (octoRepo, octoOther, other, unowned faucet.Event) {
	r.t.Helper()
	octoRepo = r.insert(42, ownerOcto, "octo/repo")
	octoOther = r.insert(43, ownerOcto, "Octo/Other")
	other = r.insert(50, ownerOther, "else/where")
	unowned = r.insert(42, 0, "")
	return octoRepo, octoOther, other, unowned
}

func pollEventsReadsEveryRepositoryThatTheTargetCovers(t *testing.T, s faucet.Store) {
	r := newTargetRig(t, s)
	octoRepo, octoOther, other, unowned := seedTargetEvents(r)

	requirePollIDs(t, s, faucet.PollQuery{EventFilter: faucet.EventFilter{Target: faucet.OwnerTarget(ownerOcto)}, Limit: 10}, octoRepo, octoOther)
	requirePollIDs(t, s, faucet.PollQuery{EventFilter: faucet.EventFilter{Target: faucet.OwnerTarget(ownerOther)}, Limit: 10}, other)
	requirePollIDs(t, s, faucet.PollQuery{EventFilter: faucet.EventFilter{Target: faucet.AllTarget()}, Limit: 10}, octoRepo, octoOther, other, unowned)
	requirePollIDs(t, s, faucet.PollQuery{EventFilter: faucet.EventFilter{Target: faucet.RepoTarget(42)}, Limit: 10}, octoRepo, unowned)

	page := requirePollIDs(t, s, faucet.PollQuery{EventFilter: faucet.EventFilter{Target: faucet.OwnerTarget(99)}, Limit: 10})
	require.Zero(t, page.Next, "an owner with no events has head 0, whatever other owners hold")
	page = requirePollIDs(t, s, faucet.PollQuery{EventFilter: faucet.EventFilter{Target: faucet.OwnerTarget(ownerOcto)}, Limit: 10}, octoRepo, octoOther)
	require.EqualValues(t, 2, page.Next, "the line of an owner counts the events of its repositories")
	require.Equal(t, []faucet.Position{1, 2, 1, 0}, []faucet.Position{octoRepo.OwnerPosition, octoOther.OwnerPosition, other.OwnerPosition, unowned.OwnerPosition},
		"an event is numbered on its owner's line, and one of no owner on none")
	require.Equal(t, []faucet.Position{1, 1, 1, 2}, []faucet.Position{octoRepo.RepoPosition, octoOther.RepoPosition, other.RepoPosition, unowned.RepoPosition})
	page = requirePollIDs(t, s, faucet.PollQuery{EventFilter: faucet.EventFilter{Target: faucet.AllTarget()}, Limit: 2}, octoRepo, octoOther)
	require.Equal(t, faucet.Position(octoOther.ID), page.Next, "a full page ends at its last event")

	got, err := s.PollEvents(r.ctx, faucet.PollQuery{EventFilter: faucet.EventFilter{Target: faucet.OwnerTarget(ownerOcto)}, Limit: 10})
	require.NoError(t, err)
	requireSameEvent(t, octoOther, got.Events[1])
}

func pollEventsKeepsOnlyTheRepositoryNamesAsked(t *testing.T, s faucet.Store) {
	r := newTargetRig(t, s)
	octoRepo, octoOther, other, unowned := seedTargetEvents(r)

	requirePollIDs(t, s, faucet.PollQuery{EventFilter: faucet.EventFilter{Target: faucet.OwnerTarget(ownerOcto), RepoNames: []string{"octo/other"}}, Limit: 10}, octoOther)
	requirePollIDs(t, s, faucet.PollQuery{EventFilter: faucet.EventFilter{Target: faucet.AllTarget(), RepoNames: []string{"octo/repo", "else/where"}}, Limit: 10}, octoRepo, other)
	requirePollIDs(t, s, faucet.PollQuery{EventFilter: faucet.EventFilter{Target: faucet.OwnerTarget(ownerOther), RepoNames: []string{"octo/repo"}}, Limit: 10})
	requirePollIDs(t, s, faucet.PollQuery{EventFilter: faucet.EventFilter{Target: faucet.RepoTarget(42), RepoNames: []string{"octo/repo"}}, Limit: 10}, octoRepo)
	page := requirePollIDs(t, s, faucet.PollQuery{EventFilter: faucet.EventFilter{Target: faucet.AllTarget(), RepoNames: []string{"nobody/here"}}, Limit: 10})
	require.Equal(t, faucet.Position(unowned.ID), page.Next, "a name nobody sent gets the head of the all target as its cursor")
}

func scanTargetsListsAnOwnerOrAllSubscriptionWithAnUnreadEventItCovers(t *testing.T, s faucet.Store) {
	r := newTargetRig(t, s)
	owner := r.subscribe(1, faucet.OwnerTarget(ownerOcto))
	all := r.subscribe(2, faucet.AllTarget())

	r.insert(50, ownerOther, "else/where")
	require.Equal(t, map[faucet.SubscriptionKeyHash]int64{all.KeyHash: 0}, r.positions(),
		"an event of another owner is something to read for the all subscription only")
	r.insert(42, 0, "")
	require.Equal(t, map[faucet.SubscriptionKeyHash]int64{all.KeyHash: 0}, r.positions(),
		"an event with no owner recorded is no owner's")

	r.insert(44, ownerOcto, "octo/new")
	require.Equal(t, map[faucet.SubscriptionKeyHash]int64{owner.KeyHash: 0, all.KeyHash: 0}, r.positions(),
		"an event of any repository of the owner is something to read for the owner subscription")

	targets, err := s.ScanTargets(r.ctx, r.now, 10, 100)
	require.NoError(t, err)
	for _, target := range targets {
		want := map[faucet.SubscriptionKeyHash]faucet.Target{owner.KeyHash: owner.Target, all.KeyHash: all.Target}[target.KeyHash]
		require.Equal(t, want, target.Target, "a scan target carries the subscription's target")
	}
}

// purgeLeavesAnOwnerOrAllSubscriptionThatLostAnEventBelowTheMark pins what a
// gap means for a subscription of many repositories: the purge deleted an
// event that its target covers and that it had not read.
func purgeLeavesAnOwnerOrAllSubscriptionThatLostAnEventBelowTheMark(t *testing.T, s faucet.Store) {
	r := newTargetRig(t, s)
	lossyOwner := r.subscribe(1, faucet.OwnerTarget(ownerOcto))
	quietOwner := r.subscribe(2, faucet.OwnerTarget(ownerOther))
	all := r.subscribe(3, faucet.AllTarget())

	lost := r.insert(44, ownerOcto, "octo/new")
	survivor := r.insert(50, ownerOther, "else/where")
	res, err := s.Purge(r.ctx, survivor.ReceivedAt)
	require.NoError(t, err)
	require.Equal(t, faucet.PurgeResult{Deleted: 1, PurgedThrough: lost.ID}, res)
	require.Equal(t, map[faucet.SubscriptionKeyHash]int64{lossyOwner.KeyHash: 0, quietOwner.KeyHash: lost.ID, all.KeyHash: 0}, r.positions(),
		"the owner that lost an event and the all subscription stay below the mark, the other owner lost nothing and moves to it")

	unowned := r.insert(42, 0, "")
	_, err = s.Purge(r.ctx, unowned.ReceivedAt)
	require.NoError(t, err)
	require.Equal(t, map[faucet.SubscriptionKeyHash]int64{lossyOwner.KeyHash: 0, quietOwner.KeyHash: lost.ID, all.KeyHash: 0}, r.positions(),
		"the survivor of the other owner is now lost too, and the first loss is not forgotten")

	quiet := r.subscribe(4, faucet.OwnerTarget(ownerOther))
	allLater := r.subscribe(5, faucet.AllTarget())
	for _, sub := range []faucet.Subscription{quiet, allLater} {
		won, err := s.EnqueueDeliveries(r.ctx, r.now, faucet.Enqueue{Key: sub.KeyHash, NewScannedThrough: survivor.ID})
		require.NoError(t, err)
		require.True(t, won)
	}
	res, err = s.Purge(r.ctx, unowned.ReceivedAt.Add(time.Hour))
	require.NoError(t, err)
	require.Equal(t, unowned.ID, res.PurgedThrough)
	positions := r.positions()
	require.Equal(t, survivor.ID, positions[allLater.KeyHash], "an event with no owner recorded is lost for an all subscription")
	_, listed := positions[quiet.KeyHash]
	require.False(t, listed, "and not for an owner subscription, which moves to the mark and has nothing to read")
}

func claimDeliveryCarriesTheTargetAndTheEventsOwner(t *testing.T, s faucet.Store) {
	r := newTargetRig(t, s)
	owner := r.subscribe(1, faucet.OwnerTarget(ownerOcto))
	ev := r.insert(44, ownerOcto, "octo/new")
	won, err := s.EnqueueDeliveries(r.ctx, r.now, faucet.Enqueue{
		Key: owner.KeyHash, NewScannedThrough: ev.ID,
		Deliveries: []faucet.NewDelivery{{EventID: ev.ID, MessageID: messageIDOf(ev)}},
	})
	require.NoError(t, err)
	require.True(t, won)

	claim, ok, err := s.ClaimDelivery(r.ctx, r.now, testLease, nil)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, faucet.OwnerTarget(ownerOcto), claim.Target)
	require.NotNil(t, claim.Event)
	require.Equal(t, ownerOcto, claim.Event.OwnerID, "the event keeps its owner, so the run can check that the target covers it")
	require.Equal(t, "octo/new", claim.Event.RepoName)
	require.True(t, claim.Target.Covers(claim.Event.RepoID, claim.Event.OwnerID))
}
