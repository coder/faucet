package faucet_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"cdr.dev/slog/v3"
	"github.com/coder/faucet/faucet"
)

const cronEvery = 10 * time.Minute

// logTime reads a time that the capture sink printed.
func logTime(t *testing.T, s string) time.Time {
	t.Helper()
	parsed, err := time.Parse("2006-01-02 15:04:05.999999999 -0700 MST", s)
	require.NoError(t, err)
	return parsed
}

func TestNewValidatesRedeliveryDeps(t *testing.T) {
	t.Parallel()

	store := newFakeRedeliveryStore(nil)
	hooks := newFakeGitHub(nil, store)
	for name, deps := range map[string]faucet.RedeliveryDeps{
		"no store":       {Hooks: hooks},
		"no hook admin":  {Store: store},
		"negative pages": {Store: store, Hooks: hooks, MaxPagesPerRun: -1},
		"negative asks":  {Store: store, Hooks: hooks, MaxAsksPerRun: -1},
	} {
		d := testDeps(store)
		d.Redelivery = &deps
		_, err := faucet.New(d)
		require.Error(t, err, name)
	}

	svc, err := faucet.New(testDeps(store))
	require.NoError(t, err)
	_, err = svc.Redeliver(context.Background())
	require.ErrorIs(t, err, faucet.ErrNoRedelivery, "a service without redelivery refuses a run")
}

func TestRedeliverAsksUntilFaucetStoresTheEventAndCountsItOnce(t *testing.T) {
	t.Parallel()
	env := newRedeliveryEnv(t)
	env.gh.record("g1", env.now().Add(-5*time.Minute), 503)

	first := env.run()
	require.Equal(t, 1, first.Tracked)
	require.Equal(t, 1, first.Asked)
	require.True(t, env.stored("g1"), "the redelivery reached faucet, which stored the event")

	second := env.run()
	require.Equal(t, 1, second.Recovered)
	require.Zero(t, second.Asked, "a stored delivery is not asked for again")
	require.Zero(t, second.Tracked, "the failed attempt is still in the list, and it must not revive a recovered delivery")
	require.Zero(t, env.run().Recovered, "a delivery recovers once")

	require.Len(t, env.logged("redelivery requested"), 1)
	recovered := env.logged("redelivery recovered")
	require.Len(t, recovered, 1)
	require.Equal(t, "g1", recovered[0]["delivery_id"])
}

func TestRedeliverTracksOnlyWhatNormalizeStores(t *testing.T) {
	t.Parallel()

	type webhook struct {
		name, event string
		payload     []byte
	}
	var webhooks []webhook
	for _, c := range []struct{ event, fixture string }{
		{"workflow_run", "workflow_run_completed"},
		{"check_run", "check_run_completed"},
		{"pull_request_review", "pull_request_review_submitted"},
		{"pull_request", "pull_request_closed"},
		{"issue_comment", "issue_comment_created_on_pr"},
	} {
		webhooks = append(webhooks,
			webhook{c.fixture, c.event, fixture(t, c.fixture)},
			webhook{c.fixture + " with action edited", c.event, mutate(t, c.fixture, func(doc map[string]any) { doc["action"] = "edited" })})
	}
	webhooks = append(webhooks,
		webhook{"ping", "ping", []byte(`{}`)},
		webhook{"a handled event with no action", "workflow_run", []byte(`{}`)},
		webhook{"an unhandled event", "push", []byte(`{"action":"created"}`)})

	var stored int
	for _, w := range webhooks {
		t.Run(w.name, func(t *testing.T) {
			t.Parallel()
			var doc struct{ Action string }
			require.NoError(t, json.Unmarshal(w.payload, &doc))
			ev, err := faucet.Normalize(w.event, w.payload, "d")
			want := 0
			if err == nil && ev != nil {
				want = 1
			}

			env := newRedeliveryEnv(t)
			env.gh.recordEvent("g", env.now().Add(-time.Minute), 500, w.event, doc.Action)
			res := env.run()

			require.Equal(t, want, res.Tracked, "the sweep tracks a failed delivery exactly when Normalize stores it")
			require.Equal(t, want, res.Asked)
		})
		ev, err := faucet.Normalize(w.event, w.payload, "d")
		if err == nil && ev != nil {
			stored++
		}
	}
	require.Equal(t, 5, stored, "the table covers every event that faucet stores")
}

func TestRedeliverDoesNotAskForADeliveryThatAlreadyWorked(t *testing.T) {
	t.Parallel()

	t.Run("a later attempt succeeded", func(t *testing.T) {
		t.Parallel()
		env := newRedeliveryEnv(t)
		env.gh.record("g", env.now().Add(-20*time.Minute), 500)
		env.gh.record("g", env.now().Add(-10*time.Minute), 202)

		res := env.run()
		require.Zero(t, res.Asked)
		require.Equal(t, 1, res.Recovered, "a delivery that GitHub delivered later is recovered, though faucet stored nothing")
	})
	t.Run("faucet stored the event though it answered 500", func(t *testing.T) {
		t.Parallel()
		env := newRedeliveryEnv(t)
		env.gh.record("g", env.now().Add(-20*time.Minute), 500)
		_, _, err := env.store.InsertEvent(context.Background(), faucet.Event{
			Source: "github", DeliveryID: "g", Type: "ci.run_completed", RepoID: repoID, Data: []byte(`{}`),
		})
		require.NoError(t, err)

		res := env.run()
		require.Zero(t, res.Asked, "an event that is stored needs no redelivery")
		require.Equal(t, 1, res.Recovered)
	})
}

func TestRedeliverGivesUpOnADeliveryFaucetAlwaysRejectsAfterTheAskLimit(t *testing.T) {
	t.Parallel()
	env := newRedeliveryEnv(t)
	env.gh.answer = failing(400, "bad")
	start := env.now()
	env.gh.record("bad", start.Add(-time.Minute), 400)

	var gaveUp, asked int
	for range 14 * 6 {
		res := env.run()
		gaveUp += res.GaveUpLimit
		asked += res.Asked
		env.advance(cronEvery)
	}

	require.Equal(t, faucet.MaxAsksPerDelivery, asked)
	require.Equal(t, 1, gaveUp, "the give-up is counted once")
	var offsets []time.Duration
	for _, at := range env.gh.askedTimes("bad") {
		offsets = append(offsets, at.Sub(start))
	}
	require.Equal(t, faucet.AskOffsets(), offsets, "each ask waits twice as long as the one before")

	lines := env.logged("redelivery gave up")
	require.Len(t, lines, 1, "and logged once")
	require.Equal(t, "bad", lines[0]["delivery_id"])
	require.Equal(t, "limit", lines[0]["reason"])
	require.Equal(t, fmt.Sprint(faucet.MaxAsksPerDelivery), lines[0]["asks"])
}

func TestRedeliverGivesUpWhenGitHubsRetentionEndsFirst(t *testing.T) {
	t.Parallel()
	env := newRedeliveryEnv(t)
	env.gh.answer = failing(400, "old")
	start := env.now()
	env.run()
	failedAt := start.Add(time.Hour)
	env.gh.record("old", failedAt, 400)

	env.advance(70 * time.Hour)
	var gaveUpExpired, gaveUpLimit int
	for env.now().Before(start.Add(76 * time.Hour)) {
		res := env.run()
		gaveUpExpired += res.GaveUpExpired
		gaveUpLimit += res.GaveUpLimit
		env.advance(cronEvery)
	}

	require.Equal(t, 1, gaveUpExpired, "the delivery is over 72 hours old, counted from its first attempt, and it ends once")
	require.Zero(t, gaveUpLimit)
	lines := env.logged("redelivery gave up")
	require.Len(t, lines, 1)
	require.Equal(t, "expired", lines[0]["reason"])
	asks := env.gh.askedTimes("old")
	require.NotEmpty(t, asks)
	for _, at := range asks {
		require.True(t, at.Before(failedAt.Add(faucet.GitHubDeliveryRetention)), "no ask goes out once GitHub's retention has ended")
	}
}

func TestAnAlwaysRejectedDeliveryDoesNotStarveNewFailures(t *testing.T) {
	t.Parallel()
	env := newRedeliveryEnv(t)
	permanents := make([]string, 100)
	for i := range permanents {
		permanents[i] = fmt.Sprintf("perm-%03d", i)
		env.gh.record(permanents[i], env.now().Add(-time.Duration(30-i/4)*time.Minute), 400)
	}
	env.gh.answer = failing(400, permanents...)

	const ticks = 30
	start := env.now()
	discovered := map[string]int{}
	for tick := range ticks {
		guid := fmt.Sprintf("fresh-%02d", tick)
		env.gh.record(guid, env.now().Add(-time.Minute), 503)
		discovered[guid] = tick
		env.run()
		env.advance(cronEvery)
	}

	for guid, tick := range discovered {
		require.True(t, env.stored(guid), "%s failed at tick %d and must be redelivered while %d permanent failures are still in line", guid, tick, len(permanents))
		times := env.gh.askedTimes(guid)
		require.Len(t, times, 1)
		require.LessOrEqual(t, times[0].Sub(start.Add(time.Duration(tick)*cronEvery)), 3*cronEvery, "%s waited too long behind the permanent failures", guid)
	}
	for _, guid := range permanents {
		require.LessOrEqual(t, len(env.gh.askedTimes(guid)), faucet.MaxAsksPerDelivery)
	}
}

func TestRedeliverSendsAtMostTheRunLimitAndTheRestWaitForTheNextRun(t *testing.T) {
	t.Parallel()
	env := newRedeliveryEnv(t)
	for i := range 120 {
		env.gh.record(fmt.Sprintf("g-%03d", i), env.now().Add(-time.Duration(120-i)*time.Second), 503)
	}

	require.Equal(t, faucet.DefaultMaxAsksPerRun, env.run().Asked)
	require.Equal(t, faucet.DefaultMaxAsksPerRun, env.run().Asked)
	third := env.run()
	require.Equal(t, 120-2*faucet.DefaultMaxAsksPerRun, third.Asked)
	require.Equal(t, 120, env.gh.totalAsks(), "every delivery was asked for once")
	env.run()
	for i := range 120 {
		require.True(t, env.stored(fmt.Sprintf("g-%03d", i)))
	}
}

func TestAFailedRequestStopsTheAsksButNotTheRunsAfterIt(t *testing.T) {
	t.Parallel()
	env := newRedeliveryEnv(t)
	for i := range 3 {
		env.gh.record(fmt.Sprintf("g-%d", i), env.now().Add(-time.Duration(3-i)*time.Minute), 503)
	}
	env.gh.askFault = func(string) error { return fmt.Errorf("GitHub answered the redelivery request with status 502") }

	res, err := env.svc.Redeliver(context.Background())
	require.ErrorContains(t, err, "502")
	require.Zero(t, res.Asked)
	require.Zero(t, env.gh.totalAsks(), "the first failure stops the asks: a wrong token or hook would otherwise fail every one")
	require.Equal(t, 3, res.Pending)

	env.gh.askFault = nil
	res = env.run()
	require.Equal(t, 2, res.Asked, "the delivery whose request failed spent an ask and waits, the other two go out")
}

// limitedAdmin lists through the fake GitHub and, while limited is set, asks
// through a GitHubHookAdmin whose GitHub answers with a rate limit.
type limitedAdmin struct {
	*fakeGitHub
	limited atomic.Bool
	real    faucet.GitHubHookAdmin
}

func (a *limitedAdmin) Redeliver(ctx context.Context, attemptID int64) error {
	if a.limited.Load() {
		return a.real.Redeliver(ctx, attemptID)
	}
	return a.fakeGitHub.Redeliver(ctx, attemptID)
}

func TestARateLimitedRequestIsNotAFinalStateAndTheScheduleAsksAgain(t *testing.T) {
	t.Parallel()
	env := newRedeliveryEnv(t)
	limit := newHookStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(http.StatusForbidden)
		_, _ = fmt.Fprint(w, `{"message":"You have exceeded a secondary rate limit."}`)
	})
	admin := &limitedAdmin{fakeGitHub: env.gh, real: newHookAdmin(t, limit, orgHook)}
	admin.limited.Store(true)
	deps := testDeps(env.store)
	deps.Clock = env.clock
	deps.Logger = slog.Make(env.logs)
	deps.Redelivery = &faucet.RedeliveryDeps{Store: env.store, Hooks: admin}
	svc, err := faucet.New(deps)
	require.NoError(t, err)
	env.svc = svc
	env.gh.record("g1", env.now().Add(-5*time.Minute), 503)

	res, err := env.svc.Redeliver(context.Background())
	require.ErrorContains(t, err, "rate limit", "the route answers an error, and the log names the limit")
	require.Zero(t, res.Asked)
	require.Equal(t, 1, res.Pending)
	require.Zero(t, res.GaveUpLimit+res.GaveUpExpired, "a rate limit gives nothing up")

	admin.limited.Store(false)
	require.Zero(t, env.run().Asked, "the refused request spent the first ask, so the second waits for its time")
	env.advance(cronEvery)
	require.Equal(t, 1, env.run().Asked)
	require.True(t, env.stored("g1"))
	require.Equal(t, 1, env.run().Recovered)
}

func TestASlowListCostsPagesAndNeverTheAsksOrTheSettle(t *testing.T) {
	t.Parallel()
	env := newRedeliveryEnv(t)
	for i := range 3 {
		env.gh.record(fmt.Sprintf("failed-%d", i), env.now().Add(-time.Duration(i+1)*time.Second), 503)
	}
	for i := range 1800 { // 18 pages, behind the failures
		env.gh.record(fmt.Sprintf("worked-%04d", i), env.now().Add(-time.Minute-time.Duration(i)*time.Second), http200)
	}
	// 18 pages need 2.7 seconds, and the run has 2. It reads for the first one.
	env.gh.listDelay = 150 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	res, err := env.svc.Redeliver(ctx)
	require.NoError(t, err, "the end of the time for the scan is like the page limit: no failure")
	require.Less(t, res.Pages, 18)
	require.Equal(t, 3, res.Tracked)
	require.Equal(t, 3, res.Asked, "a slow list does not cost the asks")
	require.Equal(t, 3, res.Pending, "a slow list does not cost the settle")
	require.Equal(t, 1, res.Ranges, "what the scan did not read is an open range")

	env.gh.listDelay = 0
	res = env.run()
	require.Zero(t, res.Ranges, "the next run reads the rest")
	require.Equal(t, 3, res.Recovered)
}

func TestSlowAsksStopStartingInTimeForARequestInFlightToEndAndForThePurge(t *testing.T) {
	t.Parallel()
	env := newRedeliveryEnv(t)
	for i := range 30 {
		env.gh.record(fmt.Sprintf("failed-%02d", i), env.now().Add(-time.Duration(i+1)*time.Second), 503)
	}
	// 30 asks need 3 seconds, and the run has 2.
	env.gh.askDelay = 100 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	res, err := env.svc.Redeliver(ctx)
	require.NoError(t, err, "no ask is in flight when the time ends, so none is canceled and none is lost")
	require.Positive(t, res.Asked)
	require.Less(t, res.Asked, 30)
	require.Equal(t, 30, res.Pending)
	require.Len(t, env.gh.askedTimes("failed-29"), 1, "the delivery that waited longest went first")
}

func TestAFailedFirstPageStopsTheAsksButStillSettles(t *testing.T) {
	t.Parallel()
	env := newRedeliveryEnv(t)
	env.gh.answer = failing(400, "old", "recent")
	start := env.now()
	env.run()
	env.gh.record("old", start.Add(time.Minute), 400)
	env.advance(71*time.Hour + 59*time.Minute)
	env.gh.record("recent", env.now().Add(-time.Minute), 400)
	env.run()
	require.Len(t, env.gh.askedTimes("recent"), 1)

	env.advance(11 * time.Minute)
	env.gh.listFault = func(int, string) error { return fmt.Errorf("GitHub answered the delivery list with status 401") }
	res, err := env.svc.Redeliver(context.Background())

	require.ErrorContains(t, err, "401")
	require.Zero(t, res.Asked, "a token that cannot list deliveries cannot be trusted with redelivery requests")
	require.Len(t, env.gh.askedTimes("recent"), 1, "the second ask of recent was due, and it did not go out")
	require.Equal(t, 1, res.GaveUpExpired, "an expiry needs no GitHub, so a broken token must not hide it")
}

func TestRedeliverReadsAListThatDoesNotFitInOneRunAcrossRuns(t *testing.T) {
	t.Parallel()

	for name, style := range map[string]cursorStyle{"keyset cursors": keysetCursor, "offset cursors": offsetCursor} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			env := newRedeliveryEnv(t, func(o *envOptions) { o.maxPages, o.style = 3, style })
			const attempts = 1000
			var failed []string
			for i := range attempts {
				guid := fmt.Sprintf("a-%04d", i)
				at := env.now().Add(-time.Duration(attempts-i) * 10 * time.Second)
				status := 200
				if i%190 == 5 {
					status = 503
					failed = append(failed, guid)
				}
				env.gh.record(guid, at, status)
			}
			require.Len(t, failed, 6)

			var runs int
			for !allStored(env, failed) {
				runs++
				require.Less(t, runs, 12, "the backlog must be read to its end within a few runs")
				env.run()
				env.advance(cronEvery)
			}
			require.Greater(t, runs, 1, "the list is longer than one run reads")
			require.Zero(t, env.run().Ranges, "the whole list was read")
			gaps := env.logged("redelivery gap")
			require.Len(t, gaps, 1, "nothing expired, so the only gap is the time before tracking began")
			require.Equal(t, "before tracking began", gaps[0]["cause"])
		})
	}
}

func allStored(env *redeliveryEnv, guids []string) bool {
	for _, guid := range guids {
		if !env.stored(guid) {
			return false
		}
	}
	return true
}

func TestTheFirstRunReadsTheLastDayAndRecordsTheRestOfRetentionAsOneGap(t *testing.T) {
	t.Parallel()
	env := newRedeliveryEnv(t)
	start := env.now()
	env.gh.record("inside", start.Add(-20*time.Hour), 503)
	env.gh.record("before", start.Add(-30*time.Hour), 503)

	first := env.run()
	require.Equal(t, 1, first.Tracked, "only the delivery inside the lookback is tracked")
	require.Equal(t, 1, first.Gaps)
	require.Zero(t, env.run().Gaps, "the gap is recorded once")

	lines := env.logged("redelivery gap")
	require.Len(t, lines, 1)
	require.Equal(t, "before tracking began", lines[0]["cause"])
	require.Equal(t, "gap", lines[0]["reason"])
	from, to := logTime(t, lines[0]["from"]), logTime(t, lines[0]["to"])
	require.True(t, start.Add(-faucet.GitHubDeliveryRetention).Equal(from))
	require.True(t, start.Add(-faucet.RedeliveryStartLookback).Equal(to))
	require.False(t, env.stored("before"), "the delivery before the lookback is inside the recorded gap, so no ask goes out for it")
}

func TestAnEmptyListIsCoveredSoTheFirstGapIsNotRecordedAgain(t *testing.T) {
	t.Parallel()
	env := newRedeliveryEnv(t)

	var gaps int
	for range 5 {
		gaps += env.run().Gaps
		env.advance(cronEvery)
	}

	require.Equal(t, 1, gaps)
}

func TestAnEmptyListMovesThePositionToGitHubsClockNotFaucets(t *testing.T) {
	t.Parallel()

	// skew is GitHub's clock minus faucet's. A position on faucet's clock puts
	// the next failure of a GitHub that runs behind it under the read bound,
	// where no run reads it and nothing counts it.
	for _, skew := range []time.Duration{-time.Hour, -10 * time.Minute, -4 * time.Minute, 0, 10 * time.Minute} {
		t.Run(skew.String(), func(t *testing.T) {
			t.Parallel()
			env := newRedeliveryEnv(t)
			env.gh.skew = skew

			require.Equal(t, 1, env.run().Gaps, "an empty list is the first run")
			env.advance(time.Minute)
			env.gh.record("g1", env.gh.now(), 503)

			res := env.run()
			require.Equal(t, 1, res.Tracked, "the failure is new on GitHub's clock, so the run reads it")
			require.Equal(t, 1, res.Asked)
			require.True(t, env.stored("g1"))
		})
	}
}

func TestAReplacedHooksPendingDeliveriesAreNotAskedForOnTheNewHook(t *testing.T) {
	t.Parallel()
	env := newRedeliveryEnv(t)
	env.gh.answer = func(string, time.Time) int { return 503 } // faucet stays down
	env.gh.record("old-1", env.now().Add(-5*time.Minute), 503)
	env.gh.record("old-2", env.now().Add(-4*time.Minute), 503)
	require.Equal(t, 2, env.run().Asked)

	// The host replaces the webhook. The new one has its own list, and an attempt
	// ID of the old one is a 404 there.
	env.gh.name = "/orgs/acme/hooks/8"
	env.gh.attempts = nil
	env.gh.record("new-1", env.now(), 503)
	env.advance(cronEvery) // the second ask of both old deliveries is due

	res, err := env.svc.Redeliver(context.Background())
	require.NoError(t, err, "a request that the new hook would refuse is never sent")
	require.Equal(t, 1, res.Asked, "the delivery of the new hook goes out")
	require.Len(t, env.gh.askedTimes("new-1"), 1)
	require.Len(t, env.gh.askedTimes("old-1"), 1, "the old hook's deliveries are not asked for again")
	require.Equal(t, 3, res.Pending)

	env.advance(faucet.GitHubDeliveryRetention)
	res = env.run()
	require.Equal(t, 3, res.GaveUpExpired, "the old deliveries end counted, as expired, with everything else that GitHub forgot")
	require.Zero(t, res.Pending)
}

func TestRedeliverRecordsAGapWhenARangeExpiresUnread(t *testing.T) {
	t.Parallel()
	env := newRedeliveryEnv(t, func(o *envOptions) { o.maxPages = 1 })
	for i := range 600 {
		env.gh.record(fmt.Sprintf("a-%04d", i), env.now().Add(-time.Duration(600-i)*6*time.Second), 200)
	}

	first := env.run()
	require.Equal(t, 1, first.Ranges, "one page was read, and the rest of the list is a range")

	var gaps int
	end := env.now().Add(faucet.GitHubDeliveryRetention + time.Hour)
	for i := 0; env.now().Before(end); i++ {
		env.advance(cronEvery)
		env.gh.record(fmt.Sprintf("trickle-%04d", i), env.now().Add(-time.Minute), 200)
		gaps += env.run().Gaps
	}

	require.Equal(t, 1, gaps, "the range expired unread, and its gap is counted once")
	var causes []string
	for _, line := range env.logged("redelivery gap") {
		causes = append(causes, line["cause"])
	}
	require.Equal(t, []string{"before tracking began", "expired unread"}, causes)
}

func TestRedeliverRecordsOnlyTheExpiredPartOfARangeAsAGap(t *testing.T) {
	t.Parallel()
	env := newRedeliveryEnv(t, func(o *envOptions) { o.maxPages = 3 })
	start := env.now()
	for i := range 2700 {
		env.gh.record(fmt.Sprintf("a-%04d", i), start.Add(time.Duration(i+1)*2*time.Minute), 200)
	}
	env.run()
	env.advance(60 * time.Hour)
	require.Equal(t, 1, env.run().Ranges, "three pages cannot read what arrived in 60 hours")

	env.advance(30 * time.Hour)
	env.useMaxPages(30)
	res := env.run()

	require.Equal(t, 1, res.Gaps)
	require.Zero(t, res.Ranges, "the part of the range that GitHub still kept was read to its end")
	lines := env.logged("redelivery gap")
	require.Len(t, lines, 2)
	require.Equal(t, "expired unread", lines[1]["cause"])
	from, to := logTime(t, lines[1]["from"]), logTime(t, lines[1]["to"])
	require.WithinDuration(t, start.Add(-faucet.RedeliveryStartLookback), from, time.Minute, "the gap starts where the position of the earlier run was: it read an empty list, so GitHub's clock less the lookback")
	require.WithinDuration(t, env.now().Add(-faucet.GitHubDeliveryRetention), to, time.Minute, "and ends where GitHub's list begins")
}

func TestTwoRunsAtOnceSendOneRequestPerDelivery(t *testing.T) {
	t.Parallel()
	env := newRedeliveryEnv(t)
	env.gh.record("g", env.now().Add(-time.Minute), 503)

	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	env.gh.listGate = func() {
		once.Do(func() {
			close(entered)
			<-release
		})
	}
	type outcome struct {
		res faucet.RedeliverResult
		err error
	}
	firstDone := make(chan outcome, 1)
	go func() {
		res, err := env.svc.Redeliver(context.Background())
		firstDone <- outcome{res, err}
	}()
	<-entered

	second, err := env.svc.Redeliver(context.Background())
	require.NoError(t, err)
	require.True(t, second.Busy, "a run that starts while another holds the lease does nothing")
	require.Zero(t, second.Asked)

	close(release)
	first := <-firstDone
	require.NoError(t, first.err)
	require.False(t, first.res.Busy)
	require.Equal(t, 1, env.gh.totalAsks())
	require.Equal(t, 1, first.res.Asked)
}

func TestARunThatDiedLeavesALeaseThatRunsOut(t *testing.T) {
	t.Parallel()
	env := newRedeliveryEnv(t)
	_, ok, err := env.store.AcquireRedeliveryLease(context.Background(), env.gh.Hook(), env.now(), 2*time.Minute)
	require.NoError(t, err)
	require.True(t, ok, "a run took the lease and died")

	require.True(t, env.run().Busy)
	env.advance(3 * time.Minute)
	require.False(t, env.run().Busy)
	require.False(t, env.run().Busy, "a finished run frees the lease")
}

func TestChangingTheHookStartsTheScanAgainAndCountsTheOpenRangesAsGaps(t *testing.T) {
	t.Parallel()
	env := newRedeliveryEnv(t, func(o *envOptions) { o.maxPages = 1 })
	for i := range 500 {
		env.gh.record(fmt.Sprintf("a-%04d", i), env.now().Add(-time.Duration(500-i)*time.Minute/10), 200)
	}
	require.Equal(t, 1, env.run().Ranges)

	env.gh.name = "/orgs/acme/hooks/8"
	res := env.run()

	require.Equal(t, 2, res.Gaps, "the range of the old hook can never be read through the new one, and the new hook starts its own tracking")
	require.Len(t, env.logged("redelivery hook changed: the scan starts again"), 1)
	var causes []string
	for _, line := range env.logged("redelivery gap") {
		causes = append(causes, line["cause"])
	}
	require.Equal(t, []string{"before tracking began", "hook changed", "before tracking began"}, causes)
}

func TestALateAttemptInsideTheOverlapIsStillRead(t *testing.T) {
	t.Parallel()
	env := newRedeliveryEnv(t)
	env.gh.record("early", env.now().Add(-time.Minute), 200)
	env.run()
	env.advance(cronEvery)

	env.gh.record("late", env.now().Add(-cronEvery-2*time.Minute), 503)
	res := env.run()

	require.Equal(t, 1, res.Tracked, "GitHub lists an attempt that ended late below newer ones, and the overlap sees it")
	require.True(t, env.stored("late"))
}

func TestRedeliverForgetsFinalDeliveriesAfterGitHubsRetention(t *testing.T) {
	t.Parallel()
	env := newRedeliveryEnv(t)
	env.gh.record("g", env.now().Add(-time.Minute), 503)
	env.run()
	env.run()
	require.Equal(t, 1, env.store.rowCount())

	env.advance(faucet.GitHubDeliveryRetention + time.Hour)
	env.run()

	require.Zero(t, env.store.rowCount(), "a final delivery is not kept for longer than GitHub keeps its attempts")
}

func TestEveryRedeliveryLogLineHoldsNoCursorOrURL(t *testing.T) {
	t.Parallel()
	env := newRedeliveryEnv(t, func(o *envOptions) { o.maxPages = 2 })
	for i := range 300 {
		env.gh.record(fmt.Sprintf("a-%04d", i), env.now().Add(-time.Duration(300-i)*time.Minute/10), 200)
	}
	env.run()
	env.advance(cronEvery)
	env.run()

	text := env.logs.text()
	require.NotContains(t, text, "v1_", "a cursor is GitHub's opaque value and stays out of logs")
	require.NotContains(t, text, "http")
}
