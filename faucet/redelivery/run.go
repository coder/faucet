// Package redelivery asks GitHub to deliver again the webhook deliveries that
// faucet failed, until faucet stores each event or gives it up. A host builds a
// Runner with New and calls Run on a schedule. A Store keeps what a run needs
// between runs, and a HookAdmin is GitHub's webhook API.
package redelivery

import (
	"context"
	"errors"
	"time"

	"golang.org/x/xerrors"

	"cdr.dev/slog/v3"
	"github.com/coder/quartz"
	"github.com/coder/superblink-events/faucet"
)

const (
	// DefaultMaxPagesPerRun is the most pages of GitHub's delivery list that
	// one run reads when Deps.MaxPagesPerRun is zero. A page holds 100
	// attempts.
	DefaultMaxPagesPerRun = 20
	// DefaultMaxAsksPerRun is the most redelivery requests that one run sends
	// when Deps.MaxAsksPerRun is zero.
	DefaultMaxAsksPerRun = 50
	// MaxAsksPerDelivery is the number of times a run asks GitHub to deliver
	// one event again, before it gives the delivery up with GiveUpLimit.
	MaxAsksPerDelivery = 7
	// ReadOverlap is how far below its position a run reads again.
	// GitHub writes an attempt down when it ends, and an attempt that ended
	// late appears below one that ended earlier. The delivery timeout is 10
	// seconds, so this is a wide margin.
	ReadOverlap = 5 * time.Minute
	// StartLookback is how far below the newest attempt the first
	// run of a webhook reads. The part of GitHub's retention before it is
	// recorded as one gap, so the first run does not spend its pages on a
	// history that nobody waits for.
	StartLookback = 24 * time.Hour

	redeliveryLeaseTTL  = 2 * time.Minute
	redeliveryAskSettle = 5 * time.Minute
)

// A run whose context has a deadline spends the time left at its start in
// four parts, so that the settle, the asks and the purge get their turn
// whatever the delivery list costs. A slow list leaves a range open, as the
// page limit does, and the next run reads on.
const (
	// redeliveryWantsShare is the share after which a call of Deps.Wants is
	// canceled, so that a slow call leaves time to read the list.
	redeliveryWantsShare = 0.1
	// redeliveryScanShare is the share for reading the list. A page in flight
	// when it ends is canceled and not counted.
	redeliveryScanShare = 0.5
	// redeliveryAskStartShare is the share after which no ask starts, so that
	// an ask that started before it has time to finish.
	redeliveryAskStartShare = 0.8
	// redeliveryAskEndShare is the share at which an ask in flight is
	// canceled. The purge has the rest.
	redeliveryAskEndShare = 0.9
)

// errScanBudget says that the time for reading the list ran out.
var errScanBudget = xerrors.New("the time for reading the delivery list ran out")

// runBudget holds the instants that split a run's deadline. The zero value
// means no deadline, and nothing ends early. A context deadline is wall time,
// so the split does not use the runner's clock.
type runBudget struct {
	wantsEnd, scanEnd, askStart, askEnd time.Time
}

func newRunBudget(ctx context.Context) runBudget {
	deadline, ok := ctx.Deadline()
	if !ok {
		return runBudget{}
	}
	start := time.Now()
	left := deadline.Sub(start)
	at := func(share float64) time.Time { return start.Add(time.Duration(float64(left) * share)) }
	return runBudget{
		wantsEnd: at(redeliveryWantsShare), scanEnd: at(redeliveryScanShare),
		askStart: at(redeliveryAskStartShare), askEnd: at(redeliveryAskEndShare),
	}
}

// pastScan says that the run must start no more pages.
func (b runBudget) pastScan() bool { return !b.scanEnd.IsZero() && !time.Now().Before(b.scanEnd) }

// pastAskStart says that the run must start no more asks.
func (b runBudget) pastAskStart() bool { return !b.askStart.IsZero() && !time.Now().Before(b.askStart) }

// until returns ctx cut at end, or ctx itself when end is zero.
func until(ctx context.Context, end time.Time) (context.Context, context.CancelFunc) {
	if end.IsZero() {
		return ctx, func() {}
	}
	return context.WithDeadline(ctx, end)
}

// askWaits is how long a delivery waits after ask n before ask n+1. The wait
// doubles, so a few asks cover an outage of hours, and a delivery that faucet
// always rejects costs MaxAsksPerDelivery requests.
var askWaits = [MaxAsksPerDelivery - 1]time.Duration{
	10 * time.Minute, 20 * time.Minute, 40 * time.Minute, 80 * time.Minute, 160 * time.Minute, 320 * time.Minute,
}

// AskOffsets returns when each ask of one delivery goes out, counted from the
// run that first read the delivery, when a run starts at every one of those
// times. The first ask is at offset zero.
func AskOffsets() []time.Duration {
	offsets := make([]time.Duration, MaxAsksPerDelivery)
	for i, wait := range askWaits {
		offsets[i+1] = offsets[i] + wait
	}
	return offsets
}

// askSchedule is askWaits followed by the wait before a give-up, which lets
// the last ask deliver before the run decides that it failed.
func askSchedule() []time.Duration {
	return append(append([]time.Duration(nil), askWaits[:]...), redeliveryAskSettle)
}

// Deps are the collaborators a Runner needs.
type Deps struct {
	// Store keeps the position and the deliveries between runs.
	Store Store
	// Hooks is GitHub's webhook API for the one webhook that delivers to faucet.
	Hooks HookAdmin
	// Clock gives each step its time of record. It is required.
	Clock quartz.Clock
	// Logger gets one line for each give-up and each gap, the only record
	// that names them: a run's reply only counts them, and their rows are
	// deleted later. The zero value logs nothing, so a host passes the logger
	// that it gives faucet.Deps.
	Logger slog.Logger
	// MaxPagesPerRun bounds the pages one run reads. Zero means
	// DefaultMaxPagesPerRun.
	MaxPagesPerRun int
	// MaxAsksPerRun bounds the redelivery requests one run sends. Zero means
	// DefaultMaxAsksPerRun.
	MaxAsksPerRun int
	// Wants says which deliveries a run also tracks although no typed rule
	// of faucet stores them. A run calls it once, before it reads the first
	// page and inside the time for reading the list. The predicate it returns
	// says, for the interests that are live then, whether faucet would store
	// a delivery of this GitHub event, action and repository
	// (HookDelivery.RepositoryID). A call that fails, or that runs past a
	// tenth of the run's time, must not let the run pass a delivery that an
	// interest wanted: the run logs the error and tracks every delivery that
	// the predicate returned with the error wants, or every delivery when
	// that predicate is nil. MaxAsksPerRun bounds the asks. Nil Deps.Wants,
	// or a nil predicate without an error, wants nothing.
	Wants func(ctx context.Context) (wants func(event, action string, repoID int64) bool, err error)
}

// wanted is the predicate that Deps.Wants returns. A nil wanted wants
// nothing.
type wanted func(event, action string, repoID int64) bool

func (w wanted) has(d HookDelivery) bool { return w != nil && w(d.Event, d.Action, d.RepositoryID) }

// Runner runs redelivery. It starts no goroutines.
type Runner struct {
	store    Store
	hooks    HookAdmin
	clock    quartz.Clock
	logger   slog.Logger
	maxPages int
	maxAsks  int
	wants    func(context.Context) (func(event, action string, repoID int64) bool, error)
}

// New returns a Runner for d.
func New(d Deps) (*Runner, error) {
	switch {
	case d.Clock == nil:
		return nil, xerrors.New("clock is required")
	case d.Store == nil:
		return nil, xerrors.New("store is required")
	case d.Hooks == nil:
		return nil, xerrors.New("hook admin is required")
	case d.MaxPagesPerRun < 0 || d.MaxAsksPerRun < 0:
		return nil, xerrors.New("run limits must not be negative")
	}
	r := &Runner{store: d.Store, hooks: d.Hooks, clock: d.Clock, logger: d.Logger, maxPages: d.MaxPagesPerRun, maxAsks: d.MaxAsksPerRun, wants: d.Wants}
	if r.maxPages == 0 {
		r.maxPages = DefaultMaxPagesPerRun
	}
	if r.maxAsks == 0 {
		r.maxAsks = DefaultMaxAsksPerRun
	}
	if r.wants == nil {
		r.wants = wantNothing
	}
	return r, nil
}

// wantNothing is the Deps.Wants of a host that stores no delivery beyond the
// typed rules.
func wantNothing(context.Context) (func(event, action string, repoID int64) bool, error) {
	return func(string, string, int64) bool { return false }, nil
}

// Result counts what one run did. Every outcome of a delivery is counted
// once, in the run that made it, and the run logs it.
type Result struct {
	// Busy is true when another run held the lease. Nothing else is set then.
	Busy bool
	// Pages is the number of pages of the delivery list the run read, and
	// Attempts the number of attempts on them that it examined.
	Pages    int
	Attempts int
	// Tracked is the number of deliveries that failed and became pending.
	Tracked int
	// Asked is the number of redelivery requests that GitHub accepted.
	Asked int
	// Recovered is the number of pending deliveries that are stored now, or
	// that a later attempt delivered.
	Recovered int
	// GaveUpLimit and GaveUpExpired count the deliveries that the run gave up.
	GaveUpLimit   int
	GaveUpExpired int
	// Gaps is the number of time ranges that the run recorded as expired
	// unread.
	Gaps int
	// Pending is the number of deliveries still pending after the run, and
	// Ranges the number of ranges of the list that are still unread.
	Pending int
	Ranges  int
}

// Run asks GitHub to deliver again each failed webhook delivery that it
// tracks, until faucet stores it. A run tracks a delivery of an event type that
// faucet stores, and a delivery that a live interest wants (Deps.Wants). Every
// such delivery ends in exactly one final state, which the run counts and logs:
// recovered, or given up because every ask was spent (GiveUpLimit) or GitHub's
// retention ended (GiveUpExpired). A time range of attempts that no run read is
// counted and logged as a gap (GiveUpGap): the range that expired before a run
// read it, the range before the first run's lookback, and the open ranges of a
// webhook that the host replaced.
//
// A run keeps its place in GitHub's list, which is newest first, in the store.
// It reads the newest attempts first, up to the position of the run before it
// (less ReadOverlap). When a run cannot read that far inside its page limit,
// it records the unread part as a range, and the next runs read the ranges,
// newest first, with what the head read leaves of their page limit. A cursor
// that GitHub gave is kept with the range, so a later run continues right
// after the last attempt read. A range whose old end leaves GitHub's retention
// before it is read becomes a gap, together with the ReadOverlap below that
// end, where a late attempt is listed.
//
// The scan takes no read bound and no gap from faucet's clock, because that
// clock and GitHub's differ. Faucet's clock is only the time of record that
// each step gives the store (ScanStep.Now). Where a read stops comes from the
// time of an attempt, which GitHub's clock had passed when it listed the
// attempt. A Date can only lower that bound: the first run after an empty list
// or the migration also reads back to the Date of that time, less DateMargin,
// and its gap covers what lies below. Where GitHub's retention has ended comes
// from the Date header of the page that saw the end of the list, and a gap
// ends DateSlack above it. After a silence, a gap starts at the Date of the
// run before, less DateMargin. A Date that is missing, or further behind an
// attempt than the slack, fails its read: on the first page the run fails,
// and on a later page the range stays open until it expires. A top that the
// store returns without a clear fails the run before it reads: the store did
// not keep the clear, or it holds a top from before there was one, which can
// be a Date.
//
// A delivery is asked for at most MaxAsksPerDelivery times, on the schedule
// that AskOffsets gives, and at most MaxAsksPerRun times in a run, the delivery
// that has waited longest first. The ask count is raised before the request,
// so a crash costs an ask and two runs never send the same one. A request that
// fails stops the asks of the run, and a failed first page stops them as well,
// because both mean that the token, the hook or GitHub is not working. A
// first page without a usable Date is a failed first page. An error of
// Deps.Wants, which the run calls before it reads the first page, fails
// nothing: the run tracks what the predicate returned with it wants.
//
// When ctx has a deadline, the run keeps to it by construction. It reads the
// list for half of the time that is left at its start, and a page that is not
// back by then is dropped. What it did not read stays an open range, as it
// does at the page limit. Then the run settles, asks until 80 percent of the
// time has passed, and purges. A slow list so costs pages and never the asks.
//
// One run holds the lease at a time. A second run that starts meanwhile
// returns at once with Busy set.
//
// The method has no auth of its own. The host decides who may call it.
func (r *Runner) Run(ctx context.Context) (Result, error) {
	scan, ok, err := r.store.AcquireRedeliveryLease(ctx, r.hooks.Hook(), r.clock.Now(), redeliveryLeaseTTL)
	if err != nil {
		return Result{}, xerrors.Errorf("acquire redelivery lease: %w", err)
	}
	if !ok {
		r.logger.Debug(ctx, "redelivery skipped: another run holds the lease")
		return Result{Busy: true}, nil
	}
	defer func() {
		releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if err := r.store.ReleaseRedeliveryLease(releaseCtx, scan.Epoch); err != nil {
			r.logger.Warn(ctx, "redelivery lease not released", slog.Error(err))
		}
	}()

	run := &redeliveryRun{runner: r, clock: r.clock, logger: r.logger, epoch: scan.Epoch, budget: newRunBudget(ctx)}
	return run.execute(ctx, scan)
}

type redeliveryRun struct {
	runner *Runner
	clock  quartz.Clock
	logger slog.Logger
	epoch  int64
	budget runBudget
	res    Result
}

func (r *redeliveryRun) execute(ctx context.Context, scan Scan) (Result, error) {
	r.res.Ranges = len(scan.Ranges)
	if scan.HookChanged {
		r.res.Ranges = 0
		r.logger.Warn(ctx, "redelivery hook changed: the scan starts again", slog.F("open_ranges_closed", len(scan.ClosedGaps)))
		for _, gap := range scan.ClosedGaps {
			r.countGap(ctx, gap, gapHookChanged)
		}
	}

	headRead, scanErr := r.scan(ctx, scan)
	errs := []error{scanErr}
	settleErr := r.settle(ctx)
	errs = append(errs, settleErr)
	if headRead && settleErr == nil && ctx.Err() == nil {
		errs = append(errs, r.ask(ctx))
	}
	errs = append(errs, r.purge(ctx))

	res := r.res
	logger := r.logger.Debug
	if res.Tracked > 0 || res.Asked > 0 || res.Recovered > 0 || res.GaveUpLimit > 0 || res.GaveUpExpired > 0 || res.Gaps > 0 {
		logger = r.logger.Info
	}
	logger(ctx, "redelivery run done",
		slog.F("pages", res.Pages), slog.F("attempts", res.Attempts), slog.F("tracked", res.Tracked), slog.F("asked", res.Asked),
		slog.F("recovered", res.Recovered), slog.F("gave_up_limit", res.GaveUpLimit), slog.F("gave_up_expired", res.GaveUpExpired),
		slog.F("gaps", res.Gaps), slog.F("pending", res.Pending), slog.F("open_ranges", res.Ranges))
	return res, errors.Join(errs...)
}

// pageRead is what one page of the delivery list tells a run.
type pageRead struct {
	failed       []FailedAttempt
	succeeded    []string
	attempts     int
	oldest       attemptTime
	reachedBound bool
	next         string
}

// finished says that a read of the range this page belongs to has seen all of
// it: the page reached the range's bound, or GitHub's list ended.
func (p pageRead) finished() bool { return p.reachedBound || p.next == "" }

func (p pageRead) endOfList() bool { return !p.reachedBound && p.next == "" }

func (r *redeliveryRun) fetch(ctx context.Context, cursor string) (DeliveryPage, error) {
	readCtx, cancel := until(ctx, r.budget.scanEnd)
	defer cancel()
	page, err := r.runner.hooks.ListDeliveries(readCtx, cursor)
	if err != nil {
		if ctx.Err() == nil && readCtx.Err() != nil {
			return DeliveryPage{}, errScanBudget
		}
		return DeliveryPage{}, xerrors.Errorf("list deliveries: %w", err)
	}
	r.res.Pages++
	return page, nil
}

// classify sends every 2xx attempt to succeeded, whatever its event: only a
// pending delivery changes for it, and the attempts of one GUID share one
// event and action. So a delivery that a run tracked while an interest wanted
// it still recovers when that interest has lapsed.
func (r *redeliveryRun) classify(page DeliveryPage, bound readBound, want wanted) pageRead {
	p := pageRead{next: page.Next}
	for _, d := range page.Deliveries {
		if bound.excludes(deliveredAt(d)) {
			p.reachedBound = true
			continue
		}
		p.attempts++
		p.oldest = p.oldest.orEarlier(deliveredAt(d))
		switch {
		case d.StatusCode >= 200 && d.StatusCode < 300:
			p.succeeded = append(p.succeeded, d.GUID)
		case faucet.StoresDelivery(d.Event, d.Action) || want.has(d):
			p.failed = append(p.failed, FailedAttempt{GUID: d.GUID, AttemptID: d.ID, DeliveredAt: d.DeliveredAt})
		}
	}
	r.res.Attempts += p.attempts
	return p
}

// loadWants calls Deps.Wants inside the first tenth of the run, so that a slow
// call costs only some of the time for reading the list. An error fails no
// typed work, and passes no delivery that an interest may want: the run then
// wants what the predicate returned with the error wants, or everything.
func (r *redeliveryRun) loadWants(ctx context.Context) wanted {
	wantCtx, cancel := until(ctx, r.budget.wantsEnd)
	defer cancel()
	want, err := r.runner.wants(wantCtx)
	if err != nil {
		r.logger.Error(ctx, "the raw interests could not be read, this run tracks every delivery that faucet may store raw", slog.Error(err))
		if want == nil {
			return func(string, string, int64) bool { return true }
		}
	}
	return want
}

func (r *redeliveryRun) step(p pageRead) ScanStep {
	return ScanStep{Epoch: r.epoch, Now: r.clock.Now(), Failed: p.failed, Succeeded: p.succeeded}
}

func gapBetween(from, to time.Time) *Gap {
	if !from.Before(to) {
		return nil
	}
	return &Gap{From: from, To: to}
}

func (r *redeliveryRun) apply(ctx context.Context, step ScanStep, cause string) (ScanResult, error) {
	res, err := r.runner.store.ApplyRedeliveryScan(ctx, step)
	if err != nil {
		return ScanResult{}, xerrors.Errorf("record page: %w", err)
	}
	r.res.Tracked += res.Tracked
	r.countRecovered(ctx, res.Recovered, "a later attempt succeeded")
	if step.Gap != nil {
		r.countGap(ctx, *step.Gap, cause)
	}
	return res, nil
}

func (r *redeliveryRun) countRecovered(ctx context.Context, guids []string, how string) {
	for _, guid := range guids {
		r.res.Recovered++
		r.logger.Info(ctx, "redelivery recovered", slog.F("delivery_id", guid), slog.F("how", how))
	}
}

// The causes of a gap, as the log names them.
const (
	gapBeforeTracking = "before tracking began"
	gapExpiredUnread  = "expired unread"
	gapHookChanged    = "hook changed"
)

func (r *redeliveryRun) countGap(ctx context.Context, gap Gap, cause string) {
	r.res.Gaps++
	r.logger.Warn(ctx, "redelivery gap",
		slog.F("reason", GiveUpGap), slog.F("cause", cause), slog.F("from", gap.From), slog.F("to", gap.To))
}

// scan reads the head of the list and then the open ranges, newest first. It
// returns whether the head read worked.
func (r *redeliveryRun) scan(ctx context.Context, scan Scan) (bool, error) {
	want := r.loadWants(ctx)
	top, clearAt, err := topAndClearOf(scan)
	if err != nil {
		return false, err
	}
	page, err := r.fetch(ctx, "")
	if err != nil {
		return false, err
	}
	date, err := checkDate(page, top)
	if err != nil {
		return false, xerrors.Errorf("first page: %w", err)
	}
	newest := newestOn(page)

	first := !top.set()
	bound, olderAt := top.bound(), top.at
	if first {
		bound = clearAt.firstRunBound(newest)
		olderAt = bound.stop
	}
	head := r.classify(page, bound, want)
	step := r.step(head)
	step.Top, step.Clear = newest.at, date.date
	cause := gapExpiredUnread
	switch {
	case first && newest.set():
		to := bound.stop
		if head.endOfList() {
			to = clearAt.firstRunGapEnd(bound, date.gapEnd(head.oldest))
		}
		step.Gap, cause = gapBetween(clearAt.firstRunGapStart(newest), to), gapBeforeTracking
	case head.endOfList():
		if from, ok := clearAt.headGapStart(top); ok {
			step.Gap = gapBetween(from, date.gapEnd(head.oldest))
		}
	}
	if !head.finished() {
		step.Open = &Range{NewerAt: head.oldest.at, OlderAt: olderAt, Cursor: head.next}
	}
	res, err := r.apply(ctx, step, cause)
	if err != nil {
		return false, err
	}
	ranges := scan.Ranges
	if step.Open != nil {
		opened := *step.Open
		opened.ID = res.OpenedID
		r.res.Ranges++
		ranges = append([]Range{opened}, ranges...)
	}
	live, err := r.closeExpired(ctx, ranges, date, clearAt)
	if err != nil {
		return true, err
	}
	return true, r.readRanges(ctx, live, top.orLater(newest), want)
}

func (r *redeliveryRun) closeExpired(ctx context.Context, ranges []Range, date pageDate, clearAt storedClear) ([]Range, error) {
	floor, ok := expiredByBoth(date, clearAt)
	if !ok {
		return ranges, nil
	}
	live := ranges[:0:0]
	for _, rg := range ranges {
		if !rg.NewerAt.Before(floor) {
			live = append(live, rg)
			continue
		}
		gap := rg.UnreadGap()
		if _, err := r.apply(ctx, ScanStep{Epoch: r.epoch, Now: r.clock.Now(), CloseID: rg.ID, Gap: &gap}, gapExpiredUnread); err != nil {
			return nil, err
		}
		r.res.Ranges--
	}
	return live, nil
}

func (r *redeliveryRun) readRanges(ctx context.Context, ranges []Range, newestKnown attemptTime, want wanted) error {
	for _, rg := range ranges {
		if err := r.readRange(ctx, rg, newestKnown, want); err != nil {
			return err
		}
	}
	return nil
}

// GitHub deletes attempts in the order of their times, so a page that reaches
// the bound ends the range without a gap.
func (r *redeliveryRun) readRange(ctx context.Context, rg Range, newestKnown attemptTime, want wanted) error {
	bound := rg.bound()
	for r.res.Pages < r.runner.maxPages && ctx.Err() == nil && !r.budget.pastScan() {
		page, err := r.fetch(ctx, rg.Cursor)
		if errors.Is(err, errScanBudget) {
			return nil // the range stays open where the last page left it
		}
		if err != nil {
			return err
		}
		p := r.classify(page, bound, want)
		step := r.step(p)
		newerEnd := rg.newerEnd().orEarlier(p.oldest)
		switch {
		case p.reachedBound:
			step.CloseID = rg.ID
		case p.endOfList():
			date, err := checkDate(page, newestKnown)
			if err != nil {
				return xerrors.Errorf("range page: %w", err)
			}
			step.CloseID = rg.ID
			step.Gap = gapBetween(bound.stop, date.gapEnd(newerEnd))
		default:
			step.Advance = &RangeAdvance{ID: rg.ID, NewerAt: newerEnd.at, Cursor: p.next}
		}
		if _, err := r.apply(ctx, step, gapExpiredUnread); err != nil {
			return err
		}
		if step.Advance == nil {
			r.res.Ranges--
			return nil
		}
		rg.NewerAt, rg.Cursor = newerEnd.at, p.next
	}
	return nil
}

func earlierOf(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

func laterOf(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func (r *redeliveryRun) settle(ctx context.Context) error {
	res, err := r.runner.store.SettleRedeliveries(ctx, SettleRequest{
		Now: r.clock.Now(), Retention: GitHubDeliveryRetention, MaxAsks: MaxAsksPerDelivery,
	})
	if err != nil {
		return xerrors.Errorf("settle deliveries: %w", err)
	}
	r.res.Pending = res.Pending
	r.countRecovered(ctx, res.Recovered, "faucet stored the event")
	for _, g := range res.GaveUp {
		switch g.Reason {
		case GiveUpLimit:
			r.res.GaveUpLimit++
		case GiveUpExpired:
			r.res.GaveUpExpired++
		}
		r.logger.Warn(ctx, "redelivery gave up", slog.F("delivery_id", g.GUID), slog.F("reason", g.Reason),
			slog.F("asks", g.Asks), slog.F("first_delivered_at", g.FirstDeliveredAt))
	}
	return nil
}

func (r *redeliveryRun) ask(ctx context.Context) error {
	schedule := askSchedule()
	askCtx, cancel := until(ctx, r.budget.askEnd)
	defer cancel()
	for r.res.Asked < r.runner.maxAsks && askCtx.Err() == nil && !r.budget.pastAskStart() {
		claim, ok, err := r.runner.store.ClaimRedelivery(ctx, ClaimRequest{
			Now: r.clock.Now(), Retention: GitHubDeliveryRetention, MaxAsks: MaxAsksPerDelivery, Schedule: schedule,
		})
		if err != nil {
			return xerrors.Errorf("claim a redelivery: %w", err)
		}
		if !ok {
			return nil
		}
		if err := r.runner.hooks.Redeliver(askCtx, claim.AttemptID); err != nil {
			r.logger.Warn(ctx, "redelivery request failed",
				slog.F("delivery_id", claim.GUID), slog.F("attempt_id", claim.AttemptID), slog.F("ask", claim.Ask), slog.Error(err))
			return xerrors.Errorf("request redelivery: %w", err)
		}
		r.res.Asked++
		r.logger.Info(ctx, "redelivery requested",
			slog.F("delivery_id", claim.GUID), slog.F("attempt_id", claim.AttemptID), slog.F("ask", claim.Ask))
	}
	return nil
}

// purge deletes what has outlived GitHub's retention, so a table of final
// deliveries does not grow without end.
func (r *redeliveryRun) purge(ctx context.Context) error {
	if _, err := r.runner.store.PurgeRedeliveries(ctx, r.clock.Now().Add(-GitHubDeliveryRetention)); err != nil {
		return xerrors.Errorf("purge redeliveries: %w", err)
	}
	return nil
}
