package faucet_test

import (
	"fmt"
	"slices"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/coder/faucet/faucet"
)

// These tests run a webhook for hours against a GitHub that lists its
// deliveries the way GitHub does, with a cron that calls Redeliver every ten
// minutes. A delivery that faucet answered with 503 or 400 on its first
// attempt is a failure, and each failure must end recovered, or as a give-up
// or a gap that the run counted and logged. silentLost counts the others.

type simPhase struct {
	from, to time.Duration // relative to the first run; negative is before it
	perHour  int
}

type simSpec struct {
	phases []simPhase
	// outage is when faucet answers 503, relative to the first run.
	outageFrom, outageLength time.Duration
	// cronDuringOutage is false when the platform that runs the cron is faucet
	// itself, which is down.
	cronDuringOutage bool
	// blip says that the first attempt at delivery number i of a phase fails
	// though faucet is up.
	blip func(phase, i int) bool
	// permanent says that faucet rejects every attempt at the delivery.
	permanent func(phase, i int) bool
	horizon   time.Duration
	// settle is how long before the horizon the last failure that counts can
	// happen. A failure needs time for its asks and its give-up.
	settle time.Duration
	pages  int
}

type simOutcome struct {
	failed, recovered, loggedGaveUp, inGap, silentLost int
	sumRecovered, sumGaveUp, sumGaps                   int
	runs                                               []faucet.RedeliverResult
}

type simDelivery struct {
	guid      string
	at        time.Time
	failedFor func(redelivery bool) bool
}

func runSim(t *testing.T, spec simSpec) simOutcome {
	t.Helper()
	if spec.settle == 0 {
		spec.settle = 150 * time.Minute
	}
	env := newRedeliveryEnv(t, func(o *envOptions) { o.maxPages = spec.pages })
	t0 := env.now()
	down := func(at time.Time) bool {
		return spec.outageLength > 0 && !at.Before(t0.Add(spec.outageFrom)) && at.Before(t0.Add(spec.outageFrom+spec.outageLength))
	}

	var deliveries []simDelivery
	for p, ph := range spec.phases {
		gap := time.Hour / time.Duration(ph.perHour)
		for i, at := 0, t0.Add(ph.from).Add(gap/2+7*time.Second); at.Before(t0.Add(ph.to)); i, at = i+1, at.Add(gap) {
			blip := spec.blip != nil && spec.blip(p, i)
			permanent := spec.permanent != nil && spec.permanent(p, i)
			deliveries = append(deliveries, simDelivery{
				guid: fmt.Sprintf("d-%d-%07d", p, i), at: at,
				failedFor: func(redelivery bool) bool { return permanent || (blip && !redelivery) },
			})
		}
	}
	sort.SliceStable(deliveries, func(i, j int) bool { return deliveries[i].at.Before(deliveries[j].at) })

	byGUID := map[string]simDelivery{}
	for _, d := range deliveries {
		byGUID[d.guid] = d
	}
	seen := map[string]bool{}
	env.gh.answer = func(guid string, at time.Time) int {
		redelivery := seen[guid]
		seen[guid] = true
		if down(at) || byGUID[guid].failedFor(redelivery) {
			return 503
		}
		return 200
	}

	var out simOutcome
	var failedFirst []simDelivery
	next := 0
	for env.now().Before(t0.Add(spec.horizon)) {
		now := env.now()
		for next < len(deliveries) && !deliveries[next].at.After(now) {
			d := deliveries[next]
			next++
			first := env.gh.deliver(d.guid, d.at)
			if first.status != 200 && !d.at.Before(t0) && d.at.Before(t0.Add(spec.horizon-spec.settle)) {
				failedFirst = append(failedFirst, d)
			}
		}
		if spec.cronDuringOutage || !down(now) {
			res := env.run()
			out.runs = append(out.runs, res)
			out.sumRecovered += res.Recovered
			out.sumGaveUp += res.GaveUpLimit + res.GaveUpExpired
			out.sumGaps += res.Gaps
		}
		env.advance(cronEvery)
	}

	gaveUp := map[string]bool{}
	for _, line := range env.logged("redelivery gave up") {
		gaveUp[line["delivery_id"]] = true
	}
	type window struct{ from, to time.Time }
	var gaps []window
	for _, line := range env.logged("redelivery gap") {
		gaps = append(gaps, window{logTime(t, line["from"]), logTime(t, line["to"])})
	}
	out.failed = len(failedFirst)
	for _, d := range failedFirst {
		switch {
		case env.stored(d.guid):
			out.recovered++
		case gaveUp[d.guid]:
			out.loggedGaveUp++
		case slices.ContainsFunc(gaps, func(w window) bool { return !d.at.Before(w.from) && !d.at.After(w.to) }):
			out.inGap++
		default:
			out.silentLost++
		}
	}
	requireCountedAndLoggedOnce(t, env, out)
	return out
}

// requireCountedAndLoggedOnce checks that the counts in the results of the
// runs and the lines in the log agree, and that no delivery has two lines.
func requireCountedAndLoggedOnce(t *testing.T, env *redeliveryEnv, out simOutcome) {
	t.Helper()
	recovered := env.logged("redelivery recovered")
	gaveUp := env.logged("redelivery gave up")
	require.Len(t, recovered, out.sumRecovered, "every recovery that a run counted is logged once")
	require.Len(t, gaveUp, out.sumGaveUp, "every give-up that a run counted is logged once")
	require.Len(t, env.logged("redelivery gap"), out.sumGaps, "every gap that a run counted is logged once")
	seen := map[string]bool{}
	for _, line := range append(recovered, gaveUp...) {
		require.False(t, seen[line["delivery_id"]], "%s has two final states", line["delivery_id"])
		seen[line["delivery_id"]] = true
	}
}

func (o simOutcome) String() string {
	return fmt.Sprintf("failed=%d recovered=%d gave_up_logged=%d in_gap=%d silent_lost=%d", o.failed, o.recovered, o.loggedGaveUp, o.inGap, o.silentLost)
}

func steady(perHour int, horizon time.Duration) []simPhase {
	return []simPhase{{from: -3 * time.Hour, to: horizon, perHour: perHour}}
}

func TestSimAnOutageIsRecoveredWithoutASilentLoss(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		spec simSpec
	}{
		{"150 an hour, 60 minute outage, faucet down", simSpec{phases: steady(150, 6*time.Hour), outageFrom: time.Minute, outageLength: 60 * time.Minute}},
		{"300 an hour, 60 minute outage, faucet down", simSpec{phases: steady(300, 6*time.Hour), outageFrom: time.Minute, outageLength: 60 * time.Minute}},
		{"600 an hour, 60 minute outage, faucet down", simSpec{phases: steady(600, 6*time.Hour), outageFrom: time.Minute, outageLength: 60 * time.Minute}},
		{"600 an hour, 30 minute outage, faucet down", simSpec{phases: steady(600, 6*time.Hour), outageFrom: time.Minute, outageLength: 30 * time.Minute}},
		{"300 an hour, 60 minute outage, cron keeps running", simSpec{phases: steady(300, 6*time.Hour), outageFrom: time.Minute, outageLength: 60 * time.Minute, cronDuringOutage: true}},
		{"1 an hour, 35 minute outage", simSpec{phases: steady(1, 6*time.Hour), outageFrom: -time.Minute, outageLength: 36 * time.Minute, cronDuringOutage: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tc.spec.horizon = 6 * time.Hour
			out := runSim(t, tc.spec)
			t.Log(out)
			require.Positive(t, out.failed)
			require.Zero(t, out.silentLost)
			require.Equal(t, out.failed, out.recovered, "an outage that ends is recovered in full")
		})
	}
}

func TestSimBlipsAtCoderOrgVolumeAreRecovered(t *testing.T) {
	t.Parallel()
	// coder/coder alone ran 320 workflow runs in an hour with 5 jobs each, so an
	// org hook that sends check_run and workflow_run carries over 4000
	// deliveries an hour. One in 100 fails once while faucet is up.
	out := runSim(t, simSpec{
		phases:  steady(4200, 6*time.Hour),
		blip:    func(_, i int) bool { return i%100 == 37 },
		horizon: 6 * time.Hour,
	})
	t.Log(out)

	require.Greater(t, out.failed, 100)
	require.Zero(t, out.silentLost)
	require.Equal(t, out.failed, out.recovered)
	for _, res := range out.runs[20:] {
		require.Zero(t, res.Ranges, "the sweep keeps up with 4200 an hour without a backlog once the first history is read")
	}
}

func TestSimAPageBoundBelowTheDocumentedVolumeLosesNothing(t *testing.T) {
	t.Parallel()
	// 2400 an hour is a page every 2.5 minutes. One failure in 200 deliveries is
	// one that a redelivery fixes.
	out := runSim(t, simSpec{
		phases:  steady(2400, 6*time.Hour),
		blip:    func(_, i int) bool { return i%200 == 100 },
		horizon: 6 * time.Hour,
	})
	t.Log(out)
	require.Greater(t, out.failed, 30)
	require.Zero(t, out.silentLost)
	require.Equal(t, out.failed, out.recovered)
}

func TestSimARetryThatNeedsASecondAskAtVolumeIsNotDropped(t *testing.T) {
	t.Parallel()
	// A delivery fails 29 minutes before the first run and again at the first
	// ask, because faucet is down until a minute later. Only the second ask
	// can store it, and 1500 deliveries an hour are listed meanwhile.
	spec := simSpec{
		phases:       steady(1500, 6*time.Hour),
		outageFrom:   -29*time.Minute - 30*time.Second,
		outageLength: 30*time.Minute + 90*time.Second,
		horizon:      6 * time.Hour,
	}
	spec.cronDuringOutage = true
	out := runSim(t, spec)
	t.Log(out)
	require.Zero(t, out.silentLost)
}

func TestSimDeliveriesThatFaucetAlwaysRejectAreGivenUpAndLoggedBesideRecoverableOnes(t *testing.T) {
	t.Parallel()
	out := runSim(t, simSpec{
		phases:    steady(600, 30*time.Hour),
		permanent: func(_, i int) bool { return i%50 == 7 },
		blip:      func(_, i int) bool { return i%50 == 21 },
		horizon:   30 * time.Hour,
		settle:    11 * time.Hour,
	})
	t.Log(out)

	require.Zero(t, out.silentLost)
	require.Positive(t, out.loggedGaveUp, "the permanent failures are given up, and logged")
	require.Positive(t, out.recovered, "and they do not keep the recoverable ones from being asked for")
	for _, res := range out.runs {
		require.LessOrEqual(t, res.Asked, faucet.DefaultMaxAsksPerRun)
	}
}

func TestSimAnOverloadIsReadWhenItEndsAndLosesNothing(t *testing.T) {
	t.Parallel()
	// Ten thousand deliveries an hour for an hour is more than one run can read
	// in the time between two runs. The runs keep their place, and read the
	// backlog when the load drops.
	phases := []simPhase{
		{from: -3 * time.Hour, to: 0, perHour: 150},
		{from: 0, to: time.Hour, perHour: 10000},
		{from: time.Hour, to: 8 * time.Hour, perHour: 150},
	}
	out := runSim(t, simSpec{
		phases:  phases,
		blip:    func(p, i int) bool { return p == 1 && i%100 == 37 },
		horizon: 8 * time.Hour,
	})
	t.Log(out)

	require.Greater(t, out.failed, 90)
	require.Zero(t, out.silentLost)
	require.Equal(t, out.failed, out.recovered, "the backlog was read in a later run, so nothing is lost")
	var peak int
	for _, res := range out.runs {
		peak = max(peak, res.Ranges)
	}
	require.Positive(t, peak, "the overload really did outrun one run")
	require.Zero(t, out.runs[len(out.runs)-1].Ranges)
}
