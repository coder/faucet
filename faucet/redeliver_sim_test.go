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
	// redeliveriesFailUntil makes faucet reject every redelivery before that
	// time, relative to the first run, though it accepts first attempts.
	redeliveriesFailUntil time.Duration
	// countFrom is the earliest first attempt that the outcome counts,
	// relative to the first run.
	countFrom time.Duration
	horizon   time.Duration
	// settle is how long before the horizon the last failure that counts can
	// happen. A failure needs time for its asks and its give-up.
	settle time.Duration
	pages  int
	// pageSize is the attempts on a page of GitHub's list. Zero means 100.
	pageSize int
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
	env := newRedeliveryEnv(t, func(o *envOptions) {
		o.maxPages = spec.pages
		if spec.pageSize != 0 {
			o.pageSize = spec.pageSize
		}
	})
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
		if down(at) || byGUID[guid].failedFor(redelivery) || (redelivery && at.Before(t0.Add(spec.redeliveriesFailUntil))) {
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
			if first.status != 200 && !d.at.Before(t0.Add(spec.countFrom)) && d.at.Before(t0.Add(spec.horizon-spec.settle)) {
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
	gaps := env.loggedGaps()
	out.failed = len(failedFirst)
	for _, d := range failedFirst {
		switch {
		case env.stored(d.guid):
			out.recovered++
		case gaveUp[d.guid]:
			out.loggedGaveUp++
		case slices.ContainsFunc(gaps, func(w gapWindow) bool { return w.holds(d.at) }):
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
	return fmt.Sprintf("failed=%d recovered=%d lost_logged=%d lost_in_gap=%d lost_silent=%d sum(gave_up)=%d sum(gaps)=%d",
		o.failed, o.recovered, o.loggedGaveUp, o.inGap, o.silentLost, o.sumGaveUp, o.sumGaps)
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
			t.Logf("RESULT %s: %s", tc.name, out)
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
	t.Logf("RESULT 4200 an hour, 1%% blips: %s", out)

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
	t.Logf("RESULT 2400 an hour, a failure every 5 minutes: %s", out)
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
	t.Logf("RESULT 1500 an hour, a 30 minute outage that ends after the first run: %s", out)
	require.Zero(t, out.silentLost)
}

func TestSimAFailureThatNeedsASecondAskAtVolumeIsNotDropped(t *testing.T) {
	t.Parallel()
	// One delivery, G, fails 29 minutes before the first run. The first ask
	// fails as well, because faucet rejects redeliveries until a minute after
	// the first run. Only the second ask can store G. 1500 deliveries an hour
	// are listed meanwhile.
	const perHour = 1500
	gap := time.Hour / perHour
	g := int((3*time.Hour - 29*time.Minute - gap/2 - 7*time.Second) / gap)

	out := runSim(t, simSpec{
		phases:                steady(perHour, 6*time.Hour),
		blip:                  func(_, i int) bool { return i == g },
		redeliveriesFailUntil: time.Minute,
		countFrom:             -30 * time.Minute,
		horizon:               6 * time.Hour,
	})

	t.Logf("RESULT 1500 an hour, G fails at -29m and at its first ask: %s", out)
	require.Equal(t, 1, out.failed)
	require.Equal(t, 1, out.recovered)
	require.Zero(t, out.silentLost)
}

// steadyCapacity is the attempts an hour that a run keeps up with, as the README
// states it. A run reads the attempts since the one it last saw, less the
// overlap, and it needs one page past the last of them to learn that it is done.
// That is two attempts less than the run holds, because it also counts the
// newest attempt that the run before it read.
func steadyCapacity(perRun int, covered time.Duration) int {
	return (perRun - 2) * int(time.Hour/covered)
}

func TestSimTheStatedCapacityKeepsUpAndATenthMoreDoesNot(t *testing.T) {
	t.Parallel()
	// The same law at a tenth of the scale: pages of 10 attempts, 20 a run.
	const pages, pageSize = 20, 10
	capacity := steadyCapacity(pages*pageSize, cronEvery+faucet.RedeliveryReadOverlap)

	for _, tc := range []struct {
		name    string
		perHour int
		grows   bool
	}{
		{"at the stated capacity", capacity, false},
		{"a tenth above it", capacity * 11 / 10, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			out := runSim(t, simSpec{
				phases:   []simPhase{{from: 0, to: 4 * time.Hour, perHour: tc.perHour}},
				horizon:  4 * time.Hour,
				pages:    pages,
				pageSize: pageSize,
			})
			t.Logf("RESULT %s (%d an hour): ranges after each run %v", tc.name, tc.perHour, rangesOf(out.runs))
			if tc.grows {
				require.Greater(t, out.runs[len(out.runs)-1].Ranges, 5, "more than a run can read is left as ranges that grow")
				return
			}
			for i, res := range out.runs {
				require.Zero(t, res.Ranges, "run %d leaves a range at the stated capacity", i)
			}
		})
	}
}

func rangesOf(runs []faucet.RedeliverResult) []int {
	counts := make([]int, len(runs))
	for i, res := range runs {
		counts[i] = res.Ranges
	}
	return counts
}
