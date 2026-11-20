package redelivery_test

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"

	"cdr.dev/slog/v3"
	"github.com/coder/quartz"
	"github.com/coder/superblink-events/faucet/redelivery"
)

type gapWindow struct{ from, to time.Time }

func (w gapWindow) holds(at time.Time) bool { return !at.Before(w.from) && !at.After(w.to) }

func (w gapWindow) String() string {
	return w.from.Format(time.DateTime) + " to " + w.to.Format(time.DateTime)
}

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
		require.True(t, covered, "%s failed at %s and has no final state: no run tracked it and no gap covers it, gaps %v", f.guid, f.at.Format(time.DateTime), gaps)
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

	for env.gh.now().Before(start.Add(redelivery.GitHubDeliveryRetention + 3*time.Hour)) {
		env.run()
		env.advance(cronEvery)
	}

	requireNoSilentLoss(t, env, failures)
	require.Zero(t, env.run().Ranges, "a range that no run read has expired by now")
}

// use replaces the runner with one that runs on store and hooks, with the
// same clock and logs.
func (e *redeliveryEnv) use(store redelivery.Store, hooks redelivery.HookAdmin) {
	e.t.Helper()
	runner, err := redelivery.New(redelivery.Deps{Store: store, Hooks: hooks, Clock: e.clock, Logger: slog.Make(e.logs)})
	require.NoError(e.t, err)
	e.runner = runner
}

// purgeSpy records when a run purges, and whether the run still has its context.
type purgeSpy struct {
	*fakeRedeliveryStore
	mu       sync.Mutex
	at       time.Time
	alive    bool
	deadline time.Time
	calls    int
}

func (s *purgeSpy) PurgeRedeliveries(ctx context.Context, before time.Time) (int64, error) {
	s.mu.Lock()
	s.calls++
	s.at, s.alive = time.Now(), ctx.Err() == nil
	s.deadline, _ = ctx.Deadline()
	s.mu.Unlock()
	return s.fakeRedeliveryStore.PurgeRedeliveries(ctx, before)
}

// The run splits the deadline of its context by time.Now. In a synctest bubble
// the clock moves only when every goroutine waits, so the purge starts at the
// same time on every machine, whatever its load.
func TestARequestInFlightIsCanceledAtNinetyPercentOfTheTimeSoThePurgeRuns(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		const limit = 4 * time.Second
		env := newRedeliveryEnv(t)
		spy := &purgeSpy{fakeRedeliveryStore: env.store}
		env.use(spy, env.gh)
		env.gh.record("g1", env.gh.now().Add(-time.Minute), 503)
		env.gh.askDelay = time.Hour // the one request never answers
		ctx, cancel := context.WithTimeout(context.Background(), limit)
		defer cancel()
		runDeadline, _ := ctx.Deadline()

		start := time.Now()
		res, err := env.runner.Run(ctx)

		require.ErrorContains(t, err, "request redelivery", "the request that did not answer is canceled and counted as a failed one")
		require.Zero(t, res.Asked)
		spy.mu.Lock()
		defer spy.mu.Unlock()
		require.Equal(t, 1, spy.calls)
		require.True(t, spy.alive, "the purge runs inside the run's time")
		require.True(t, spy.deadline.Equal(runDeadline), "the purge runs under the time of the whole run, not the request's")
		require.Equal(t, limit*9/10, spy.at.Sub(start), "the request is canceled at 90 percent of the time, and the purge starts then, with the last tenth left")
	})
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
					_, _ = env.runner.Run(context.Background())
					env.gh.listFault = nil

					lateAt := top.Add(-98*every - every/2)
					env.gh.record("L", lateAt, 503)
					listed := env.gh.now()
					for i := range newAttempts {
						env.gh.record(fmt.Sprintf("n-%04d", i), listed.Add(time.Duration(i+1)*(cronEvery-time.Second)/time.Duration(newAttempts+1)), http200)
					}
					env.advance(cronEvery)
					_, _ = env.runner.Run(context.Background())
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

func TestALateFailureEndsInAGapWhenTheCronIsDownLongerThanGitHubKeepsIt(t *testing.T) {
	t.Parallel()
	env := newRedeliveryEnv(t)
	env.clock.WithLogger(quartz.NoOpLogger)
	a := env.gh.record("A", env.gh.now().Add(-time.Second), http200)
	env.run()
	l := env.gh.record("L", a.at.Add(-3*time.Second), 503)

	env.advance(80 * time.Hour)
	env.run()

	requireNoSilentLoss(t, env, []ghAttempt{l})
}

func TestALateFailureEndsInAGapWhenGitHubDeletesItBeforeARunReadsTheRangeAboveIt(t *testing.T) {
	t.Parallel()

	for name, listStart := range map[string]time.Duration{
		"the range expires unread":                 8 * time.Hour,
		"the range is read to the end of the list": time.Minute,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			env := newRedeliveryEnv(t, func(o *envOptions) { o.maxPages = 1 })
			env.clock.WithLogger(quartz.NoOpLogger)
			t0 := env.gh.now()
			a := env.gh.record("A", t0.Add(-time.Second), http200)
			env.run()
			l := env.gh.record("L", a.at.Add(-3*time.Second), 503)
			for i := range 150 {
				env.gh.record(fmt.Sprintf("b-%03d", i), t0.Add(cronEvery-time.Duration(i+1)*2*time.Second), http200)
			}
			env.advance(cronEvery)
			require.Equal(t, 1, env.run().Ranges, "the burst leaves a range down to A")

			env.advance(t0.Add(listStart + redelivery.GitHubDeliveryRetention).Sub(env.gh.now()))
			env.useMaxPages(redelivery.DefaultMaxPagesPerRun)
			env.run()

			requireNoSilentLoss(t, env, []ghAttempt{l})
		})
	}
}

func TestARangeThatExpiresUnreadEndsAsAGapFromTheOverlapBelowItsOldEndToTheOldestAttemptReadAboveIt(t *testing.T) {
	t.Parallel()
	env := newRedeliveryEnv(t, func(o *envOptions) { o.maxPages, o.pageSize = 1, 3 })
	env.clock.WithLogger(quartz.NoOpLogger)
	t0 := env.gh.now()
	top := env.gh.record("A", t0, http200)
	env.run()

	unread := env.gh.record("unread", top.at.Add(-4*time.Minute), 503)
	oldestRead := env.gh.record("read", top.at.Add(-3*time.Minute), 503)
	env.gh.record("new", top.at.Add(time.Second), http200)
	env.advance(cronEvery)
	require.Equal(t, 1, env.run().Ranges, "the one page ends at an attempt inside the overlap, above a late one")

	env.advance(redelivery.GitHubDeliveryRetention + time.Hour)
	require.False(t, env.gh.lists(unread.guid))
	require.Equal(t, 1, env.run().Ranges, "the stored Date is from before the outage, so it closes nothing yet")
	env.advance(cronEvery)
	before := len(env.loggedGaps())
	res := env.run()

	require.Zero(t, res.Ranges)
	require.Equal(t, 1, res.Pages, "the head read took the only page, so no read of the range closed it")
	require.Equal(t, []gapWindow{{from: top.at.Add(-redelivery.ReadOverlap), to: oldestRead.at}}, env.loggedGaps()[before:],
		"the gap starts where a read of the range would have stopped and ends at the oldest attempt read above it")
	requireNoSilentLoss(t, env, []ghAttempt{unread})
}

const (
	newestAttemptAge    = time.Minute
	dateLagPastTheSlack = newestAttemptAge + redelivery.DateSlack + time.Minute
)

func requireListed(t *testing.T, env *redeliveryEnv, a ghAttempt) {
	t.Helper()
	require.True(t, env.gh.lists(a.guid), "GitHub must list %s, or the scenario proves nothing", a.guid)
}

func TestAnExpiredFailureIsInsideTheGapWhenFaucetsClockLagsGitHubs(t *testing.T) {
	t.Parallel()

	for _, lag := range []time.Duration{10 * time.Minute, time.Hour} {
		t.Run(lag.String(), func(t *testing.T) {
			t.Parallel()
			env := newRedeliveryEnv(t)
			env.clock.WithLogger(quartz.NoOpLogger)
			env.gh.skew = lag
			t0 := env.gh.now()
			env.gh.record("A", t0.Add(-time.Minute), http200)
			env.run()

			expiredFor := lag / 2
			f := env.gh.record("F", t0.Add(8*time.Hour-expiredFor), 503)
			env.advance(8 * time.Hour)
			requireListed(t, env, f)
			env.advance(redelivery.GitHubDeliveryRetention)
			require.True(t, f.at.After(env.clock.Now().Add(-redelivery.GitHubDeliveryRetention)), "faucet's clock says that GitHub still keeps F")
			env.run()

			requireNoSilentLoss(t, env, []ghAttempt{f})
		})
	}
}

func TestTheFirstRunsGapStartsBelowGitHubsListWhenFaucetsClockRunsAhead(t *testing.T) {
	t.Parallel()

	for _, ahead := range []time.Duration{10 * time.Minute, time.Hour} {
		t.Run(ahead.String(), func(t *testing.T) {
			t.Parallel()
			env := newRedeliveryEnv(t)
			env.clock.WithLogger(quartz.NoOpLogger)
			env.gh.skew = -ahead
			t0 := env.gh.now()
			stillListedFor := ahead / 2
			f := env.gh.record("F", t0.Add(-redelivery.GitHubDeliveryRetention+stillListedFor), 503)
			env.gh.record("A", t0.Add(-time.Minute), http200)
			requireListed(t, env, f)
			require.False(t, f.at.After(env.clock.Now().Add(-redelivery.GitHubDeliveryRetention)), "faucet's clock says that the 72 hours of F are over")

			env.run()

			requireNoSilentLoss(t, env, []ghAttempt{f})
		})
	}
}

func TestADateAheadOfGitHubsListDoesNotMoveThePositionPastTheNextFailure(t *testing.T) {
	t.Parallel()

	for _, lead := range []time.Duration{10 * time.Minute, time.Hour} {
		t.Run(lead.String(), func(t *testing.T) {
			t.Parallel()
			env := newRedeliveryEnv(t)
			env.clock.WithLogger(quartz.NoOpLogger)
			env.gh.date = dateOff(lead)
			env.run()
			env.advance(time.Minute)
			f := env.gh.record("F", env.gh.now(), 503)
			env.advance(cronEvery)

			res := env.run()

			require.Equal(t, 1, res.Tracked, "the list was empty before F, so F is above everything that a run has read")
			require.True(t, env.stored(f.guid))
		})
	}
}

func TestAPageThatIsSlowToArriveLosesNoFailureThatGitHubDeletesMeanwhile(t *testing.T) {
	t.Parallel()

	for _, latency := range []time.Duration{600 * time.Millisecond, 5 * time.Second} {
		t.Run(latency.String(), func(t *testing.T) {
			t.Parallel()
			env := newRedeliveryEnv(t, func(o *envOptions) { o.maxPages = 1 })
			env.clock.WithLogger(quartz.NoOpLogger)
			t0 := env.gh.now()
			env.gh.record("A", t0.Add(-time.Second), http200)
			env.run()

			f := env.gh.record("F", t0.Add(time.Minute), 503)
			for i := range 150 {
				env.gh.record(fmt.Sprintf("b-%03d", i), t0.Add(2*time.Minute+time.Duration(i)*4*time.Second), http200)
			}
			env.advance(15 * time.Minute)
			require.Equal(t, 1, env.run().Ranges, "the burst leaves a range down to A")
			requireListed(t, env, f)

			oneAndAHalfPages := latency * 3 / 2
			env.advance(f.at.Add(redelivery.GitHubDeliveryRetention - oneAndAHalfPages).Sub(env.gh.now()))
			env.useMaxPages(redelivery.DefaultMaxPagesPerRun)
			env.slowPages(latency)
			require.Zero(t, env.run().Ranges, "the range is read to the end of the list")
			require.False(t, env.gh.lists(f.guid), "GitHub deleted F between the first page and the page of the range")

			requireNoSilentLoss(t, env, []ghAttempt{f})
		})
	}
}

func TestOneDateFarAheadDoesNotStayAsTheStartOfLaterGaps(t *testing.T) {
	t.Parallel()
	env := newRedeliveryEnv(t)
	env.clock.WithLogger(quartz.NoOpLogger)
	t0 := env.gh.now()
	env.gh.record("A", t0.Add(-time.Minute), http200)
	env.gh.date = dateOff(100 * time.Hour)
	env.run()
	env.gh.date = nil
	env.advance(cronEvery)
	env.run()

	f := env.gh.record("F", t0.Add(time.Hour), 503)
	env.advance(2 * time.Hour)
	requireListed(t, env, f)
	env.advance(78 * time.Hour)
	env.run()

	requireNoSilentLoss(t, env, []ghAttempt{f})
}

func TestOneDateFarAheadClosesNoRangeThatGitHubStillLists(t *testing.T) {
	t.Parallel()

	for _, lead := range []time.Duration{80 * time.Hour, 30 * 24 * time.Hour} {
		t.Run(lead.String(), func(t *testing.T) {
			t.Parallel()
			env := newRedeliveryEnv(t, func(o *envOptions) { o.maxPages = 1 })
			env.clock.WithLogger(quartz.NoOpLogger)
			t0 := env.gh.now()
			env.gh.record("A", t0.Add(-time.Second), http200)
			env.run()
			var failures []ghAttempt
			for i := range 150 {
				status := http200
				if i < 50 && i%10 == 0 {
					status = 503
				}
				a := env.gh.record(fmt.Sprintf("b-%03d", i), t0.Add(time.Minute+time.Duration(i)*2*time.Second), status)
				if status != http200 {
					failures = append(failures, a)
				}
			}
			env.advance(cronEvery)
			require.Equal(t, 1, env.run().Ranges, "one page holds the newest 100, and the failures are in the range below")

			env.advance(cronEvery)
			env.useMaxPages(redelivery.DefaultMaxPagesPerRun)
			env.gh.date = func(_ int, cursor string, at time.Time) time.Time {
				if cursor == "" {
					return at.Add(lead)
				}
				return at
			}
			res := env.run()

			require.Zero(t, res.Gaps, "GitHub lists the whole range, so nothing expired")
			require.Zero(t, res.Ranges)
			for _, f := range failures {
				require.True(t, env.tracked(f.guid), "%s is in a range that GitHub still lists, so the run reads it", f.guid)
			}
		})
	}
}

func TestTheFirstRunClosesNoRangeOnTheWordOfOneDate(t *testing.T) {
	t.Parallel()
	env := newRedeliveryEnv(t, func(o *envOptions) { o.maxPages = 1 })
	env.clock.WithLogger(quartz.NoOpLogger)
	t0 := env.gh.now()
	var failures []ghAttempt
	for i := range 150 {
		status := http200
		if i < 50 && i%10 == 0 {
			status = 503
		}
		a := env.gh.record(fmt.Sprintf("b-%03d", i), t0.Add(-time.Hour+time.Duration(i)*2*time.Second), status)
		if status != http200 {
			failures = append(failures, a)
		}
	}

	env.gh.date = dateOff(80 * time.Hour)
	first := env.run()
	require.Equal(t, 1, first.Ranges, "the range that the first run opens stays open")
	require.Equal(t, 1, first.Gaps, "and the only gap is the time before tracking")

	env.gh.date = nil
	env.advance(cronEvery)
	env.useMaxPages(redelivery.DefaultMaxPagesPerRun)
	require.Zero(t, env.run().Ranges)
	for _, f := range failures {
		require.True(t, env.tracked(f.guid), "%s is in a range that GitHub still lists, so the next run reads it", f.guid)
	}
}

func TestTheFirstRunOfAHookWithoutAClearRecordsOnlyTheTimeBeforeItsLookback(t *testing.T) {
	t.Parallel()
	env := newRedeliveryEnv(t)
	env.clock.WithLogger(quartz.NoOpLogger)
	t0 := env.gh.now()
	newest := env.gh.record("newer", t0.Add(-50*time.Hour), 503)
	env.gh.record("older", t0.Add(-60*time.Hour), 503)

	res := env.run()

	require.Equal(t, 2, res.Tracked)
	gaps := env.loggedGaps()
	require.Len(t, gaps, 1)
	require.True(t, newest.at.Add(-redelivery.GitHubDeliveryRetention).Equal(gaps[0].from), "%s", gaps[0])
	require.True(t, newest.at.Add(-redelivery.StartLookback).Equal(gaps[0].to), "the gap ends at the bound of the run, below every attempt that it read: %s", gaps[0])
}

func TestTrackingThatBeganWithAnEmptyListOrTheMigrationHoldsThroughAnOutage(t *testing.T) {
	t.Parallel()

	for _, began := range []string{"an empty first run", "the migration"} {
		for _, tc := range []struct {
			outage    time.Duration
			listEmpty bool
		}{{30 * time.Hour, false}, {80 * time.Hour, false}, {80 * time.Hour, true}} {
			t.Run(fmt.Sprintf("%s,outage=%s,listEmpty=%v", began, tc.outage, tc.listEmpty), func(t *testing.T) {
				t.Parallel()
				env := newRedeliveryEnv(t)
				env.clock.WithLogger(quartz.NoOpLogger)
				t0 := env.gh.now()
				if began == "the migration" {
					env.store.seedMigratedTop(t0)
				} else {
					env.run()
				}
				f := env.gh.record("F", t0.Add(time.Hour), 503)
				env.advance(2 * time.Hour)
				requireListed(t, env, f)
				env.advance(tc.outage - 2*time.Hour)
				if !tc.listEmpty {
					env.gh.record("G", env.gh.now().Add(-time.Second), http200)
				}
				for range 3 {
					env.run()
					env.advance(cronEvery)
				}

				if tc.outage < redelivery.GitHubDeliveryRetention {
					require.True(t, env.stored(f.guid), "GitHub still lists F, and tracking began before F, so the run reads it")
					return
				}
				requireNoSilentLoss(t, env, []ghAttempt{f})
			})
		}
	}
}

func TestAfterTheMigrationATopThatWasADateAheadDropsNoFailure(t *testing.T) {
	t.Parallel()

	for _, lead := range []time.Duration{time.Hour, 7 * time.Hour} {
		t.Run(fmt.Sprintf("the cron keeps running,lead=%s", lead), func(t *testing.T) {
			t.Parallel()
			env := newRedeliveryEnv(t)
			env.clock.WithLogger(quartz.NoOpLogger)
			env.store.seedMigratedTop(env.gh.now().Add(lead))

			var failures []ghAttempt
			for i := range 12 {
				failures = append(failures, env.gh.record(fmt.Sprintf("f-%02d", i), env.gh.now().Add(-time.Second), 503))
				env.advance(cronEvery)
				env.run()
			}

			for _, f := range failures {
				require.True(t, env.stored(f.guid), "%s was listed above everything that a run had read", f.guid)
			}
		})
	}
	for _, lead := range []time.Duration{time.Hour, 6 * time.Hour} {
		t.Run(fmt.Sprintf("the cron is down for 80 hours,lead=%s", lead), func(t *testing.T) {
			t.Parallel()
			env := newRedeliveryEnv(t)
			env.clock.WithLogger(quartz.NoOpLogger)
			env.store.seedMigratedTop(env.gh.now().Add(lead))

			var failures []ghAttempt
			for i := range 30 {
				env.advance(cronEvery)
				failures = append(failures, env.gh.record(fmt.Sprintf("f-%02d", i), env.gh.now().Add(-time.Second), 503))
			}
			requireListed(t, env, failures[0])
			env.advance(80 * time.Hour)
			env.gh.record("G", env.gh.now().Add(-time.Second), http200)
			for range 3 {
				env.run()
				env.advance(cronEvery)
			}

			requireNoSilentLoss(t, env, failures)
		})
	}
}

func TestATopThatTheStoreReturnsWithoutAClearFailsTheRunUntilItMovesToTheClear(t *testing.T) {
	t.Parallel()
	env := newRedeliveryEnv(t)
	env.clock.WithLogger(quartz.NoOpLogger)
	t0 := env.gh.now()
	env.store.seedTopWithoutClear(t0.Add(-time.Hour))
	f := env.gh.record("F", t0.Add(-time.Minute), 503)

	res, err := env.runner.Run(context.Background())

	require.ErrorContains(t, err, "a top without a clear")
	require.Zero(t, res.Pages, "the run reads nothing below a top that may be a Date")
	require.Zero(t, res.Asked)
	require.False(t, env.tracked(f.guid))

	env.store.applyMigration000008()
	require.Equal(t, 1, env.run().Tracked, "the migration moves the top to the clear, and the next run is a first run")
	require.True(t, env.tracked(f.guid))
}

func TestALateWriteBelowTheTopIsInsideTheGapWhileTheDateLeadsByLessThanTheMarginAndTheOverlap(t *testing.T) {
	t.Parallel()

	for _, lead := range []time.Duration{0, time.Hour, redelivery.DateMargin + redelivery.ReadOverlap - 10*time.Second} {
		t.Run(lead.String(), func(t *testing.T) {
			t.Parallel()
			env := newRedeliveryEnv(t)
			env.clock.WithLogger(quartz.NoOpLogger)
			env.gh.date = dateOff(lead)
			a := env.gh.record("A", env.gh.now().Add(-time.Second), http200)
			env.run()
			l := env.gh.record("L", a.at.Add(-3*time.Second), 503)
			requireListed(t, env, l)

			env.advance(80 * time.Hour)
			env.run()

			requireNoSilentLoss(t, env, []ghAttempt{l})
		})
	}
}

func TestARangeIsReadThroughPagesThatCarryNoDate(t *testing.T) {
	t.Parallel()
	env := newRedeliveryEnv(t, func(o *envOptions) { o.maxPages = 1 })
	env.clock.WithLogger(quartz.NoOpLogger)
	t0 := env.gh.now()
	env.gh.record("Z", t0.Add(-10*time.Minute), http200)
	env.gh.record("A", t0.Add(-time.Second), http200)
	env.run()
	var failures []ghAttempt
	for i := range 250 {
		status := http200
		if i < 150 && i%10 == 0 {
			status = 503
		}
		a := env.gh.record(fmt.Sprintf("b-%03d", i), t0.Add(time.Minute+time.Duration(i)*2*time.Second), status)
		if status != http200 {
			failures = append(failures, a)
		}
	}
	env.advance(cronEvery)
	require.Equal(t, 1, env.run().Ranges)

	env.gh.date = func(_ int, cursor string, at time.Time) time.Time {
		if cursor != "" {
			return time.Time{}
		}
		return at
	}
	env.advance(cronEvery)
	env.useMaxPages(redelivery.DefaultMaxPagesPerRun)
	res := env.run()

	require.Zero(t, res.Ranges, "a page that reaches the bound of its range ends it")
	require.Zero(t, res.Gaps)
	for _, f := range failures {
		require.True(t, env.tracked(f.guid), "%s is on a page without a Date, and the run reads it", f.guid)
	}
}

func TestARangeWhosePageFailsStaysOpenUntilGitHubHasDeletedItAndEndsAsAGap(t *testing.T) {
	t.Parallel()

	const refusedCursor = "GitHub refuses the cursor of an attempt that it deleted"
	for name, date := range map[string]func(int, string, time.Time) time.Time{
		"the page that ends the list has no Date": func(_ int, cursor string, at time.Time) time.Time {
			if cursor != "" {
				return time.Time{}
			}
			return at
		},
		"the Date of the page that ends the list is more than the slack behind the newest attempt of the run": func(_ int, cursor string, at time.Time) time.Time {
			if cursor != "" {
				return at.Add(-dateLagPastTheSlack)
			}
			return at
		},
		refusedCursor: nil,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			env := newRedeliveryEnv(t, func(o *envOptions) { o.maxPages = 1 })
			env.clock.WithLogger(quartz.NoOpLogger)
			t0 := env.gh.now()
			var failures []ghAttempt
			var newestOfTheBurst ghAttempt
			for i := range 150 {
				status := http200
				if i < 50 && i%10 == 0 {
					status = 503
				}
				a := env.gh.record(fmt.Sprintf("b-%03d", i), t0.Add(-12*time.Minute+time.Duration(i)*4*time.Second), status)
				if status != http200 {
					failures = append(failures, a)
				}
				newestOfTheBurst = a
			}
			require.Equal(t, 1, env.run().Ranges, "the one page holds the newest 100, and the failures are in the range below")
			requireListed(t, env, failures[0])

			trickle := func() {
				env.advance(cronEvery)
				env.gh.record(fmt.Sprintf("t-%s", env.gh.now().Sub(t0)), env.gh.now().Add(-newestAttemptAge), http200)
			}
			theBurstHasExpired := t0.Add(redelivery.GitHubDeliveryRetention)
			faultBegins := theBurstHasExpired.Add(-2 * cronEvery)
			if name == refusedCursor {
				faultBegins = theBurstHasExpired
			}
			for env.gh.now().Before(faultBegins.Add(-cronEvery)) {
				trickle()
				require.Equal(t, 1, env.run().Ranges)
			}
			trickle()
			if name == refusedCursor {
				require.False(t, env.gh.lists(newestOfTheBurst.guid), "GitHub refuses a cursor only when it has deleted the attempt there")
			} else {
				requireListed(t, env, failures[0])
			}
			env.gh.date, env.gh.refusesStaleCursors = date, name == refusedCursor
			env.useMaxPages(redelivery.DefaultMaxPagesPerRun)

			res, err := env.runner.Run(context.Background())
			require.Error(t, err, "the page of the range fails")
			require.Equal(t, 1, res.Ranges, "and the range stays open")
			open := 1
			for range 6 {
				trickle()
				res, _ = env.runner.Run(context.Background())
				open = res.Ranges
			}

			require.Zero(t, open, "the range ends without a read once GitHub has deleted all of it")
			requireNoSilentLoss(t, env, failures)
		})
	}
}

func TestARangeWhoseLastPageIsEmptyEndsAsAGapUpToTheOldestAttemptReadAboveIt(t *testing.T) {
	t.Parallel()
	env := newRedeliveryEnv(t, func(o *envOptions) { o.maxPages = 1 })
	env.clock.WithLogger(quartz.NoOpLogger)
	t0 := env.gh.now()
	env.gh.record("A", t0.Add(-time.Minute), http200)
	env.run()

	l := env.gh.record("L", t0.Add(time.Hour), 503)
	var onePage []ghAttempt
	for i := range 100 {
		onePage = append(onePage, env.gh.record(fmt.Sprintf("b-%03d", i), t0.Add(3*time.Hour+time.Duration(i)*4*time.Second), http200))
	}
	env.advance(3*time.Hour + 15*time.Minute)
	require.Equal(t, 1, env.run().Ranges, "the one page holds the burst, and L is in the range below it")
	requireListed(t, env, l)

	oldestRead := onePage[0]
	insideTheSlack := redelivery.DateSlack / 2
	env.advance(oldestRead.at.Add(redelivery.GitHubDeliveryRetention - insideTheSlack).Sub(env.gh.now()))
	requireListed(t, env, oldestRead)
	require.False(t, env.gh.lists(l.guid), "GitHub has deleted all of the range")
	env.useMaxPages(redelivery.DefaultMaxPagesPerRun)
	res := env.run()

	require.Zero(t, res.Ranges)
	requireNoSilentLoss(t, env, []ghAttempt{l})
	for _, gap := range env.loggedGaps() {
		require.False(t, gap.to.After(oldestRead.at), "the gap %s reaches above the oldest attempt that a run read above it", gap)
	}
}

func TestAWholeSecondDateAsFarBehindAsTheSlackStillEndsTheGapAboveWhatGitHubDeleted(t *testing.T) {
	t.Parallel()

	for _, lag := range []time.Duration{0, redelivery.DateSlack - time.Second, redelivery.DateSlack} {
		t.Run(lag.String(), func(t *testing.T) {
			t.Parallel()
			env := newRedeliveryEnv(t)
			env.clock.WithLogger(quartz.NoOpLogger)
			env.gh.date = func(_ int, _ string, at time.Time) time.Time { return at.Add(-lag).Truncate(time.Second) }
			t0 := env.gh.now()
			env.advance(time.Second)
			env.gh.record("A", t0, http200)
			env.run()
			env.advance(time.Hour)
			f := env.gh.record("F", t0.Add(time.Hour+400*time.Millisecond), 503)
			requireListed(t, env, f)

			deleted := f.at.Add(redelivery.GitHubDeliveryRetention)
			laterInTheSameSecond := 300 * time.Millisecond
			env.advance(deleted.Add(laterInTheSameSecond).Sub(env.gh.now()))
			require.False(t, env.gh.lists(f.guid))
			require.True(t, env.gh.now().Truncate(time.Second).Before(deleted), "a Date drops the fraction of its second, which puts it before the deletion of F")
			env.run()

			requireNoSilentLoss(t, env, []ghAttempt{f})
		})
	}
}

func TestAWholeSecondDateAsFarBehindAsTheSlackPassesTheCheckAgainstAnAttemptLaterInItsSecond(t *testing.T) {
	t.Parallel()

	for _, lag := range []time.Duration{redelivery.DateSlack - 500*time.Millisecond, redelivery.DateSlack} {
		t.Run(lag.String(), func(t *testing.T) {
			t.Parallel()
			env := newRedeliveryEnv(t)
			env.clock.WithLogger(quartz.NoOpLogger)
			wholeSecondDate := func(at time.Time) time.Time { return at.Add(-lag).Truncate(time.Second) }
			env.gh.date = func(_ int, _ string, at time.Time) time.Time { return wholeSecondDate(at) }
			intoItsSecond := 200 * time.Millisecond
			env.advance(env.gh.now().Truncate(time.Second).Add(time.Second + intoItsSecond).Sub(env.gh.now()))

			for i := range 6 {
				f := env.gh.record(fmt.Sprintf("f-%d", i), env.gh.now().Add(-50*time.Millisecond), 503)
				require.True(t, wholeSecondDate(env.gh.now()).Before(f.at.Add(-redelivery.DateSlack)),
					"the Date drops more of its second than the slack leaves above it, or the scenario proves nothing")
				res, err := env.runner.Run(context.Background())
				require.NoError(t, err, "the clock that the Date shows is no more than the slack behind %s", f.guid)
				require.Equal(t, 1, res.Tracked)
				env.advance(cronEvery)
			}
		})
	}
}

func TestAFirstPageWithoutAUsableDateFailsTheRunWhichAsksForNothing(t *testing.T) {
	t.Parallel()

	for name, date := range map[string]func(int, string, time.Time) time.Time{
		"no Date": func(int, string, time.Time) time.Time { return time.Time{} },
		"a Date more than the slack behind the newest attempt": dateOff(-dateLagPastTheSlack),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			env := newRedeliveryEnv(t)
			env.clock.WithLogger(quartz.NoOpLogger)
			env.gh.answer = failing(400, "due")
			env.gh.record("due", env.gh.now().Add(-time.Minute), 400)
			require.Equal(t, 1, env.run().Asked)
			env.advance(cronEvery)
			env.gh.record("new", env.gh.now().Add(-newestAttemptAge), 503)

			env.gh.date = date
			res, err := env.runner.Run(context.Background())

			require.ErrorContains(t, err, "Date")
			require.Zero(t, res.Tracked, "a page whose Date is not usable is not used")
			require.Zero(t, res.Asked, "the second ask of due was due, and it did not go out")
			require.Equal(t, 1, res.Pending, "the settle needs no GitHub, so it still runs")

			env.gh.date = nil
			res = env.run()
			require.Equal(t, 1, res.Tracked, "the failed run moved no position, so the next run reads what it left")
			require.Equal(t, 2, res.Asked)
		})
	}
}

func TestRunsWithoutADateFailLoudlyAndTheNextRunWithOneLosesNothing(t *testing.T) {
	t.Parallel()

	for _, listed := range []string{"an empty hook", "a hook whose failure expired during an outage"} {
		t.Run(listed, func(t *testing.T) {
			t.Parallel()
			env := newRedeliveryEnv(t)
			env.clock.WithLogger(quartz.NoOpLogger)
			var failures []ghAttempt
			if listed != "an empty hook" {
				t0 := env.gh.now()
				env.gh.record("A", t0.Add(-time.Minute), http200)
				env.run()
				failures = append(failures, env.gh.record("F", t0.Add(time.Hour), 503))
				env.advance(2 * time.Hour)
				requireListed(t, env, failures[0])
				env.advance(78 * time.Hour)
			}

			env.gh.date = func(int, string, time.Time) time.Time { return time.Time{} }
			for range 12 {
				res, err := env.runner.Run(context.Background())
				require.ErrorContains(t, err, "Date", "a list without a Date says nothing about what GitHub has deleted")
				require.Zero(t, res.Gaps)
				env.advance(cronEvery)
			}
			env.gh.date = nil
			res := env.run()

			require.Equal(t, len(failures), res.Gaps, "the run with a Date records what expired, and an empty hook lost nothing")
			if len(failures) > 0 {
				requireNoSilentLoss(t, env, failures)
			}
		})
	}
}

func TestAChangedHookStartsWithoutTheOldHooksClear(t *testing.T) {
	t.Parallel()
	env := newRedeliveryEnv(t)
	env.clock.WithLogger(quartz.NoOpLogger)
	env.run()
	env.advance(80 * time.Hour)

	env.gh.name = "/orgs/acme/hooks/8"
	res := env.run()

	require.Zero(t, res.Gaps, "the new hook has no attempt yet, so nothing of it expired")
	require.Empty(t, env.logged("redelivery gap"))
}

func TestAChangedHooksOpenRangeEndsAsAGapThatHoldsTheOverlapBelowIt(t *testing.T) {
	t.Parallel()
	env := newRedeliveryEnv(t, func(o *envOptions) { o.maxPages = 1 })
	env.clock.WithLogger(quartz.NoOpLogger)
	t0 := env.gh.now()
	a := env.gh.record("A", t0.Add(-time.Second), http200)
	env.run()
	l := env.gh.record("L", a.at.Add(-3*time.Second), 503)
	for i := range 150 {
		env.gh.record(fmt.Sprintf("b-%03d", i), t0.Add(time.Minute+time.Duration(i)*2*time.Second), http200)
	}
	env.advance(cronEvery)
	require.Equal(t, 1, env.run().Ranges, "the burst leaves a range down to A")
	requireListed(t, env, l)

	env.gh.name = "/orgs/acme/hooks/8"
	env.gh.attempts = nil
	env.advance(cronEvery)
	env.run()

	requireNoSilentLoss(t, env, []ghAttempt{l})
}

func TestAQuietHookLogsNoGapInTenDays(t *testing.T) {
	t.Parallel()

	for name, date := range map[string]func(int, string, time.Time) time.Time{
		"its Date is right":             nil,
		"its Date is 4 minutes behind":  dateOff(-4 * time.Minute),
		"its Date is an hour ahead":     dateOff(time.Hour),
		"its Date is a whole second":    func(_ int, _ string, at time.Time) time.Time { return at.Truncate(time.Second) },
		"its Date is an hour behind it": dateOff(-redelivery.DateSlack),
	} {
		for _, listed := range []string{"an empty hook", "a hook with one attempt every 80 hours"} {
			t.Run(listed+","+name, func(t *testing.T) {
				t.Parallel()
				env := newRedeliveryEnv(t)
				env.clock.WithLogger(quartz.NoOpLogger)
				env.gh.date = date
				want := 0
				if listed != "an empty hook" {
					for i := range 3 {
						env.gh.record(fmt.Sprintf("a-%d", i), env.gh.now().Add(time.Hour+time.Duration(i)*80*time.Hour+500*time.Millisecond), http200)
					}
					want = 1
				}

				var gaps int
				for range 10 * 24 * int(time.Hour/cronEvery) {
					gaps += env.run().Gaps
					env.advance(cronEvery)
				}

				require.Equal(t, want, gaps, "a silence is not a gap, and only the time before the first attempt is one")
				for _, line := range env.logged("redelivery gap") {
					require.Equal(t, "before tracking began", line["cause"])
				}
			})
		}
	}
}

func TestAGapReachesPastGitHubsRetentionByTheSlackASecondAndAsFarAsTheDateRunsAhead(t *testing.T) {
	t.Parallel()

	for _, lead := range []time.Duration{0, redelivery.DateMargin} {
		t.Run(lead.String(), func(t *testing.T) {
			t.Parallel()
			env := newRedeliveryEnv(t)
			env.clock.WithLogger(quartz.NoOpLogger)
			env.gh.record("A", env.gh.now().Add(-time.Minute), http200)
			env.run()
			env.advance(80 * time.Hour)
			env.gh.date = dateOff(lead)
			before := len(env.loggedGaps())
			require.Equal(t, 1, env.run().Gaps)

			retentionEnd := env.gh.now().Add(-redelivery.GitHubDeliveryRetention)
			require.Equal(t, redelivery.DateSlack+time.Second+lead, env.loggedGaps()[before].to.Sub(retentionEnd))
		})
	}
}

func TestAQuietHookLogsAGapOnlyAfterAnOutageThatTheDateMarginsDoNotCover(t *testing.T) {
	t.Parallel()

	threshold := redelivery.GitHubDeliveryRetention - redelivery.DateSlack - redelivery.DateMargin - redelivery.ReadOverlap
	for _, listed := range []string{"an empty hook", "a hook whose last attempt is 10 hours old"} {
		for _, tc := range []struct {
			apart time.Duration
			gaps  int
		}{{threshold - time.Minute, 0}, {threshold + time.Minute, 1}} {
			t.Run(fmt.Sprintf("%s,apart=%s", listed, tc.apart), func(t *testing.T) {
				t.Parallel()
				env := newRedeliveryEnv(t)
				env.clock.WithLogger(quartz.NoOpLogger)
				if listed != "an empty hook" {
					env.gh.record("A", env.gh.now().Add(-10*time.Hour), http200)
				}
				env.run()
				env.advance(tc.apart)

				require.Equal(t, tc.gaps, env.run().Gaps)
			})
		}
	}
}
