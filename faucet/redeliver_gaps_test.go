package faucet_test

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"cdr.dev/slog/v3"
	"github.com/coder/faucet/faucet"
	"github.com/coder/quartz"
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

// failEvery gives GitHub a failed attempt every interval of the last span, at
// GitHub's times.
func failEvery(env *redeliveryEnv, every, span time.Duration) []ghAttempt {
	var failures []ghAttempt
	for at := env.gh.now().Add(-span + every); !at.After(env.gh.now()); at = at.Add(every) {
		failures = append(failures, env.gh.record(fmt.Sprintf("f-%05d", len(failures)), at, 503))
	}
	return failures
}

func TestAGapReachesGitHubsRetentionWhateverFaucetsClockSays(t *testing.T) {
	t.Parallel()

	// skew is GitHub's clock minus faucet's. The failures sit 50 minutes apart,
	// so that a skew of 3 hours spans several of them.
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
	// is silent: every run reads the overlap below the same newest attempt
	// again, and that must neither open a range per run nor keep the pages from
	// the range that holds the burst.
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
	for range 8 {
		ranges = append(ranges, env.run().Ranges)
		env.advance(cronEvery)
	}

	require.True(t, allStored(env, failed), "every failure of the burst is asked for within a few runs of silence: %v", ranges)
	require.Equal(t, 0, ranges[len(ranges)-1], "a quiet hook reads all that is open: %v", ranges)
	require.LessOrEqual(t, slices.Max(ranges), 2, "a silent run opens no range for an overlap that a range already reads: %v", ranges)
}

// dateAdmin gives the pages of its HookAdmin another Date header: none, or one
// that is shifted.
type dateAdmin struct {
	faucet.HookAdmin
	missing bool
	shift   time.Duration
}

func (a dateAdmin) ListDeliveries(ctx context.Context, cursor string) (faucet.DeliveryPage, error) {
	page, err := a.HookAdmin.ListDeliveries(ctx, cursor)
	page.At = page.At.Add(a.shift)
	if a.missing {
		page.At = time.Time{}
	}
	return page, err
}

// use replaces the service with one that runs on store and hooks, with the
// same clock and logs.
func (e *redeliveryEnv) use(store faucet.RedeliveryStore, hooks faucet.HookAdmin) {
	e.t.Helper()
	deps := testDeps(e.store)
	deps.Clock = e.clock
	deps.Logger = slog.Make(e.logs)
	deps.Redelivery = &faucet.RedeliveryDeps{Store: store, Hooks: hooks}
	svc, err := faucet.New(deps)
	require.NoError(e.t, err)
	e.svc = svc
}

func TestADateAheadOfGitHubsListDoesNotHideTheNextFailure(t *testing.T) {
	t.Parallel()

	for _, ahead := range []time.Duration{time.Hour, 12 * time.Hour} {
		t.Run(ahead.String(), func(t *testing.T) {
			t.Parallel()
			env := newRedeliveryEnv(t)
			env.use(env.store, dateAdmin{HookAdmin: env.gh, shift: ahead})
			for range 3 {
				require.Zero(t, env.run().Tracked)
				env.advance(cronEvery)
			}

			env.gh.record("g1", env.gh.now().Add(-time.Minute), 503)
			res := env.run()

			require.Equal(t, 1, res.Tracked, "a Date that is wrong must not put the position past an attempt that GitHub has not listed yet")
			require.True(t, env.stored("g1"))
		})
	}
}

func TestAMissingDateRecordsNoGapOnEveryEmptyRun(t *testing.T) {
	t.Parallel()

	t.Run("a new hook", func(t *testing.T) {
		t.Parallel()
		env := newRedeliveryEnv(t)
		env.use(env.store, dateAdmin{HookAdmin: env.gh, missing: true})
		var gaps int
		for range 5 {
			gaps += env.run().Gaps
			env.advance(cronEvery)
		}
		require.Zero(t, gaps, "an empty list and no Date tell nothing about what expired")

		env.gh.record("g1", env.gh.now().Add(-time.Minute), 503)
		var tracked int
		for range 5 {
			res := env.run()
			gaps, tracked = gaps+res.Gaps, tracked+res.Tracked
			env.advance(cronEvery)
		}
		require.Equal(t, 1, tracked)
		require.Equal(t, 1, gaps, "an attempt gives a time to take the first gap from, and it is recorded once")
	})

	t.Run("a hook quiet for longer than GitHub keeps attempts", func(t *testing.T) {
		t.Parallel()
		env := newRedeliveryEnv(t)
		env.use(env.store, dateAdmin{HookAdmin: env.gh, missing: true})
		env.gh.record("g1", env.gh.now().Add(-time.Minute), http200)
		env.run()
		env.advance(faucet.GitHubDeliveryRetention + time.Hour)

		var gaps int
		for range 6 {
			gaps += env.run().Gaps
			env.advance(cronEvery)
		}
		require.Zero(t, gaps)
	})
}

// purgeSpy records when a run purges, and whether the run still has its context.
type purgeSpy struct {
	*fakeRedeliveryStore
	mu    sync.Mutex
	at    time.Time
	alive bool
	calls int
}

func (s *purgeSpy) PurgeRedeliveries(ctx context.Context, before time.Time) (int64, error) {
	s.mu.Lock()
	s.calls++
	s.at, s.alive = time.Now(), ctx.Err() == nil
	s.mu.Unlock()
	return s.fakeRedeliveryStore.PurgeRedeliveries(ctx, before)
}

func TestARequestInFlightIsCanceledAtNinetyPercentOfTheTimeSoThePurgeRuns(t *testing.T) {
	t.Parallel()

	const limit = 4 * time.Second
	env := newRedeliveryEnv(t)
	spy := &purgeSpy{fakeRedeliveryStore: env.store}
	env.use(spy, env.gh)
	env.gh.record("g1", env.gh.now().Add(-time.Minute), 503)
	env.gh.askDelay = time.Hour // the one request never answers
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()

	start := time.Now()
	res, err := env.svc.Redeliver(ctx)

	require.ErrorContains(t, err, "request redelivery", "the request that did not answer is canceled and counted as a failed one")
	require.Zero(t, res.Asked)
	spy.mu.Lock()
	defer spy.mu.Unlock()
	require.Equal(t, 1, spy.calls)
	require.True(t, spy.alive, "the purge runs inside the run's time")
	purgedAfter := spy.at.Sub(start)
	require.Greater(t, purgedAfter, limit*85/100, "a request in flight is not canceled before 90 percent of the time has passed")
	require.Less(t, purgedAfter, limit*975/1000, "the purge keeps the last tenth of the time")
}

func TestAnAttemptThatGotNoAnswerOrARedirectIsAFailureAndOnlyA2xxIsNot(t *testing.T) {
	t.Parallel()

	for status, failure := range map[int]bool{
		0:   true, // GitHub got no HTTP response at all
		301: true,
		302: true,
		400: true,
		503: true,
		200: false,
		204: false,
		299: false,
	} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			t.Parallel()
			env := newRedeliveryEnv(t)
			env.gh.record("g1", env.gh.now().Add(-time.Minute), status)

			res := env.run()

			require.Equal(t, failure, res.Tracked == 1, "status %d", status)
			require.Equal(t, failure, env.stored("g1"), "a failure is asked for until faucet stores it")
		})
	}
}

func TestAPageWithoutADateTakesTheEndOfTheFirstGapFromItsNewestAttempt(t *testing.T) {
	t.Parallel()

	env := newRedeliveryEnv(t)
	env.use(env.store, dateAdmin{HookAdmin: env.gh, missing: true})
	failures := failEvery(env, 50*time.Minute, faucet.GitHubDeliveryRetention)

	env.run()

	requireNoSilentLoss(t, env, failures)
}

func TestALateAttemptInsideTheOverlapIsReadWhateverNumberOfAttemptsAreNew(t *testing.T) {
	t.Parallel()

	// k attempts lie in the delta before T, and the run before reads them, so
	// its position is T. Then a failed attempt L is listed, dated T less delta,
	// below the position, and newAttempts attempts arrive before the next run. The
	// first page holds the new ones and the newest of the old ones, and L is
	// below it, so the run reads it only through the overlap.
	for _, delta := range []time.Duration{15 * time.Second, 2 * time.Minute, 4 * time.Minute} {
		for _, k := range []int{1, 5, 30} {
			t.Run(fmt.Sprintf("delta=%s,k=%d", delta, k), func(t *testing.T) {
				t.Parallel()
				var lost []int
				for newAttempts := range 201 {
					env := newRedeliveryEnv(t)
					env.clock.WithLogger(quartz.NoOpLogger)
					env.run()
					env.advance(cronEvery)
					top := env.gh.now().Add(-time.Second)
					for i := range k {
						env.gh.record(fmt.Sprintf("k-%03d", i), top.Add(-time.Duration(i)*delta/time.Duration(k+1)), http200)
					}
					env.run()

					env.gh.record("L", top.Add(-delta), 503)
					listed := env.gh.now()
					for i := range newAttempts {
						env.gh.record(fmt.Sprintf("n-%04d", i), listed.Add(time.Duration(i+1)*(cronEvery-time.Second)/time.Duration(newAttempts+1)), http200)
					}
					env.advance(cronEvery)
					env.run()
					env.advance(cronEvery)
					env.run()

					if !env.stored("L") {
						lost = append(lost, newAttempts)
					}
				}
				require.Empty(t, lost, "L failed inside the overlap, so a run must read it, and it was not read when this many attempts were new")
			})
		}
	}
}
