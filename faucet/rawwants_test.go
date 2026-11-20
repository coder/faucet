package faucet_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/coder/superblink-events/faucet"
)

// rawWantsDown is a RawStore that cannot say what interests want.
type rawWantsDown struct{ faucet.RawStore }

func (rawWantsDown) RawWants(context.Context, time.Time) ([]faucet.RawWant, error) {
	return nil, errRawStoreDown
}

func TestRawWantsSaysWhatLiveInterestsWant(t *testing.T) {
	t.Parallel()

	h := newIngestHarness(t)
	h.openLease(ingestInterest{event: "push"})
	h.openLease(ingestInterest{event: "issues", actions: []string{"closed", "opened"}})
	h.subscribe(ingestInterest{event: "release", actions: []string{"published"}})
	h.subscribe(ingestInterest{event: "issues", actions: []string{"labeled"}})
	h.subscribe(ingestInterest{event: "star", repoID: otherRepoID})

	wants, err := h.svc.RawWants(h.ctx)
	require.NoError(t, err)

	for _, tc := range []struct {
		name          string
		event, action string
		repoID        int64
		want          bool
	}{
		{name: "a lease without actions wants a delivery without one", event: "push", repoID: repoID, want: true},
		{name: "a lease without actions wants every action", event: "push", action: "anything", repoID: repoID, want: true},
		{name: "an action that a lease lists", event: "issues", action: "opened", repoID: repoID, want: true},
		{name: "an action that a subscription of the same event lists", event: "issues", action: "labeled", repoID: repoID, want: true},
		{name: "an action that a subscription lists", event: "release", action: "published", repoID: repoID, want: true},
		{name: "a subscription of another repository", event: "star", repoID: otherRepoID, want: true},
		{name: "an action that no interest lists", event: "issues", action: "deleted", repoID: repoID},
		{name: "a delivery without an action for interests that list some", event: "issues", repoID: repoID},
		{name: "an event that no interest names", event: "fork", repoID: repoID},
		{name: "another repository", event: "push", repoID: otherRepoID},
		{name: "a delivery without a repository", event: "push"},
	} {
		require.Equal(t, tc.want, wants(tc.event, tc.action, tc.repoID), tc.name)
	}
}

func TestRawWantsIsASnapshotOfTheLiveInterests(t *testing.T) {
	t.Parallel()

	h := newIngestHarness(t)
	h.openLease(ingestInterest{event: "push"})
	h.subscribe(ingestInterest{event: "release"})
	live, err := h.svc.RawWants(h.ctx)
	require.NoError(t, err)

	h.clock.Advance(time.Hour)
	expired, err := h.svc.RawWants(h.ctx)
	require.NoError(t, err)

	for _, event := range []string{"push", "release"} {
		require.True(t, live(event, "", repoID), "%s: the lookup keeps what it read", event)
		require.False(t, expired(event, "", repoID), "%s: an expired interest wants nothing", event)
	}
}

func TestRawWantsWithoutARawStoreWantsNothing(t *testing.T) {
	t.Parallel()

	h := newIngestHarness(t, withoutRawStore)
	h.openLease(ingestInterest{event: "push"})

	wants, err := h.svc.RawWants(h.ctx)
	require.NoError(t, err)
	require.False(t, wants("push", "", repoID))
}

func TestRawWantsReturnsTheStoreError(t *testing.T) {
	t.Parallel()

	h := newIngestHarness(t, func(d *faucet.Deps) { d.RawStore = rawWantsDown{d.RawStore} })

	wants, err := h.svc.RawWants(h.ctx)
	require.ErrorIs(t, err, errRawStoreDown)
	require.True(t, wants("push", "", otherRepoID), "without the interests, every event that the service offers raw is wanted")
	require.True(t, wants("issues", "closed", repoID))
	require.False(t, wants("pull_request", "opened", repoID), "a typed event is no raw event")

	only := newIngestHarness(t, func(d *faucet.Deps) { d.RawStore, d.RawEvents = rawWantsDown{d.RawStore}, []string{"push"} })
	wants, err = only.svc.RawWants(only.ctx)
	require.ErrorIs(t, err, errRawStoreDown)
	require.True(t, wants("push", "", repoID))
	require.False(t, wants("issues", "closed", repoID), "an event that the service does not offer is not wanted")
}
