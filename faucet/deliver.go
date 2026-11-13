package faucet

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
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
// event type, that match its sha and pr arguments, and only while it is live.
// Each event is one delivery, with one webhook-id for every attempt. A delivery
// gets at most 5 attempts in 15 minutes, after 15 seconds, 1 minute, 3 minutes
// and 6 minutes. A 2xx ends it. A 410 and a 413 end it too, and neither ends
// the subscription. Every delivery that ends in the call is in the result,
// delivered or with its reason.
//
// A subscription that a purge overtook, one whose unread events were deleted,
// gets one gap envelope when the call queues its next events. The queueing
// makes no request: the envelope goes out in the send phase, ahead of the
// deliveries, on the same workers and under the same deadline. It is one
// attempt with no retry, and the result counts it as sent or failed. A call
// that ends before it sends the envelope counts it as failed.
//
// Calls may overlap, in one process or in several. A claim gives an attempt to
// one caller for 30 seconds, and a caller whose claim lapsed records nothing.
// A call that dies between a claim and its result leaves the delivery to the
// next call after the lease, and the attempt counts.
//
// The error is the first failure of the store. The result holds what happened
// before it. The method has no auth of its own. The host decides who may call
// it.
func (s *Service) DeliverDue(ctx context.Context) (DeliverResult, error) {
	if s.webhook == nil {
		return DeliverResult{}, ErrNoWebhook
	}
	run := &deliveryRun{svc: s, wh: s.webhook, result: DeliverResult{GaveUp: map[GiveUpReason]int{}}}
	if err := run.enqueue(ctx); err != nil {
		run.abandonUnsentGaps(ctx, gapRunFailed)
		return run.result, run.unlessEnded(ctx, err)
	}
	if err := run.sweepExpired(ctx); err != nil {
		run.abandonUnsentGaps(ctx, gapRunFailed)
		return run.result, run.unlessEnded(ctx, err)
	}
	if err := run.sweepPreviousSecrets(ctx); err != nil {
		return run.result, run.unlessEnded(ctx, err)
	}
	err := run.send(ctx)
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
	// gaps are the gap envelopes to send, in the order the enqueue found them.
	// The enqueue appends and the send phase takes them, so nothing reads the
	// slice while the other writes it. nextGap is the first one not yet taken.
	gaps    []queuedGap
	nextGap atomic.Int64
}

// queuedGap is a subscription whose enqueue was won and whose read position
// was below the purge mark. Its envelope is the one that the send phase owes.
type queuedGap struct {
	target        ScanTarget
	purgedThrough int64
}

func (r *deliveryRun) add(update func(*DeliverResult)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	update(&r.result)
}

// unlessEnded drops an error that the end of the run's context caused, and
// says the run is incomplete instead. A store call that the deadline cut short
// is no store failure, and the run's result is still what it did.
func (r *deliveryRun) unlessEnded(ctx context.Context, err error) error {
	if ctx.Err() == nil {
		return err
	}
	r.add(func(res *DeliverResult) { res.Incomplete = true })
	return nil
}

// enqueue reads the events of the subscriptions that have some, in batches
// that start with the ones furthest behind. It leaves the sends the time they
// need: it starts no subscription with less than twice minTimeToStart left.
func (r *deliveryRun) enqueue(ctx context.Context) error {
	s := r.svc
	for range maxScanBatches {
		now := s.clock.Now()
		targets, err := s.store.ScanTargets(ctx, now, MaxPendingDeliveries, maxScanTargetsPerBatch)
		if err != nil {
			return xerrors.Errorf("select subscriptions to scan: %w", err)
		}
		for _, target := range targets {
			if ctx.Err() != nil || r.outOfTimeFor(ctx, 2*minTimeToStart) {
				r.add(func(res *DeliverResult) { res.Incomplete = true })
				return nil
			}
			if err := r.enqueueFor(ctx, now, target); err != nil {
				return err
			}
		}
		if len(targets) < maxScanTargetsPerBatch {
			return nil
		}
	}
	r.add(func(res *DeliverResult) { res.Incomplete = true })
	return nil
}

func (r *deliveryRun) enqueueFor(ctx context.Context, now time.Time, target ScanTarget) error {
	s := r.svc
	logger := s.logger.With(slog.F("subscription", target.KeyHash.ID()))
	var args pollArguments
	if err := json.Unmarshal(target.Arguments, &args); err != nil {
		logger.Error(ctx, "stored subscription has arguments that do not decode")
		return nil
	}
	page, err := s.store.PollEvents(ctx, PollQuery{
		RepoID:  target.RepoID,
		Types:   []string{target.EventType},
		HeadSHA: strings.ToLower(args.SHA),
		PR:      args.PR,
		After:   target.ScannedThrough,
		Limit:   MaxPendingDeliveries - target.Pending,
	})
	if err != nil {
		return xerrors.Errorf("read events for a subscription: %w", err)
	}
	if len(page.Events) == 0 && page.Next == target.ScannedThrough {
		return nil
	}
	e := Enqueue{Key: target.KeyHash, ExpectedScannedThrough: target.ScannedThrough, NewScannedThrough: page.Next}
	for _, ev := range page.Events {
		e.Deliveries = append(e.Deliveries, NewDelivery{EventID: ev.ID, MessageID: r.wh.messageID(target.KeyHash, ev.ID)})
	}
	won, err := s.store.EnqueueDeliveries(ctx, now, e)
	if err != nil {
		return xerrors.Errorf("queue deliveries: %w", err)
	}
	if !won {
		return nil
	}
	gap := target.ScannedThrough < page.PurgedThrough
	r.add(func(res *DeliverResult) {
		res.Enqueued += len(e.Deliveries)
		if gap {
			res.Gaps++
			r.gaps = append(r.gaps, queuedGap{target: target, purgedThrough: page.PurgedThrough})
		}
	})
	return nil
}

const (
	gapRunEnded  = "run_ended"
	gapRunFailed = "run_failed"
)

func (r *deliveryRun) gapLogger(g queuedGap) slog.Logger {
	return r.svc.logger.With(slog.F("subscription", g.target.KeyHash.ID()),
		slog.F("scanned_through", g.target.ScannedThrough), slog.F("purged_through", g.purgedThrough))
}

// sendGaps takes queued gaps until none is left or the run is out of time.
func (r *deliveryRun) sendGaps(ctx context.Context) {
	for ctx.Err() == nil && !r.outOfTime(ctx) {
		i := int(r.nextGap.Add(1)) - 1
		if i >= len(r.gaps) {
			return
		}
		r.sendGap(ctx, r.gaps[i])
	}
}

// sendGap tells a subscription that a purge deleted events before the run read
// them: one request, no retry, counted and logged once. The cursor is the
// purge mark. Every event that still exists lies above it, so a client that
// resumes from it loses none of them, and every watermark that a later event
// carries is at or above it.
func (r *deliveryRun) sendGap(ctx context.Context, g queuedGap) {
	failure := r.wh.sendEnvelope(ctx, r.svc.clock.Now(), envelopeGap, g.target.KeyHash, g.target.CallbackURL, g.target.sealedSecrets(),
		gapEnvelope{Type: envelopeGap, Cursor: encodeCursor(g.purgedThrough)})
	logger := r.gapLogger(g)
	if failure == "" {
		logger.Warn(ctx, "a purge deleted events before the subscription read them, the gap envelope was sent")
	} else {
		logger.Warn(ctx, "a purge deleted events before the subscription read them, the gap envelope was not delivered", slog.F("reason", failure))
	}
	r.add(func(res *DeliverResult) {
		if failure == "" {
			res.GapsSent++
		} else {
			res.GapsFailed++
		}
	})
}

// abandonUnsentGaps counts the gaps that no worker took as failed, one log line
// each. Their enqueue is won and moved the read position past the purge mark,
// so no later run finds them: they are never dropped silently.
func (r *deliveryRun) abandonUnsentGaps(ctx context.Context, reason string) {
	first := min(int(r.nextGap.Swap(int64(len(r.gaps)))), len(r.gaps))
	for _, g := range r.gaps[first:] {
		r.gapLogger(g).Warn(ctx, "a purge deleted events before the subscription read them, the gap envelope was not sent", slog.F("reason", reason))
	}
	if first < len(r.gaps) {
		r.add(func(res *DeliverResult) {
			res.GapsFailed += len(r.gaps) - first
			res.Incomplete = true
		})
	}
}

func (r *deliveryRun) sweepExpired(ctx context.Context) error {
	deleted, err := r.svc.store.ExpireDeliveries(ctx, r.svc.clock.Now())
	if err != nil {
		return xerrors.Errorf("expire deliveries: %w", err)
	}
	if deleted > 0 {
		r.svc.logger.Info(ctx, "deliveries of expired subscriptions ended", slog.F("count", deleted))
		r.add(func(res *DeliverResult) { res.GaveUp[GiveUpSubscriptionExpired] += int(deleted) })
	}
	return nil
}

func (r *deliveryRun) sweepPreviousSecrets(ctx context.Context) error {
	cleared, err := r.svc.store.ClearExpiredPreviousSecrets(ctx, r.svc.clock.Now())
	if err != nil {
		return xerrors.Errorf("clear expired previous secrets: %w", err)
	}
	if cleared > 0 {
		r.svc.logger.Info(ctx, "previous secrets of ended rotations removed", slog.F("count", cleared))
		r.add(func(res *DeliverResult) { res.PreviousSecretsCleared += int(cleared) })
	}
	return nil
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
			r.sendGaps(ctx)
			for !stopped.Load() {
				if ctx.Err() != nil || r.outOfTime(ctx) {
					r.add(func(res *DeliverResult) { res.Incomplete = true })
					return
				}
				if reserved.Add(1) > maxClaimsPerRun {
					r.add(func(res *DeliverResult) { res.Incomplete = true })
					return
				}
				claim, ok, err := r.svc.store.ClaimDelivery(ctx, r.svc.clock.Now(), DeliveryLease)
				if err != nil {
					fail(xerrors.Errorf("claim a delivery: %w", err))
					return
				}
				if !ok {
					reserved.Add(-1)
					return
				}
				r.add(func(res *DeliverResult) { res.Claimed++ })
				if err := r.attempt(ctx, claim); err != nil {
					fail(err)
					return
				}
			}
		})
	}
	wg.Wait()
	r.abandonUnsentGaps(ctx, gapRunEnded)
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

func (r *deliveryRun) attempt(ctx context.Context, claim DeliveryClaim) error {
	s := r.svc
	logger := s.logger.With(
		slog.F("subscription", claim.Ref.Key.ID()), slog.F("event_id", claim.Ref.EventID),
		slog.F("attempt", claim.Ref.Attempt), slog.F("host", callbackHost(claim.CallbackURL)))
	out := r.decide(ctx, claim)
	if out.abandoned {
		logger.Info(ctx, "the run ended during the request, the delivery waits for its lease")
		return nil
	}

	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), finishTimeout)
	defer cancel()
	res, err := s.store.FinishDelivery(finishCtx, claim.Ref, s.clock.Now(), out.finish)
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
func (r *deliveryRun) decide(ctx context.Context, claim DeliveryClaim) attemptOutcome {
	s := r.svc
	now := s.clock.Now()
	ev := claim.Event
	switch {
	case ev == nil:
		return giveUp(GiveUpEventPurged)
	case ev.RepoID != claim.RepoID:
		s.logger.Error(ctx, "event belongs to another repository than the subscription's",
			slog.F("subscription", claim.Ref.Key.ID()), slog.F("event_id", ev.ID))
		return giveUp(GiveUpRepoMismatch)
	case claim.Ref.Attempt > MaxDeliveryAttempts:
		return giveUp(GiveUpAttemptsExhausted)
	case now.Sub(claim.EnqueuedAt) > DeliveryWindow:
		return giveUp(GiveUpWindowExpired)
	}
	secrets, err := r.wh.signingSecrets(now, claim.Ref.Key, claim.sealedSecrets())
	if err != nil {
		return attemptOutcome{finish: Finish{Kind: FinishDead, Unreadable: claim.SecretEnc}, reason: GiveUpSecretUnreadable}
	}
	body, outcome, ok := buildBody(claim, *ev)
	if !ok {
		return outcome
	}
	headers, err := signWithSecrets(secrets, claim.MessageID, now, body)
	if err != nil {
		return attemptOutcome{finish: Finish{Kind: FinishDead, Unreadable: claim.SecretEnc}, reason: GiveUpSecretUnreadable}
	}
	status, category := r.wh.post(ctx, claim.CallbackURL, claim.Ref.Key.ID(), headers, body)
	if status == 0 && ctx.Err() != nil {
		return attemptOutcome{abandoned: true}
	}
	return classify(claim, now, status, category)
}

func buildBody(claim DeliveryClaim, ev Event) ([]byte, attemptOutcome, bool) {
	occ, err := occurrenceOf(ev)
	if err != nil {
		return nil, giveUp(GiveUpEventMalformed), false
	}
	body, err := json.Marshal(webhookBody{
		EventID: claim.MessageID, Name: occ.Name, Timestamp: occ.Timestamp, Data: occ.Data, Cursor: encodeCursor(claim.Watermark),
	})
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
func classify(claim DeliveryClaim, now time.Time, status int, category CallbackReason) attemptOutcome {
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
	switch retryAt := now.Add(deliveryBackoff[min(int(claim.Ref.Attempt), len(deliveryBackoff))-1]); {
	case claim.Ref.Attempt >= MaxDeliveryAttempts:
		out.finish, out.reason = Finish{Kind: FinishGiveUp, Failure: out.category}, GiveUpAttemptsExhausted
	case retryAt.Sub(claim.EnqueuedAt) > DeliveryWindow:
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
