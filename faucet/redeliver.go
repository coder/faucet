package faucet

import (
	"context"
	"errors"
	"time"

	"golang.org/x/xerrors"

	"cdr.dev/slog/v3"
)

const (
	// DefaultMaxPagesPerRun is the most pages of GitHub's delivery list that
	// one run reads when RedeliveryDeps.MaxPagesPerRun is zero. A page holds
	// 100 attempts.
	DefaultMaxPagesPerRun = 20
	// DefaultMaxAsksPerRun is the most redelivery requests that one run sends
	// when RedeliveryDeps.MaxAsksPerRun is zero.
	DefaultMaxAsksPerRun = 50
	// MaxAsksPerDelivery is the number of times a run asks GitHub to deliver
	// one event again, before it gives the delivery up with GiveUpLimit.
	MaxAsksPerDelivery = 7
	// RedeliveryReadOverlap is how far below its position a run reads again.
	// GitHub writes an attempt down when it ends, and an attempt that ended
	// late appears below one that ended earlier. The delivery timeout is 10
	// seconds, so this is a wide margin.
	RedeliveryReadOverlap = 5 * time.Minute
	// RedeliveryStartLookback is how far back the first run of a webhook reads.
	// The part of GitHub's retention before it is recorded as one gap, so the
	// first run does not spend its pages on a history that nobody waits for.
	RedeliveryStartLookback = 24 * time.Hour

	redeliveryLeaseTTL  = 2 * time.Minute
	redeliveryAskSettle = 5 * time.Minute
)

// A run whose context has a deadline spends the time left at its start in
// four parts, so that the settle, the asks and the purge get their turn
// whatever the delivery list costs. A slow list leaves a range open, as the
// page limit does, and the next run reads on.
const (
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
// so the split does not use the service's clock.
type runBudget struct {
	scanEnd, askStart, askEnd time.Time
}

func newRunBudget(ctx context.Context) runBudget {
	deadline, ok := ctx.Deadline()
	if !ok {
		return runBudget{}
	}
	start := time.Now()
	left := deadline.Sub(start)
	at := func(share float64) time.Time { return start.Add(time.Duration(float64(left) * share)) }
	return runBudget{scanEnd: at(redeliveryScanShare), askStart: at(redeliveryAskStartShare), askEnd: at(redeliveryAskEndShare)}
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

// ErrNoRedelivery is what Redeliver returns when Deps had no Redelivery.
var ErrNoRedelivery = xerrors.New("redelivery is not configured")

// RedeliveryDeps turns on redelivery. Nil in Deps leaves it off.
type RedeliveryDeps struct {
	// Store keeps the position and the deliveries between runs.
	Store RedeliveryStore
	// Hooks is GitHub's webhook API for the one webhook that delivers to faucet.
	Hooks HookAdmin
	// MaxPagesPerRun bounds the pages one run reads. Zero means
	// DefaultMaxPagesPerRun.
	MaxPagesPerRun int
	// MaxAsksPerRun bounds the redelivery requests one run sends. Zero means
	// DefaultMaxAsksPerRun.
	MaxAsksPerRun int
}

type redeliveryState struct {
	store    RedeliveryStore
	hooks    HookAdmin
	maxPages int
	maxAsks  int
}

func newRedeliveryState(d RedeliveryDeps) (*redeliveryState, error) {
	switch {
	case d.Store == nil:
		return nil, xerrors.New("store is required")
	case d.Hooks == nil:
		return nil, xerrors.New("hook admin is required")
	case d.MaxPagesPerRun < 0 || d.MaxAsksPerRun < 0:
		return nil, xerrors.New("run limits must not be negative")
	}
	state := &redeliveryState{store: d.Store, hooks: d.Hooks, maxPages: d.MaxPagesPerRun, maxAsks: d.MaxAsksPerRun}
	if state.maxPages == 0 {
		state.maxPages = DefaultMaxPagesPerRun
	}
	if state.maxAsks == 0 {
		state.maxAsks = DefaultMaxAsksPerRun
	}
	return state, nil
}

// RedeliverResult counts what one Redeliver run did. Every outcome of a
// delivery is counted once, in the run that made it, and the run logs it.
type RedeliverResult struct {
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

// Redeliver asks GitHub to deliver again each failed webhook delivery of an
// event type that faucet stores, until faucet stores it. Every such delivery
// ends in exactly one final state, which the run counts and logs: recovered,
// or given up because every ask was spent (GiveUpLimit) or GitHub's retention
// ended (GiveUpExpired). A time range of attempts that no run read is counted
// and logged as a gap (GiveUpGap): the range that expired before a run read it,
// the range before the first run's lookback, and the open ranges of a webhook
// that the host replaced.
//
// A run keeps its place in GitHub's list, which is newest first, in the store.
// It reads the newest attempts first, up to the position of the run before it
// (less RedeliveryReadOverlap). When a run cannot read that far inside its
// page limit, it records the unread part as a range, and the next runs read
// the ranges, newest first, with what the head read leaves of their page
// limit. A cursor that GitHub gave is kept with the range, so a later run
// continues right after the last attempt read. A range whose old end leaves
// GitHub's retention before it is read becomes a gap.
//
// A delivery is asked for at most MaxAsksPerDelivery times, on the schedule
// that AskOffsets gives, and at most MaxAsksPerRun times in a run, the delivery
// that has waited longest first. The ask count is raised before the request,
// so a crash costs an ask and two runs never send the same one. A request that
// fails stops the asks of the run, and a failed first page stops them as well,
// because both mean that the token, the hook or GitHub is not working.
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
func (s *Service) Redeliver(ctx context.Context) (RedeliverResult, error) {
	if s.redelivery == nil {
		return RedeliverResult{}, ErrNoRedelivery
	}
	rd := s.redelivery
	scan, ok, err := rd.store.AcquireRedeliveryLease(ctx, rd.hooks.Hook(), s.clock.Now(), redeliveryLeaseTTL)
	if err != nil {
		return RedeliverResult{}, xerrors.Errorf("acquire redelivery lease: %w", err)
	}
	if !ok {
		s.logger.Info(ctx, "redelivery skipped: another run holds the lease")
		return RedeliverResult{Busy: true}, nil
	}
	defer func() {
		releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if err := rd.store.ReleaseRedeliveryLease(releaseCtx, scan.Epoch); err != nil {
			s.logger.Warn(ctx, "redelivery lease not released", slog.Error(err))
		}
	}()

	run := &redeliveryRun{svc: s, state: rd, epoch: scan.Epoch, budget: newRunBudget(ctx)}
	return run.execute(ctx, scan)
}

type redeliveryRun struct {
	svc    *Service
	state  *redeliveryState
	epoch  int64
	budget runBudget
	res    RedeliverResult
	// floor is where GitHub's retention begins, on GitHub's own clock. It is
	// zero when GitHub gave this run no time to take it from, and then the run
	// records no gap, because it cannot tell what expired.
	floor time.Time
}

func (r *redeliveryRun) execute(ctx context.Context, scan RedeliveryScan) (RedeliverResult, error) {
	r.res.Ranges = len(scan.Ranges)
	if scan.HookChanged {
		r.res.Ranges = 0
		r.svc.logger.Warn(ctx, "redelivery hook changed: the scan starts again", slog.F("open_ranges_closed", len(scan.ClosedGaps)))
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
	r.svc.logger.Info(ctx, "redelivery run done",
		slog.F("pages", res.Pages), slog.F("attempts", res.Attempts), slog.F("tracked", res.Tracked), slog.F("asked", res.Asked),
		slog.F("recovered", res.Recovered), slog.F("gave_up_limit", res.GaveUpLimit), slog.F("gave_up_expired", res.GaveUpExpired),
		slog.F("gaps", res.Gaps), slog.F("pending", res.Pending), slog.F("open_ranges", res.Ranges))
	return res, errors.Join(errs...)
}

// pageRead is what one page of the delivery list tells a run.
type pageRead struct {
	failed    []FailedAttempt
	succeeded []string
	attempts  int
	// newest is the time of the newest attempt on the page. oldest is the
	// time of the oldest one at or after the bound, and zero when there is none.
	newest, oldest time.Time
	// at is the page's DeliveryPage.At.
	at           time.Time
	reachedBound bool
	next         string
}

// githubNow is GitHub's own time at this page: the Date header, but never
// earlier than the newest attempt that the page lists. It is zero when the page
// has neither. Faucet's clock does not belong here.
func (p pageRead) githubNow() time.Time {
	if p.newest.After(p.at) {
		return p.newest
	}
	return p.at
}

// finished says that a read of the range this page belongs to has seen all of
// it: the page reached the range's bound, or GitHub's list ended.
func (p pageRead) finished() bool { return p.reachedBound || p.next == "" }

func (r *redeliveryRun) read(ctx context.Context, cursor string, bound time.Time) (pageRead, error) {
	readCtx, cancel := until(ctx, r.budget.scanEnd)
	defer cancel()
	page, err := r.state.hooks.ListDeliveries(readCtx, cursor)
	if err != nil {
		if ctx.Err() == nil && readCtx.Err() != nil {
			return pageRead{}, errScanBudget
		}
		return pageRead{}, xerrors.Errorf("list deliveries: %w", err)
	}
	r.res.Pages++
	p := pageRead{next: page.Next, at: page.At}
	for _, d := range page.Deliveries {
		if d.DeliveredAt.After(p.newest) {
			p.newest = d.DeliveredAt
		}
		if d.DeliveredAt.Before(bound) {
			p.reachedBound = true
			continue
		}
		p.attempts++
		if p.oldest.IsZero() || d.DeliveredAt.Before(p.oldest) {
			p.oldest = d.DeliveredAt
		}
		if !storesDelivery(d.Event, d.Action) {
			continue
		}
		if d.StatusCode >= 200 && d.StatusCode < 300 {
			p.succeeded = append(p.succeeded, d.GUID)
			continue
		}
		p.failed = append(p.failed, FailedAttempt{GUID: d.GUID, AttemptID: d.ID, DeliveredAt: d.DeliveredAt})
	}
	r.res.Attempts += p.attempts
	return p, nil
}

func (p pageRead) step(epoch int64, now time.Time) ScanStep {
	return ScanStep{Epoch: epoch, Now: now, Failed: p.failed, Succeeded: p.succeeded}
}

// gapOf says which part of a range expired before a run read it. olderAt is
// the range's old end. coveredFrom is how far down a read has got, or the
// range's new end when it got nowhere. Only the part below the retention floor
// is gone. A part above it that holds no attempt was quiet. A zero floor means
// that nothing says what is gone.
func gapOf(olderAt, floor, coveredFrom time.Time) *RedeliveryGap {
	if floor.IsZero() {
		return nil
	}
	to := coveredFrom
	if floor.Before(to) {
		to = floor
	}
	if !olderAt.Before(to) {
		return nil
	}
	return &RedeliveryGap{From: olderAt, To: to}
}

func (r *redeliveryRun) apply(ctx context.Context, step ScanStep, cause string) (ScanResult, error) {
	res, err := r.state.store.ApplyRedeliveryScan(ctx, step)
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
		r.svc.logger.Info(ctx, "redelivery recovered", slog.F("delivery_id", guid), slog.F("how", how))
	}
}

// The causes of a gap, as the log names them.
const (
	gapBeforeTracking = "before tracking began"
	gapExpiredUnread  = "expired unread"
	gapHookChanged    = "hook changed"
)

func (r *redeliveryRun) countGap(ctx context.Context, gap RedeliveryGap, cause string) {
	r.res.Gaps++
	r.svc.logger.Warn(ctx, "redelivery gap",
		slog.F("reason", GiveUpGap), slog.F("cause", cause), slog.F("from", gap.From), slog.F("to", gap.To))
}

// scan reads the head of the list and then the open ranges, newest first. It
// returns whether the head read worked.
func (r *redeliveryRun) scan(ctx context.Context, scan RedeliveryScan) (bool, error) {
	now := r.svc.clock.Now()
	first := scan.Top.IsZero()
	olderAt, bound := scan.Top, scan.Top.Add(-RedeliveryReadOverlap)
	if first {
		olderAt = now.Add(-RedeliveryStartLookback)
		bound = olderAt
	}

	head, err := r.read(ctx, "", bound)
	if err != nil {
		return false, err
	}
	githubNow := head.githubNow()
	if !githubNow.IsZero() {
		r.floor = githubNow.Add(-GitHubDeliveryRetention)
	}
	step := head.step(r.epoch, now)
	// The position is a time of GitHub's: the newest attempt, or GitHub's clock
	// when the list is empty. Faucet's clock does not belong here (see
	// DeliveryPage.At).
	step.Top = head.newest
	if step.Top.IsZero() {
		step.Top = head.at
	}
	cause := gapExpiredUnread
	switch {
	case first:
		cause = gapBeforeTracking
		if !r.floor.IsZero() && r.floor.Before(olderAt) {
			step.Gap = &RedeliveryGap{From: r.floor, To: olderAt}
		}
	case head.finished():
		covered := githubNow
		if !head.oldest.IsZero() {
			covered = head.oldest
		}
		step.Gap = gapOf(olderAt, r.floor, covered)
	}
	// A page that reaches the position of the run before has read all that is
	// new. What lies below it, down to the overlap, is what earlier runs read or
	// the range that they left open for it. A range of it again would only
	// stand ahead of the older ranges and take the pages of every run.
	readAllNew := !first && !head.oldest.IsZero() && !head.oldest.After(olderAt)
	if !head.finished() && !readAllNew {
		step.Open = &RedeliveryRange{NewerAt: head.oldest, OlderAt: olderAt, Cursor: head.next}
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
		ranges = append([]RedeliveryRange{opened}, ranges...)
	}
	return true, r.readRanges(ctx, ranges)
}

func (r *redeliveryRun) readRanges(ctx context.Context, ranges []RedeliveryRange) error {
	live := ranges[:0:0]
	for _, rg := range ranges {
		if !rg.NewerAt.Before(r.floor) {
			live = append(live, rg)
			continue
		}
		gap := RedeliveryGap{From: rg.OlderAt, To: laterOf(rg.NewerAt, rg.OlderAt)}
		if _, err := r.apply(ctx, ScanStep{Epoch: r.epoch, Now: r.svc.clock.Now(), CloseID: rg.ID, Gap: &gap}, gapExpiredUnread); err != nil {
			return err
		}
		r.res.Ranges--
	}
	for _, rg := range live {
		for r.res.Pages < r.state.maxPages && ctx.Err() == nil && !r.budget.pastScan() {
			now := r.svc.clock.Now()
			p, err := r.read(ctx, rg.Cursor, rg.OlderAt.Add(-RedeliveryReadOverlap))
			if errors.Is(err, errScanBudget) {
				return nil // the range stays open where the last page left it
			}
			if err != nil {
				return err
			}
			step := p.step(r.epoch, now)
			if !p.finished() {
				step.Advance = &RangeAdvance{ID: rg.ID, NewerAt: earlierOf(rg.NewerAt, p.oldest), Cursor: p.next}
				if _, err := r.apply(ctx, step, gapExpiredUnread); err != nil {
					return err
				}
				rg.Cursor = p.next
				continue
			}
			covered := rg.NewerAt
			if !p.oldest.IsZero() {
				covered = p.oldest
			}
			step.CloseID = rg.ID
			step.Gap = gapOf(rg.OlderAt, r.floor, covered)
			if _, err := r.apply(ctx, step, gapExpiredUnread); err != nil {
				return err
			}
			r.res.Ranges--
			break
		}
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
	res, err := r.state.store.SettleRedeliveries(ctx, SettleRequest{
		Now: r.svc.clock.Now(), Retention: GitHubDeliveryRetention, MaxAsks: MaxAsksPerDelivery,
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
		r.svc.logger.Warn(ctx, "redelivery gave up", slog.F("delivery_id", g.GUID), slog.F("reason", g.Reason),
			slog.F("asks", g.Asks), slog.F("first_delivered_at", g.FirstDeliveredAt))
	}
	return nil
}

func (r *redeliveryRun) ask(ctx context.Context) error {
	schedule := askSchedule()
	askCtx, cancel := until(ctx, r.budget.askEnd)
	defer cancel()
	for r.res.Asked < r.state.maxAsks && askCtx.Err() == nil && !r.budget.pastAskStart() {
		claim, ok, err := r.state.store.ClaimRedelivery(ctx, ClaimRequest{
			Now: r.svc.clock.Now(), Retention: GitHubDeliveryRetention, MaxAsks: MaxAsksPerDelivery, Schedule: schedule,
		})
		if err != nil {
			return xerrors.Errorf("claim a redelivery: %w", err)
		}
		if !ok {
			return nil
		}
		if err := r.state.hooks.Redeliver(askCtx, claim.AttemptID); err != nil {
			r.svc.logger.Warn(ctx, "redelivery request failed",
				slog.F("delivery_id", claim.GUID), slog.F("attempt_id", claim.AttemptID), slog.F("ask", claim.Ask), slog.Error(err))
			return xerrors.Errorf("request redelivery: %w", err)
		}
		r.res.Asked++
		r.svc.logger.Info(ctx, "redelivery requested",
			slog.F("delivery_id", claim.GUID), slog.F("attempt_id", claim.AttemptID), slog.F("ask", claim.Ask))
	}
	return nil
}

// purge deletes what has outlived GitHub's retention, so a table of final
// deliveries does not grow without end.
func (r *redeliveryRun) purge(ctx context.Context) error {
	if _, err := r.state.store.PurgeRedeliveries(ctx, r.svc.clock.Now().Add(-GitHubDeliveryRetention)); err != nil {
		return xerrors.Errorf("purge redeliveries: %w", err)
	}
	return nil
}
