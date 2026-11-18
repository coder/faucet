package faucet

import (
	"context"
	"encoding/json"
	"time"

	"golang.org/x/xerrors"
)

// RawLeaseID names one poll lease. The poll cursor carries it.
type RawLeaseID [16]byte

var (
	// ErrRawInterestLimit is returned by RawStore.OpenRawLease and
	// RawStore.UpsertRawSubscription when an event of a repository already
	// has RawLimits.PerTarget live subscriptions and poll leases.
	ErrRawInterestLimit = xerrors.New("raw interest limit reached")
	// ErrRawShareLimit is returned by RawStore.UpsertRawSubscription when the
	// principal already holds RawLimits.PerPrincipalTarget live subscriptions
	// of the event of the repository.
	ErrRawShareLimit = xerrors.New("raw subscription limit of the principal reached")
)

// RawLimits bound the raw interests that RawStore.OpenRawLease and
// RawStore.UpsertRawSubscription store. Each must be positive.
type RawLimits struct {
	// Live is the most live leases, or subscriptions, of one principal.
	Live int
	// PerTarget is the most live subscriptions and leases of one event of one
	// repository.
	PerTarget int
	// PerPrincipalTarget is the most live leases, and the most live
	// subscriptions, of one event of one repository that one principal holds,
	// so that no principal takes all of PerTarget.
	PerPrincipalTarget int
}

// Validate checks that every limit is positive.
func (l RawLimits) Validate() error {
	if l.Live <= 0 || l.PerTarget <= 0 || l.PerPrincipalTarget <= 0 {
		return xerrors.Errorf("raw limits must be positive, got %+v", l)
	}
	return nil
}

// RawEvent is one stored GitHub delivery of an event that no typed family
// stores.
type RawEvent struct {
	// ID is assigned by the store. It is the position of a raw cursor.
	ID int64
	// DeliveryID is the X-GitHub-Delivery header. It is unique per raw event.
	DeliveryID string
	// Event is GitHub's event name, for example "push".
	Event string
	// Action is the action member of the payload. It is empty when the
	// payload has none.
	Action string
	RepoID int64
	// Body is the payload as stored. It is nil when no interest could get the
	// event, and once no live interest has it unread.
	Body json.RawMessage
	// Fields are the field paths that Body holds. Nil with a Body means the
	// whole payload.
	Fields []string
	// ReceivedAt is assigned by the store.
	ReceivedAt time.Time
}

// RawDelivery is what a webhook request tells about a delivery before any
// interest is read.
type RawDelivery struct {
	DeliveryID string
	// Event is GitHub's event name, and Action the action member of the
	// payload, empty when it has none.
	Event  string
	Action string
	RepoID int64
}

// Validate checks the fields a caller supplies to RawStore.IngestRaw.
func (d RawDelivery) Validate() error {
	switch {
	case d.DeliveryID == "":
		return xerrors.New("delivery ID is required")
	case d.Event == "":
		return xerrors.New("event is required")
	case d.RepoID <= 0:
		return xerrors.New("repo ID must be positive")
	}
	return nil
}

// RawInterest is one live interest that matches a delivery: a webhook
// subscription, or a poll lease when IsLease is set.
type RawInterest struct {
	// Subscription and Incarnation are the key and the CreatedAt of a webhook
	// subscription.
	Subscription SubscriptionKeyHash
	Incarnation  time.Time
	// Lease is the ID of a poll lease.
	Lease   RawLeaseID
	IsLease bool
	// Fields are the field paths that the interest asked for. Nil means the
	// whole payload.
	Fields []string
	// Pending is the number of unsent deliveries that the subscription holds,
	// or of unread events that the lease holds. Under concurrent ingests it
	// can be a few too low.
	Pending int32
}

// RawOutbox is one delivery that a subscription is to get.
type RawOutbox struct {
	Key         SubscriptionKeyHash
	Incarnation time.Time
}

// RawGap names a subscription that misses the event and is owed a gap
// envelope: it holds the most unsent deliveries, or the event is too large
// for it.
type RawGap struct {
	Key         SubscriptionKeyHash
	Incarnation time.Time
}

// RawPlan is what RawStore.IngestRaw writes for one delivery. The caller of
// IngestRaw builds it from the interests that match.
type RawPlan struct {
	// Body is the payload to store. Nil means that no interest is served, so
	// the row keeps only the delivery ID.
	Body []byte
	// Fields are the field paths that Body holds. Nil with a Body means the
	// whole payload.
	Fields []string
	// Deliveries has one entry per subscription that is served.
	Deliveries []RawOutbox
	// Unread are the leases that get the event.
	Unread []RawLeaseID
	// Gaps are the subscriptions that miss the event.
	Gaps []RawGap
	// Lost are the leases that miss the event. Each keeps one mark, at the
	// highest raw ID that it missed, and the read that passes the mark counts
	// it in RawPage.Skipped. A mark is no unread event: it keeps no body.
	Lost []RawLeaseID
}

// Validate checks a plan before RawStore.IngestRaw writes it.
func (p RawPlan) Validate() error {
	switch {
	case p.Fields != nil && p.Body == nil:
		return xerrors.New("field paths need a body")
	case p.Body == nil && (len(p.Unread) > 0 || len(p.Deliveries) > 0):
		return xerrors.New("an interest is served, so the plan needs a body")
	}
	return nil
}

// RawIngest is what RawStore.IngestRaw reports.
type RawIngest struct {
	// Matched is false when no live interest wanted the delivery. The store
	// then wrote nothing.
	Matched bool
	// Duplicate is true when the delivery ID was already stored. The store
	// then wrote nothing.
	Duplicate bool
	// ID is the raw ID of the stored event.
	ID int64
}

// RawLease is one poller's interest in a raw event name.
type RawLease struct {
	ID RawLeaseID
	// Key is the hash of the login, the event name and the canonical
	// arguments, made with NewSubscriptionKeyHash and an empty callback URL.
	Key       SubscriptionKeyHash
	Principal string
	// Event is GitHub's event name. Actions empty means every action, and
	// Fields nil the whole payload.
	Event     string
	Actions   []string
	Fields    []string
	RepoID    int64
	ExpiresAt time.Time
}

// Validate checks the fields a caller supplies to RawStore.OpenRawLease.
func (l RawLease) Validate(now time.Time) error {
	switch {
	case l.ID == (RawLeaseID{}):
		return xerrors.New("lease ID is required")
	case l.Principal == "":
		return xerrors.New("principal is required")
	case l.Event == "":
		return xerrors.New("event is required")
	case l.RepoID <= 0:
		return xerrors.New("repo ID must be positive")
	case !l.ExpiresAt.After(now):
		return xerrors.New("expiry must be after now")
	}
	return nil
}

// RawRead is one poll of a lease.
type RawRead struct {
	Lease  RawLeaseID
	Key    SubscriptionKeyHash
	RepoID int64
	// After is the cursor position: the poller has every event at or below it.
	After int64
	// Limit is the most events of a page. It must be positive. An unread
	// event that the page passes over does not count.
	Limit int32
	// MaxBytes ends a page after the event that takes the bodies read to this
	// size or more. It must be positive. The first event always fits.
	MaxBytes int64
	// NotBefore passes over the events received before it. Zero means no floor.
	NotBefore time.Time
	Now       time.Time
	// ExpiresAt is the lease's expiry after the read.
	ExpiresAt time.Time
}

// Validate checks the fields a caller supplies to RawStore.ReadRawLease.
func (q RawRead) Validate() error {
	switch {
	case q.After < 0:
		return xerrors.New("after must not be negative")
	case q.Limit <= 0:
		return xerrors.New("limit must be positive")
	case q.MaxBytes <= 0:
		return xerrors.New("max bytes must be positive")
	case !q.ExpiresAt.After(q.Now):
		return xerrors.New("expiry must be after now")
	}
	return nil
}

// RawPage is the result of RawStore.ReadRawLease.
type RawPage struct {
	// Events are the unread events above RawRead.After in ID order.
	Events []RawEvent
	// Next is the position to poll with next. With More set it is the ID of
	// the last event. Otherwise it is the highest of RawRead.After, the last
	// unread event that the page returned or passed over, and the highest raw
	// ID stored, read in the same snapshot as the page, so that a later read
	// with it skips no event.
	Next int64
	// More says that the page ended at its limit of events or bytes, and that
	// the lease holds an unread event or a mark beyond it. A page ends early
	// only after an event, so a page with More holds at least one.
	More bool
	// Skipped is the number of unread events that the page passed over: the
	// lease missed them (RawPlan.Lost), their row is gone, their body is
	// cleared, or they were received before RawRead.NotBefore.
	Skipped int
	// AckedThrough is the highest position that the poller had presented before
	// this read.
	AckedThrough int64
}

// RawDeliveryClaim is a raw delivery that a run now owns for DeliveryLease.
type RawDeliveryClaim struct {
	// Ref names the attempt. Its EventID is the raw ID, and its MessageID is
	// empty: the service derives the webhook-id.
	Ref        DeliveryRef
	EnqueuedAt time.Time
	// EventType and Arguments are the subscription's event name and canonical
	// arguments.
	EventType string
	Arguments json.RawMessage
	// CallbackURL and the secrets are the subscription's as of the claim.
	// SecretEnc is the sealed secret, never the plaintext.
	CallbackURL         string
	SecretEnc           []byte
	PreviousSecretEnc   []byte
	PreviousSecretUntil time.Time
	// RepoID is the repository the subscription is granted now.
	RepoID int64
	// Watermark is the lowest unsent raw ID of the subscription, minus one.
	Watermark int64
	// Event is nil when the event is gone or its body is cleared.
	Event *RawEvent
}

// RawPurge says what RawStore.PurgeRawEvents removes. A zero ClearBefore or
// DeleteBefore turns its rule off.
type RawPurge struct {
	// Now decides which leases have expired.
	Now time.Time
	// ClearBefore drops the body of every event received before it, also
	// while a delivery or an unread event still names it. It bounds how long
	// a poller that never moves its cursor keeps payloads.
	ClearBefore time.Time
	// DeleteBefore deletes every event received before it.
	DeleteBefore time.Time
}

// Validate checks the fields a caller supplies to RawStore.PurgeRawEvents.
func (p RawPurge) Validate() error {
	if p.Now.IsZero() {
		return xerrors.New("purge time is required")
	}
	return nil
}

// RawPurgeResult is what RawStore.PurgeRawEvents reports.
type RawPurgeResult struct {
	// ExpiredLeases is the number of poll leases that had expired.
	ExpiredLeases int64
	// Cleared is the number of raw events whose body the call dropped: no
	// delivery and no unread event named them any more, or they were received
	// before RawPurge.ClearBefore.
	Cleared int64
	// Deleted is the number of raw events that the call deleted at the
	// retention.
	Deleted int64
}

// RawWant is one thing that live interests want: the deliveries of an event
// of a repository. Actions empty means every action.
type RawWant struct {
	RepoID int64
	// Event is GitHub's event name.
	Event   string
	Actions []string
}

// RawStore persists the GitHub events that no typed family stores, for the
// interests that want them: live webhook subscriptions of a raw event name,
// and poll leases. It must be backed by the same database as the Store,
// because the Store's methods read, renew and delete the subscriptions that
// UpsertRawSubscription writes. A raw subscription is a row of the Store with
// Cursor math.MaxInt64, which keeps the event log's delivery queue from ever
// reading it.
//
// A raw subscription's event is its EventType without the "github." prefix,
// and its actions and fields are the actions and fields members of its
// Arguments, each a list of strings that is absent when empty.
//
// Matching happens once, in IngestRaw. Every other method reads what
// IngestRaw wrote for one interest: a delivery for a subscription, an unread
// event for a lease. IngestRaw takes no lock on a subscription or a lease, and
// a delivery or gap whose subscription is gone, expired or made again is
// never claimed. A method that removes many rows never waits for a row that
// another caller holds: it leaves that row to the next call.
//
// Raw IDs must be visible in commit order, as event IDs are in Store.
type RawStore interface {
	// IngestRaw stores one delivery for the interests that are live at now and
	// match it: a subscription or lease of d.RepoID and d.Event whose actions
	// are empty or hold d.Action. With no such interest it writes nothing and
	// reports Matched=false, whether or not the delivery ID is stored.
	// Otherwise it calls plan once with the matching interests, outside any
	// transaction. With a delivery ID that is already stored it then writes
	// nothing and reports Duplicate. Otherwise it writes, in one transaction,
	// the event with the plan's body and fields, one delivery per
	// plan.Deliveries due at now, one unread event per plan.Unread, a gap owed
	// by each of plan.Gaps through the new raw ID, and the new raw ID as the
	// mark of each of plan.Lost. An interest that is acknowledged before the
	// call starts is among the interests. When ctx has ended by the time plan
	// returns, it writes nothing and returns ctx's error.
	IngestRaw(ctx context.Context, d RawDelivery, now time.Time, plan func([]RawInterest) RawPlan) (RawIngest, error)
	// RawHead returns the highest raw ID stored, or 0.
	RawHead(ctx context.Context) (int64, error)
	// UpsertRawSubscription is Store.UpsertSubscription for a raw
	// subscription, with limits.Live as maxLive, and two more limits for the
	// event of sub's EventType and sub.RepoID, which count live rows under
	// other keys than sub's: with limits.PerPrincipalTarget subscriptions of
	// the principal it returns ErrRawShareLimit, and with limits.PerTarget
	// subscriptions and leases it returns ErrRawInterestLimit. Either writes
	// nothing. Concurrent calls of it and OpenRawLease never pass PerTarget
	// together.
	UpsertRawSubscription(ctx context.Context, sub Subscription, now time.Time, limits RawLimits) (Subscription, error)
	// OpenRawLease stores lease, which starts at the head: it holds no unread
	// event, and its acknowledged position is the head, which the call
	// returns. It deletes the principal's expired leases first. When the
	// principal then holds limits.Live live leases, the one polled longest
	// ago ends, with its unread events, to make room, and so does its one of
	// lease.Event and lease.RepoID polled longest ago when it holds
	// limits.PerPrincipalTarget of those. A lease that a poll holds is waited
	// for, so neither limit is passed, and the poll's time counts in which
	// lease was polled longest ago. When the event and repository then have
	// limits.PerTarget live subscriptions and leases, it returns
	// ErrRawInterestLimit and changes nothing. Concurrent calls of it and
	// UpsertRawSubscription never pass PerTarget together. A lease ID that is
	// already stored is an error.
	OpenRawLease(ctx context.Context, lease RawLease, now time.Time, limits RawLimits) (head int64, err error)
	// ReadRawLease serves one poll. found is false, and nothing changes,
	// unless a lease with q.Lease, q.Key and q.RepoID is live at q.Now. For
	// such a lease it raises the acknowledged position to q.After, sets the
	// expiry to q.ExpiresAt, returns the unread events above q.After, and
	// deletes the unread events at or below it, all in one transaction. The
	// page ends at q.Limit events, and after the event that takes the bodies
	// read to q.MaxBytes or more.
	ReadRawLease(ctx context.Context, q RawRead) (page RawPage, found bool, err error)
	// ClaimRawDelivery is Store.ClaimDelivery for raw deliveries. It takes a
	// delivery only while a subscription with its key and its incarnation is
	// live at now.
	ClaimRawDelivery(ctx context.Context, now time.Time, lease time.Duration, skip []string) (claim RawDeliveryClaim, ok bool, err error)
	// FinishRawDelivery is Store.FinishDelivery for raw deliveries. A
	// FinishDead that deletes the subscription also ends the other raw
	// deliveries of that incarnation, and Removed counts them.
	FinishRawDelivery(ctx context.Context, ref DeliveryRef, now time.Time, f Finish) (FinishResult, error)
	// ClaimRawGap is Store.ClaimGap for the gaps that IngestRaw recorded. The
	// claim's Through is the highest raw ID that the subscription missed.
	ClaimRawGap(ctx context.Context, now time.Time, lease time.Duration, skip []string) (claim GapClaim, ok bool, err error)
	// FinishRawGap is Store.FinishGap for the gaps that IngestRaw recorded.
	FinishRawGap(ctx context.Context, ref GapRef) error
	// ExpireRawDeliveries deletes the raw deliveries and gaps that no live
	// subscription holds: the subscription expired at or before now, was
	// deleted, or was made again. It leaves the ones that a claim holds, and
	// returns how many deliveries it deleted. A call that fails deletes
	// nothing, so that no delivery leaves the queue uncounted.
	ExpireRawDeliveries(ctx context.Context, now time.Time) (int64, error)
	// PurgeRawEvents deletes the leases that expired at or before p.Now with
	// their unread events, drops the body of every raw event that no delivery
	// and no unread event names any more or that was received before
	// p.ClearBefore, and deletes the raw events received before
	// p.DeleteBefore. The mark of a lease that missed an event does not name
	// it. An event without a body keeps its delivery ID until then, so a
	// delivery that GitHub sends again is a duplicate.
	PurgeRawEvents(ctx context.Context, p RawPurge) (RawPurgeResult, error)
	// RawWants lists what the interests that are live at now want, without
	// duplicates.
	RawWants(ctx context.Context, now time.Time) ([]RawWant, error)
}
