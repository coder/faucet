// Package storetest is a contract test suite for faucet.Store implementations.
package storetest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/coder/faucet/faucet"
)

type subtest struct {
	name string
	run  func(t *testing.T, s faucet.Store)
}

// Run runs the contract against stores returned by newStore. Every call to
// newStore must return an empty store that does not share state with the
// others, because the subtests run in parallel.
func Run(t *testing.T, newStore func(t *testing.T) faucet.Store) {
	subtests := []subtest{
		{"InsertEventIsIdempotentPerDelivery", insertEventIsIdempotentPerDelivery},
		{"InsertEventValidatesInput", insertEventValidatesInput},
		{"EventsAfterReturnsIDOrderAndLimit", eventsAfterReturnsIDOrderAndLimit},
		{"ConcurrentInsertsCommitInIDOrder", concurrentInsertsCommitInIDOrder},
		{"PollEventsAppliesFiltersBeforeLimit", pollEventsAppliesFiltersBeforeLimit},
		{"PollEventsReturnsHeadCursorWhenNothingMatches", pollEventsReturnsHeadCursorWhenNothingMatches},
		{"PollEventsReturnsLastIDWhenPageIsFull", pollEventsReturnsLastIDWhenPageIsFull},
		{"PollEventsValidatesQuery", pollEventsValidatesQuery},
		{"CursorAtReturnsHighestIDReceivedBefore", cursorAtReturnsHighestIDReceivedBefore},
		{"ConcurrentDuplicateDeliveryStoresOnce", concurrentDuplicateDeliveryStoresOnce},
		{"PurgeDeletesOnlyEventsReceivedBefore", purgeDeletesOnlyEventsReceivedBefore},
		{"PollEventsAndCursorAtNeverFallBelowThePurgeMark", pollEventsAndCursorAtNeverFallBelowThePurgeMark},
		{"PurgeUnderConcurrentInsertsKeepsNewEventsAndAnExactMark", purgeUnderConcurrentInsertsKeepsNewEventsAndAnExactMark},
		{"PollEventsNeverHidesAConcurrentPurge", pollEventsNeverHidesAConcurrentPurge},
		{"UpsertSubscriptionStoresThenRefreshes", upsertSubscriptionStoresThenRefreshes},
		{"UpsertSubscriptionTreatsAnExpiredRowAsAbsent", upsertSubscriptionTreatsAnExpiredRowAsAbsent},
		{"UpsertSubscriptionKeepsTheReplacedSecretForTheGraceWindow", upsertSubscriptionKeepsTheReplacedSecretForTheGraceWindow},
		{"UpsertSubscriptionStartsNoWindowWithoutAGrace", upsertSubscriptionStartsNoWindowWithoutAGrace},
		{"UpsertSubscriptionDropsThePreviousSecretOfALapsedRow", upsertSubscriptionDropsThePreviousSecretOfALapsedRow},
		{"ClearExpiredPreviousSecretsClearsOnlyEndedWindows", clearExpiredPreviousSecretsClearsOnlyEndedWindows},
		{"DeleteLiveSubscriptionReturnsThePreviousSecret", deleteLiveSubscriptionReturnsThePreviousSecret},
		{"DeleteSubscriptionRemovesOnlyTheCallersRow", deleteSubscriptionRemovesOnlyTheCallersRow},
		{"DeleteLiveSubscriptionGivesTheRowToExactlyOneCaller", deleteLiveSubscriptionGivesTheRowToExactlyOneCaller},
		{"UpsertSubscriptionEnforcesThePrincipalLimit", upsertSubscriptionEnforcesThePrincipalLimit},
		{"UpsertSubscriptionLimitHoldsUnderConcurrency", upsertSubscriptionLimitHoldsUnderConcurrency},
		{"UpsertSubscriptionValidatesInput", upsertSubscriptionValidatesInput},
		{"DeleteExpiredSubscriptionsDeletesOnlyExpiredRows", deleteExpiredSubscriptionsDeletesOnlyExpiredRows},
	}
	subtests = append(subtests, deliverySubtests...)
	for _, st := range subtests {
		t.Run(st.name, func(t *testing.T) {
			t.Parallel()
			st.run(t, newStore(t))
		})
	}
}

func newEvent(deliveryID string) faucet.Event {
	return faucet.Event{
		Source:     "github",
		DeliveryID: deliveryID,
		Type:       "ci.run_completed",
		RepoID:     42,
		PRs:        []int32{7, 9},
		HeadSHA:    "0123abcd",
		Data:       json.RawMessage(`{"conclusion":"success"}`),
	}
}

// requireSameEvent compares events field by field. Stores may re-encode Data,
// for example jsonb reorders keys and adds whitespace, so it is compared as JSON.
func requireSameEvent(t *testing.T, want, got faucet.Event) {
	t.Helper()
	require.Equal(t, want.ID, got.ID)
	require.Equal(t, want.Source, got.Source)
	require.Equal(t, want.DeliveryID, got.DeliveryID)
	require.Equal(t, want.Type, got.Type)
	require.Equal(t, want.RepoID, got.RepoID)
	require.ElementsMatch(t, want.PRs, got.PRs)
	require.Equal(t, want.HeadSHA, got.HeadSHA)
	require.JSONEq(t, string(want.Data), string(got.Data))
	require.True(t, want.ReceivedAt.Equal(got.ReceivedAt), "received_at differs: %s vs %s", want.ReceivedAt, got.ReceivedAt)
}

func insertEventIsIdempotentPerDelivery(t *testing.T, s faucet.Store) {
	ctx := context.Background()

	first := newEvent("delivery-1")
	stored, inserted, err := s.InsertEvent(ctx, first)
	require.NoError(t, err)
	require.True(t, inserted)
	require.Positive(t, stored.ID)
	require.False(t, stored.ReceivedAt.IsZero())
	first.ID, first.ReceivedAt = stored.ID, stored.ReceivedAt
	requireSameEvent(t, first, stored)

	retry := newEvent("delivery-1")
	retry.Type = "pr.review_submitted"
	retry.PRs = nil
	retry.Data = json.RawMessage(`{"retry":true}`)
	again, inserted, err := s.InsertEvent(ctx, retry)
	require.NoError(t, err)
	require.False(t, inserted)
	requireSameEvent(t, stored, again)

	events, err := s.EventsAfter(ctx, 0, 10)
	require.NoError(t, err)
	require.Len(t, events, 1)
	requireSameEvent(t, stored, events[0])
}

func insertEventValidatesInput(t *testing.T, s faucet.Store) {
	ctx := context.Background()

	bad := map[string]func(ev *faucet.Event){
		"empty delivery ID": func(ev *faucet.Event) { ev.DeliveryID = "" },
		"empty type":        func(ev *faucet.Event) { ev.Type = "" },
		"empty source":      func(ev *faucet.Event) { ev.Source = "" },
		"zero repo ID":      func(ev *faucet.Event) { ev.RepoID = 0 },
		"nil data":          func(ev *faucet.Event) { ev.Data = nil },
		"empty data":        func(ev *faucet.Event) { ev.Data = json.RawMessage{} },
		"invalid JSON data": func(ev *faucet.Event) { ev.Data = json.RawMessage(`{"a":`) },
		"NUL in a value":    func(ev *faucet.Event) { ev.Data = json.RawMessage(`{"msg":"a\u0000b"}`) },
		"NUL in a key":      func(ev *faucet.Event) { ev.Data = json.RawMessage(`{"a\u0000":[1]}`) },
	}
	for name, mutate := range bad {
		ev := newEvent("delivery-bad")
		mutate(&ev)
		_, inserted, err := s.InsertEvent(ctx, ev)
		require.Error(t, err, name)
		require.False(t, inserted, name)
	}

	events, err := s.EventsAfter(ctx, 0, 10)
	require.NoError(t, err)
	require.Empty(t, events)
}

func eventsAfterReturnsIDOrderAndLimit(t *testing.T, s faucet.Store) {
	ctx := context.Background()

	var stored []faucet.Event
	for i := range 5 {
		ev := newEvent(fmt.Sprintf("delivery-%d", i))
		if i == 0 {
			ev.PRs, ev.HeadSHA = nil, ""
		}
		got, inserted, err := s.InsertEvent(ctx, ev)
		require.NoError(t, err)
		require.True(t, inserted)
		stored = append(stored, got)
	}
	require.True(t, slices.IsSortedFunc(stored, func(a, b faucet.Event) int { return int(a.ID - b.ID) }),
		"IDs must increase with insertion order")
	require.Empty(t, stored[0].PRs)
	require.Empty(t, stored[0].HeadSHA)

	requireEvents := func(afterID int64, limit int32, want []faucet.Event) {
		t.Helper()
		got, err := s.EventsAfter(ctx, afterID, limit)
		require.NoError(t, err)
		require.Len(t, got, len(want))
		for i := range want {
			requireSameEvent(t, want[i], got[i])
		}
	}
	requireEvents(0, 100, stored)
	requireEvents(0, 2, stored[:2])
	requireEvents(stored[1].ID, 2, stored[2:4])
	requireEvents(stored[3].ID, 100, stored[4:])
	requireEvents(stored[4].ID, 100, nil)

	for _, limit := range []int32{0, -1} {
		_, err := s.EventsAfter(ctx, 0, limit)
		require.Error(t, err, "limit %d", limit)
	}
}

// concurrentInsertsCommitInIDOrder checks the Store ordering guarantee. Writers
// insert concurrently while readers poll with a cursor, the way a subscriber
// does. If a lower ID could commit after a higher one, a reader's cursor would
// pass it and the reader would never see it.
func concurrentInsertsCommitInIDOrder(t *testing.T, s faucet.Store) {
	const (
		writers          = 16
		insertsPerWriter = 25
		readers          = 3
		pageSize         = 20
	)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	var (
		mu       sync.Mutex
		inserted []int64
		writerWG sync.WaitGroup
	)
	for w := range writers {
		writerWG.Go(func() {
			for i := range insertsPerWriter {
				stored, ok, err := s.InsertEvent(ctx, newEvent(fmt.Sprintf("delivery-%d-%d", w, i)))
				if err != nil || !ok {
					t.Errorf("writer %d insert %d: inserted=%t err=%v", w, i, ok, err)
					return
				}
				mu.Lock()
				inserted = append(inserted, stored.ID)
				mu.Unlock()
			}
		})
	}

	writersDone := make(chan struct{})
	go func() {
		writerWG.Wait()
		close(writersDone)
	}()

	seen := make([][]int64, readers)
	var readerWG sync.WaitGroup
	for r := range readers {
		readerWG.Go(func() {
			var cursor int64
			finished := false
			for {
				select {
				case <-writersDone:
					finished = true
				default:
				}
				events, err := s.EventsAfter(ctx, cursor, pageSize)
				if err != nil {
					t.Errorf("reader %d: %v", r, err)
					return
				}
				for _, ev := range events {
					seen[r] = append(seen[r], ev.ID)
					cursor = ev.ID
				}
				// A page read after the writers finished that comes back empty
				// means the reader has drained everything.
				if finished && len(events) == 0 {
					return
				}
			}
		})
	}
	readerWG.Wait()
	<-writersDone
	if t.Failed() {
		return
	}

	slices.Sort(inserted)
	require.Len(t, inserted, writers*insertsPerWriter)
	for r, ids := range seen {
		require.True(t, slices.IsSorted(ids), "reader %d saw IDs out of order", r)
		missed, extra := difference(inserted, ids), difference(ids, inserted)
		require.Empty(t, missed, "reader %d skipped committed IDs", r)
		require.Empty(t, extra, "reader %d saw IDs nobody inserted", r)
		require.Len(t, ids, len(inserted), "reader %d saw an ID twice", r)
	}
}

const (
	shaA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	shaB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// seedPollEvents inserts, in this order:
//
//	[0] repo 42 ci.run_completed     sha A PRs 7
//	[1] repo 42 pr.closed            no sha PRs 7
//	[2] repo 42 ci.run_completed     sha B PRs 9
//	[3] repo 42 pr.review_submitted  sha A PRs 7 and 9
//	[4] repo 42 ci.run_completed     sha A no PRs
//	[5] repo 43 ci.run_completed     sha A PRs 7
func seedPollEvents(t *testing.T, s faucet.Store) []faucet.Event {
	t.Helper()
	specs := []struct {
		repoID  int64
		typ     string
		headSHA string
		prs     []int32
	}{
		{42, "ci.run_completed", shaA, []int32{7}},
		{42, "pr.closed", "", []int32{7}},
		{42, "ci.run_completed", shaB, []int32{9}},
		{42, "pr.review_submitted", shaA, []int32{7, 9}},
		{42, "ci.run_completed", shaA, nil},
		{43, "ci.run_completed", shaA, []int32{7}},
	}
	stored := make([]faucet.Event, 0, len(specs))
	for i, sp := range specs {
		ev := newEvent(fmt.Sprintf("poll-%d", i))
		ev.RepoID, ev.Type, ev.HeadSHA, ev.PRs = sp.repoID, sp.typ, sp.headSHA, sp.prs
		got, inserted, err := s.InsertEvent(context.Background(), ev)
		require.NoError(t, err)
		require.True(t, inserted)
		stored = append(stored, got)
	}
	return stored
}

func requirePollIDs(t *testing.T, s faucet.Store, q faucet.PollQuery, want ...faucet.Event) faucet.PollPage {
	t.Helper()
	page, err := s.PollEvents(context.Background(), q)
	require.NoError(t, err)
	require.Len(t, page.Events, len(want))
	for i := range want {
		requireSameEvent(t, want[i], page.Events[i])
	}
	return page
}

func pollEventsAppliesFiltersBeforeLimit(t *testing.T, s faucet.Store) {
	e := seedPollEvents(t, s)
	base := faucet.PollQuery{RepoID: 42, Limit: 100}

	tests := []struct {
		name   string
		mutate func(q *faucet.PollQuery)
		want   []faucet.Event
	}{
		{"repo only", func(*faucet.PollQuery) {}, []faucet.Event{e[0], e[1], e[2], e[3], e[4]}},
		{"other repo", func(q *faucet.PollQuery) { q.RepoID = 43 }, []faucet.Event{e[5]}},
		{"unknown repo", func(q *faucet.PollQuery) { q.RepoID = 99 }, nil},
		{"one type", func(q *faucet.PollQuery) { q.Types = []string{"pr.closed"} }, []faucet.Event{e[1]}},
		{"two types", func(q *faucet.PollQuery) { q.Types = []string{"pr.closed", "pr.review_submitted"} }, []faucet.Event{e[1], e[3]}},
		{"sha", func(q *faucet.PollQuery) { q.HeadSHA = shaA }, []faucet.Event{e[0], e[3], e[4]}},
		{"pr", func(q *faucet.PollQuery) { q.PR = 9 }, []faucet.Event{e[2], e[3]}},
		{"after", func(q *faucet.PollQuery) { q.After = e[2].ID }, []faucet.Event{e[3], e[4]}},
		{"all filters", func(q *faucet.PollQuery) {
			q.Types, q.HeadSHA, q.PR, q.After = []string{"pr.review_submitted"}, shaA, 7, e[0].ID
		}, []faucet.Event{e[3]}},
		{"filters that exclude everything", func(q *faucet.PollQuery) { q.Types, q.PR = []string{"pr.closed"}, 9 }, nil},
	}
	for _, tt := range tests {
		q := base
		tt.mutate(&q)
		requirePollIDs(t, s, q, tt.want...)
	}

	q := base
	q.Types, q.HeadSHA, q.Limit = []string{"ci.run_completed"}, shaA, 1
	page := requirePollIDs(t, s, q, e[0])
	q.After = page.Next
	page = requirePollIDs(t, s, q, e[4])
	q.After = page.Next
	requirePollIDs(t, s, q)

	q = base
	q.Types, q.PR, q.Limit = []string{"pr.review_submitted"}, 9, 1
	requirePollIDs(t, s, q, e[3])
}

func pollEventsReturnsHeadCursorWhenNothingMatches(t *testing.T, s faucet.Store) {
	empty, err := s.PollEvents(context.Background(), faucet.PollQuery{RepoID: 42, Limit: 10})
	require.NoError(t, err)
	require.Empty(t, empty.Events)
	require.Zero(t, empty.Next, "an empty store has head 0")

	e := seedPollEvents(t, s)
	head := e[5].ID

	for name, q := range map[string]faucet.PollQuery{
		"type nobody sent":                 {RepoID: 42, Types: []string{"pr.comment_created"}, Limit: 10},
		"repo with no events":              {RepoID: 99, Limit: 10},
		"already at the head":              {RepoID: 42, After: e[4].ID, Limit: 10},
		"newer events only in other repos": {RepoID: 43, After: e[5].ID, Limit: 10},
	} {
		page := requirePollIDs(t, s, q)
		require.Equal(t, head, page.Next, name)
	}

	page := requirePollIDs(t, s, faucet.PollQuery{RepoID: 42, After: head + 100, Limit: 10})
	require.Equal(t, head+100, page.Next, "the cursor never moves backwards")

	page = requirePollIDs(t, s, faucet.PollQuery{RepoID: 42, Limit: 100}, e[0], e[1], e[2], e[3], e[4])
	require.Equal(t, head, page.Next, "a page with room left reports the head, not its last event")
}

func pollEventsReturnsLastIDWhenPageIsFull(t *testing.T, s faucet.Store) {
	e := seedPollEvents(t, s)

	page := requirePollIDs(t, s, faucet.PollQuery{RepoID: 42, Limit: 2}, e[0], e[1])
	require.Equal(t, e[1].ID, page.Next)

	page = requirePollIDs(t, s, faucet.PollQuery{RepoID: 42, Limit: 5}, e[0], e[1], e[2], e[3], e[4])
	require.Equal(t, e[4].ID, page.Next, "a page that is exactly full still reports its last event")

	var got []faucet.Event
	q := faucet.PollQuery{RepoID: 42, Limit: 2}
	for range 10 {
		page, err := s.PollEvents(context.Background(), q)
		require.NoError(t, err)
		got = append(got, page.Events...)
		if len(page.Events) == 0 {
			break
		}
		q.After = page.Next
	}
	require.Len(t, got, 5)
	for i := range got {
		requireSameEvent(t, e[i], got[i])
	}
}

func pollEventsValidatesQuery(t *testing.T, s faucet.Store) {
	good := faucet.PollQuery{RepoID: 42, Limit: 10}
	for name, mutate := range map[string]func(q *faucet.PollQuery){
		"zero repo ID":     func(q *faucet.PollQuery) { q.RepoID = 0 },
		"negative repo ID": func(q *faucet.PollQuery) { q.RepoID = -1 },
		"zero limit":       func(q *faucet.PollQuery) { q.Limit = 0 },
		"negative limit":   func(q *faucet.PollQuery) { q.Limit = -1 },
		"negative after":   func(q *faucet.PollQuery) { q.After = -1 },
		"negative pr":      func(q *faucet.PollQuery) { q.PR = -1 },
	} {
		q := good
		mutate(&q)
		_, err := s.PollEvents(context.Background(), q)
		require.Error(t, err, name)
	}
	_, err := s.PollEvents(context.Background(), good)
	require.NoError(t, err)
}

// cursorAtReturnsHighestIDReceivedBefore takes its boundaries from the stored
// ReceivedAt values, so it does not depend on the clock of the test process
// matching the clock of the store.
func cursorAtReturnsHighestIDReceivedBefore(t *testing.T, s faucet.Store) {
	ctx := context.Background()

	none, err := s.CursorAt(ctx, time.Now())
	require.NoError(t, err)
	require.Zero(t, none)

	var stored []faucet.Event
	for i := range 3 {
		got, _, err := s.InsertEvent(ctx, newEvent(fmt.Sprintf("cursor-%d", i)))
		require.NoError(t, err)
		stored = append(stored, got)
		time.Sleep(2 * time.Millisecond)
	}

	for name, tt := range map[string]struct {
		at   time.Time
		want int64
	}{
		"before every event":           {stored[0].ReceivedAt.Add(-time.Hour), 0},
		"exactly at the first event":   {stored[0].ReceivedAt, 0},
		"exactly at the second event":  {stored[1].ReceivedAt, stored[0].ID},
		"between the second and third": {stored[1].ReceivedAt.Add(time.Millisecond), stored[1].ID},
		"exactly at the third event":   {stored[2].ReceivedAt, stored[1].ID},
		"after every event":            {stored[2].ReceivedAt.Add(time.Hour), stored[2].ID},
	} {
		got, err := s.CursorAt(ctx, tt.at)
		require.NoError(t, err, name)
		require.Equal(t, tt.want, got, name)
	}

	page, err := s.PollEvents(ctx, faucet.PollQuery{RepoID: 42, After: stored[0].ID, Limit: 10})
	require.NoError(t, err)
	require.Len(t, page.Events, 2)
	require.Equal(t, stored[1].ID, page.Events[0].ID, "CursorAt then PollEvents returns the events from that time on")
}

func difference(a, b []int64) []int64 {
	var out []int64
	for _, id := range a {
		if !slices.Contains(b, id) {
			out = append(out, id)
		}
	}
	return out
}

// GitHub can redeliver a webhook while the first delivery is still in flight.
// One race window is narrow, so the test runs several rounds to catch a store
// that handles it wrongly on most runs, not only some.
func concurrentDuplicateDeliveryStoresOnce(t *testing.T, s faucet.Store) {
	ctx := context.Background()
	const (
		rounds  = 8
		writers = 16
	)

	type result struct {
		id       int64
		inserted bool
		err      error
	}
	for round := range rounds {
		deliveryID := fmt.Sprintf("delivery-dup-%d", round)
		results := make([]result, writers)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := range writers {
			wg.Go(func() {
				<-start
				stored, inserted, err := s.InsertEvent(ctx, newEvent(deliveryID))
				results[i] = result{id: stored.ID, inserted: inserted, err: err}
			})
		}
		close(start)
		wg.Wait()

		insertedCount := 0
		for _, r := range results {
			require.NoError(t, r.err, "round %d", round)
			require.Equal(t, results[0].id, r.id, "round %d: every writer must get the stored row", round)
			if r.inserted {
				insertedCount++
			}
		}
		require.Equal(t, 1, insertedCount, "round %d", round)
	}

	events, err := s.EventsAfter(ctx, 0, rounds+1)
	require.NoError(t, err)
	require.Len(t, events, rounds)
}

func insertSpaced(t *testing.T, s faucet.Store, prefix string, n int) []faucet.Event {
	t.Helper()
	stored := make([]faucet.Event, 0, n)
	for i := range n {
		got, inserted, err := s.InsertEvent(context.Background(), newEvent(fmt.Sprintf("%s-%d", prefix, i)))
		require.NoError(t, err)
		require.True(t, inserted)
		stored = append(stored, got)
		time.Sleep(2 * time.Millisecond)
	}
	return stored
}

func requireRemaining(t *testing.T, s faucet.Store, want ...faucet.Event) {
	t.Helper()
	got, err := s.EventsAfter(context.Background(), 0, 1000)
	require.NoError(t, err)
	require.Len(t, got, len(want))
	for i := range want {
		requireSameEvent(t, want[i], got[i])
	}
}

// purgeDeletesOnlyEventsReceivedBefore takes its boundaries from the stored
// ReceivedAt values, like cursorAtReturnsHighestIDReceivedBefore.
func purgeDeletesOnlyEventsReceivedBefore(t *testing.T, s faucet.Store) {
	ctx := context.Background()

	res, err := s.Purge(ctx, time.Now().Add(time.Hour))
	require.NoError(t, err)
	require.Equal(t, faucet.PurgeResult{}, res, "purging an empty store deletes nothing and leaves the mark at 0")

	e := insertSpaced(t, s, "purge", 4)

	res, err = s.Purge(ctx, e[0].ReceivedAt)
	require.NoError(t, err)
	require.Equal(t, faucet.PurgeResult{}, res, "an event received exactly at the boundary is kept")
	requireRemaining(t, s, e[0], e[1], e[2], e[3])

	res, err = s.Purge(ctx, e[2].ReceivedAt)
	require.NoError(t, err)
	require.Equal(t, faucet.PurgeResult{Deleted: 2, PurgedThrough: e[1].ID}, res)
	requireRemaining(t, s, e[2], e[3])

	res, err = s.Purge(ctx, e[1].ReceivedAt)
	require.NoError(t, err)
	require.Equal(t, faucet.PurgeResult{Deleted: 0, PurgedThrough: e[1].ID}, res, "an older boundary deletes nothing and the mark never moves down")

	res, err = s.Purge(ctx, e[3].ReceivedAt)
	require.NoError(t, err)
	require.Equal(t, faucet.PurgeResult{Deleted: 1, PurgedThrough: e[2].ID}, res)
	requireRemaining(t, s, e[3])

	res, err = s.Purge(ctx, e[3].ReceivedAt.Add(time.Hour))
	require.NoError(t, err)
	require.Equal(t, faucet.PurgeResult{Deleted: 1, PurgedThrough: e[3].ID}, res)
	requireRemaining(t, s)

	res, err = s.Purge(ctx, e[3].ReceivedAt.Add(2*time.Hour))
	require.NoError(t, err)
	require.Equal(t, faucet.PurgeResult{Deleted: 0, PurgedThrough: e[3].ID}, res, "purging an empty store keeps the mark")
}

func pollEventsAndCursorAtNeverFallBelowThePurgeMark(t *testing.T, s faucet.Store) {
	ctx := context.Background()
	q := faucet.PollQuery{RepoID: 42, Limit: 10}

	before, err := s.PollEvents(ctx, q)
	require.NoError(t, err)
	require.Zero(t, before.PurgedThrough, "a store that never purged reports mark 0")

	e := insertSpaced(t, s, "mark", 3)
	_, err = s.Purge(ctx, e[2].ReceivedAt.Add(time.Hour))
	require.NoError(t, err)
	mark := e[2].ID

	for _, after := range []int64{0, e[0].ID, mark, mark + 5} {
		q.After = after
		page, err := s.PollEvents(ctx, q)
		require.NoError(t, err)
		require.Empty(t, page.Events, "after %d", after)
		require.Equal(t, mark, page.PurgedThrough, "after %d", after)
		require.GreaterOrEqual(t, page.Next, max(mark, after), "after %d: a quiet store must not send a poller back below the mark", after)
	}

	cursor, err := s.CursorAt(ctx, time.Now().Add(time.Hour))
	require.NoError(t, err)
	require.Equal(t, mark, cursor, "CursorAt is never below the mark, so a poll that starts now is not a truncated one")

	fresh, _, err := s.InsertEvent(ctx, newEvent("mark-fresh"))
	require.NoError(t, err)
	q.After = 0
	page := requirePollIDs(t, s, q, fresh)
	require.Equal(t, mark, page.PurgedThrough)
	require.Equal(t, fresh.ID, page.Next)
}

// purgeUnderConcurrentInsertsKeepsNewEventsAndAnExactMark runs purges while
// writers insert. The purge needs no insert lock because it deletes and sets
// the mark in one statement and IDs commit in order. The test pins what that
// guarantees: no new event is deleted, and the mark is exactly the highest ID
// the purge deleted, not the head ID it happened to see.
func purgeUnderConcurrentInsertsKeepsNewEventsAndAnExactMark(t *testing.T, s faucet.Store) {
	const (
		writers          = 8
		insertsPerWriter = 20
		purges           = 6
	)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	old := insertSpaced(t, s, "old", 10)
	cutoff := old[len(old)-1].ReceivedAt.Add(time.Millisecond)
	time.Sleep(5 * time.Millisecond)

	var (
		mu       sync.Mutex
		inserted []int64
		purged   []faucet.PurgeResult
		wg       sync.WaitGroup
	)
	start := make(chan struct{})
	for w := range writers {
		wg.Go(func() {
			<-start
			for i := range insertsPerWriter {
				stored, ok, err := s.InsertEvent(ctx, newEvent(fmt.Sprintf("new-%d-%d", w, i)))
				if err != nil || !ok {
					t.Errorf("writer %d insert %d: inserted=%t err=%v", w, i, ok, err)
					return
				}
				mu.Lock()
				inserted = append(inserted, stored.ID)
				mu.Unlock()
			}
		})
	}
	for range purges {
		wg.Go(func() {
			<-start
			res, err := s.Purge(ctx, cutoff)
			if err != nil {
				t.Errorf("purge: %v", err)
				return
			}
			mu.Lock()
			purged = append(purged, res)
			mu.Unlock()
		})
	}
	close(start)
	wg.Wait()
	if t.Failed() {
		return
	}

	var deleted int64
	for _, res := range purged {
		deleted += res.Deleted
		require.Equal(t, old[len(old)-1].ID, res.PurgedThrough, "the mark is the highest purged ID, not the head")
	}
	require.EqualValues(t, len(old), deleted, "the old events are deleted exactly once across the concurrent purges")

	remaining, err := s.EventsAfter(ctx, 0, 1000)
	require.NoError(t, err)
	got := make([]int64, len(remaining))
	for i, ev := range remaining {
		got[i] = ev.ID
	}
	slices.Sort(inserted)
	require.Equal(t, inserted, got, "every event inserted during the purge survives, and nothing else does")
}

// pollEventsNeverHidesAConcurrentPurge checks that a page and the mark come
// from one snapshot. A poller holding an old cursor sees every event above it
// that was not purged. If the mark were read separately from the page, a purge
// between the two reads would delete events from the page and leave the mark
// too low to tell the poller.
func pollEventsNeverHidesAConcurrentPurge(t *testing.T, s faucet.Store) {
	const (
		rounds  = 20
		batch   = 8
		pollers = 4
	)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	var cursor int64
	for round := range rounds {
		events := make([]faucet.Event, batch)
		for i := range events {
			got, _, err := s.InsertEvent(ctx, newEvent(fmt.Sprintf("round-%d-%d", round, i)))
			require.NoError(t, err)
			events[i] = got
		}
		boundary := events[batch-1].ReceivedAt.Add(time.Millisecond)

		start, purged := make(chan struct{}), make(chan struct{})
		var wg sync.WaitGroup
		for range pollers {
			wg.Go(func() {
				<-start
				for done := false; !done; {
					select {
					case <-purged:
						done = true
					default:
					}
					page, err := s.PollEvents(ctx, faucet.PollQuery{RepoID: 42, After: cursor, Limit: 100})
					if err != nil {
						t.Errorf("poll: %v", err)
						return
					}
					seen := make(map[int64]bool, len(page.Events))
					for _, ev := range page.Events {
						seen[ev.ID] = true
					}
					for _, ev := range events {
						if !seen[ev.ID] && ev.ID > page.PurgedThrough {
							t.Errorf("round %d: event %d is neither in the page nor at or below the purge mark %d", round, ev.ID, page.PurgedThrough)
							return
						}
					}
				}
			})
		}
		wg.Go(func() {
			<-start
			defer close(purged)
			if _, err := s.Purge(ctx, boundary); err != nil {
				t.Errorf("purge: %v", err)
			}
		})
		close(start)
		wg.Wait()
		if t.Failed() {
			return
		}
		cursor = events[batch-1].ID
	}
}

var subscriptionBase = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

func newSubscription(principal, callbackURL string, expiresAt time.Time) faucet.Subscription {
	const eventType = "ci.run_completed"
	arguments := json.RawMessage(`{"repo":"octo/repo"}`)
	return faucet.Subscription{
		KeyHash:     faucet.NewSubscriptionKeyHash(principal, callbackURL, eventType, arguments),
		Principal:   principal,
		CallbackURL: callbackURL,
		EventType:   eventType,
		Arguments:   arguments,
		RepoID:      42,
		SecretEnc:   []byte("sealed-1"),
		Cursor:      7,
		ExpiresAt:   expiresAt,
	}
}

func subscriptionAt(n int, expiresAt time.Time) faucet.Subscription {
	return newSubscription("octocat", fmt.Sprintf("https://hooks.example.test/%d", n), expiresAt)
}

func upsertSubscriptionStoresThenRefreshes(t *testing.T, s faucet.Store) {
	ctx := context.Background()
	now := subscriptionBase
	sub := subscriptionAt(1, now.Add(10*time.Minute))

	stored, err := s.UpsertSubscription(ctx, sub, now, 10)
	require.NoError(t, err)
	require.Equal(t, sub.KeyHash, stored.KeyHash)
	require.Equal(t, sub.Principal, stored.Principal)
	require.Equal(t, sub.CallbackURL, stored.CallbackURL)
	require.Equal(t, sub.EventType, stored.EventType)
	require.JSONEq(t, string(sub.Arguments), string(stored.Arguments))
	require.EqualValues(t, 42, stored.RepoID)
	require.Equal(t, []byte("sealed-1"), stored.SecretEnc)
	require.EqualValues(t, 7, stored.Cursor)
	require.True(t, now.Equal(stored.CreatedAt), "created_at is the call's now, got %s", stored.CreatedAt)
	require.True(t, now.Equal(stored.UpdatedAt))
	require.True(t, sub.ExpiresAt.Equal(stored.ExpiresAt))

	later := now.Add(time.Minute)
	refresh := sub
	refresh.RepoID = 43
	refresh.SecretEnc = []byte("sealed-2")
	refresh.Cursor = 99
	refresh.ExpiresAt = later.Add(5 * time.Minute)
	again, err := s.UpsertSubscription(ctx, refresh, later, 10)
	require.NoError(t, err)
	require.EqualValues(t, 43, again.RepoID, "a refresh replaces the repository")
	require.Equal(t, []byte("sealed-2"), again.SecretEnc, "a refresh replaces the sealed secret")
	require.True(t, refresh.ExpiresAt.Equal(again.ExpiresAt), "a refresh sets the new expiry, even a shorter one")
	require.True(t, later.Equal(again.UpdatedAt))
	require.True(t, now.Equal(again.CreatedAt), "a refresh keeps created_at")
	require.EqualValues(t, 7, again.Cursor, "a refresh of a live subscription keeps the cursor and ignores the supplied one")
}

// rotate refreshes sub at now with sealed as its secret and a window of grace.
func rotate(t *testing.T, s faucet.Store, sub faucet.Subscription, sealed string, now time.Time, grace time.Duration) faucet.Subscription {
	t.Helper()
	sub.SecretEnc = []byte(sealed)
	sub.RotationGrace = grace
	sub.ExpiresAt = now.Add(time.Hour)
	row, err := s.UpsertSubscription(context.Background(), sub, now, 10)
	require.NoError(t, err)
	return row
}

func upsertSubscriptionKeepsTheReplacedSecretForTheGraceWindow(t *testing.T, s faucet.Store) {
	sub := subscriptionAt(1, subscriptionBase.Add(time.Hour))
	first := rotate(t, s, sub, "sealed-1", subscriptionBase, 5*time.Minute)
	require.Nil(t, first.PreviousSecretEnc, "a new subscription has no previous secret")
	require.True(t, first.PreviousSecretUntil.IsZero())

	at := subscriptionBase.Add(time.Minute)
	rotated := rotate(t, s, sub, "sealed-2", at, 5*time.Minute)
	require.Equal(t, []byte("sealed-2"), rotated.SecretEnc)
	require.Equal(t, []byte("sealed-1"), rotated.PreviousSecretEnc, "a different secret moves the old one to previous")
	require.True(t, at.Add(5*time.Minute).Equal(rotated.PreviousSecretUntil), "the window ends one grace after the refresh, got %s", rotated.PreviousSecretUntil)
	require.Zero(t, rotated.RotationGrace, "the grace is an input and is never returned")

	same := rotate(t, s, sub, "sealed-2", at.Add(2*time.Minute), 5*time.Minute)
	require.Equal(t, []byte("sealed-1"), same.PreviousSecretEnc, "a refresh with the same secret changes nothing")
	require.True(t, rotated.PreviousSecretUntil.Equal(same.PreviousSecretUntil), "and does not extend the window, got %s", same.PreviousSecretUntil)

	twice := at.Add(3 * time.Minute)
	again := rotate(t, s, sub, "sealed-3", twice, 2*time.Minute)
	require.Equal(t, []byte("sealed-3"), again.SecretEnc)
	require.Equal(t, []byte("sealed-2"), again.PreviousSecretEnc, "a second rotation keeps one previous secret, the one it replaced")
	require.True(t, twice.Add(2*time.Minute).Equal(again.PreviousSecretUntil))
}

func upsertSubscriptionStartsNoWindowWithoutAGrace(t *testing.T, s faucet.Store) {
	sub := subscriptionAt(1, subscriptionBase.Add(time.Hour))
	rotate(t, s, sub, "sealed-1", subscriptionBase, 5*time.Minute)
	rotated := rotate(t, s, sub, "sealed-2", subscriptionBase.Add(time.Minute), 5*time.Minute)
	require.NotNil(t, rotated.PreviousSecretEnc)

	dropped := rotate(t, s, sub, "sealed-3", subscriptionBase.Add(2*time.Minute), 0)
	require.Equal(t, []byte("sealed-3"), dropped.SecretEnc)
	require.Nil(t, dropped.PreviousSecretEnc, "a rotation without a grace drops the previous secret")
	require.True(t, dropped.PreviousSecretUntil.IsZero())
}

func upsertSubscriptionDropsThePreviousSecretOfALapsedRow(t *testing.T, s faucet.Store) {
	sub := subscriptionAt(1, subscriptionBase.Add(10*time.Minute))
	sub.SecretEnc = []byte("sealed-1")
	_, err := s.UpsertSubscription(context.Background(), sub, subscriptionBase, 10)
	require.NoError(t, err)
	sub.RotationGrace = 5 * time.Minute
	sub.SecretEnc, sub.ExpiresAt = []byte("sealed-2"), subscriptionBase.Add(11*time.Minute)
	rotated, err := s.UpsertSubscription(context.Background(), sub, subscriptionBase.Add(time.Minute), 10)
	require.NoError(t, err)
	require.NotNil(t, rotated.PreviousSecretEnc)

	lapsedAt := rotated.ExpiresAt.Add(time.Minute)
	sub.SecretEnc, sub.ExpiresAt = []byte("sealed-3"), lapsedAt.Add(10*time.Minute)
	replaced, err := s.UpsertSubscription(context.Background(), sub, lapsedAt, 10)
	require.NoError(t, err)
	require.Nil(t, replaced.PreviousSecretEnc, "a lapsed row is replaced whole, without its previous secret")
	require.True(t, replaced.PreviousSecretUntil.IsZero())
}

func clearExpiredPreviousSecretsClearsOnlyEndedWindows(t *testing.T, s faucet.Store) {
	ctx := context.Background()
	ended := subscriptionAt(1, subscriptionBase.Add(time.Hour))
	running := subscriptionAt(2, subscriptionBase.Add(time.Hour))
	plain := subscriptionAt(3, subscriptionBase.Add(time.Hour))
	for _, sub := range []faucet.Subscription{ended, running, plain} {
		rotate(t, s, sub, "sealed-1", subscriptionBase, time.Minute)
	}
	rotate(t, s, ended, "sealed-2", subscriptionBase.Add(time.Minute), time.Minute)
	rotate(t, s, running, "sealed-2", subscriptionBase.Add(time.Minute), 5*time.Minute)

	cleared, err := s.ClearExpiredPreviousSecrets(ctx, subscriptionBase.Add(2*time.Minute).Add(-time.Second))
	require.NoError(t, err)
	require.Zero(t, cleared, "a window that has not ended stays")

	cleared, err = s.ClearExpiredPreviousSecrets(ctx, subscriptionBase.Add(2*time.Minute))
	require.NoError(t, err)
	require.EqualValues(t, 1, cleared, "a window ends at its until, and only that row is cleared")

	cleared, err = s.ClearExpiredPreviousSecrets(ctx, subscriptionBase.Add(2*time.Minute))
	require.NoError(t, err)
	require.Zero(t, cleared, "clearing is idempotent")

	same := rotate(t, s, ended, "sealed-2", subscriptionBase.Add(3*time.Minute), time.Minute)
	require.Equal(t, []byte("sealed-2"), same.SecretEnc, "the subscription and its current secret survive the sweep")
	require.Nil(t, same.PreviousSecretEnc)
	require.True(t, same.PreviousSecretUntil.IsZero())
	kept := rotate(t, s, running, "sealed-2", subscriptionBase.Add(3*time.Minute), time.Minute)
	require.Equal(t, []byte("sealed-1"), kept.PreviousSecretEnc, "a window that runs is not cleared")
	require.True(t, subscriptionBase.Add(6*time.Minute).Equal(kept.PreviousSecretUntil))
}

func deleteLiveSubscriptionReturnsThePreviousSecret(t *testing.T, s faucet.Store) {
	sub := subscriptionAt(1, subscriptionBase.Add(time.Hour))
	rotate(t, s, sub, "sealed-1", subscriptionBase, 5*time.Minute)
	at := subscriptionBase.Add(time.Minute)
	rotate(t, s, sub, "sealed-2", at, 5*time.Minute)

	deleted, found, err := s.DeleteLiveSubscription(context.Background(), sub.Principal, sub.KeyHash, at)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, []byte("sealed-2"), deleted.SecretEnc)
	require.Equal(t, []byte("sealed-1"), deleted.PreviousSecretEnc, "the row that ends carries the secrets that sign its last envelope")
	require.True(t, at.Add(5*time.Minute).Equal(deleted.PreviousSecretUntil))
}

func upsertSubscriptionTreatsAnExpiredRowAsAbsent(t *testing.T, s faucet.Store) {
	ctx := context.Background()
	expiresAt := subscriptionBase.Add(10 * time.Minute)
	sub := subscriptionAt(1, expiresAt)
	_, err := s.UpsertSubscription(ctx, sub, subscriptionBase, 10)
	require.NoError(t, err)

	lastLive := expiresAt.Add(-time.Second)
	live := sub
	live.Cursor, live.ExpiresAt = 99, lastLive.Add(time.Minute)
	kept, err := s.UpsertSubscription(ctx, live, lastLive, 10)
	require.NoError(t, err)
	require.EqualValues(t, 7, kept.Cursor, "one second before expiry the row is live")
	require.True(t, subscriptionBase.Equal(kept.CreatedAt))

	lapsedAt := expiresAt.Add(time.Minute)
	lapsed := live
	lapsed.ExpiresAt = lapsedAt.Add(10 * time.Minute)
	replaced, err := s.UpsertSubscription(ctx, lapsed, lapsedAt, 10)
	require.NoError(t, err)
	require.EqualValues(t, 99, replaced.Cursor, "an expired row is replaced whole, so the supplied cursor wins")
	require.True(t, lapsedAt.Equal(replaced.CreatedAt))
}

func deleteSubscriptionRemovesOnlyTheCallersRow(t *testing.T, s faucet.Store) {
	ctx := context.Background()
	mine := subscriptionAt(1, subscriptionBase.Add(time.Hour))
	other := subscriptionAt(2, subscriptionBase.Add(time.Hour))
	for _, sub := range []faucet.Subscription{mine, other} {
		_, err := s.UpsertSubscription(ctx, sub, subscriptionBase, 10)
		require.NoError(t, err)
	}

	deleted, err := s.DeleteSubscription(ctx, "mallory", mine.KeyHash)
	require.NoError(t, err)
	require.False(t, deleted, "another principal's key deletes nothing")

	deleted, err = s.DeleteSubscription(ctx, "octocat", faucet.NewSubscriptionKeyHash("octocat", "https://hooks.example.test/unknown", mine.EventType, mine.Arguments))
	require.NoError(t, err)
	require.False(t, deleted, "an unknown key deletes nothing")

	deleted, err = s.DeleteSubscription(ctx, "octocat", mine.KeyHash)
	require.NoError(t, err)
	require.True(t, deleted, "the owner's delete finds the row that the others left alone")
	deleted, err = s.DeleteSubscription(ctx, "octocat", mine.KeyHash)
	require.NoError(t, err)
	require.False(t, deleted, "the row is gone")
	deleted, err = s.DeleteSubscription(ctx, "octocat", other.KeyHash)
	require.NoError(t, err)
	require.True(t, deleted, "deleting one subscription leaves the principal's others")

	gone := subscriptionAt(3, subscriptionBase.Add(time.Minute))
	_, err = s.UpsertSubscription(ctx, gone, subscriptionBase, 10)
	require.NoError(t, err)
	deleted, err = s.DeleteSubscription(ctx, "octocat", gone.KeyHash)
	require.NoError(t, err)
	require.True(t, deleted, "an expired row is deleted too")
}

func deleteLiveSubscriptionGivesTheRowToExactlyOneCaller(t *testing.T, s faucet.Store) {
	ctx := context.Background()
	live := subscriptionAt(1, subscriptionBase.Add(time.Hour))
	lapsing := subscriptionAt(2, subscriptionBase.Add(time.Minute))
	for _, sub := range []faucet.Subscription{live, lapsing} {
		_, err := s.UpsertSubscription(ctx, sub, subscriptionBase, 10)
		require.NoError(t, err)
	}

	_, found, err := s.DeleteLiveSubscription(ctx, "mallory", live.KeyHash, subscriptionBase)
	require.NoError(t, err)
	require.False(t, found, "another principal's key deletes nothing")
	unknown := faucet.NewSubscriptionKeyHash("octocat", "https://hooks.example.test/unknown", live.EventType, live.Arguments)
	_, found, err = s.DeleteLiveSubscription(ctx, "octocat", unknown, subscriptionBase)
	require.NoError(t, err)
	require.False(t, found, "an unknown key deletes nothing")
	_, found, err = s.DeleteLiveSubscription(ctx, "octocat", lapsing.KeyHash, subscriptionBase.Add(2*time.Minute))
	require.NoError(t, err)
	require.False(t, found, "a lapsed row counts as absent")
	lapsedStillThere, err := s.DeleteSubscription(ctx, "octocat", lapsing.KeyHash)
	require.NoError(t, err)
	require.True(t, lapsedStillThere, "and is left for the sweep")

	var (
		wg      sync.WaitGroup
		results [8]struct {
			row   faucet.Subscription
			found bool
			err   error
		}
	)
	for i := range results {
		wg.Go(func() {
			results[i].row, results[i].found, results[i].err = s.DeleteLiveSubscription(ctx, "octocat", live.KeyHash, subscriptionBase)
		})
	}
	wg.Wait()
	winners := 0
	for _, res := range results {
		require.NoError(t, res.err)
		if res.found {
			winners++
			require.Equal(t, live.KeyHash, res.row.KeyHash)
			require.Equal(t, live.CallbackURL, res.row.CallbackURL)
			require.Equal(t, live.SecretEnc, res.row.SecretEnc, "the winner gets the sealed secret to sign with")
		}
	}
	require.Equal(t, 1, winners, "of concurrent callers, one removes the row and gets it")
	gone, err := s.DeleteSubscription(ctx, "octocat", live.KeyHash)
	require.NoError(t, err)
	require.False(t, gone)
}

func upsertSubscriptionEnforcesThePrincipalLimit(t *testing.T, s faucet.Store) {
	ctx := context.Background()
	const limit = 3
	soon := subscriptionBase.Add(time.Minute)
	later := subscriptionBase.Add(time.Hour)

	subs := []faucet.Subscription{subscriptionAt(0, soon), subscriptionAt(1, later), subscriptionAt(2, later)}
	for _, sub := range subs {
		_, err := s.UpsertSubscription(ctx, sub, subscriptionBase, limit)
		require.NoError(t, err)
	}

	refused := subscriptionAt(3, later)
	_, err := s.UpsertSubscription(ctx, refused, subscriptionBase, limit)
	require.ErrorIs(t, err, faucet.ErrSubscriptionLimit)
	deleted, err := s.DeleteSubscription(ctx, "octocat", refused.KeyHash)
	require.NoError(t, err)
	require.False(t, deleted, "a refused subscription writes nothing")

	_, err = s.UpsertSubscription(ctx, subs[1], subscriptionBase, limit)
	require.NoError(t, err, "refreshing a subscription at the limit is not a new one")

	_, err = s.UpsertSubscription(ctx, newSubscription("hubot", "https://hooks.example.test/0", later), subscriptionBase, limit)
	require.NoError(t, err, "the limit is per principal")

	afterExpiry := subscriptionBase.Add(2 * time.Minute)
	refused.ExpiresAt = later
	_, err = s.UpsertSubscription(ctx, refused, afterExpiry, limit)
	require.NoError(t, err, "an expired subscription does not count against the limit")
}

// upsertSubscriptionLimitHoldsUnderConcurrency checks the limit against
// callers that race. A count taken before the insert, without a lock per
// principal, lets several of them pass.
func upsertSubscriptionLimitHoldsUnderConcurrency(t *testing.T, s faucet.Store) {
	const (
		limit   = 5
		callers = 4 * limit
	)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		accepted  int
		exhausted int
	)
	start := make(chan struct{})
	for n := range callers {
		wg.Go(func() {
			<-start
			_, err := s.UpsertSubscription(ctx, subscriptionAt(n, subscriptionBase.Add(time.Hour)), subscriptionBase, limit)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				accepted++
			case errors.Is(err, faucet.ErrSubscriptionLimit):
				exhausted++
			default:
				t.Errorf("caller %d: %v", n, err)
			}
		})
	}
	close(start)
	wg.Wait()
	require.Equal(t, limit, accepted)
	require.Equal(t, callers-limit, exhausted)
}

func upsertSubscriptionValidatesInput(t *testing.T, s faucet.Store) {
	ctx := context.Background()
	good := subscriptionAt(1, subscriptionBase.Add(time.Hour))

	bad := map[string]func(sub *faucet.Subscription){
		"empty principal":      func(sub *faucet.Subscription) { sub.Principal = "" },
		"empty callback URL":   func(sub *faucet.Subscription) { sub.CallbackURL = "" },
		"empty event type":     func(sub *faucet.Subscription) { sub.EventType = "" },
		"arguments not object": func(sub *faucet.Subscription) { sub.Arguments = json.RawMessage(`[1]`) },
		"arguments not JSON":   func(sub *faucet.Subscription) { sub.Arguments = json.RawMessage(`{"repo":`) },
		"zero repo ID":         func(sub *faucet.Subscription) { sub.RepoID = 0 },
		"no sealed secret":     func(sub *faucet.Subscription) { sub.SecretEnc = nil },
		"negative cursor":      func(sub *faucet.Subscription) { sub.Cursor = -1 },
		"already expired":      func(sub *faucet.Subscription) { sub.ExpiresAt = subscriptionBase },
		"zero key hash":        func(sub *faucet.Subscription) { sub.KeyHash = faucet.SubscriptionKeyHash{} },
		"negative grace":       func(sub *faucet.Subscription) { sub.RotationGrace = -time.Second },
		"grace past the expiry": func(sub *faucet.Subscription) {
			sub.RotationGrace = time.Hour + time.Second
		},
		"another principal's key hash": func(sub *faucet.Subscription) {
			sub.KeyHash = faucet.NewSubscriptionKeyHash("mallory", sub.CallbackURL, sub.EventType, sub.Arguments)
		},
	}
	for name, mutate := range bad {
		sub := good
		mutate(&sub)
		_, err := s.UpsertSubscription(ctx, sub, subscriptionBase, 10)
		require.Error(t, err, name)
		deleted, err := s.DeleteSubscription(ctx, good.Principal, good.KeyHash)
		require.NoError(t, err)
		require.False(t, deleted, "%s writes nothing", name)
	}

	_, err := s.UpsertSubscription(ctx, good, subscriptionBase, 0)
	require.Error(t, err, "a limit of zero")
}

func deleteExpiredSubscriptionsDeletesOnlyExpiredRows(t *testing.T, s faucet.Store) {
	ctx := context.Background()
	soon := subscriptionAt(1, subscriptionBase.Add(time.Minute))
	later := subscriptionAt(2, subscriptionBase.Add(time.Hour))
	for _, sub := range []faucet.Subscription{soon, later} {
		_, err := s.UpsertSubscription(ctx, sub, subscriptionBase, 10)
		require.NoError(t, err)
	}

	deleted, err := s.DeleteExpiredSubscriptions(ctx, subscriptionBase.Add(time.Minute-time.Second))
	require.NoError(t, err)
	require.Zero(t, deleted, "nothing has expired yet")

	deleted, err = s.DeleteExpiredSubscriptions(ctx, subscriptionBase.Add(time.Minute))
	require.NoError(t, err)
	require.EqualValues(t, 1, deleted, "a subscription expires at its expiry")

	found, err := s.DeleteSubscription(ctx, "octocat", soon.KeyHash)
	require.NoError(t, err)
	require.False(t, found)
	found, err = s.DeleteSubscription(ctx, "octocat", later.KeyHash)
	require.NoError(t, err)
	require.True(t, found, "the live subscription survived")
}
