package faucet_test

import (
	"context"
	"fmt"
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

func TestABurstThatARunCannotReadEndsEveryFailureRecoveredOrInACountedGap(t *testing.T) {
	t.Parallel()

	// A burst of 5 attempts a second for 500 seconds holds 25 pages, and a run
	// reads 9. The failures are in its oldest part. The hook is silent after the
	// burst, so every run reads the same newest pages, and what no run reads
	// leaves GitHub's retention as a gap.
	env := newRedeliveryEnv(t, func(o *envOptions) { o.maxPages = 9 })
	env.clock.WithLogger(quartz.NoOpLogger)
	env.run()
	env.advance(time.Minute)
	start := env.gh.now()
	var failures []ghAttempt
	for i := range 2500 {
		status := http200
		if i < 1000 && i%50 == 7 {
			status = 503
		}
		a := env.gh.record(fmt.Sprintf("a-%04d", i), start.Add(time.Duration(i)*200*time.Millisecond), status)
		if status != http200 {
			failures = append(failures, a)
		}
	}
	env.advance(cronEvery)

	for env.gh.now().Before(start.Add(faucet.GitHubDeliveryRetention + 3*time.Hour)) {
		env.run()
		env.advance(cronEvery)
	}

	requireNoSilentLoss(t, env, failures)
	require.Zero(t, env.run().Ranges, "a range that no run read has expired by now")
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

func TestALateAttemptIsReadWhileARangeStandsAtThePosition(t *testing.T) {
	t.Parallel()

	// Run A reads a burst that ends at T. Run B finds nothing newer, and its first
	// page does not reach the overlap, so it opens a range at T that stays open:
	// GitHub fails its range pages, or the burst is larger than its pages. Then L
	// is listed, dated in the overlap just above where B's first page ended, and
	// newAttempts arrive before run C. C must still read L.
	for _, cause := range []string{"GitHub fails the range pages", "the burst is larger than the pages"} {
		for _, gapAB := range []time.Duration{time.Minute, 3 * time.Minute, cronEvery} {
			t.Run(fmt.Sprintf("%s,gapAB=%s", cause, gapAB), func(t *testing.T) {
				t.Parallel()
				var lost []int
				for _, newAttempts := range []int{0, 1, 5, 20, 60, 99, 150} {
					env := newRedeliveryEnv(t)
					env.clock.WithLogger(quartz.NoOpLogger)
					env.run()
					env.advance(cronEvery)
					top := env.gh.now().Add(-time.Second)
					burst, every := 300, time.Second
					if cause != "GitHub fails the range pages" {
						burst, every = 3000, 100*time.Millisecond
					}
					for i := range burst {
						env.gh.record(fmt.Sprintf("b-%05d", i), top.Add(-time.Duration(i)*every), http200)
					}
					env.run()
					env.advance(gapAB)
					if cause == "GitHub fails the range pages" {
						env.gh.listFault = func(_ int, cursor string) error {
							if cursor != "" {
								return fmt.Errorf("GitHub answered the delivery list with status 502")
							}
							return nil
						}
					}
					_, _ = env.svc.Redeliver(context.Background())
					env.gh.listFault = nil

					lateAt := top.Add(-98*every - every/2)
					env.gh.record("L", lateAt, 503)
					listed := env.gh.now()
					for i := range newAttempts {
						env.gh.record(fmt.Sprintf("n-%04d", i), listed.Add(time.Duration(i+1)*(cronEvery-time.Second)/time.Duration(newAttempts+1)), http200)
					}
					env.advance(cronEvery)
					_, _ = env.svc.Redeliver(context.Background())
					for range 6 {
						env.advance(cronEvery)
						env.run()
					}

					if !env.stored("L") {
						lost = append(lost, newAttempts)
					}
				}
				require.Empty(t, lost, "L is listed late inside the overlap, so a run must still read it when this many attempts are new")
			})
		}
	}
}
