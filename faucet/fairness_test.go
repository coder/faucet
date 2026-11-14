package faucet_test

import (
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// stall makes the endpoints at paths of rcv hold every request until the
// returned function runs, and tracks how many are held at once.
func stall(t *testing.T, rcv *hookReceiver, paths ...string) (release func(), peak *atomic.Int32) {
	t.Helper()
	gate := make(chan struct{})
	release = sync.OnceFunc(func() { close(gate) })
	t.Cleanup(release)
	var held atomic.Int32
	peak = &atomic.Int32{}
	hold := func() int {
		now := held.Add(1)
		defer held.Add(-1)
		for seen := peak.Load(); now > seen && !peak.CompareAndSwap(seen, now); seen = peak.Load() {
		}
		<-gate
		return http.StatusOK
	}
	for _, path := range paths {
		rcv.answer(path, func(int) int { return hold() })
		rcv.answerEnvelope(path, "gap", hold)
	}
	return release, peak
}

// awaitOneRequest waits for the healthy endpoint to get a request while the
// stalled ones still hold theirs.
func awaitOneRequest(t *testing.T, rcv *hookReceiver, path string, why string) {
	t.Helper()
	require.Eventually(t, func() bool { return len(rcv.requests(path)) == 1 }, 5*time.Second, 10*time.Millisecond, why)
}

func TestDeliverDueOneStallingEndpointDoesNotDelayAnotherSubscriptionsDeliveries(t *testing.T) {
	t.Parallel()

	d := newDeliverHarness(t)
	slow := d.endpoint("/slow")
	d.subscribeEndpoint(slow)
	release, peak := stall(t, d.rcv, slow.path)
	healthyRcv := newHookReceiver(t)
	d.subscribe(subscribeTo(args("repo", otherRepo), healthyRcv.url("/ok"), newSecret(t, 32)))
	d.advance(time.Second)
	const queued = 12
	for range queued {
		d.newRunEvent()
	}
	d.insert(otherRepoID, "ci.run_completed", "")

	wait := d.runDeliverDue()
	awaitOneRequest(t, healthyRcv, "/ok", "while the slow endpoint holds its requests, another subscription's event is sent")
	require.LessOrEqual(t, peak.Load(), int32(2), "the slow endpoint holds at most 2 of the run's workers")

	release()
	res := wait()
	d.record(res)
	require.Equal(t, queued+1, res.Delivered)
}

func TestDeliverDueStallingGapEndpointsDoNotStarveAHealthyGap(t *testing.T) {
	t.Parallel()

	d := newDeliverHarness(t)
	d.ledgerSkip = true
	const stalled = 10
	paths := make([]string, stalled)
	for i := range stalled {
		paths[i] = "/slow" + string(rune('a'+i))
		d.subscribeEndpoint(d.endpoint(paths[i]))
	}
	release, peak := stall(t, d.rcv, paths...)
	d.advance(time.Second)
	d.newRunEvent()
	d.newRunEvent()
	healthyRcv := newHookReceiver(t)
	d.subscribe(subscribeTo(repoArgs(), healthyRcv.url("/ok"), newSecret(t, 32)))
	d.advance(time.Second)
	d.newRunEvent()
	d.advance(5 * time.Minute)
	d.newRunEvent()
	res, err := d.store.Purge(d.ctx, d.clock.Now().Add(-time.Minute))
	require.NoError(t, err)
	require.EqualValues(t, 3, res.Deleted)

	wait := d.runDeliverDue()
	awaitOneRequest(t, healthyRcv, "/ok", "while the slow gap endpoints hold their requests, a healthy gap is sent")
	require.LessOrEqual(t, peak.Load(), int32(2), "the slow endpoint holds at most 2 of the run's workers")
	release()
	got := wait()
	d.record(got)
	require.Equal(t, stalled+1, got.GapsSent)
}
