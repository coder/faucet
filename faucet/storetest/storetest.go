// Package storetest is a contract test suite for faucet.Store implementations.
package storetest

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/coder/faucet/faucet"
)

// Run runs the contract against stores returned by newStore. Every call to
// newStore must return an empty store that does not share state with the
// others, because the subtests run in parallel.
func Run(t *testing.T, newStore func(t *testing.T) faucet.Store) {
	subtests := []struct {
		name string
		run  func(t *testing.T, s faucet.Store)
	}{
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
	}
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
