package faucet

import (
	"cmp"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/xerrors"

	"cdr.dev/slog/v3"
)

const (
	deliverWorkers = 8
	// A stalled endpoint costs a request timeout per attempt, so it must not
	// hold every worker while other subscriptions wait.
	defaultMaxWorkersPerOrigin = 2
	// maxClaimsPerRun bounds the work of one DeliverDue, so a run fits the
	// deadline of the cron request that starts it.
	maxClaimsPerRun = 200
	// minTimeToStart is the least time that must be left on the run's deadline
	// for it to start another attempt: a request takes up to 5 seconds, and the
	// result needs a moment to be recorded.
	minTimeToStart = 10 * time.Second
	// maxScanBatches bounds how many subscriptions one run reads events for:
	// maxScanBatches times maxScanTargetsPerBatch. The enqueue stops earlier
	// when less than two attempts' worth of time is left, so that the sends
	// still have theirs.
	maxScanBatches         = 5
	maxScanTargetsPerBatch = 200
	// scanTimeout bounds the reads of one principal's subscriptions in a run.
	// The reads of a run share the few seconds before the sends need the rest
	// of its deadline, so one principal must leave most of them to others.
	scanTimeout = time.Second
	// scanWindow is how many events of its target one read of a subscription
	// looks at, matching or not.
	scanWindow = 10_000
	// maxEventBodyBytes is the largest body that faucet sends, the delivery
	// profile of SEP-3415.
	maxEventBodyBytes = 256 << 10
	// finishTimeout lets an attempt that has ended record its result even when
	// the run's context is done.
	finishTimeout    = 5 * time.Second
	deliveryAgent    = "faucet-webhook"
	messageIDPrefix  = "msg_"
	messageIDEntropy = 16
)

// ErrNoWebhook is what DeliverDue returns when the service has no WebhookDeps.
var ErrNoWebhook = xerrors.New("webhook delivery is not configured")

// webhookBody is the JSON body of an event delivery. eventId is the webhook-id
// of the delivery, not the GitHub delivery ID that events/poll uses, so a
// receiver can deduplicate on either and a delivery to two subscriptions has
// two IDs. cursor is the watermark that SEP-3415 describes.
//
// The webhook-id of a subscription and an event is an HMAC under a key that
// the operator holds, over the subscription's key hash and the event's stored
// ID. It has the entropy that SEP-3415 asks for and is the same on every retry,
// as the SEP requires. It is also the same when a subscription lapses and is
// made again and the event is sent anew, so a receiver that deduplicates on it
// drops what it already processed. It is not derived from the upstream eventId,
// which the SEP forbids, and nobody without the key can compute it. An
// operator who changes the key gives up the replay match once.
type webhookBody struct {
	EventID   string          `json:"eventId"`
	Name      string          `json:"name"`
	Timestamp time.Time       `json:"timestamp"`
	Data      json.RawMessage `json:"data"`
	Cursor    string          `json:"cursor"`
}

// DeliverDue sends the webhooks that are due. It queues the events that live
// subscriptions have not read yet, sends the due deliveries through the
// service's guarded client, and records how each attempt ended. Hosts call it
// on a schedule, for example every minute from a cron route, and a call
// finishes inside the deadline of ctx: it starts no attempt with less than 10
// seconds left.
//
// A subscription gets only the events of the repository it was granted, of its
// event type, that match its sha, pr and field filter arguments, and only while
// it is live.
// Each event is one delivery, with one webhook-id for every attempt. A delivery
// gets at most 5 attempts in 15 minutes, after 15 seconds, 1 minute, 3 minutes
// and 6 minutes. A 2xx ends it. A 410 and a 413 end it too, and neither ends
// the subscription. Every delivery that ends in the call is in the result,
// delivered or with its reason.
//
// A subscription that a purge overtook, one whose unread events were deleted,
// gets one gap envelope when the call queues its next events. The queueing
// makes no request: it records the gap on the subscription, and the envelope
// goes out in the send phase, on the same workers and under the same deadline,
// to the subscription as it is then. It is one attempt with no retry, and the
// result counts it as sent or failed. Each worker takes gaps before its first
// delivery, one after another, until none is left that its callback origin
// limit allows, and then takes deliveries and looks for no more gaps. A gap
// whose origin is at the limit when a worker looks is passed over. A gap that no
// worker took before the call ended stays recorded, and the next call sends it.
//
// With a RawStore the call also sends the raw deliveries that webhook
// deliveries queued, with the same attempts, schedule and reasons, and the gap
// envelope of a raw subscription that held the most unsent deliveries when
// events arrived. Both kinds share the workers, the limit per callback origin
// and the limit of a call. Each worker asks for the other kind first after
// each delivery it takes, so neither kind waits behind a long queue of the
// other.
//
// By default at most 2 of the 8 workers of a call send to one callback origin
// at a time, for deliveries and gaps alike, so one endpoint that stalls cannot
// hold up the others. WebhookDeps.MaxWorkersPerOrigin changes the number.
//
// Calls may overlap, in one process or in several. A claim gives an attempt to
// one caller for 30 seconds, and a caller whose claim lapsed records nothing.
// A call that dies between a claim and its result leaves the delivery to the
// next call after the lease, and the attempt counts.
//
// The call reads each subscription's events 10,000 at a time and stores its
// read position after each, and the reads of one principal's subscriptions
// get a second of the call. A read that runs out of that second is no
// failure: the result is Incomplete, and the next call goes on from where it
// stopped. A subscription whose events the call cannot read, because the
// Store fails, is skipped for the rest of the call, and the call goes on with
// the others and the sends.
//
// The error is the first failure of the Store. The result holds what happened
// before it, and also what happened after it when the failure was a read. A
// failure of the RawStore is no error of the call: it is logged and counted in
// RawFailures, and the run goes on with the typed deliveries.
// The method has no auth of its own. The host decides who may call it.
func (s *Service) DeliverDue(ctx context.Context) (DeliverResult, error) {
	if s.webhook == nil {
		return DeliverResult{}, ErrNoWebhook
	}
	run := &deliveryRun{svc: s, wh: s.webhook, result: DeliverResult{GaveUp: map[GiveUpReason]int{}}, gate: endpointGate{limit: s.webhook.workersPerOrigin}}
	if err := run.enqueue(ctx); err != nil {
		return run.result, cmp.Or(run.scanErr, run.unlessEnded(ctx, err))
	}
	if err := run.sweepExpired(ctx); err != nil {
		return run.result, cmp.Or(run.scanErr, run.unlessEnded(ctx, err))
	}
	run.sweepPreviousSecrets(ctx)
	err := cmp.Or(run.scanErr, run.send(ctx))
	res := run.result
	logger := s.logger.Debug
	if res.Claimed > 0 || res.Enqueued > 0 || res.GaveUpTotal() > 0 {
		logger = s.logger.Info
	}
	logger(ctx, "deliveries run",
		slog.F("enqueued", res.Enqueued), slog.F("gaps", res.Gaps), slog.F("gaps_sent", res.GapsSent), slog.F("gaps_failed", res.GapsFailed), slog.F("claimed", res.Claimed), slog.F("sent", res.Sent),
		slog.F("delivered", res.Delivered), slog.F("retried", res.Retried), slog.F("gave_up", res.GaveUpTotal()),
		slog.F("superseded", res.Superseded), slog.F("incomplete", res.Incomplete))
	return res, err
}

type deliveryRun struct {
	svc *Service
	wh  *webhookState

	mu     sync.Mutex
	result DeliverResult

	// claimMu makes a worker's claim and its entry into the endpoint gate one
	// step, so no two workers can both take the last place of an endpoint.
	claimMu sync.Mutex
	gate    endpointGate

	// rawDown is set by the first failure of the RawStore, and ends the raw
	// part of the run.
	rawDown atomic.Bool

	// scanErr is the first failure of a subscription's read, which the run
	// returns after the sends. Only enqueue sets it, before the sends start.
	scanErr error
}

type endpointGate struct {
	// limit is how many workers may send to one origin. Negative is no limit.
	limit   int
	mu      sync.Mutex
	workers map[string]int
}

func (g *endpointGate) full() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	full := []string{}
	if g.limit < 0 {
		return full
	}
	for origin, n := range g.workers {
		if n >= g.limit {
			full = append(full, origin)
		}
	}
	return full
}

func (g *endpointGate) enter(origin string) {
	if origin == "" {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.workers == nil {
		g.workers = map[string]int{}
	}
	g.workers[origin]++
}

func (g *endpointGate) leave(origin string) {
	if origin == "" {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.workers[origin]--
	if g.workers[origin] <= 0 {
		delete(g.workers, origin)
	}
}

// The claims are the only store calls of the send phase that the run's context
// bounds, so they tell the end of the run from a store failure here. A worker
// whose claim the end cut short claims nothing and stops, and the run is
// incomplete. The error of the send phase then holds only store failures, and
// no order of the workers' errors can put the end of the run before one.
func (r *deliveryRun) claimDelivery(ctx context.Context) (DeliveryClaim, bool, error) {
	r.claimMu.Lock()
	defer r.claimMu.Unlock()
	claim, ok, err := r.svc.store.ClaimDelivery(ctx, r.svc.clock.Now(), DeliveryLease, r.gate.full())
	if ok {
		r.gate.enter(CallbackOrigin(claim.CallbackURL))
	}
	return claim, ok, r.unlessEnded(ctx, err)
}

type claimFunc func(context.Context) (outbound, bool, error)

// claimKinds are the kinds of delivery that the run sends: typed ones, and
// raw ones when the service has a RawStore.
func (r *deliveryRun) claimKinds() []claimFunc {
	kinds := []claimFunc{r.claimTyped}
	if r.svc.raw != nil {
		kinds = append(kinds, r.claimRaw)
	}
	return kinds
}

// claimNext takes the next due delivery of the first kind that has one.
func claimNext(ctx context.Context, kinds []claimFunc) (outbound, bool, error) {
	for _, claim := range kinds {
		if o, ok, err := claim(ctx); err != nil || ok {
			return o, ok, err
		}
	}
	return outbound{}, false, nil
}

func (r *deliveryRun) claimTyped(ctx context.Context) (outbound, bool, error) {
	claim, ok, err := r.claimDelivery(ctx)
	if err != nil {
		return outbound{}, false, xerrors.Errorf("claim a delivery: %w", err)
	}
	if !ok {
		return outbound{}, false, nil
	}
	return r.typedOutbound(claim), true, nil
}

func (r *deliveryRun) claimGap(ctx context.Context, kind gapKind) (GapClaim, bool, error) {
	r.claimMu.Lock()
	defer r.claimMu.Unlock()
	if kind.raw && r.rawDown.Load() {
		return GapClaim{}, false, nil
	}
	claim, ok, err := kind.claim(ctx, r.svc.clock.Now(), DeliveryLease, r.gate.full())
	err = r.unlessEnded(ctx, err)
	if err != nil && kind.raw {
		// The raw part ends while the claim lock is held, so no other worker
		// claims after the failure.
		r.rawFailed(ctx, xerrors.Errorf("claim a %s: %w", kind.noun, err))
		return GapClaim{}, false, nil
	}
	if ok {
		r.gate.enter(CallbackOrigin(claim.CallbackURL))
	}
	return claim, ok, err
}

func (r *deliveryRun) add(update func(*DeliverResult)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	update(&r.result)
}

// unlessEnded drops an error that the end of the run's context caused, and
// says the run is incomplete instead. A store call that the deadline cut short
// is no store failure, and the run's result is still what it did. Any other
// error stays the run's error, also when the context ended meanwhile. It is for
// the error of a call that ran under the run's context. A call under a context
// of its own, such as a recorded result, keeps its error whatever it is.
func (r *deliveryRun) unlessEnded(ctx context.Context, err error) error {
	if ctx.Err() == nil || !xerrors.Is(err, context.Canceled) && !xerrors.Is(err, context.DeadlineExceeded) {
		return err
	}
	r.add(func(res *DeliverResult) { res.Incomplete = true })
	return nil
}

// enqueue reads the events of the subscriptions that have some, in batches
// that start with the ones furthest behind. It leaves the sends the time they
// need: it starts no subscription with less than twice minTimeToStart left.
// The reads of one principal's subscriptions share scanTimeout, so no
// principal can take the run from the others. A subscription that the run
// did not read to its end is not read again in the same run.
func (r *deliveryRun) enqueue(ctx context.Context) error {
	s := r.svc
	unfinished := map[SubscriptionKeyHash]bool{}
	spent := map[string]time.Duration{}
	for range maxScanBatches {
		now := s.clock.Now()
		targets, err := s.store.ScanTargets(ctx, now, MaxPendingDeliveries, maxScanTargetsPerBatch)
		if err != nil {
			return xerrors.Errorf("select subscriptions to scan: %w", err)
		}
		for _, target := range targets {
			if unfinished[target.KeyHash] {
				continue
			}
			if ctx.Err() != nil || r.outOfTimeFor(ctx, 2*minTimeToStart) {
				r.add(func(res *DeliverResult) { res.Incomplete = true })
				return nil
			}
			left := scanTimeout - spent[target.Principal]
			if left <= 0 {
				r.add(func(res *DeliverResult) { res.Incomplete = true })
				unfinished[target.KeyHash] = true
				continue
			}
			start := time.Now()
			if !r.scan(ctx, now, target, left) {
				unfinished[target.KeyHash] = true
			}
			spent[target.Principal] += time.Since(start)
		}
		if len(targets) < maxScanTargetsPerBatch {
			return nil
		}
	}
	r.add(func(res *DeliverResult) { res.Incomplete = true })
	return nil
}

// scan reads a subscription's events scanWindow at a time and queues each
// window's matches with the window's end as the new read position, so the
// time to read a subscription far behind does not grow with how far behind
// it is, and a read that runs out of time keeps the windows it finished. It
// reads until it reaches the head, the subscription holds
// MaxPendingDeliveries, or budget is used up, and reports whether it got to
// the end. A failure that is not the end of the run's time is kept as the
// run's error.
func (r *deliveryRun) scan(ctx context.Context, now time.Time, target ScanTarget, budget time.Duration) bool {
	logger := r.svc.logger.With(slog.F("subscription", target.KeyHash.ID()))
	var args pollArguments
	if err := json.Unmarshal(target.Arguments, &args); err != nil || !args.withinLimits() {
		if err := r.endUndecodable(ctx, now, target, logger); err != nil {
			r.scanErr = cmp.Or(r.scanErr, r.unlessEnded(ctx, err))
			return false
		}
		return true
	}
	readCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	for {
		if readCtx.Err() != nil {
			r.ranOutOfTime(ctx, logger)
			return false
		}
		done, err := r.enqueueWindow(ctx, readCtx, now, &target, args)
		switch {
		case err == nil && done:
			return true
		case err == nil:
			continue
		case ctx.Err() != nil && r.unlessEnded(ctx, err) == nil:
			return false
		case readCtx.Err() != nil && xerrors.Is(err, context.DeadlineExceeded):
			r.ranOutOfTime(ctx, logger)
			return false
		}
		r.scanErr = cmp.Or(r.scanErr, err)
		logger.Warn(ctx, "skipped a subscription whose events could not be read", slog.Error(err))
		return false
	}
}

func (r *deliveryRun) ranOutOfTime(ctx context.Context, logger slog.Logger) {
	r.add(func(res *DeliverResult) { res.Incomplete = true })
	logger.Info(ctx, "a subscription's read ran out of time, and the next run goes on from where it stopped")
}

// enqueueWindow reads the next window of target's events under readCtx and
// queues its matches under ctx, so the time of the reads cannot cut a write
// short. It moves target on, and reports whether the read is done.
func (r *deliveryRun) enqueueWindow(ctx, readCtx context.Context, now time.Time, target *ScanTarget, args pollArguments) (bool, error) {
	s := r.svc
	page, err := s.store.PollEvents(readCtx, PollQuery{
		Target:    target.Target,
		Types:     target.EventTypes,
		HeadSHA:   strings.ToLower(args.SHA),
		PR:        args.PR,
		Fields:    args.fieldFilters(func(string) []string { return target.EventTypes }),
		RepoNames: args.repoNames(),
		After:     target.ScannedThrough,
		Limit:     MaxPendingDeliveries - target.Pending,
		MaxRead:   scanWindow,
	})
	if err != nil {
		return false, xerrors.Errorf("read events for a subscription: %w", err)
	}
	if len(page.Events) == 0 && page.Next == target.ScannedThrough {
		return true, nil
	}
	e := Enqueue{Key: target.KeyHash, ExpectedScannedThrough: target.ScannedThrough, NewScannedThrough: page.Next}
	gap := target.ScannedThrough < page.PurgedThrough
	if gap {
		e.GapThrough = page.PurgedThrough
	}
	for _, ev := range page.Events {
		e.Deliveries = append(e.Deliveries, NewDelivery{EventID: ev.ID, MessageID: r.wh.messageID(target.KeyHash, ev.ID)})
	}
	won, err := s.store.EnqueueDeliveries(ctx, now, e)
	if err != nil {
		return false, xerrors.Errorf("queue deliveries: %w", err)
	}
	if !won {
		return true, nil
	}
	r.add(func(res *DeliverResult) {
		res.Enqueued += len(e.Deliveries)
		if gap {
			res.Gaps++
		}
	})
	target.ScannedThrough = page.Next
	target.Pending += int32(len(e.Deliveries)) //nolint:gosec // At most MaxPendingDeliveries, the limit of the read.
	return target.Pending >= MaxPendingDeliveries, nil
}

// endUndecodable ends a subscription whose stored arguments do not decode, or
// hold a list past the limits that a subscription stored before them can
// hold, so that no later run reads it again.
func (r *deliveryRun) endUndecodable(ctx context.Context, now time.Time, target ScanTarget, logger slog.Logger) error {
	_, found, err := r.svc.store.DeleteLiveSubscription(ctx, target.Principal, target.KeyHash, now)
	if err != nil {
		return xerrors.Errorf("end a subscription whose arguments do not decode: %w", err)
	}
	if found {
		logger.Error(ctx, "ended a subscription whose stored arguments do not decode or pass the limits", slog.F("deliveries_ended", target.Pending))
		r.add(func(res *DeliverResult) { res.GaveUp[GiveUpArgumentsUndecodable] += int(target.Pending) })
	}
	return nil
}

// gapKind is what differs between the gaps that a purge left on typed
// subscriptions and the gaps that a raw ingest recorded.
type gapKind struct {
	// raw says that the RawStore holds the gaps.
	raw    bool
	noun   string
	claim  func(ctx context.Context, now time.Time, lease time.Duration, skip []string) (GapClaim, bool, error)
	finish func(ctx context.Context, ref GapRef) error
	cursor func(through int64) string
	// through names the log field of the gap's position: the purge mark of
	// a typed gap, and the newest event that a raw subscription missed.
	through string
	// sent and failed are the log lines of a gap envelope that was delivered
	// and of one that was not.
	sent, failed string
}

func (r *deliveryRun) typedGaps() gapKind {
	return gapKind{
		noun: "gap", claim: r.svc.store.ClaimGap, finish: r.svc.store.FinishGap, cursor: encodeCursor, through: "purged_through",
		sent:   "a purge deleted events before the subscription read them, the gap envelope was sent",
		failed: "a purge deleted events before the subscription read them, the gap envelope was not delivered",
	}
}

// sendGaps sends the typed gaps and then, with a RawStore, the raw ones.
func (r *deliveryRun) sendGaps(ctx context.Context) error {
	kinds := []gapKind{r.typedGaps()}
	if r.svc.raw != nil {
		kinds = append(kinds, r.rawGaps())
	}
	for _, kind := range kinds {
		if kind.raw && r.rawDown.Load() {
			continue
		}
		err := r.sendGapsOf(ctx, kind)
		switch {
		case err != nil && kind.raw:
			r.rawFailed(ctx, err)
		case err != nil:
			return err
		}
	}
	return nil
}

func (r *deliveryRun) sendGapsOf(ctx context.Context, kind gapKind) error {
	for ctx.Err() == nil && !r.outOfTime(ctx) {
		claim, ok, err := r.claimGap(ctx, kind)
		if err != nil {
			return xerrors.Errorf("claim a %s: %w", kind.noun, err)
		}
		if !ok {
			return nil
		}
		if err := r.sendGap(ctx, kind, claim); err != nil {
			return err
		}
	}
	return nil
}

// sendGap tells a subscription that it missed events: one request, no retry,
// counted and logged once. For a typed gap the cursor is the purge mark. Every
// event that still exists lies above it, so a client that resumes from it
// loses none of them, and every watermark that a later event carries is at or
// above it. A request that the end of the run cut short has no result: the
// claim keeps its lease, and a later run sends the gap again.
func (r *deliveryRun) sendGap(ctx context.Context, kind gapKind, claim GapClaim) error {
	defer r.gate.leave(CallbackOrigin(claim.CallbackURL))
	logger := r.svc.logger.With(slog.F("subscription", claim.Ref.Key.ID()), slog.F(kind.through, claim.Ref.Through))
	failure := r.wh.sendEnvelope(ctx, r.svc.clock.Now(), envelopeGap, claim.Ref.Key, claim.CallbackURL, claim.sealedSecrets(),
		gapEnvelope{Type: envelopeGap, Cursor: kind.cursor(claim.Ref.Through)})
	if failure != "" && ctx.Err() != nil {
		logger.Info(ctx, "the run ended during the request, the gap envelope waits for its lease")
		return nil
	}
	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), finishTimeout)
	defer cancel()
	if err := kind.finish(finishCtx, claim.Ref); err != nil {
		return xerrors.Errorf("record the end of a %s: %w", kind.noun, err)
	}
	if failure == "" {
		logger.Warn(ctx, kind.sent)
	} else {
		logger.Warn(ctx, kind.failed, slog.F("reason", failure))
	}
	r.add(func(res *DeliverResult) {
		if failure == "" {
			res.GapsSent++
		} else {
			res.GapsFailed++
		}
	})
	return nil
}

// sweepExpired ends the deliveries, typed and raw, that no live subscription
// holds any more.
func (r *deliveryRun) sweepExpired(ctx context.Context) error {
	now := r.svc.clock.Now()
	deleted, err := r.svc.store.ExpireDeliveries(ctx, now)
	if err != nil {
		return xerrors.Errorf("expire deliveries: %w", err)
	}
	if r.svc.raw != nil {
		rawDeleted, err := r.svc.raw.ExpireRawDeliveries(ctx, now)
		if err = r.unlessEnded(ctx, err); err != nil {
			r.rawFailed(ctx, xerrors.Errorf("expire raw deliveries: %w", err))
		}
		deleted += rawDeleted
	}
	if deleted > 0 {
		r.svc.logger.Info(ctx, "deliveries of expired subscriptions ended", slog.F("count", deleted))
		r.add(func(res *DeliverResult) { res.GaveUp[GiveUpSubscriptionExpired] += int(deleted) })
	}
	return nil
}

// rawFailed records a failure of the RawStore and ends the raw part of the
// run.
func (r *deliveryRun) rawFailed(ctx context.Context, err error) {
	r.rawDown.Store(true)
	r.svc.logger.Error(ctx, "a raw delivery step failed, the run goes on with the typed deliveries", slog.Error(err))
	r.add(func(res *DeliverResult) { res.RawFailures++ })
}

// A sweep failure ends nothing else: signing ignores a window that ended, so
// the sends that follow stay correct.
func (r *deliveryRun) sweepPreviousSecrets(ctx context.Context) {
	cleared, err := r.svc.store.ClearExpiredPreviousSecrets(ctx, r.svc.clock.Now())
	if err != nil {
		if err = r.unlessEnded(ctx, err); err != nil {
			r.svc.logger.Error(ctx, "clear expired previous secrets", slog.Error(err))
		}
		return
	}
	if cleared > 0 {
		r.svc.logger.Info(ctx, "previous secrets of ended rotations removed", slog.F("count", cleared))
		r.add(func(res *DeliverResult) { res.PreviousSecretsCleared += int(cleared) })
	}
}

func (r *deliveryRun) send(ctx context.Context) error {
	var (
		wg       sync.WaitGroup
		reserved atomic.Int64
		stopped  atomic.Bool
		errMu    sync.Mutex
		firstErr error
	)
	fail := func(err error) {
		errMu.Lock()
		defer errMu.Unlock()
		if firstErr == nil {
			firstErr = err
		}
		stopped.Store(true)
	}
	for range deliverWorkers {
		wg.Go(func() {
			if err := r.sendGaps(ctx); err != nil {
				fail(err)
				return
			}
			kinds := r.claimKinds()
			for !stopped.Load() {
				if ctx.Err() != nil || r.outOfTime(ctx) {
					r.add(func(res *DeliverResult) { res.Incomplete = true })
					return
				}
				if reserved.Add(1) > maxClaimsPerRun {
					r.add(func(res *DeliverResult) { res.Incomplete = true })
					return
				}
				o, ok, err := claimNext(ctx, kinds)
				if err != nil {
					fail(err)
					return
				}
				if !ok {
					reserved.Add(-1)
					return
				}
				// A worker asks the other kind first after each claim, so
				// that neither kind waits behind a long queue of the other.
				slices.Reverse(kinds)
				r.add(func(res *DeliverResult) { res.Claimed++ })
				err = r.attempt(ctx, o)
				switch {
				case err != nil && o.raw:
					r.rawFailed(ctx, err)
				case err != nil:
					fail(err)
					return
				}
			}
		})
	}
	wg.Wait()
	return firstErr
}

func (r *deliveryRun) outOfTime(ctx context.Context) bool {
	return r.outOfTimeFor(ctx, minTimeToStart)
}

func (r *deliveryRun) outOfTimeFor(ctx context.Context, least time.Duration) bool {
	deadline, ok := ctx.Deadline()
	return ok && r.svc.clock.Until(deadline) < least
}

// attemptOutcome is how one attempt ended, before the store has it.
type attemptOutcome struct {
	finish Finish
	// reason names the end of a give-up or a dead secret.
	reason GiveUpReason
	sent   bool
	// abandoned says the run's context ended during the request, so the
	// attempt has no result. The store keeps the lease and the attempt, and a
	// later run retries after the lease.
	abandoned bool
	// category is the failure of an attempt that the endpoint or the network
	// failed, for the log.
	category CallbackReason
}

func giveUp(reason GiveUpReason) attemptOutcome {
	return attemptOutcome{finish: Finish{Kind: FinishGiveUp}, reason: reason}
}

// outbound is one claimed attempt of a typed or a raw delivery: what the
// shared attempt path needs, and the steps where the two kinds differ.
type outbound struct {
	// raw says that the RawStore holds the delivery.
	raw         bool
	ref         DeliveryRef
	messageID   string
	enqueuedAt  time.Time
	callbackURL string
	secretEnc   []byte
	secrets     sealedSecrets
	// mustEnd names why the delivery ends before any request, or is empty.
	mustEnd func(ctx context.Context) GiveUpReason
	// build returns the body to send, or the outcome that ends the attempt.
	build func(ctx context.Context) ([]byte, attemptOutcome, bool)
	// finish records the result in the store that holds the delivery.
	finish func(ctx context.Context, ref DeliveryRef, now time.Time, f Finish) (FinishResult, error)
}

func (r *deliveryRun) typedOutbound(claim DeliveryClaim) outbound {
	s := r.svc
	return outbound{
		ref: claim.Ref, messageID: claim.MessageID, enqueuedAt: claim.EnqueuedAt,
		callbackURL: claim.CallbackURL, secretEnc: claim.SecretEnc, secrets: claim.sealedSecrets(),
		mustEnd: func(ctx context.Context) GiveUpReason {
			switch ev := claim.Event; {
			case ev == nil:
				return GiveUpEventPurged
			case !claim.Target.Covers(ev.RepoID, ev.OwnerID):
				s.logger.Error(ctx, "event belongs to a repository that the subscription's target does not cover",
					slog.F("subscription", claim.Ref.Key.ID()), slog.F("event_id", ev.ID))
				return GiveUpRepoMismatch
			}
			return ""
		},
		build:  func(context.Context) ([]byte, attemptOutcome, bool) { return buildBody(claim, *claim.Event) },
		finish: s.store.FinishDelivery,
	}
}

func (r *deliveryRun) attempt(ctx context.Context, o outbound) error {
	defer r.gate.leave(CallbackOrigin(o.callbackURL))
	s := r.svc
	logger := s.logger.With(
		slog.F("subscription", o.ref.Key.ID()), slog.F("event_id", o.ref.EventID),
		slog.F("attempt", o.ref.Attempt), slog.F("host", callbackHost(o.callbackURL)))
	out := r.decide(ctx, o)
	if out.abandoned {
		logger.Info(ctx, "the run ended during the request, the delivery waits for its lease")
		return nil
	}

	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), finishTimeout)
	defer cancel()
	res, err := o.finish(finishCtx, o.ref, s.clock.Now(), out.finish)
	if err != nil {
		return xerrors.Errorf("record the result of a delivery: %w", err)
	}
	if !res.Accepted {
		logger.Info(ctx, "the delivery is no longer this attempt's, the result is dropped")
		r.add(func(tally *DeliverResult) {
			tally.Superseded++
			if out.sent {
				tally.Sent++
			}
		})
		return nil
	}
	switch out.finish.Kind {
	case FinishDelivered:
		logger.Info(ctx, "webhook delivered")
	case FinishRetry:
		logger.Info(ctx, "webhook failed, will retry", slog.F("category", string(out.category)), slog.F("retry_at", out.finish.RetryAt))
	default:
		logger.Warn(ctx, "webhook given up", slog.F("reason", string(out.reason)), slog.F("category", string(out.category)), slog.F("ended", res.Removed))
	}
	r.add(func(tally *DeliverResult) {
		if out.sent {
			tally.Sent++
		}
		switch out.finish.Kind {
		case FinishDelivered:
			tally.Delivered++
		case FinishRetry:
			tally.Retried++
		default:
			tally.GaveUp[out.reason] += int(res.Removed)
		}
	})
	return nil
}

func callbackHost(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// decide runs one attempt up to the point that the store needs its result. It
// sends nothing for a delivery that must end first: a purged event, an event
// of another repository, one that went past its attempts or its window, one
// too large, or one whose secret does not open.
func (r *deliveryRun) decide(ctx context.Context, o outbound) attemptOutcome {
	s := r.svc
	now := s.clock.Now()
	if reason := o.mustEnd(ctx); reason != "" {
		return giveUp(reason)
	}
	switch {
	case o.ref.Attempt > MaxDeliveryAttempts:
		return giveUp(GiveUpAttemptsExhausted)
	case now.Sub(o.enqueuedAt) > DeliveryWindow:
		return giveUp(GiveUpWindowExpired)
	}
	secrets, err := r.wh.signingSecrets(ctx, now, o.ref.Key, o.secrets)
	if err != nil {
		return attemptOutcome{finish: Finish{Kind: FinishDead, Unreadable: o.secretEnc}, reason: GiveUpSecretUnreadable}
	}
	body, outcome, ok := o.build(ctx)
	if !ok {
		return outcome
	}
	headers, err := signWithSecrets(secrets, o.messageID, now, body)
	if err != nil {
		return attemptOutcome{finish: Finish{Kind: FinishDead, Unreadable: o.secretEnc}, reason: GiveUpSecretUnreadable}
	}
	status, category := r.wh.post(ctx, o.callbackURL, o.ref.Key.ID(), headers, body)
	if status == 0 && ctx.Err() != nil {
		return attemptOutcome{abandoned: true}
	}
	return classify(o, now, status, category)
}

func buildBody(claim DeliveryClaim, ev Event) ([]byte, attemptOutcome, bool) {
	occ, err := occurrenceOf(claim.EventType, ev, claim.Target)
	if err != nil {
		return nil, giveUp(GiveUpEventMalformed), false
	}
	return encodeWebhookBody(webhookBody{
		EventID: claim.MessageID, Name: occ.Name, Timestamp: occ.Timestamp, Data: occ.Data, Cursor: encodeCursor(claim.Watermark),
	})
}

func encodeWebhookBody(b webhookBody) ([]byte, attemptOutcome, bool) {
	body, err := json.Marshal(b)
	if err != nil {
		return nil, giveUp(GiveUpEventMalformed), false
	}
	if len(body) > maxEventBodyBytes {
		return nil, giveUp(GiveUpEventTooLarge), false
	}
	return body, attemptOutcome{}, true
}

// deliveryFailureOf maps an error of the client to a category. A response that
// the client refuses to read, one with a body past 64 KiB or headers past 16
// KiB, is the endpoint's fault, so it counts as a server error. The transport
// has no error value for the headers, so the check reads its text and keeps
// nothing of it.
func deliveryFailureOf(err error) CallbackReason {
	if xerrors.Is(err, ErrResponseTooLarge) || strings.Contains(err.Error(), "response headers exceeded") {
		return CallbackHTTP5xx
	}
	return callbackReasonOf(err)
}

// classify turns the answer of the endpoint into the next step: 2xx ends the
// delivery, 410 and 413 end it without a retry and leave the subscription's
// delivery status alone (SEP-3415), and every other answer fails
// the attempt. A failed attempt is retried after its backoff unless it was the
// last one or the next one would fall outside the window.
func classify(o outbound, now time.Time, status int, category CallbackReason) attemptOutcome {
	out := attemptOutcome{sent: true, category: category}
	switch {
	case status >= 200 && status <= 299:
		out.finish = Finish{Kind: FinishDelivered}
		return out
	case status == http.StatusGone:
		out.finish, out.reason = Finish{Kind: FinishGiveUp}, GiveUpEndpointGone
		return out
	case status == http.StatusRequestEntityTooLarge:
		out.finish, out.reason = Finish{Kind: FinishGiveUp}, GiveUpEndpointPayloadTooLarge
		return out
	case status >= 500:
		out.category = CallbackHTTP5xx
	case status > 0:
		out.category = CallbackHTTP4xx
	}
	switch retryAt := now.Add(deliveryBackoff[min(int(o.ref.Attempt), len(deliveryBackoff))-1]); {
	case o.ref.Attempt >= MaxDeliveryAttempts:
		out.finish, out.reason = Finish{Kind: FinishGiveUp, Failure: out.category}, GiveUpAttemptsExhausted
	case retryAt.Sub(o.enqueuedAt) > DeliveryWindow:
		out.finish, out.reason = Finish{Kind: FinishGiveUp, Failure: out.category}, GiveUpWindowExpired
	default:
		out.finish = Finish{Kind: FinishRetry, RetryAt: retryAt, Failure: out.category}
	}
	return out
}

// messageID is the webhook-id of the delivery of one event to one subscription.
func (wh *webhookState) messageID(key SubscriptionKeyHash, eventID int64) string {
	mac := hmac.New(sha256.New, wh.idKey)
	_, _ = mac.Write(key[:])
	_, _ = mac.Write([]byte(strconv.FormatInt(eventID, 10)))
	return messageIDPrefix + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)[:messageIDEntropy])
}
