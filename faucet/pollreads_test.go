package faucet_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/stretchr/testify/require"

	"github.com/coder/superblink-events/faucet"
)

const (
	pollLimit  = 5
	readWindow = 50_000
)

// requireBoundedReads checks that every read of events looked at no more than
// one window of positions of the target's line.
func requireBoundedReads(t *testing.T, queries []faucet.PollQuery) {
	t.Helper()
	require.NotEmpty(t, queries)
	for _, q := range queries {
		require.Positive(t, q.Through, "a read from %d has no upper bound", q.After)
		require.LessOrEqual(t, q.Through-q.After, int64(readWindow), "a read from %d looks at too many positions", q.After)
	}
}

// A poll from an old cursor with a filter that matches nothing must not be one
// statement over the whole history. Each poll reads a window of the target's
// positions and says hasMore while it stops short of the head, and the cursor
// carries the read on to the event.
func TestAPollReadsTheHistoryAWindowAtATime(t *testing.T) {
	t.Parallel()

	h := newEventsHarness(t)
	h.store.appendRunEvents(120_000)
	want := h.insert(repoID, "ci.run_completed", testShaA)

	params := pollParams("ci.run_completed", repoArgs("sha", testShaA), cursorOf("v2.0"))
	var got []string
	for range 10 {
		page := h.poll(params)
		got = append(got, page.eventIDs()...)
		if len(got) > 0 {
			break
		}
		require.True(t, page.HasMore, "an empty page short of the head says hasMore")
		params.Cursor = cursorOf(page.Cursor)
	}
	require.Equal(t, []string{want.DeliveryID}, got)
	requireBoundedReads(t, h.store.pollQueries())
}

// A wait reads the same windows, one after another without sleeping, so it
// returns the event at once.
func TestAWaitReadsTheHistoryAWindowAtATime(t *testing.T) {
	t.Parallel()

	h := newWaitHarness(t)
	h.store.appendRunEvents(120_000)
	want := h.insert(repoID, "ci.run_completed", testShaA)

	res := h.call(repoArgs("types", []string{"ci.run_completed"}, "sha", testShaA, "cursor", "v2.0", "timeout_s", 1)).success(t)
	require.Equal(t, positionsOf(want), res.ids())
	requireBoundedReads(t, h.store.pollQueries())
}

// heldReads holds every read of events that filters by sha until release
// closes, and counts the reads it holds.
type heldReads struct {
	faucet.Store
	held    atomic.Int32
	release chan struct{}
}

func (s *heldReads) PollEvents(ctx context.Context, q faucet.PollQuery) (faucet.PollPage, error) {
	if q.HeadSHA != "" {
		s.held.Add(1)
		select {
		case <-s.release:
		case <-ctx.Done():
			return faucet.PollPage{}, ctx.Err()
		}
	}
	return s.Store.PollEvents(ctx, q)
}

func requireRefusedPoll(t *testing.T, rpcErr *jsonrpc.Error) {
	t.Helper()
	require.NotNil(t, rpcErr)
	require.EqualValues(t, codeResourceExhausted, rpcErr.Code, rpcErr.Message)
	require.Contains(t, rpcErr.Message, "too many polls")
	require.JSONEq(t, `{"limit":"polls","max":5}`, string(rpcErr.Data), "the error names the limit, as SEP-3415 asks")
}

// One login can run a few polls at once, so it cannot hold every connection
// of the store. The poll over the limit fails at once, and another login
// still polls.
func TestOneLoginCanRunOnlyAFewPollsAtOnce(t *testing.T) {
	t.Parallel()

	store := &heldReads{release: make(chan struct{})}
	h := newWebhookHarness(t, func(d *faucet.Deps) { store.Store = d.Store; d.Store = store })
	t.Cleanup(func() { close(store.release) })
	// The SDK writes to the params of a call, so each call gets its own.
	slow := func() *rawParams { return pollParams("ci.run_completed", repoArgs("sha", testShaA), cursorOf("v2.0")) }

	done := make(chan error, pollLimit)
	for range pollLimit {
		go func() {
			_, err := h.call(methodPoll, slow())
			done <- err
		}()
	}
	require.Eventually(t, func() bool { return store.held.Load() == pollLimit }, 5*time.Second, 10*time.Millisecond)

	requireRefusedPoll(t, h.pollError(slow()))
	h.as("hubot")
	h.poll(pollParams("ci.run_completed", repoArgs(), cursorOf("v2.0")))

	store.release <- struct{}{}
	require.NoError(t, <-done, "a poll that ends gives its place back")
	h.as("octocat")
	go func() {
		_, err := h.call(methodPoll, slow())
		done <- err
	}()
	require.Eventually(t, func() bool { return store.held.Load() == pollLimit+1 }, 5*time.Second, 10*time.Millisecond)
}

// heldLeases holds every lease that a raw poll opens until release closes.
type heldLeases struct {
	faucet.RawStore
	held    atomic.Int32
	release chan struct{}
}

func (s *heldLeases) OpenRawLease(ctx context.Context, lease faucet.RawLease, now time.Time, limits faucet.RawLimits) (int64, error) {
	s.held.Add(1)
	select {
	case <-s.release:
	case <-ctx.Done():
		return 0, ctx.Err()
	}
	return s.RawStore.OpenRawLease(ctx, lease, now, limits)
}

func TestThePollLimitCountsRawPolls(t *testing.T) {
	t.Parallel()

	raws := &heldLeases{release: make(chan struct{})}
	h := newRawPollHarness(t, func(d *faucet.Deps) { raws.RawStore = d.RawStore; d.RawStore = raws })
	t.Cleanup(func() { close(raws.release) })

	for range pollLimit {
		go func() { _, _ = h.call(methodPoll, pollParams("github.push", repoArgs(), nil)) }()
	}
	require.Eventually(t, func() bool { return raws.held.Load() == pollLimit }, 5*time.Second, 10*time.Millisecond)
	requireRefusedPoll(t, h.pollError(pollParams("github.push", repoArgs(), nil)))
}
