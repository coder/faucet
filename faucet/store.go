// Package faucet turns GitHub webhooks into events that agents can wait for.
package faucet

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"time"

	"golang.org/x/xerrors"
)

// Event is one stored GitHub webhook, already normalized.
type Event struct {
	// ID is assigned by the store. It counts the events of every target, so it
	// is the place of the event on the line of the all target only.
	ID     int64
	Source string
	// DeliveryID is the X-GitHub-Delivery header. It is unique per event.
	DeliveryID string
	// Type is the normalized event type, for example "ci.run_completed".
	Type   string
	RepoID int64
	// OwnerID is the GitHub ID of the repository's owner, and RepoName its
	// full name, owner/name, as the delivery wrote it. An event stored before
	// faucet recorded them, or of a payload that names none, has 0 and "".
	OwnerID  int64
	RepoName string
	PRs      []int32
	// HeadSHA is empty when the event carries none.
	HeadSHA string
	Data    json.RawMessage
	// ReceivedAt is assigned by the store.
	ReceivedAt time.Time
	// RepoPosition and OwnerPosition are the event's places on the line of its
	// repository and on the line of its owner, which it lacks (0) when
	// OwnerID is 0. The store assigns them.
	RepoPosition  Position
	OwnerPosition Position
}

// Validate checks the fields a caller supplies to Store.InsertEvent.
// ID, ReceivedAt and the positions are ignored because the store assigns them.
//
// Validate accepts any valid JSON without NUL characters. Postgres jsonb
// rejects a few other values, such as numbers beyond the float64 range or lone
// UTF-16 surrogates. Data built with encoding/json never contains them, and
// InsertEvent returns the database error if a caller sends one anyway.
func (e Event) Validate() error {
	switch {
	case e.Source == "":
		return xerrors.New("source is required")
	case e.DeliveryID == "":
		return xerrors.New("delivery ID is required")
	case e.Type == "":
		return xerrors.New("type is required")
	case e.RepoID <= 0:
		return xerrors.New("repo ID must be positive")
	case e.OwnerID < 0:
		return xerrors.New("owner ID must not be negative")
	case len(e.Data) == 0:
		return xerrors.New("data is required")
	case !json.Valid(e.Data):
		return xerrors.New("data is not valid JSON")
	}
	var v any
	if err := json.Unmarshal(e.Data, &v); err != nil {
		return xerrors.Errorf("decode data: %w", err)
	}
	if hasNUL(v) {
		// Postgres jsonb rejects U+0000 in any string.
		return xerrors.New("data contains a NUL character")
	}
	return nil
}

// EventFilter selects the events one reader wants to see.
type EventFilter struct {
	Target Target
	// RepoNames limits the result to events whose RepoName, in lower case,
	// is one of these lower-case full names. Empty means no limit.
	RepoNames []string
	// Types limits the result to these event types. Empty means every type.
	Types []string
	// HeadSHA limits the result to events with this head SHA. Empty means no limit.
	HeadSHA string
	// PR limits the result to events that mention this pull request number.
	// Zero means no limit.
	PR int32
	// Fields limits the result to events that pass every filter.
	Fields []FieldFilter
}

// Validate checks the fields of a filter.
func (f EventFilter) Validate() error {
	switch {
	case f.Target.Validate() != nil:
		return xerrors.Errorf("target: %w", f.Target.Validate())
	case f.PR < 0:
		return xerrors.New("pr must not be negative")
	case slices.ContainsFunc(f.Fields, func(f FieldFilter) bool { return f.Field == "" }):
		return xerrors.New("a field filter must name a field")
	}
	return nil
}

// PollQuery reads one page of the line of its Target, for a caller of that
// target.
type PollQuery struct {
	EventFilter
	// After is the cursor: only events at positions above After are returned.
	After Position
	// Limit is the page size. It must be positive.
	Limit int32
	// Through, when positive, limits the result to events at positions at or
	// below Through, so a read looks at no more than Through - After events of
	// the Target however few of them match. Zero means no limit.
	Through Position
}

// Validate checks the fields a caller supplies to Store.PollEvents.
func (q PollQuery) Validate() error {
	return validateRead(q.EventFilter, int64(q.After), int64(q.Through), q.Limit)
}

// ScanQuery reads the events of a subscription by event ID, as the delivery
// scan keeps its read position.
type ScanQuery struct {
	EventFilter
	// After is the read position: only events with ID > After are returned.
	After int64
	// Limit is the page size. It must be positive.
	Limit int32
	// Through, when positive, limits the result to events with ID <= Through,
	// so a read looks at no more than Through - After IDs however few of them
	// match or belong to the Target. Zero means no limit.
	Through int64
}

// Validate checks the fields a caller supplies to Store.ScanEvents.
func (q ScanQuery) Validate() error {
	return validateRead(q.EventFilter, q.After, q.Through, q.Limit)
}

func validateRead(f EventFilter, after, through int64, limit int32) error {
	switch {
	case f.Validate() != nil:
		return f.Validate()
	case limit <= 0:
		return xerrors.New("limit must be positive")
	case after < 0:
		return xerrors.New("after must not be negative")
	case through < 0:
		return xerrors.New("through must not be negative")
	}
	return nil
}

// StartQuery is where a subscription of Target asks to start.
type StartQuery struct {
	Target Target
	// Cursor is the position that delivery starts after. Nil starts at Since.
	Cursor *Position
	// Since, when not zero, is the oldest receive time that the subscription
	// reads: a start below the position of the line at Since moves up to it.
	// Zero sets no time floor, so the store reads nothing but the purge mark.
	Since time.Time
}

// Validate checks the fields a caller supplies to Store.StartSubscription.
func (q StartQuery) Validate() error {
	if q.Cursor != nil && *q.Cursor < 0 {
		return xerrors.New("cursor must not be negative")
	}
	return nil
}

// Start is where Store.StartSubscription starts a subscription.
type Start struct {
	// Position is the position on the line of the Target that delivery
	// starts after: the Cursor, moved up to the line's purge mark and to its
	// position at Since, and down to its head. With no Cursor, it is the
	// position at Since, which is never below the purge mark.
	Position Position
	// After is the event ID to store as Subscription.Cursor: the events of
	// the Target above After are the events above Position. It is never below
	// the purge mark of the all target, and when Position is the head it is
	// the highest event ID, so a subscription of a quiet target does not read
	// the other targets' events, or a purge of them, as a gap.
	After int64
	// Truncated is true when Position is above the Cursor because the line's
	// purge mark or its position at Since is.
	Truncated bool
}

// FieldFilter narrows the events of some types by one member of their data.
type FieldFilter struct {
	// Field names a string or boolean member of the data, for example
	// "head_branch".
	Field string
	// Values are the texts that pass: a string member passes when its value is
	// one of them, and a boolean member when "true" or "false" is. An event
	// without the member does not pass.
	Values []string
	// Types are the event types that the filter narrows. An event of another
	// type passes it.
	Types []string
}

// PollPage is the result of Store.PollEvents. Every number in it is a
// position on the line of the query's Target.
type PollPage struct {
	// Events are the matching events in the order of the line.
	Events []Event
	// Next is the cursor to poll with next. It is the position of the last
	// event when the page is full. Otherwise it is the head of the line when
	// the store answered, or Through when that is lower. It is never lower
	// than the query's After unless After is above the head, which reads as
	// the head, and not lower than PurgedThrough unless Through is.
	Next Position
	// PurgedThrough is the purge mark of the line: the highest position that a
	// purge has deleted, or 0. The store reads it in the same snapshot as
	// Events, so a caller whose cursor is below it knows that events above the
	// cursor may be gone, whatever a purge did while the page was read.
	PurgedThrough Position
}

// ScanPage is the result of Store.ScanEvents. Its numbers are event IDs.
type ScanPage struct {
	// Events are the matching events in ID order.
	Events []Event
	// Next is the read position to scan from next. It is the last event's ID
	// when the page is full. Otherwise it is the highest event ID the store
	// had when it answered, or Through when that is lower, so a subscription
	// whose target is quiet does not rescan other targets' events. It is
	// never lower than the query's After, and not lower than PurgedThrough
	// unless Through is.
	Next int64
	// PurgedThrough is the purge mark: the highest event ID any purge has
	// deleted, or 0, read in the same snapshot as Events.
	PurgedThrough int64
}

// PurgeResult is what Store.Purge reports.
type PurgeResult struct {
	// Deleted is the number of events this call deleted.
	Deleted int64
	// PurgedThrough is the purge mark after the call.
	PurgedThrough int64
	// ExpiredSubscriptions and EndedDeliveries are what Service.Purge removed
	// with the expired subscriptions: the subscriptions, and the deliveries that
	// they still held and that no run was sending. A run that held one of those
	// deliveries counts it as superseded. Store.Purge leaves both at zero.
	ExpiredSubscriptions int64
	EndedDeliveries      int64
	// Raw is what Service.Purge removed of the raw events, when the service
	// has a RawStore. Its raw deliveries are counted in EndedDeliveries.
	// Store.Purge leaves it at zero.
	Raw RawPurgeResult
	// RawFailures is the number of the RawStore calls of Service.Purge that
	// failed. Store.Purge leaves it at zero.
	RawFailures int64
}

func hasNUL(v any) bool {
	switch v := v.(type) {
	case string:
		return strings.ContainsRune(v, 0)
	case []any:
		return slices.ContainsFunc(v, hasNUL)
	case map[string]any:
		for k, e := range v {
			if strings.ContainsRune(k, 0) || hasNUL(e) {
				return true
			}
		}
	}
	return false
}

// Store persists events and serves them to pollers.
//
// Implementations must guarantee that event IDs, and the positions of each
// line, are visible in commit order: a reader that has seen ID n has already
// seen every ID below n that will ever be visible. Without this, a poller that
// advances its cursor to n can skip an event that commits late.
type Store interface {
	// InsertEvent stores ev once per DeliveryID. It returns the stored event and
	// inserted=false when the DeliveryID already exists, in which case stored is
	// the existing row and ev is discarded. A stored event gets the next
	// position of its repository's line, and of its owner's when OwnerID is
	// not 0: one above the head of the line.
	InsertEvent(ctx context.Context, ev Event) (stored Event, inserted bool, err error)
	// EventsAfter returns events with ID > afterID in ID order, at most limit rows.
	// limit must be positive.
	EventsAfter(ctx context.Context, afterID int64, limit int32) ([]Event, error)
	// PollEvents returns one page of the line of q.Target. Every filter is
	// applied before the limit, and the events and PollPage.Next come from one
	// snapshot. Because the events of a line are visible in the order of their
	// positions, polling again with After set to Next never skips a matching
	// event. What it returns depends on the events of q.Target alone: other
	// targets' events, and purges of them, change none of it.
	PollEvents(ctx context.Context, q PollQuery) (PollPage, error)
	// ScanEvents returns one page for q by event ID, as PollEvents did before
	// targets had lines, for the delivery scan, which keeps its read position
	// as an event ID. Nothing that it returns reaches a caller.
	ScanEvents(ctx context.Context, q ScanQuery) (ScanPage, error)
	// PositionAt returns the highest position on target's line of an event
	// received before t, or 0 when there is none. It is never lower than the
	// line's purge mark, so polling with it as the cursor returns the events
	// from t on and is not reported as a gap. It returns the events from t on
	// only if InsertEvent stamps ReceivedAt in ID order: an event must never
	// carry an earlier time than a lower ID.
	PositionAt(ctx context.Context, target Target, t time.Time) (Position, error)
	// StartSubscription works out where a subscription of q.Target starts,
	// from one snapshot, so that a purge that commits alongside it is either
	// in Start.Truncated or below Start.After, where the delivery scan reports
	// it as a gap.
	StartSubscription(ctx context.Context, q StartQuery) (Start, error)
	// Purge deletes the events received before the given time and raises the
	// purge mark to the highest ID it deleted, and the purge mark of every line
	// that lost events to the highest position it lost, in one transaction. A
	// mark never moves down, and a line whose events are all deleted keeps its
	// head, so the next event of the target gets the next position. It needs no insert lock: events commit in ID order, so
	// no event at or below the mark can appear after the purge.
	//
	// In the same transaction it moves every subscription whose read position
	// is at or above the old mark and below the new one up to the new mark,
	// unless an event that its target covers, of its event types, lies above
	// its position and at or below the new mark. What stays below the mark
	// is then exactly what lost events, so a subscription that had nothing to
	// read is not reported as a gap.
	Purge(ctx context.Context, before time.Time) (PurgeResult, error)
	// UpsertSubscription stores sub and returns the stored row. It calls
	// sub.Validate(now). A row with the same KeyHash that expires after now is
	// live: the call replaces its Target, SecretEnc and ExpiresAt and keeps its
	// CreatedAt, Cursor and Position, except that a changed Target gets the
	// position of its own line at Cursor: the newest position at or below
	// Cursor, raised to the line's purge mark unless Cursor is below the
	// purge mark of the all target. When the new SecretEnc differs from the row's, the
	// row's becomes PreviousSecretEnc until now plus RotationGrace (and a
	// previous secret from an earlier rotation is dropped). When it is equal,
	// the previous secret and its end stay as they were. A row that has
	// expired counts as absent and is replaced whole. When the principal
	// already holds maxLive live subscriptions under other keys it returns
	// ErrSubscriptionLimit and writes nothing. maxLive must be positive.
	UpsertSubscription(ctx context.Context, sub Subscription, now time.Time, maxLive int) (Subscription, error)
	// DeleteSubscription deletes the subscription with this key if it belongs
	// to principal, live or not, and reports whether it did. Another
	// principal's subscription is never deleted, whatever key is given.
	DeleteSubscription(ctx context.Context, principal string, key SubscriptionKeyHash) (bool, error)
	// DeleteLiveSubscription deletes the subscription with this key if it
	// belongs to principal and is live at now, and returns the row it deleted.
	// found is false, and nothing is deleted, for another principal's key, an
	// unknown key and a row that has expired, which counts as absent. Of
	// concurrent callers for one row, exactly one gets found=true.
	DeleteLiveSubscription(ctx context.Context, principal string, key SubscriptionKeyHash, now time.Time) (deleted Subscription, found bool, err error)
	// DeleteSubscriptionsOf deletes every subscription of principal, live or
	// not, with the deliveries and gaps that it holds, and returns how many
	// it deleted. It deletes no other principal's subscription.
	DeleteSubscriptionsOf(ctx context.Context, principal string) (int64, error)
	// DeleteExpiredSubscriptions deletes every subscription that expired at or
	// before now and returns how many it deleted.
	DeleteExpiredSubscriptions(ctx context.Context, now time.Time) (int64, error)
	// ClearExpiredPreviousSecrets removes the previous secret of every
	// subscription whose PreviousSecretUntil is at or before now, and returns
	// how many rows it cleared.
	ClearExpiredPreviousSecrets(ctx context.Context, now time.Time) (int64, error)
	// RecordRepository records repo, which a signed delivery named, at
	// seenAt, the time of record, when the store holds no row of its ID. A
	// row that exists keeps its names, so a replayed or late delivery cannot
	// move a name back to it. The login of repo's owner is RecordOwner's or
	// AddOwner's to record. It calls repo.Validate.
	RecordRepository(ctx context.Context, repo Repository, seenAt time.Time) error
	// RenameRepository applies a repository event that renamed or transferred
	// a repository, and judges each body once: a rename whose Body it judged
	// before changes nothing (RenameSeenBefore), so a replay of a transfer
	// cannot give the repository a name back after it returned to the name
	// that the transfer moved it from. It records the Body in the same
	// transaction as the names, and a failed call records neither. With no
	// row of rename.Repo.ID, it inserts one (Renamed). When the row's full
	// name is rename.From, ignoring case, either as the row holds it or under
	// the login that the row's owner holds now, it gives the row the names of
	// rename.Repo at seenAt (Renamed). GitHub writes From under the owner's
	// login at the time, which the row lacks when the owner was renamed after
	// it was seen, and lifts a quarantine unless Late. Any other name is
	// stale or newer: a rename that is not Late quarantines the row
	// (RenameQuarantined) and keeps its names, so that neither a missed
	// rename nor an old body sent again binds a name to the repository, and
	// a Late one changes nothing (RenameLate). Kept bodies can chain, so a
	// quarantine keeps no name of the rename that caused it. A quarantined
	// row resolves no name in RepositoryByName, and RecordRepository leaves
	// it so. It calls rename.Repo.Validate.
	RenameRepository(ctx context.Context, rename RepositoryRename, seenAt time.Time) (RenameOutcome, error)
	// RecordOwner records that a signed delivery named the account owner.ID
	// with owner.Login. The account keeps the login of its latest
	// delivery, because GitHub sends no event for each repository of an
	// account that it renames. One login names one account, ignoring case:
	// another account that held the login gives it up, and holds no login
	// until a delivery names it again. It calls owner.Validate.
	RecordOwner(ctx context.Context, owner Owner) error
	// AddOwner records owner as RecordOwner does when the store holds no row
	// of the account and no account holds its login, and writes nothing
	// otherwise. A delivery that GitHub sent again names an account as it was
	// then, so it may add an account but must move no login. It calls
	// owner.Validate.
	AddOwner(ctx context.Context, owner Owner) error
	// OwnerByID returns the account of id with the login that the store
	// holds for it, and ErrUnknownOwner when it holds no row of it.
	OwnerByID(ctx context.Context, id int64) (Owner, error)
	// RepositoryByName returns the repository written fullName, ignoring case
	// as GitHub does: a repository whose name, as it was first seen or renamed
	// to, is the name part, of the account that holds the owner part as its
	// login now. When more than one row holds the name, as when a repository
	// was deleted with no event that says so and another took its name, it
	// returns the one that took the name latest. The result's names are the
	// ones that it matched. It returns ErrUnknownRepository when no row holds
	// the name.
	RepositoryByName(ctx context.Context, fullName string) (Repository, error)
	// OwnerByLogin returns the account that holds login now, ignoring case,
	// with the login as its latest delivery wrote it. It returns
	// ErrUnknownOwner when no account holds it.
	OwnerByLogin(ctx context.Context, login string) (Owner, error)

	// The rest of the interface is the delivery queue. A subscription's
	// deliveries end with it: deleting a subscription deletes the deliveries it
	// holds. A new subscription starts reading the log after its Cursor, and a
	// refresh of a live one leaves the read position alone.

	// ScanTargets returns the live subscriptions at now that hold fewer than
	// maxPending unsent deliveries and have something to read: an event that
	// their target covers, of their event types, above their read position,
	// or a purge mark above it, and whose principal is none of
	// skipPrincipals. The ones furthest behind come first, at most limit.
	ScanTargets(ctx context.Context, now time.Time, maxPending, limit int32, skipPrincipals ...string) ([]ScanTarget, error)
	// EnqueueDeliveries queues e.Deliveries and moves the subscription's read
	// position to e.NewScannedThrough in one transaction, and only if the
	// subscription is live at now and still at e.ExpectedScannedThrough. It
	// reports whether it did. The queued deliveries are due at now. A non-zero
	// e.GapThrough is recorded as a gap that ClaimGap hands out.
	EnqueueDeliveries(ctx context.Context, now time.Time, e Enqueue) (bool, error)
	// ClaimDelivery takes the due delivery with the earliest due time, of a
	// subscription that is live at now, and no other caller holds. A delivery
	// is due when its next attempt time is at or before now, and held while
	// its lease runs. The call sets the lease to now+lease and raises the
	// attempt count by one in one step, so no two callers hold one attempt.
	// It passes over the deliveries whose subscription's CallbackOrigin is in
	// skip, so a caller can keep one endpoint from taking all its workers. ok
	// is false when nothing else is due.
	ClaimDelivery(ctx context.Context, now time.Time, lease time.Duration, skip []string) (claim DeliveryClaim, ok bool, err error)
	// ClaimGap takes a gap envelope that no other caller holds, of a
	// subscription that is live at now and whose CallbackOrigin is not in skip,
	// and sets its lease to now+lease. The gap is the one that
	// EnqueueDeliveries recorded: it stays owed until FinishGap, so a gap that no
	// run took, or whose run died, goes to the next run. The claim carries the
	// subscription's secrets as of now. ok is false when no gap can be taken.
	ClaimGap(ctx context.Context, now time.Time, lease time.Duration, skip []string) (claim GapClaim, ok bool, err error)
	// FinishGap ends the gap in ref and releases its lease. It changes nothing
	// for a subscription that is gone or is another incarnation. A gap with a
	// mark above ref.Through, recorded since the claim, stays owed.
	FinishGap(ctx context.Context, ref GapRef) error
	// FinishDelivery records how the attempt in ref ended. It changes nothing
	// and reports Accepted=false unless ref.Attempt is the delivery's current
	// attempt and the subscription's CreatedAt is ref.Incarnation. Delivered and
	// Failure update the subscription's DeliveryStatus at now: a delivered
	// attempt sets LastDeliveryAt and clears the failure, and a failure sets
	// LastError and keeps the earlier FailedSince.
	FinishDelivery(ctx context.Context, ref DeliveryRef, now time.Time, f Finish) (FinishResult, error)
	// ExpireDeliveries deletes the deliveries of subscriptions that expired at
	// or before now, except the ones that a claim holds, and returns how many
	// it deleted.
	ExpireDeliveries(ctx context.Context, now time.Time) (int64, error)
}
