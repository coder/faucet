package faucet_test

import (
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/coder/faucet/faucet"
)

type stalledEndpoints struct {
	release func()
	held    atomic.Int32
	peak    atomic.Int32
}

func stall(t *testing.T, rcv *hookReceiver, paths ...string) *stalledEndpoints {
	t.Helper()
	gate := make(chan struct{})
	s := &stalledEndpoints{release: sync.OnceFunc(func() { close(gate) })}
	t.Cleanup(s.release)
	hold := func() int {
		now := s.held.Add(1)
		defer s.held.Add(-1)
		for seen := s.peak.Load(); now > seen; seen = s.peak.Load() {
			if s.peak.CompareAndSwap(seen, now) {
				break
			}
		}
		<-gate
		return http.StatusOK
	}
	for _, path := range paths {
		rcv.answer(path, func(int) int { return hold() })
		rcv.answerEnvelope(path, "gap", hold)
	}
	return s
}

func (s *stalledEndpoints) requireHeldAtMost(t *testing.T, n int32) {
	t.Helper()
	require.Eventually(t, func() bool { return s.held.Load() >= n }, 5*time.Second, 10*time.Millisecond, "the stalled endpoints hold %d requests", n)
	time.Sleep(200 * time.Millisecond)
	require.LessOrEqual(t, s.peak.Load(), n, "a stalled endpoint holds at most %d of the run's workers", n)
}

func TestDeliverDueOneStallingEndpointDoesNotDelayAnotherSubscriptionsDeliveries(t *testing.T) {
	t.Parallel()

	d := newDeliverHarness(t)
	slow := d.endpoint("/slow")
	d.subscribeEndpoint(slow)
	stalled := stall(t, d.rcv, slow.path)
	healthyRcv := newHookReceiver(t)
	d.subscribe(subscribeTo(args("repo", otherRepo), healthyRcv.url("/ok"), newSecret(t, 32)))
	d.advance(time.Second)
	const queued = 12
	for range queued {
		d.newRunEvent()
	}
	d.insert(otherRepoID, "ci.run_completed", "")

	wait := d.runDeliverDue()
	require.Eventually(t, func() bool { return len(healthyRcv.requests("/ok")) == 1 }, 5*time.Second, 10*time.Millisecond,
		"while the slow endpoint holds its requests, another subscription's event is sent")
	stalled.requireHeldAtMost(t, 2)

	stalled.release()
	res := wait()
	d.record(res)
	require.Equal(t, queued+1, res.Delivered)
}

func TestDeliverDueStallingGapEndpointsDoNotStarveAHealthyGap(t *testing.T) {
	t.Parallel()

	d := newDeliverHarness(t)
	d.ledgerSkip = true
	const stalling = 10
	paths := make([]string, stalling)
	for i := range stalling {
		paths[i] = "/slow" + string(rune('a'+i))
		d.subscribeEndpoint(d.endpoint(paths[i]))
	}
	held := stall(t, d.rcv, paths...)
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
	require.Eventually(t, func() bool { return len(healthyRcv.envelopesOf("/ok", "gap")) == 1 }, 5*time.Second, 10*time.Millisecond,
		"while the slow gap endpoints hold their requests, a healthy gap is sent")
	held.requireHeldAtMost(t, 2)
	held.release()
	got := wait()
	d.record(got)
	require.Equal(t, stalling+1, got.GapsSent)
}

// A host whose subscribers all call back to one origin sends with 2 of the 8
// workers by default, and raises that with MaxWorkersPerOrigin.
func TestDeliverDueSendsToOneOriginWithAsManyWorkersAsMaxWorkersPerOriginAllows(t *testing.T) {
	t.Parallel()

	const workers = 8
	for name, c := range map[string]struct{ setting, want int }{
		"Default": {0, 2},
		"Five":    {5, 5},
		"NoCap":   {-1, workers},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			d := newDeliverHarness(t, func(deps *faucet.Deps) { deps.Webhook.MaxWorkersPerOrigin = c.setting })
			slow := d.endpoint("/slow")
			d.subscribeEndpoint(slow)
			stalled := stall(t, d.rcv, slow.path)
			d.advance(time.Second)
			const queued = 2 * workers
			for range queued {
				d.newRunEvent()
			}

			wait := d.runDeliverDue()
			stalled.requireHeldAtMost(t, int32(c.want))
			stalled.release()
			res := wait()
			d.record(res)
			require.Equal(t, queued, res.Delivered)
		})
	}
}
