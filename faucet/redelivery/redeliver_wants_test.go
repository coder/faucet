package redelivery_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"

	"cdr.dev/slog/v3"
	"github.com/coder/superblink-events/faucet"
	"github.com/coder/superblink-events/faucet/redelivery"
)

// pushInterest is a live interest in the push events of one repository, as a
// host's Deps.Wants reports it. A predicate keeps what was live when a run
// loaded it.
type pushInterest struct {
	repoID int64
	live   bool
	err    error
	calls  int
}

func (p *pushInterest) wants(context.Context) (func(event, action string, repoID int64) bool, error) {
	p.calls++
	if p.err != nil {
		return nil, p.err
	}
	live := p.live
	return func(event, _ string, repoID int64) bool { return live && event == "push" && repoID == p.repoID }, nil
}

func newWantingEnv(t *testing.T, interest *pushInterest, opts ...func(*envOptions)) *redeliveryEnv {
	t.Helper()
	return newRedeliveryEnv(t, append(opts, func(o *envOptions) { o.wants = interest.wants })...)
}

// failedPush records a push that faucet failed a minute ago, which GitHub
// lists with repository_id repoID, or with null when repoID is 0.
func (e *redeliveryEnv) failedPush(guid string, repoID int64) {
	e.gh.add(ghAttempt{guid: guid, at: e.now().Add(-time.Minute), status: http.StatusInternalServerError, event: "push", repoID: repoID})
}

func TestRedeliverTracksAFailedDeliveryThatALiveInterestWants(t *testing.T) {
	t.Parallel()
	env := newWantingEnv(t, &pushInterest{repoID: 42, live: true})
	env.failedPush("wanted", 42)
	env.failedPush("other-repo", 43)
	env.failedPush("no-repo", 0)

	res := env.run()
	require.Equal(t, 1, res.Tracked)
	require.Equal(t, 1, res.Asked)
	require.Len(t, env.gh.askedTimes("wanted"), 1, "the run that tracks a wanted delivery asks for it")
	require.False(t, env.tracked("other-repo"), "the interest wants one repository")
	require.False(t, env.tracked("no-repo"), "a delivery that GitHub lists without a repository is in none")
}

func TestAWantedDeliveryRecoversWhenFaucetAnswersItsRedeliveryWith200(t *testing.T) {
	t.Parallel()
	env := newWantingEnv(t, &pushInterest{repoID: 42, live: true})
	env.failedPush("pushed", 42)
	first := env.run()
	require.Equal(t, 1, first.Tracked)
	require.Equal(t, 1, first.Asked)

	env.advance(cronEvery)
	second := env.run()
	require.Equal(t, 1, second.Recovered, "faucet stored the copy and answered 200")
	require.Zero(t, second.Pending)
	require.Zero(t, second.Asked, "the second ask was due, and a recovered delivery needs none")
	require.Len(t, env.gh.askedTimes("pushed"), 1)
	recovered := env.logged("redelivery recovered")
	require.Len(t, recovered, 1)
	require.Equal(t, "pushed", recovered[0]["delivery_id"])
}

func TestALapsedInterestEndsItsPendingDeliveryAsRecoveredOnA2xxAttempt(t *testing.T) {
	t.Parallel()
	interest := &pushInterest{repoID: 42, live: true}
	env := newWantingEnv(t, interest)
	env.gh.answer = func(string, time.Time) int { return http.StatusAccepted }
	env.failedPush("pushed", 42)
	first := env.run()
	require.Equal(t, 1, first.Tracked)
	require.Equal(t, 1, first.Asked)

	interest.live = false
	env.advance(cronEvery)
	wants, err := interest.wants(context.Background())
	require.NoError(t, err)
	require.False(t, faucet.StoresDelivery("push", ""))
	require.False(t, wants("push", "", 42))

	second := env.run()
	require.Equal(t, 1, second.Recovered, "faucet answered the redelivery 202, so the delivery has reached faucet")
	require.Zero(t, second.Pending)
	require.Zero(t, second.Asked, "the second ask was due, and a recovered delivery needs none")
	require.Len(t, env.gh.askedTimes("pushed"), 1)
	recovered := env.logged("redelivery recovered")
	require.Len(t, recovered, 1)
	require.Equal(t, "pushed", recovered[0]["delivery_id"])
}

// A run that cannot read the raw interests must not move past a raw delivery
// that one of them wanted, so it tracks every delivery that the fallback that
// Wants returned with its error wants, and every delivery without one.
func TestAWantsThatFailsTracksEveryDeliveryThatFaucetMayWant(t *testing.T) {
	t.Parallel()
	outage := errors.New("the interests are out of reach")
	for name, tc := range map[string]struct {
		fallback   func(event, action string, repoID int64) bool
		starsAsked bool
	}{
		"with the fallback that Wants returned": {func(event, _ string, _ int64) bool { return event == "push" }, false},
		"without one":                           {nil, true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			env := newRedeliveryEnv(t, func(o *envOptions) {
				o.wants = func(context.Context) (func(string, string, int64) bool, error) { return tc.fallback, outage }
			})
			env.failedPush("pushed", 42)
			env.gh.add(ghAttempt{guid: "starred", at: env.now().Add(-time.Minute), status: http.StatusInternalServerError, event: "star", repoID: 42})
			env.gh.record("typed", env.now().Add(-time.Minute), 500)

			res, err := env.runner.Run(context.Background())
			require.NoError(t, err, "a run without the raw interests is no failed run")
			require.True(t, env.tracked("typed"))
			require.True(t, env.tracked("pushed"), "a raw delivery that an interest may want is asked for")
			require.Equal(t, tc.starsAsked, env.tracked("starred"))
			asked := 2
			if tc.starsAsked {
				asked = 3
			}
			require.Equal(t, asked, res.Asked)
			lost := env.logged("the raw interests could not be read, this run tracks every delivery that faucet may store raw")
			require.Len(t, lost, 1)
			require.Contains(t, lost[0]["error"], outage.Error())
		})
	}
}

// In a synctest bubble the clock moves only when every goroutine waits, so the
// call ends at the same time on every machine, whatever its load.
func TestAWantsThatDoesNotAnswerCostsATenthOfTheRunAndNoTypedWork(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		const limit = 2 * time.Second
		env := newRedeliveryEnv(t)
		env.gh.record("typed", env.now().Add(-time.Minute), 500)
		spy := &purgeSpy{fakeRedeliveryStore: env.store}
		var cut time.Duration
		start := time.Now()
		runner, err := redelivery.New(redelivery.Deps{
			Store: spy, Hooks: env.gh, Clock: env.clock, Logger: slog.Make(env.logs),
			Wants: func(ctx context.Context) (func(string, string, int64) bool, error) {
				<-ctx.Done()
				cut = time.Since(start)
				return nil, ctx.Err()
			},
		})
		require.NoError(t, err)
		ctx, cancel := context.WithTimeout(context.Background(), limit)
		defer cancel()

		res, err := runner.Run(ctx)

		require.NoError(t, err)
		require.Equal(t, limit/10, cut, "the call is cut at a tenth of the run")
		require.Equal(t, 1, res.Pages)
		require.Equal(t, 1, res.Asked, "the typed delivery is asked for")
		spy.mu.Lock()
		defer spy.mu.Unlock()
		require.Equal(t, 1, spy.calls)
		require.True(t, spy.alive, "the purge runs inside the run's time")
	})
}

func TestARunThatHoldsTheLeaseLoadsWhatIsWantedOnce(t *testing.T) {
	t.Parallel()
	interest := &pushInterest{repoID: 42, live: true}
	env := newWantingEnv(t, interest, func(o *envOptions) { o.pageSize = 1 })
	for i := range 3 {
		env.gh.record(fmt.Sprintf("g-%d", i), env.now().Add(-time.Duration(i+1)*time.Minute), http200)
	}

	require.Equal(t, 3, env.run().Pages)
	require.Equal(t, 1, interest.calls, "once per run, not once per page")

	_, ok, err := env.store.AcquireRedeliveryLease(context.Background(), env.gh.Hook(), env.now(), 2*time.Minute)
	require.NoError(t, err)
	require.True(t, ok)
	require.True(t, env.run().Busy)
	require.Equal(t, 1, interest.calls, "a run that finds the lease busy does not call it")

	env.advance(3 * time.Minute)
	require.False(t, env.run().Busy)
	require.Equal(t, 2, interest.calls)
}

func TestWantsLeavesTheOutcomesOfTypedDeliveriesAsTheyWere(t *testing.T) {
	t.Parallel()

	t.Run("a pending typed delivery recovers on a 2xx attempt", func(t *testing.T) {
		t.Parallel()
		env := newWantingEnv(t, &pushInterest{repoID: 42, live: true})
		env.gh.answer = failing(503, "typed")
		env.gh.record("typed", env.now().Add(-time.Minute), 503)
		require.Equal(t, 1, env.run().Tracked)

		env.advance(cronEvery)
		env.gh.record("typed", env.now().Add(-time.Minute), http.StatusAccepted)
		res := env.run()
		require.Equal(t, 1, res.Recovered)
		require.Zero(t, res.Asked)
		require.Zero(t, res.Pending)
	})
	t.Run("a 2xx attempt of an untracked delivery changes no delivery", func(t *testing.T) {
		t.Parallel()
		env := newWantingEnv(t, &pushInterest{repoID: 42, live: true})
		env.gh.answer = failing(503, "typed")
		env.gh.record("typed", env.now().Add(-time.Minute), 503)
		require.Equal(t, 1, env.run().Asked)
		require.Zero(t, env.run().Asked)
		before := env.store.deliveries()
		require.Len(t, before, 1)

		env.advance(time.Minute)
		at := env.now().Add(-time.Second)
		env.gh.add(ghAttempt{guid: "push-of-another-repo", at: at, status: http200, event: "push", repoID: 43})
		env.gh.add(ghAttempt{guid: "wanted-push", at: at, status: http200, event: "push", repoID: 42})
		env.gh.record("untracked", at, http200)
		res := env.run()
		require.Zero(t, res.Tracked)
		require.Zero(t, res.Recovered)
		require.Equal(t, before, env.store.deliveries())
	})
}
