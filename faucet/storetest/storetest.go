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

func difference(a, b []int64) []int64 {
	var out []int64
	for _, id := range a {
		if !slices.Contains(b, id) {
			out = append(out, id)
		}
	}
	return out
}
