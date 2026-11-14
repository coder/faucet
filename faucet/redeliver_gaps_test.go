package faucet_test

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/coder/faucet/faucet"
)

type gapWindow struct{ from, to time.Time }

func (w gapWindow) holds(at time.Time) bool { return !at.Before(w.from) && !at.After(w.to) }

// loggedGaps are the time ranges of the gaps that the runs logged.
func (e *redeliveryEnv) loggedGaps() []gapWindow {
	var gaps []gapWindow
	for _, line := range e.logged("redelivery gap") {
		gaps = append(gaps, gapWindow{logTime(e.t, line["from"]), logTime(e.t, line["to"])})
	}
	return gaps
}

func (e *redeliveryEnv) tracked(guid string) bool {
	e.store.rmu.Lock()
	defer e.store.rmu.Unlock()
	_, ok := e.store.rows[guid]
	return ok
}

// requireNoSilentLoss fails for a failed attempt that no run tracked and that
// no logged gap covers.
func requireNoSilentLoss(t *testing.T, env *redeliveryEnv, failures []ghAttempt) {
	t.Helper()
	gaps := env.loggedGaps()
	require.NotEmpty(t, gaps, "the scenario must end in a gap, or it proves nothing")
	for _, f := range failures {
		if env.tracked(f.guid) || env.stored(f.guid) {
			continue
		}
		covered := false
		for _, w := range gaps {
			covered = covered || w.holds(f.at)
		}
		require.True(t, covered, "%s failed at %s and has no final state: no run tracked it and no gap covers it", f.guid, f.at.Format(time.TimeOnly))
	}
}

func TestAGapReachesGitHubsRetentionWhateverFaucetsClockSays(t *testing.T) {
	t.Parallel()

	// skew is GitHub's clock minus faucet's. The failures sit 50 minutes apart,
	// so that a skew of 3 hours spans several of them.
	failEvery := func(env *redeliveryEnv, every, span time.Duration) []ghAttempt {
		var failures []ghAttempt
		for at := env.gh.now().Add(-span + every); !at.After(env.gh.now()); at = at.Add(every) {
			failures = append(failures, env.gh.record(fmt.Sprintf("f-%05d", len(failures)), at, 503))
		}
		return failures
	}

	t.Run("the first run, with faucet's clock ahead", func(t *testing.T) {
		t.Parallel()
		env := newRedeliveryEnv(t)
		env.gh.skew = -3 * time.Hour
		failures := failEvery(env, 50*time.Minute, faucet.GitHubDeliveryRetention)

		env.run()

		requireNoSilentLoss(t, env, failures)
	})

	for name, tc := range map[string]struct {
		every time.Duration
		pages int
	}{
		"a list that fits on one page":   {every: 50 * time.Minute},
		"a list that takes several runs": {every: 2 * time.Minute, pages: 20},
	} {
		t.Run("a cron that was down for 75 hours, with faucet's clock behind, and "+name, func(t *testing.T) {
			t.Parallel()
			env := newRedeliveryEnv(t, func(o *envOptions) { o.maxPages = tc.pages })
			env.gh.skew = 3 * time.Hour
			env.run()
			env.advance(75 * time.Hour)
			failures := failEvery(env, tc.every, 75*time.Hour)

			for range 4 {
				env.run()
				env.advance(cronEvery)
			}

			requireNoSilentLoss(t, env, failures)
		})
	}
}

func TestAQuietHookDrainsTheRangesThatABurstLeftOpen(t *testing.T) {
	t.Parallel()

	// A burst of 5 attempts a second for 500 seconds holds 25 pages, and a run
	// reads 9. The failures are in its oldest part. After the burst the hook
	// is silent: every run reads the same newest attempts, and the part of the
	// burst that no run has read must not wait behind them.
	env := newRedeliveryEnv(t, func(o *envOptions) { o.maxPages = 9 })
	env.run()
	env.advance(time.Minute)
	start := env.gh.now()
	var failed []string
	for i := range 2500 {
		guid := fmt.Sprintf("a-%04d", i)
		status := http200
		if i < 1000 && i%50 == 7 {
			status = 503
			failed = append(failed, guid)
		}
		env.gh.record(guid, start.Add(time.Duration(i)*200*time.Millisecond), status)
	}
	env.advance(cronEvery)

	var ranges []int
	for range 6 {
		ranges = append(ranges, env.run().Ranges)
		env.advance(cronEvery)
	}

	require.True(t, allStored(env, failed), "every failure of the burst is asked for within a few runs of silence")
	require.Equal(t, 0, ranges[len(ranges)-1], "a quiet hook reads all that is open")
	require.LessOrEqual(t, slices.Max(ranges), 1, "a silent run opens no range of what an earlier run read: %v", ranges)
}
