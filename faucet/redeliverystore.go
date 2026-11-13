package faucet

import (
	"context"
	"math"
	"time"

	"golang.org/x/xerrors"
)

// GitHubDeliveryRetention is how long GitHub keeps a webhook delivery. A
// delivery that nobody has read or redelivered by then is gone.
const GitHubDeliveryRetention = 72 * time.Hour

// ErrRedeliveryLeaseLost is what a RedeliveryStore returns when a write names
// a lease that another run has replaced. The run must stop.
var ErrRedeliveryLeaseLost = xerrors.New("redelivery lease lost")

// GiveUpReason says why Service.Redeliver stopped working on something it
// could not recover.
type GiveUpReason string

const (
	// GiveUpLimit means every ask was spent and the delivery was still not stored.
	GiveUpLimit GiveUpReason = "limit"
	// GiveUpExpired means GitHub's retention ended before the delivery was stored.
	GiveUpExpired GiveUpReason = "expired"
	// GiveUpGap means a time range of attempts expired before any run read it.
	// It names no delivery, because the attempts were never read.
	GiveUpGap GiveUpReason = "gap"
)

// FailedAttempt is one attempt of GitHub to deliver an event that faucet
// stores, which faucet answered with anything but 2xx.
type FailedAttempt struct {
	GUID        string
	AttemptID   int64
	DeliveredAt time.Time
}

// Validate checks the fields a caller supplies to ApplyRedeliveryScan.
func (f FailedAttempt) Validate() error {
	switch {
	case f.GUID == "":
		return xerrors.New("attempt GUID is required")
	case f.AttemptID <= 0:
		return xerrors.New("attempt ID must be positive")
	case f.DeliveredAt.IsZero():
		return xerrors.New("attempt time is required")
	}
	return nil
}

// RedeliveryRange is a stretch of GitHub's delivery list that no run has read
// yet. The list is newest first: a run reads from NewerAt down to OlderAt.
type RedeliveryRange struct {
	// ID is assigned by the store.
	ID int64
	// NewerAt is the time of the oldest attempt read so far above the range.
	NewerAt time.Time
	// OlderAt is the newest time that earlier runs had already covered. The
	// range ends when a read reaches an attempt older than OlderAt less the
	// read overlap.
	OlderAt time.Time
	// Cursor continues the list right after the last attempt read. It is
	// GitHub's opaque value. The store keeps it and never reads it.
	Cursor string
}

// RedeliveryGap is a time range of attempts that expired unread.
type RedeliveryGap struct {
	From, To time.Time
}

// RedeliveryScan is what a run knows when it takes the lease.
type RedeliveryScan struct {
	// Epoch fences the run's writes. A later lease has a higher epoch.
	Epoch int64
	// Top is the time of the newest attempt that an earlier run read, or zero
	// before the first run.
	Top time.Time
	// Ranges are the open ranges, newest first.
	Ranges []RedeliveryRange
	// HookChanged is true when the lease found state of another webhook. The
	// store has reset Top and closed that state's open ranges as ClosedGaps.
	HookChanged bool
	ClosedGaps  []RedeliveryGap
}

// RangeAdvance moves an open range down the list.
type RangeAdvance struct {
	ID      int64
	NewerAt time.Time
	Cursor  string
}

// ScanStep is what one page of the delivery list changes. A store applies it
// in one transaction, so a position never moves without the attempts that
// justify it.
type ScanStep struct {
	Epoch int64
	Now   time.Time
	// Failed are the page's failed attempts of events that faucet stores.
	Failed []FailedAttempt
	// Succeeded are the GUIDs of the page's attempts of such events that
	// faucet answered with 2xx.
	Succeeded []string
	// Top raises the stored top to this time. A zero time, or one that is not
	// later than the stored top, leaves it.
	Top time.Time
	// At most one of Open, Advance and CloseID is set. CloseID deletes an open
	// range that has been read to its end.
	Open    *RedeliveryRange
	Advance *RangeAdvance
	CloseID int64
	// Gap records a time range that expired before any run read it, as a final
	// state. It needs no range.
	Gap *RedeliveryGap
}

// Validate checks the fields a caller supplies to ApplyRedeliveryScan.
func (s ScanStep) Validate() error {
	if s.Epoch <= 0 || s.Now.IsZero() {
		return xerrors.New("scan step needs a lease epoch and a time")
	}
	for _, f := range s.Failed {
		if err := f.Validate(); err != nil {
			return err
		}
	}
	for _, guid := range s.Succeeded {
		if guid == "" {
			return xerrors.New("attempt GUID is required")
		}
	}
	moves := 0
	for _, set := range []bool{s.Open != nil, s.Advance != nil, s.CloseID != 0} {
		if set {
			moves++
		}
	}
	switch {
	case moves > 1:
		return xerrors.New("a scan step moves at most one range")
	case s.Open != nil && (s.Open.Cursor == "" || s.Open.NewerAt.IsZero() || s.Open.OlderAt.IsZero()):
		return xerrors.New("an open range needs a cursor and two times")
	case s.Advance != nil && (s.Advance.ID <= 0 || s.Advance.Cursor == "" || s.Advance.NewerAt.IsZero()):
		return xerrors.New("an advanced range needs an ID, a cursor and a time")
	case s.CloseID < 0:
		return xerrors.New("a closed range needs an ID")
	case s.Gap != nil && (s.Gap.From.IsZero() || s.Gap.To.Before(s.Gap.From)):
		return xerrors.New("a gap needs a time range")
	}
	return nil
}

// ScanResult reports what a ScanStep changed.
type ScanResult struct {
	// Tracked is the number of deliveries that became pending.
	Tracked int
	// Recovered are the GUIDs that went from pending to recovered because the
	// step saw a successful attempt.
	Recovered []string
	// OpenedID is the ID of the range that Open created.
	OpenedID int64
}

// SettleRequest asks the store to end what has reached an end.
type SettleRequest struct {
	Now       time.Time
	Retention time.Duration
	// MaxAsks is the most asks one delivery gets.
	MaxAsks int
}

// Validate checks the fields a caller supplies to SettleRedeliveries.
func (r SettleRequest) Validate() error {
	if r.Now.IsZero() || r.Retention <= 0 || r.MaxAsks <= 0 || r.MaxAsks > math.MaxInt32 {
		return xerrors.New("settle needs a time, a retention and an ask limit")
	}
	return nil
}

// GaveUp is a delivery that a settle ended without a recovery.
type GaveUp struct {
	GUID             string
	Reason           GiveUpReason
	Asks             int
	FirstDeliveredAt time.Time
}

// SettleResult reports the transitions that one settle made. Each transition
// happens once, so a caller can count and log it as it comes.
type SettleResult struct {
	// Recovered are the pending deliveries that faucet has stored since.
	Recovered []string
	GaveUp    []GaveUp
	// Pending is the number of deliveries still pending after the settle.
	Pending int
}

// ClaimRequest asks the store for the next delivery to ask GitHub for.
type ClaimRequest struct {
	Now       time.Time
	Retention time.Duration
	MaxAsks   int
	// Schedule has MaxAsks entries. After ask n a delivery waits Schedule[n-1]
	// before the next ask, and after the last ask before a settle gives it up.
	Schedule []time.Duration
}

// Validate checks the fields a caller supplies to ClaimRedelivery.
func (r ClaimRequest) Validate() error {
	switch {
	case r.Now.IsZero() || r.Retention <= 0:
		return xerrors.New("claim needs a time and a retention")
	case r.MaxAsks <= 0 || r.MaxAsks > math.MaxInt32 || len(r.Schedule) != r.MaxAsks:
		return xerrors.New("claim needs one schedule entry per ask")
	}
	for _, wait := range r.Schedule {
		if wait <= 0 {
			return xerrors.New("every schedule entry must be positive")
		}
	}
	return nil
}

// RedeliveryClaim is one delivery that a run may ask GitHub for.
type RedeliveryClaim struct {
	GUID string
	// AttemptID is the newest failed attempt known for the delivery. GitHub's
	// redelivery endpoint takes it.
	AttemptID int64
	// Ask is this claim's number, from 1.
	Ask int
}

// RedeliveryStore keeps what Service.Redeliver needs between runs. It is
// separate from Store, so a host that never redelivers implements nothing.
// storetest checks it.
//
// Every method that takes a time uses that time and never the database
// clock, so a test with a mock clock sees what a production run sees.
type RedeliveryStore interface {
	// AcquireRedeliveryLease starts a run for hook. It returns false when
	// another run holds a lease that has not run out at now. A lease lasts ttl.
	// When the stored scan belongs to another hook, the call resets it: Top
	// goes back to zero and the open ranges close as gaps (see HookChanged).
	AcquireRedeliveryLease(ctx context.Context, hook string, now time.Time, ttl time.Duration) (RedeliveryScan, bool, error)
	// ReleaseRedeliveryLease ends the lease of epoch. It does nothing when
	// another lease has replaced it.
	ReleaseRedeliveryLease(ctx context.Context, epoch int64) error
	// ApplyRedeliveryScan applies step in one transaction. It returns
	// ErrRedeliveryLeaseLost, and changes nothing, when step.Epoch is not the
	// current epoch. A failed attempt makes a delivery pending, unless the
	// delivery already has a final state. A pending delivery keeps the
	// earliest attempt time it has seen as its first time, and the newest
	// failed attempt as the one to ask for.
	ApplyRedeliveryScan(ctx context.Context, step ScanStep) (ScanResult, error)
	// SettleRedeliveries moves each pending delivery to its final state, if it
	// has one, in this order: recovered when an event with its GUID is stored,
	// expired when GitHub's retention has ended, limit when every ask is spent
	// and the last wait has passed.
	SettleRedeliveries(ctx context.Context, req SettleRequest) (SettleResult, error)
	// ClaimRedelivery picks the pending delivery that is due the longest, that
	// has asks left, that is inside GitHub's retention and that is not stored.
	// It raises the delivery's ask count and sets its next due time in the one
	// statement that picks it, so two runs never get the same ask.
	ClaimRedelivery(ctx context.Context, req ClaimRequest) (RedeliveryClaim, bool, error)
	// PurgeRedeliveries deletes final deliveries and closed gaps that ended
	// before the given time. It never deletes a pending delivery.
	PurgeRedeliveries(ctx context.Context, before time.Time) (int64, error)
}
