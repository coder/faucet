package faucet_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/xerrors"

	"github.com/coder/faucet/faucet"
	"github.com/coder/quartz"
)

func newPurgeService(t *testing.T) (*faucet.Service, *fakeStore, *quartz.Mock) {
	t.Helper()
	clock := quartz.NewMock(t)
	store := &fakeStore{now: func() time.Time { return clock.Now() }}
	deps := testDeps(store)
	deps.Clock = clock
	svc, err := faucet.New(deps)
	require.NoError(t, err)
	return svc, store, clock
}

func TestPurgeDeletesEventsReceivedBeforeTheGivenTime(t *testing.T) {
	t.Parallel()

	svc, store, clock := newPurgeService(t)
	ctx := context.Background()
	var kept []faucet.Event
	for i, age := range []time.Duration{40, 35, 10, 1} {
		clock.Set(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Add(-age * 24 * time.Hour))
		ev, _, err := store.InsertEvent(ctx, faucet.Event{
			Source: "github", DeliveryID: string(rune('a' + i)), Type: "pr.closed", RepoID: repoID,
			Data: []byte(`{"merged":true}`),
		})
		require.NoError(t, err)
		kept = append(kept, ev)
	}
	clock.Set(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))

	res, err := svc.Purge(ctx, clock.Now().Add(-30*24*time.Hour))
	require.NoError(t, err)
	require.Equal(t, faucet.PurgeResult{Deleted: 2, PurgedThrough: kept[1].ID}, res)
	require.Equal(t, []faucet.Event{kept[2], kept[3]}, store.stored())
}

func TestPurgeRefusesATimeThatWouldDeleteTooMuch(t *testing.T) {
	t.Parallel()

	svc, store, clock := newPurgeService(t)
	ctx := context.Background()
	kept, _, err := store.InsertEvent(ctx, faucet.Event{
		Source: "github", DeliveryID: "d1", Type: "pr.closed", RepoID: repoID, Data: []byte(`{"merged":true}`),
	})
	require.NoError(t, err)
	clock.Advance(time.Hour)

	for name, before := range map[string]time.Time{
		"the zero time":        {},
		"a time in the future": clock.Now().Add(time.Second),
	} {
		_, err := svc.Purge(ctx, before)
		require.Error(t, err, name)
		require.Equal(t, []faucet.Event{kept}, store.stored(), "%s deletes nothing", name)
	}

	res, err := svc.Purge(ctx, clock.Now())
	require.NoError(t, err, "now is allowed")
	require.EqualValues(t, 1, res.Deleted)
}

func TestPurgeReturnsTheStoreError(t *testing.T) {
	t.Parallel()

	svc, store, clock := newPurgeService(t)
	store.err = xerrors.New("database is down")

	_, err := svc.Purge(context.Background(), clock.Now())
	require.ErrorIs(t, err, store.err)
}
