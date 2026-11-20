package pgstore

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"golang.org/x/xerrors"

	"github.com/coder/superblink-events/faucet"
	"github.com/coder/superblink-events/faucet/pgstore/database"
)

// rawNamePrefix starts the event type of every raw subscription.
const rawNamePrefix = "github."

// rawEventsLockKey is the FNV-1a 64-bit hash of "faucet_raw_events", read as an
// int64. Every writer of faucet_raw_events must take this advisory lock first.
const rawEventsLockKey int64 = 9141572664397693276

var _ faucet.RawStore = (*Store)(nil)

// IngestRaw reads the interests and calls plan before it begins a transaction,
// so a plan that projects a large payload holds no lock. It then holds
// rawEventsLockKey from the insert of the event to the commit, as InsertEvent
// holds eventsLockKey, so raw IDs become visible in commit order. It locks no
// subscription and no lease: a lease's loss is a row of its own.
func (s *Store) IngestRaw(ctx context.Context, d faucet.RawDelivery, now time.Time, plan func([]faucet.RawInterest) faucet.RawPlan) (_ faucet.RawIngest, err error) {
	defer func() { err = endedBy(ctx, err) }()
	if err := d.Validate(); err != nil {
		return faucet.RawIngest{}, xerrors.Errorf("validate delivery: %w", err)
	}
	if plan == nil {
		return faucet.RawIngest{}, xerrors.New("plan is required")
	}
	rows, err := s.q.RawInterests(ctx, database.RawInterestsParams{RepoID: d.RepoID, OwnerID: d.OwnerID, RepoName: d.RepoName, Event: d.Event, Now: now, Action: d.Action})
	if err != nil {
		return faucet.RawIngest{}, xerrors.Errorf("read interests: %w", err)
	}
	if len(rows) == 0 {
		return faucet.RawIngest{}, nil
	}
	interests := make([]faucet.RawInterest, len(rows))
	for i, row := range rows {
		if interests[i], err = toRawInterest(row); err != nil {
			return faucet.RawIngest{}, err
		}
	}
	p := plan(interests)
	if err := ctx.Err(); err != nil {
		return faucet.RawIngest{}, xerrors.Errorf("plan: %w", err)
	}
	if err := p.Validate(); err != nil {
		return faucet.RawIngest{}, xerrors.Errorf("validate plan: %w", err)
	}

	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return faucet.RawIngest{}, xerrors.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	q := s.q.WithTx(tx)

	if err := q.LockEvents(ctx, rawEventsLockKey); err != nil {
		return faucet.RawIngest{}, xerrors.Errorf("lock raw events: %w", err)
	}
	id, err := q.InsertRawEvent(ctx, database.InsertRawEventParams{
		DeliveryID: d.DeliveryID,
		Event:      d.Event,
		Action:     d.Action,
		RepoID:     d.RepoID,
		OwnerID:    d.OwnerID,
		HasBody:    p.Body != nil,
		Body:       p.Body,
		Fields:     p.Fields,
	})
	if xerrors.Is(err, sql.ErrNoRows) {
		if err := tx.Commit(); err != nil {
			return faucet.RawIngest{}, xerrors.Errorf("commit: %w", err)
		}
		return faucet.RawIngest{Matched: true, Duplicate: true}, nil
	}
	if err != nil {
		return faucet.RawIngest{}, xerrors.Errorf("insert raw event: %w", err)
	}
	if err := writeRawPlan(ctx, q, id, now, p); err != nil {
		return faucet.RawIngest{}, err
	}
	if err := tx.Commit(); err != nil {
		return faucet.RawIngest{}, xerrors.Errorf("commit: %w", err)
	}
	return faucet.RawIngest{Matched: true, ID: id}, nil
}

func writeRawPlan(ctx context.Context, q *database.Queries, id int64, now time.Time, p faucet.RawPlan) error {
	if len(p.Deliveries) > 0 {
		params := database.InsertRawDeliveriesParams{
			RawEventID:   id,
			Now:          now,
			KeyHashes:    make([][]byte, 0, len(p.Deliveries)),
			Incarnations: make([]time.Time, 0, len(p.Deliveries)),
		}
		for _, o := range p.Deliveries {
			params.KeyHashes = append(params.KeyHashes, o.Key[:])
			params.Incarnations = append(params.Incarnations, o.Incarnation)
		}
		if err := q.InsertRawDeliveries(ctx, params); err != nil {
			return xerrors.Errorf("insert raw deliveries: %w", err)
		}
	}
	if len(p.Unread) > 0 {
		if err := q.InsertRawUnread(ctx, database.InsertRawUnreadParams{RawEventID: id, LeaseIds: leaseIDs(p.Unread)}); err != nil {
			return xerrors.Errorf("insert unread events: %w", err)
		}
	}
	if len(p.Gaps) > 0 {
		params := database.RaiseRawGapsParams{
			Through:      id,
			KeyHashes:    make([][]byte, 0, len(p.Gaps)),
			Incarnations: make([]time.Time, 0, len(p.Gaps)),
		}
		for _, g := range p.Gaps {
			params.KeyHashes = append(params.KeyHashes, g.Key[:])
			params.Incarnations = append(params.Incarnations, g.Incarnation)
		}
		if err := q.RaiseRawGaps(ctx, params); err != nil {
			return xerrors.Errorf("raise raw gaps: %w", err)
		}
	}
	if len(p.Lost) > 0 {
		if err := q.RaiseRawLost(ctx, database.RaiseRawLostParams{RawEventID: id, LeaseIds: leaseIDs(p.Lost)}); err != nil {
			return xerrors.Errorf("raise lost rows: %w", err)
		}
	}
	return nil
}

func (s *Store) RawHead(ctx context.Context) (_ int64, err error) {
	defer func() { err = endedBy(ctx, err) }()
	head, err := s.q.RawHead(ctx)
	if err != nil {
		return 0, xerrors.Errorf("select raw head: %w", err)
	}
	return head, nil
}

// UpsertRawSubscription takes the lock of the subscription's event and
// repository after the lock of its principal, as OpenRawLease does, so the two
// never wait for each other in a cycle.
func (s *Store) UpsertRawSubscription(ctx context.Context, sub faucet.Subscription, now time.Time, limits faucet.RawLimits) (_ faucet.Subscription, err error) {
	defer func() { err = endedBy(ctx, err) }()
	if err := limits.Validate(); err != nil {
		return faucet.Subscription{}, err
	}
	event, ok := strings.CutPrefix(sub.EventType, rawNamePrefix)
	if !ok {
		return faucet.Subscription{}, xerrors.Errorf("%q is not a raw event name", sub.EventType)
	}
	return s.upsertSubscriptionChecked(ctx, sub, now, limits.Live, func(q *database.Queries) error {
		count, err := countRawTarget(ctx, q, sub.Principal, event, sub.Target, now, sub.KeyHash[:])
		switch {
		case err != nil:
			return err
		case count.OwnSubscriptions >= int64(limits.PerPrincipalTarget):
			return faucet.ErrRawShareLimit
		case count.Interests >= int64(limits.PerTarget):
			return faucet.ErrRawInterestLimit
		}
		return nil
	})
}

// countRawTarget counts the live interests of an event of a target, but the
// subscription of except. The lock that it takes first holds until the
// commit, so no other new interest of the event and target is stored
// meanwhile.
func countRawTarget(ctx context.Context, q *database.Queries, principal, event string, target faucet.Target, now time.Time, except []byte) (database.CountRawInterestsRow, error) {
	kind := string(target.Kind())
	if err := q.LockRawTarget(ctx, database.LockRawTargetParams{Event: event, TargetKind: kind, RepoID: target.RepoID(), OwnerID: target.OwnerID()}); err != nil {
		return database.CountRawInterestsRow{}, xerrors.Errorf("lock raw interests: %w", err)
	}
	count, err := q.CountRawInterests(ctx, database.CountRawInterestsParams{
		TargetKind: kind, RepoID: target.RepoID(), OwnerID: target.OwnerID(),
		Event: event, Now: now, ExceptKey: except, Principal: principal,
	})
	if err != nil {
		return database.CountRawInterestsRow{}, xerrors.Errorf("count raw interests: %w", err)
	}
	return count, nil
}

// OpenRawLease takes a lock per principal, so two opens of one principal cannot
// both pass the limit. The lock is another key than the one that
// UpsertSubscription takes, so a poll never waits for a subscribe. It then
// locks the principal's live leases, so that a lease to end is picked by the
// times that the polls it waited for wrote.
func (s *Store) OpenRawLease(ctx context.Context, lease faucet.RawLease, now time.Time, limits faucet.RawLimits) (_ int64, err error) {
	defer func() { err = endedBy(ctx, err) }()
	if err := limits.Validate(); err != nil {
		return 0, err
	}
	if err := lease.Validate(now); err != nil {
		return 0, xerrors.Errorf("validate lease: %w", err)
	}

	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return 0, xerrors.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	q := s.q.WithTx(tx)

	if err := q.LockRawLeases(ctx, lease.Principal); err != nil {
		return 0, xerrors.Errorf("lock raw leases: %w", err)
	}
	if _, err := q.LockLiveRawLeasesOf(ctx, database.LockLiveRawLeasesOfParams{Principal: lease.Principal, Now: now}); err != nil {
		return 0, xerrors.Errorf("lock the live raw leases: %w", err)
	}
	ended, err := q.EndRawLeasesOf(ctx, database.EndRawLeasesOfParams{Principal: lease.Principal, Now: now, MaxLive: int64(limits.Live)})
	if err != nil {
		return 0, xerrors.Errorf("end raw leases: %w", err)
	}
	count, err := countRawTarget(ctx, q, lease.Principal, lease.Event, lease.Target, now, nil)
	if err != nil {
		return 0, err
	}
	if count.OwnLeases >= int64(limits.PerPrincipalTarget) {
		oldest, err := q.EndOldestRawLeaseOfTarget(ctx, database.EndOldestRawLeaseOfTargetParams{
			Principal: lease.Principal, Event: lease.Event, Now: now,
			TargetKind: string(lease.Target.Kind()), RepoID: lease.Target.RepoID(), OwnerID: lease.Target.OwnerID(),
		})
		if err != nil {
			return 0, xerrors.Errorf("end the oldest raw lease of the event: %w", err)
		}
		ended = append(ended, oldest...)
		count.Interests -= int64(len(oldest))
	}
	if count.Interests >= int64(limits.PerTarget) {
		return 0, faucet.ErrRawInterestLimit
	}
	if len(ended) > 0 {
		if err := q.DeleteRawUnreadOf(ctx, ended); err != nil {
			return 0, xerrors.Errorf("delete unread events of ended leases: %w", err)
		}
	}
	head, err := q.InsertRawLease(ctx, database.InsertRawLeaseParams{
		ID:         lease.ID[:],
		KeyHash:    lease.Key[:],
		Principal:  lease.Principal,
		Event:      lease.Event,
		Actions:    nonNil(lease.Actions),
		Fields:     lease.Fields,
		TargetKind: string(lease.Target.Kind()),
		RepoID:     lease.Target.RepoID(),
		OwnerID:    lease.Target.OwnerID(),
		Repos:      nonNil(lease.RepoNames),
		Now:        now,
		ExpiresAt:  lease.ExpiresAt,
	})
	if err != nil {
		return 0, xerrors.Errorf("insert raw lease: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, xerrors.Errorf("commit: %w", err)
	}
	return head, nil
}

// ReadRawLease reads the page and the head in one statement, because raw IDs
// become visible in commit order: an event that commits after that snapshot
// has a higher ID than the head it read, so Next never passes it. The
// acknowledgement runs last, so its row locks last only until the commit.
func (s *Store) ReadRawLease(ctx context.Context, r faucet.RawRead) (_ faucet.RawPage, _ bool, err error) {
	defer func() { err = endedBy(ctx, err) }()
	if err := r.Validate(); err != nil {
		return faucet.RawPage{}, false, xerrors.Errorf("validate read: %w", err)
	}

	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return faucet.RawPage{}, false, xerrors.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	q := s.q.WithTx(tx)

	acked, err := q.TouchRawLease(ctx, database.TouchRawLeaseParams{
		After: r.After, Now: r.Now, ExpiresAt: r.ExpiresAt, ID: r.Lease[:], KeyHash: r.Key[:],
		TargetKind: string(r.Target.Kind()), RepoID: r.Target.RepoID(), OwnerID: r.Target.OwnerID(),
	})
	if xerrors.Is(err, sql.ErrNoRows) {
		return faucet.RawPage{}, false, nil
	}
	if err != nil {
		return faucet.RawPage{}, false, xerrors.Errorf("touch raw lease: %w", err)
	}
	rows, err := q.ReadRawLeasePage(ctx, database.ReadRawLeasePageParams{
		NotBefore: sql.NullTime{Time: r.NotBefore, Valid: !r.NotBefore.IsZero()},
		LeaseID:   r.Lease[:],
		After:     r.After,
		RowLimit:  int64(r.Limit),
		MaxBytes:  r.MaxBytes,
	})
	if err != nil {
		return faucet.RawPage{}, false, xerrors.Errorf("read raw lease page: %w", err)
	}
	if len(rows) == 0 {
		return faucet.RawPage{}, false, xerrors.New("read raw lease page: statement returned no summary row")
	}
	summary := rows[0]
	page := faucet.RawPage{More: summary.More, Skipped: int(summary.Skipped), AckedThrough: acked}
	for _, row := range rows {
		if row.ID.Valid {
			page.Events = append(page.Events, faucet.RawEvent{
				ID:         row.ID.Int64,
				DeliveryID: row.DeliveryID.String,
				Event:      row.Event.String,
				Action:     row.Action.String,
				RepoID:     row.RepoID.Int64,
				OwnerID:    row.OwnerID.Int64,
				Body:       row.Body,
				Fields:     row.Fields,
				ReceivedAt: row.ReceivedAt.Time,
			})
		}
	}
	page.Next = summary.LastOpen
	if !page.More {
		page.Next = max(r.After, summary.Head, summary.LastOpen)
	}
	if err := q.AckRawUnread(ctx, database.AckRawUnreadParams{LeaseID: r.Lease[:], After: r.After}); err != nil {
		return faucet.RawPage{}, false, xerrors.Errorf("acknowledge unread events: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return faucet.RawPage{}, false, xerrors.Errorf("commit: %w", err)
	}
	return page, true, nil
}

// ClaimRawDelivery is one statement, like ClaimDelivery. A row is due only
// through a join on the live subscription of its incarnation, so a delivery
// that a subscription made again inherits is never sent.
func (s *Store) ClaimRawDelivery(ctx context.Context, now time.Time, lease time.Duration, skip []string) (_ faucet.RawDeliveryClaim, _ bool, err error) {
	defer func() { err = endedBy(ctx, err) }()
	if lease <= 0 {
		return faucet.RawDeliveryClaim{}, false, xerrors.Errorf("lease must be positive, got %s", lease)
	}
	row, err := s.q.ClaimRawDelivery(ctx, database.ClaimRawDeliveryParams{Now: now, LeaseUntil: sql.NullTime{Time: now.Add(lease), Valid: true}, SkipOrigins: nonNil(skip)})
	if xerrors.Is(err, sql.ErrNoRows) {
		return faucet.RawDeliveryClaim{}, false, nil
	}
	if err != nil {
		return faucet.RawDeliveryClaim{}, false, xerrors.Errorf("claim raw delivery: %w", err)
	}
	key, err := toKeyHash(row.KeyHash)
	if err != nil {
		return faucet.RawDeliveryClaim{}, false, err
	}
	target, err := toTarget(row.TargetKind, row.RepoID, row.OwnerID)
	if err != nil {
		return faucet.RawDeliveryClaim{}, false, err
	}
	claim := faucet.RawDeliveryClaim{
		Ref:                 faucet.DeliveryRef{Key: key, EventID: row.RawEventID, Attempt: row.Attempts, Incarnation: row.Incarnation},
		EnqueuedAt:          row.EnqueuedAt,
		EventType:           row.EventType,
		Arguments:           row.Arguments,
		CallbackURL:         row.CallbackUrl,
		SecretEnc:           row.SecretEnc,
		PreviousSecretEnc:   row.PreviousSecretEnc,
		PreviousSecretUntil: row.PreviousSecretUntil.Time,
		Target:              target,
		Watermark:           row.Watermark,
	}
	if row.StoredEventID.Valid {
		claim.Event = &faucet.RawEvent{
			ID:         row.StoredEventID.Int64,
			DeliveryID: row.DeliveryID.String,
			Event:      row.Event.String,
			Action:     row.Action.String,
			RepoID:     row.EventRepoID.Int64,
			OwnerID:    row.EventOwnerID.Int64,
			Body:       row.Body,
			Fields:     row.Fields,
			ReceivedAt: row.ReceivedAt.Time,
		}
	}
	return claim, true, nil
}

// FinishRawDelivery takes the lock of the subscription row before it touches
// the delivery, as FinishDelivery does, so the incarnation check holds until
// the commit.
func (s *Store) FinishRawDelivery(ctx context.Context, ref faucet.DeliveryRef, now time.Time, f faucet.Finish) (_ faucet.FinishResult, err error) {
	defer func() { err = endedBy(ctx, err) }()
	if ref.Attempt < 1 {
		return faucet.FinishResult{}, xerrors.Errorf("attempt must be at least 1, got %d", ref.Attempt)
	}
	if err := f.Validate(); err != nil {
		return faucet.FinishResult{}, xerrors.Errorf("validate finish: %w", err)
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return faucet.FinishResult{}, xerrors.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	q := s.q.WithTx(tx)

	createdAt, err := q.LockSubscription(ctx, ref.Key[:])
	if xerrors.Is(err, sql.ErrNoRows) || err == nil && !createdAt.Equal(ref.Incarnation) {
		return faucet.FinishResult{}, nil
	}
	if err != nil {
		return faucet.FinishResult{}, xerrors.Errorf("lock subscription: %w", err)
	}
	result, err := finishRawLocked(ctx, q, ref, now, f)
	if err != nil || !result.Accepted {
		return faucet.FinishResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return faucet.FinishResult{}, xerrors.Errorf("commit: %w", err)
	}
	return result, nil
}

func finishRawLocked(ctx context.Context, q *database.Queries, ref faucet.DeliveryRef, now time.Time, f faucet.Finish) (faucet.FinishResult, error) {
	var affected int64
	var err error
	if f.Kind == faucet.FinishRetry {
		affected, err = q.RescheduleRawDelivery(ctx, database.RescheduleRawDeliveryParams{NextAttemptAt: f.RetryAt, KeyHash: ref.Key[:], RawEventID: ref.EventID, Attempt: ref.Attempt})
	} else {
		affected, err = q.EndRawDelivery(ctx, database.EndRawDeliveryParams{KeyHash: ref.Key[:], RawEventID: ref.EventID, Attempt: ref.Attempt})
	}
	if err != nil {
		return faucet.FinishResult{}, xerrors.Errorf("update raw delivery: %w", err)
	}
	if affected == 0 {
		return faucet.FinishResult{}, nil
	}
	result := faucet.FinishResult{Accepted: true}
	if f.Kind != faucet.FinishRetry {
		result.Removed = 1
	}
	if f.Gap {
		err = q.RaiseRawGaps(ctx, database.RaiseRawGapsParams{Through: ref.EventID, KeyHashes: [][]byte{ref.Key[:]}, Incarnations: []time.Time{ref.Incarnation}})
		if err != nil {
			return faucet.FinishResult{}, xerrors.Errorf("raise raw gap: %w", err)
		}
	}
	switch {
	case f.Kind == faucet.FinishDelivered:
		err = q.RecordDeliverySuccess(ctx, database.RecordDeliverySuccessParams{Now: sql.NullTime{Time: now, Valid: true}, KeyHash: ref.Key[:]})
	case f.Failure != "":
		err = q.RecordDeliveryFailure(ctx, database.RecordDeliveryFailureParams{
			LastError: sql.NullString{String: string(f.Failure), Valid: true}, Now: sql.NullTime{Time: now, Valid: true}, KeyHash: ref.Key[:],
		})
	case f.Kind == faucet.FinishDead:
		var others int64
		others, err = deleteDeadRawSubscription(ctx, q, ref, f.Unreadable)
		result.Removed += others
	}
	if err != nil {
		return faucet.FinishResult{}, err
	}
	return result, nil
}

// deleteDeadRawSubscription deletes the subscription only if it still holds the
// sealed bytes that could not be opened, as deleteDeadSubscription does. No
// foreign key takes the raw deliveries with it, so it deletes those of the
// incarnation and returns how many.
func deleteDeadRawSubscription(ctx context.Context, q *database.Queries, ref faucet.DeliveryRef, unreadable []byte) (int64, error) {
	deleted, err := q.DeleteSubscriptionIfSealed(ctx, database.DeleteSubscriptionIfSealedParams{KeyHash: ref.Key[:], SecretEnc: unreadable})
	if err != nil {
		return 0, xerrors.Errorf("delete subscription: %w", err)
	}
	if deleted == 0 {
		return 0, nil
	}
	others, err := q.DeleteRawDeliveriesOf(ctx, database.DeleteRawDeliveriesOfParams{KeyHash: ref.Key[:], Incarnation: ref.Incarnation})
	if err != nil {
		return 0, xerrors.Errorf("delete raw deliveries: %w", err)
	}
	return others, nil
}

// ClaimRawGap is one statement, like ClaimGap.
func (s *Store) ClaimRawGap(ctx context.Context, now time.Time, lease time.Duration, skip []string) (_ faucet.GapClaim, _ bool, err error) {
	defer func() { err = endedBy(ctx, err) }()
	if lease <= 0 {
		return faucet.GapClaim{}, false, xerrors.Errorf("lease must be positive, got %s", lease)
	}
	row, err := s.q.ClaimRawGap(ctx, database.ClaimRawGapParams{Now: now, LeaseUntil: sql.NullTime{Time: now.Add(lease), Valid: true}, SkipOrigins: nonNil(skip)})
	if xerrors.Is(err, sql.ErrNoRows) {
		return faucet.GapClaim{}, false, nil
	}
	if err != nil {
		return faucet.GapClaim{}, false, xerrors.Errorf("claim raw gap: %w", err)
	}
	key, err := toKeyHash(row.KeyHash)
	if err != nil {
		return faucet.GapClaim{}, false, err
	}
	return faucet.GapClaim{
		Ref:                 faucet.GapRef{Key: key, Incarnation: row.Incarnation, Through: row.Through},
		CallbackURL:         row.CallbackUrl,
		SecretEnc:           row.SecretEnc,
		PreviousSecretEnc:   row.PreviousSecretEnc,
		PreviousSecretUntil: row.PreviousSecretUntil.Time,
	}, true, nil
}

// FinishRawGap deletes the gap only while its mark is the claim's, so a gap
// that an ingest raised since the claim stays owed and only loses its lease.
func (s *Store) FinishRawGap(ctx context.Context, ref faucet.GapRef) (err error) {
	defer func() { err = endedBy(ctx, err) }()
	if err := s.q.EndRawGap(ctx, database.EndRawGapParams{KeyHash: ref.Key[:], Incarnation: ref.Incarnation, Through: ref.Through}); err != nil {
		return xerrors.Errorf("end raw gap: %w", err)
	}
	if err := s.q.ReleaseRawGap(ctx, database.ReleaseRawGapParams{KeyHash: ref.Key[:], Incarnation: ref.Incarnation}); err != nil {
		return xerrors.Errorf("release raw gap: %w", err)
	}
	return nil
}

// ExpireRawDeliveries deletes the deliveries and the gaps in one transaction,
// so that a call that fails has deleted nothing, and the count it would have
// returned is never lost.
func (s *Store) ExpireRawDeliveries(ctx context.Context, now time.Time) (_ int64, err error) {
	defer func() { err = endedBy(ctx, err) }()
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return 0, xerrors.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	q := s.q.WithTx(tx)
	deleted, err := q.ExpireRawDeliveries(ctx, now)
	if err != nil {
		return 0, xerrors.Errorf("expire raw deliveries: %w", err)
	}
	if err := q.ExpireRawGaps(ctx, now); err != nil {
		return 0, xerrors.Errorf("expire raw gaps: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, xerrors.Errorf("commit: %w", err)
	}
	return deleted, nil
}

// PurgeRawEvents runs each step as a statement of its own, with no transaction
// around them: each step is complete by itself, a later step reads what an
// earlier one committed, so the unread rows of an expired lease stop holding
// bodies in the same call, and short statements keep their row locks short.
func (s *Store) PurgeRawEvents(ctx context.Context, p faucet.RawPurge) (_ faucet.RawPurgeResult, err error) {
	defer func() { err = endedBy(ctx, err) }()
	if err := p.Validate(); err != nil {
		return faucet.RawPurgeResult{}, xerrors.Errorf("validate purge: %w", err)
	}
	var res faucet.RawPurgeResult
	if res.ExpiredLeases, err = s.q.DeleteExpiredRawLeases(ctx, p.Now); err != nil {
		return faucet.RawPurgeResult{}, xerrors.Errorf("delete expired raw leases: %w", err)
	}
	if err := s.q.DeleteOrphanedRawUnread(ctx); err != nil {
		return faucet.RawPurgeResult{}, xerrors.Errorf("delete unread events of ended leases: %w", err)
	}
	if res.Cleared, err = s.q.ClearRawBodies(ctx, sql.NullTime{Time: p.ClearBefore, Valid: !p.ClearBefore.IsZero()}); err != nil {
		return faucet.RawPurgeResult{}, xerrors.Errorf("clear raw bodies: %w", err)
	}
	if !p.DeleteBefore.IsZero() {
		if res.Deleted, err = s.q.DeleteRawEventsBefore(ctx, p.DeleteBefore); err != nil {
			return faucet.RawPurgeResult{}, xerrors.Errorf("delete raw events: %w", err)
		}
	}
	return res, nil
}

func (s *Store) RawWants(ctx context.Context, now time.Time) (_ []faucet.RawWant, err error) {
	defer func() { err = endedBy(ctx, err) }()
	rows, err := s.q.RawWants(ctx, now)
	if err != nil {
		return nil, xerrors.Errorf("select raw wants: %w", err)
	}
	wants := make([]faucet.RawWant, len(rows))
	for i, row := range rows {
		target, err := toTarget(row.TargetKind, row.RepoID, row.OwnerID)
		if err != nil {
			return nil, err
		}
		wants[i] = faucet.RawWant{Target: target, Event: row.Event}
		if len(row.Actions) > 0 {
			wants[i].Actions = row.Actions
		}
	}
	return wants, nil
}

func toRawInterest(row database.RawInterestsRow) (faucet.RawInterest, error) {
	if row.IsLease {
		id, err := toLeaseID(row.ID)
		if err != nil {
			return faucet.RawInterest{}, err
		}
		return faucet.RawInterest{Lease: id, IsLease: true, Fields: row.Fields, Pending: row.Pending}, nil
	}
	key, err := toKeyHash(row.ID)
	if err != nil {
		return faucet.RawInterest{}, err
	}
	return faucet.RawInterest{Subscription: key, Incarnation: row.Incarnation, Fields: row.Fields, Pending: row.Pending}, nil
}

func toLeaseID(raw []byte) (faucet.RawLeaseID, error) {
	var id faucet.RawLeaseID
	if len(raw) != len(id) {
		return faucet.RawLeaseID{}, xerrors.Errorf("stored lease ID has %d bytes, want %d", len(raw), len(id))
	}
	copy(id[:], raw)
	return id, nil
}

func leaseIDs(ids []faucet.RawLeaseID) [][]byte {
	out := make([][]byte, len(ids))
	for i := range ids {
		out[i] = ids[i][:]
	}
	return out
}
