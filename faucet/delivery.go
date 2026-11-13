package faucet

import (
	"encoding/json"
	"time"

	"golang.org/x/xerrors"
)

const (
	// MaxPendingDeliveries is the most unsent deliveries one subscription
	// holds. The enqueue stops there, and the events behind the cap stay in
	// the log until the deliveries ahead of them end.
	MaxPendingDeliveries = 100
	// MaxDeliveryAttempts is the most requests that one delivery gets.
	MaxDeliveryAttempts = 5
	// DeliveryWindow is how long after the enqueue a delivery may still send.
	DeliveryWindow = 15 * time.Minute
	// DeliveryLease is how long a claim keeps a delivery from other runs. A
	// run that crashes leaves the lease to run out.
	DeliveryLease = 30 * time.Second
)

// deliveryBackoff is the wait after attempt n, at index n-1. The fifth
// attempt has no wait, because nothing follows it.
var deliveryBackoff = [MaxDeliveryAttempts - 1]time.Duration{15 * time.Second, time.Minute, 3 * time.Minute, 6 * time.Minute}

// DeliveryStatus is what a subscription has recorded about its deliveries, for
// the deliveryStatus object of SEP-3415. A zero time means never.
type DeliveryStatus struct {
	// LastDeliveryAt is when the endpoint last answered 2xx.
	LastDeliveryAt time.Time
	// LastError is the category of the last failed attempt since then. It is
	// empty while the endpoint is healthy.
	LastError CallbackReason
	// FailedSince is when the first failed attempt since the last 2xx came.
	FailedSince time.Time
}

// ScanTarget is a live subscription whose events the enqueue has not read to
// the end of the log.
type ScanTarget struct {
	KeyHash SubscriptionKeyHash
	// EventType and Arguments are the subscription's own.
	EventType string
	Arguments json.RawMessage
	// RepoID is the repository the principal could read at the last subscribe.
	RepoID int64
	// ScannedThrough is the highest event ID the enqueue has read for it.
	ScannedThrough int64
	// Pending is the number of deliveries it holds that have not ended.
	Pending int32
}

// NewDelivery is one event that a subscription is to receive.
type NewDelivery struct {
	EventID int64
	// MessageID is the webhook-id of the delivery: random, one per (subscription,
	// event), and the same on every attempt.
	MessageID string
}

// Enqueue is a request to queue the deliveries that a read of the event log
// found, and to move the subscription's read position past them.
type Enqueue struct {
	Key SubscriptionKeyHash
	// ExpectedScannedThrough is the position the caller read from. The store
	// queues nothing when the subscription is no longer at it.
	ExpectedScannedThrough int64
	NewScannedThrough      int64
	Deliveries             []NewDelivery
}

// Validate checks the fields a caller supplies to Store.EnqueueDeliveries.
func (e Enqueue) Validate() error {
	switch {
	case e.ExpectedScannedThrough < 0:
		return xerrors.New("expected position must not be negative")
	case e.NewScannedThrough < e.ExpectedScannedThrough:
		return xerrors.New("the position must not move back")
	}
	for _, d := range e.Deliveries {
		switch {
		case d.EventID <= e.ExpectedScannedThrough || d.EventID > e.NewScannedThrough:
			return xerrors.New("every event must lie above the old position and at or below the new one")
		case !validMessageID(d.MessageID):
			return xerrors.New("message ID is not a valid webhook id")
		}
	}
	return nil
}

// DeliveryRef names one attempt of one delivery. Store.FinishDelivery changes
// a delivery only for the attempt that the claim returned, and only while the
// row still has the message ID that the claim saw, so a subscription that was
// deleted and made again does not take over the result of an older attempt.
type DeliveryRef struct {
	Key       SubscriptionKeyHash
	EventID   int64
	MessageID string
	// Attempt counts from 1 and rises at every claim.
	Attempt int32
}

// DeliveryClaim is a delivery that a run now owns for DeliveryLease.
type DeliveryClaim struct {
	Ref        DeliveryRef
	MessageID  string
	EnqueuedAt time.Time
	// CallbackURL and SecretEnc are the subscription's. SecretEnc is the sealed
	// secret, never the plaintext.
	CallbackURL string
	SecretEnc   []byte
	// RepoID is the repository the subscription is granted now.
	RepoID int64
	// Watermark is the lowest unsent event ID of the subscription, minus one.
	Watermark int64
	// Event is nil when a purge deleted the event.
	Event *Event
}

// FinishKind says how an attempt ended.
type FinishKind int

const (
	// FinishDelivered ends the delivery: the endpoint answered 2xx.
	FinishDelivered FinishKind = iota + 1
	// FinishRetry keeps the delivery and sets the time of the next attempt.
	FinishRetry
	// FinishGiveUp ends the delivery without a success.
	FinishGiveUp
	// FinishDead ends the delivery because the sealed secret cannot be opened,
	// and deletes the subscription if it still holds those sealed bytes.
	FinishDead
)

// Finish is the outcome of one attempt.
type Finish struct {
	Kind FinishKind
	// RetryAt is the time of the next attempt, for FinishRetry.
	RetryAt time.Time
	// Failure is the category that the subscription records as lastError.
	// FinishRetry needs one. FinishGiveUp carries one when a failed attempt was
	// the last, and leaves it empty when faucet ended the delivery or the
	// endpoint refused this one with 410 or 413.
	Failure CallbackReason
	// Unreadable is the sealed secret that could not be opened, for FinishDead.
	Unreadable []byte
}

// Validate checks the fields a caller supplies to Store.FinishDelivery.
func (f Finish) Validate() error {
	switch {
	case f.Kind == FinishDelivered && (!f.RetryAt.IsZero() || f.Failure != "" || f.Unreadable != nil):
		return xerrors.New("a delivered attempt carries no retry time, failure or sealed secret")
	case f.Kind == FinishRetry && (f.RetryAt.IsZero() || !f.Failure.valid() || f.Unreadable != nil):
		return xerrors.New("a retry needs a retry time and a failure category")
	case f.Kind == FinishGiveUp && (!f.RetryAt.IsZero() || f.Unreadable != nil || f.Failure != "" && !f.Failure.valid()):
		return xerrors.New("a give-up carries no retry time or sealed secret, and its failure must be a category")
	case f.Kind == FinishDead && (len(f.Unreadable) == 0 || !f.RetryAt.IsZero() || f.Failure != ""):
		return xerrors.New("a dead delivery needs the sealed secret and nothing else")
	case f.Kind < FinishDelivered || f.Kind > FinishDead:
		return xerrors.New("unknown finish kind")
	}
	return nil
}

// FinishResult is what Store.FinishDelivery reports.
type FinishResult struct {
	// Accepted is false when the attempt is not the delivery's current one, or
	// the delivery is gone. The store then changed nothing.
	Accepted bool
	// Removed is the number of deliveries the call ended: one, and for a
	// FinishDead that deleted the subscription, the others it held.
	Removed int64
}

// GiveUpReason says why a delivery ended without a success. Every reason is
// counted in DeliverResult and named in one log line.
type GiveUpReason string

const (
	GiveUpEndpointGone            GiveUpReason = "endpoint_gone"
	GiveUpEndpointPayloadTooLarge GiveUpReason = "endpoint_payload_too_large"
	GiveUpEventTooLarge           GiveUpReason = "event_too_large"
	GiveUpEventMalformed          GiveUpReason = "event_malformed"
	GiveUpAttemptsExhausted       GiveUpReason = "attempts_exhausted"
	GiveUpWindowExpired           GiveUpReason = "window_expired"
	GiveUpEventPurged             GiveUpReason = "event_purged"
	GiveUpRepoMismatch            GiveUpReason = "repo_mismatch"
	GiveUpSecretUnreadable        GiveUpReason = "secret_unreadable"
	GiveUpSubscriptionExpired     GiveUpReason = "subscription_expired"
)

// DeliverResult counts what one DeliverDue run did. Every delivery that ended
// in the run is in Delivered or in one entry of GaveUp.
type DeliverResult struct {
	// Enqueued is the deliveries queued for events that the run read.
	Enqueued int
	// Gaps is the subscriptions that missed events because a purge deleted
	// them before the run read them.
	Gaps int
	// Claimed is the attempts that the run took. Sent is how many of them made
	// a request.
	Claimed int
	Sent    int
	// Delivered is the deliveries that the endpoint answered with 2xx.
	Delivered int
	// Retried is the failed attempts that were scheduled again.
	Retried int
	// GaveUp is the deliveries that ended without a success, by reason.
	GaveUp map[GiveUpReason]int
	// Superseded is the attempts whose result the store refused, because
	// another run owns the delivery now or its subscription was deleted. The
	// other run counts its end.
	Superseded int
	// Incomplete says that due work was left, because the run reached its
	// limit or its deadline.
	Incomplete bool
}

// GaveUpTotal is the number of deliveries that ended without a success.
func (r DeliverResult) GaveUpTotal() int {
	total := 0
	for _, n := range r.GaveUp {
		total += n
	}
	return total
}
