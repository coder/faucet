package pgstore

import (
	"context"
	"database/sql"
	"time"

	"golang.org/x/xerrors"

	"github.com/coder/faucet/faucet"
	"github.com/coder/faucet/faucet/pgstore/database"
)

func (s *Store) ScanTargets(ctx context.Context, now time.Time, maxPending, limit int32) (_ []faucet.ScanTarget, err error) {
	defer func() { err = endedBy(ctx, err) }()
	if maxPending <= 0 || limit <= 0 {
		return nil, xerrors.New("max pending and limit must be positive")
	}
	rows, err := s.q.ScanTargets(ctx, database.ScanTargetsParams{Now: now, MaxPending: maxPending, RowLimit: limit})
	if err != nil {
		return nil, xerrors.Errorf("select scan targets: %w", err)
	}
	targets := make([]faucet.ScanTarget, len(rows))
	for i, row := range rows {
		key, err := toKeyHash(row.KeyHash)
		if err != nil {
			return nil, err
		}
		targets[i] = faucet.ScanTarget{
			KeyHash:        key,
			Principal:      row.Principal,
			EventType:      row.EventType,
			Arguments:      row.Arguments,
			RepoID:         row.RepoID,
			ScannedThrough: row.ScannedThrough,
			Pending:        row.Pending,
		}
	}
	return targets, nil
}

// EnqueueDeliveries moves the read position first, with a compare on the
// position the caller read from. A concurrent caller that read from the same
// position waits on the row lock and then finds it moved, so only one of them
// queues the deliveries.
func (s *Store) EnqueueDeliveries(ctx context.Context, now time.Time, e faucet.Enqueue) (_ bool, err error) {
	defer func() { err = endedBy(ctx, err) }()
	if err := e.Validate(); err != nil {
		return false, xerrors.Errorf("validate enqueue: %w", err)
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return false, xerrors.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	q := s.q.WithTx(tx)

	moved, err := q.AdvanceScan(ctx, database.AdvanceScanParams{
		NewScannedThrough:      e.NewScannedThrough,
		GapThrough:             e.GapThrough,
		KeyHash:                e.Key[:],
		ExpectedScannedThrough: e.ExpectedScannedThrough,
		Now:                    now,
	})
	if err != nil {
		return false, xerrors.Errorf("advance scan position: %w", err)
	}
	if moved == 0 {
		return false, nil
	}
	if len(e.Deliveries) > 0 {
		eventIDs := make([]int64, len(e.Deliveries))
		messageIDs := make([]string, len(e.Deliveries))
		for i, d := range e.Deliveries {
			eventIDs[i], messageIDs[i] = d.EventID, d.MessageID
		}
		if err := q.InsertDeliveries(ctx, database.InsertDeliveriesParams{KeyHash: e.Key[:], Now: now, EventIds: eventIDs, MessageIds: messageIDs}); err != nil {
			return false, xerrors.Errorf("insert deliveries: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return false, xerrors.Errorf("commit: %w", err)
	}
	return true, nil
}

// ClaimDelivery is one statement. FOR UPDATE SKIP LOCKED hands a due row to one
// caller, and the update in the same statement sets the lease and raises the
// attempt, so a caller that arrives after the commit finds a held row and
// skips it.
func (s *Store) ClaimDelivery(ctx context.Context, now time.Time, lease time.Duration, skip []string) (_ faucet.DeliveryClaim, _ bool, err error) {
	defer func() { err = endedBy(ctx, err) }()
	if lease <= 0 {
		return faucet.DeliveryClaim{}, false, xerrors.Errorf("lease must be positive, got %s", lease)
	}
	row, err := s.q.ClaimDelivery(ctx, database.ClaimDeliveryParams{Now: now, LeaseUntil: sql.NullTime{Time: now.Add(lease), Valid: true}, SkipOrigins: nonNil(skip)})
	if xerrors.Is(err, sql.ErrNoRows) {
		return faucet.DeliveryClaim{}, false, nil
	}
	if err != nil {
		return faucet.DeliveryClaim{}, false, xerrors.Errorf("claim delivery: %w", err)
	}
	key, err := toKeyHash(row.KeyHash)
	if err != nil {
		return faucet.DeliveryClaim{}, false, err
	}
	claim := faucet.DeliveryClaim{
		Ref:                 faucet.DeliveryRef{Key: key, EventID: row.EventID, MessageID: row.MessageID, Attempt: row.Attempts, Incarnation: row.Incarnation},
		MessageID:           row.MessageID,
		EnqueuedAt:          row.EnqueuedAt,
		CallbackURL:         row.CallbackUrl,
		SecretEnc:           row.SecretEnc,
		PreviousSecretEnc:   row.PreviousSecretEnc,
		PreviousSecretUntil: row.PreviousSecretUntil.Time,
		RepoID:              row.RepoID,
		Watermark:           row.Watermark,
	}
	if row.StoredEventID.Valid {
		claim.Event = &faucet.Event{
			ID:         row.StoredEventID.Int64,
			Source:     row.Source.String,
			DeliveryID: row.DeliveryID.String,
			Type:       row.Type.String,
			RepoID:     row.EventRepoID.Int64,
			PRs:        row.Prs,
			HeadSHA:    row.HeadSha.String,
			Data:       row.Data,
			ReceivedAt: row.ReceivedAt.Time,
		}
	}
	return claim, true, nil
}

// ClaimGap is one statement, like ClaimDelivery: the lease is set by the same
// statement that selects the row, so a caller that arrives after the commit
// finds a held gap and skips it.
func (s *Store) ClaimGap(ctx context.Context, now time.Time, lease time.Duration, skip []string) (_ faucet.GapClaim, _ bool, err error) {
	defer func() { err = endedBy(ctx, err) }()
	if lease <= 0 {
		return faucet.GapClaim{}, false, xerrors.Errorf("lease must be positive, got %s", lease)
	}
	row, err := s.q.ClaimGap(ctx, database.ClaimGapParams{Now: now, LeaseUntil: sql.NullTime{Time: now.Add(lease), Valid: true}, SkipOrigins: nonNil(skip)})
	if xerrors.Is(err, sql.ErrNoRows) {
		return faucet.GapClaim{}, false, nil
	}
	if err != nil {
		return faucet.GapClaim{}, false, xerrors.Errorf("claim gap: %w", err)
	}
	key, err := toKeyHash(row.KeyHash)
	if err != nil {
		return faucet.GapClaim{}, false, err
	}
	return faucet.GapClaim{
		Ref:                 faucet.GapRef{Key: key, Incarnation: row.Incarnation, Through: row.GapThrough},
		CallbackURL:         row.CallbackUrl,
		SecretEnc:           row.SecretEnc,
		PreviousSecretEnc:   row.PreviousSecretEnc,
		PreviousSecretUntil: row.PreviousSecretUntil.Time,
	}, true, nil
}

func (s *Store) FinishGap(ctx context.Context, ref faucet.GapRef) (err error) {
	defer func() { err = endedBy(ctx, err) }()
	if err := s.q.FinishGap(ctx, database.FinishGapParams{Through: ref.Through, KeyHash: ref.Key[:], Incarnation: ref.Incarnation}); err != nil {
		return xerrors.Errorf("finish gap: %w", err)
	}
	return nil
}

// nonNil turns a nil list into an empty one: lib/pq sends a nil slice as NULL,
// and a comparison with NULL would match no row.
func nonNil(list []string) []string {
	if list == nil {
		return []string{}
	}
	return list
}

// FinishDelivery takes the lock of the subscription row before it touches the
// delivery. DeleteSubscription locks them in the other order (the subscription,
// then the deliveries that cascade), and two transactions that took the two
// locks in opposite orders could wait on each other. The lock also makes the
// incarnation check hold: the row cannot be replaced between the check and the
// update.
func (s *Store) FinishDelivery(ctx context.Context, ref faucet.DeliveryRef, now time.Time, f faucet.Finish) (_ faucet.FinishResult, err error) {
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
	result, err := finishLocked(ctx, q, ref, now, f)
	if err != nil || !result.Accepted {
		return faucet.FinishResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return faucet.FinishResult{}, xerrors.Errorf("commit: %w", err)
	}
	return result, nil
}

func finishLocked(ctx context.Context, q *database.Queries, ref faucet.DeliveryRef, now time.Time, f faucet.Finish) (faucet.FinishResult, error) {
	var affected int64
	var err error
	if f.Kind == faucet.FinishRetry {
		affected, err = q.RescheduleDelivery(ctx, database.RescheduleDeliveryParams{NextAttemptAt: f.RetryAt, KeyHash: ref.Key[:], EventID: ref.EventID, MessageID: ref.MessageID, Attempt: ref.Attempt})
	} else {
		affected, err = q.EndDelivery(ctx, database.EndDeliveryParams{KeyHash: ref.Key[:], EventID: ref.EventID, MessageID: ref.MessageID, Attempt: ref.Attempt})
	}
	if err != nil {
		return faucet.FinishResult{}, xerrors.Errorf("update delivery: %w", err)
	}
	if affected == 0 {
		return faucet.FinishResult{}, nil
	}
	result := faucet.FinishResult{Accepted: true}
	if f.Kind != faucet.FinishRetry {
		result.Removed = 1
	}
	switch {
	case f.Kind == faucet.FinishDelivered:
		err = q.RecordDeliverySuccess(ctx, database.RecordDeliverySuccessParams{Now: sql.NullTime{Time: now, Valid: true}, KeyHash: ref.Key[:]})
	case f.Failure != "":
		err = q.RecordDeliveryFailure(ctx, database.RecordDeliveryFailureParams{
			LastError: sql.NullString{String: string(f.Failure), Valid: true}, Now: sql.NullTime{Time: now, Valid: true}, KeyHash: ref.Key[:],
		})
	case f.Kind == faucet.FinishDead:
		result.Removed, err = deleteDeadSubscription(ctx, q, ref.Key, f.Unreadable)
	}
	if err != nil {
		return faucet.FinishResult{}, err
	}
	return result, nil
}

// deleteDeadSubscription deletes the subscription only if it still holds the
// sealed bytes that could not be opened, so a refresh that sealed a new secret
// keeps its row. It returns the deliveries that ended: the one that the
// caller already deleted, and the others that went with the subscription.
func deleteDeadSubscription(ctx context.Context, q *database.Queries, key faucet.SubscriptionKeyHash, unreadable []byte) (int64, error) {
	others, err := q.CountDeliveries(ctx, key[:])
	if err != nil {
		return 0, xerrors.Errorf("count deliveries: %w", err)
	}
	deleted, err := q.DeleteSubscriptionIfSealed(ctx, database.DeleteSubscriptionIfSealedParams{KeyHash: key[:], SecretEnc: unreadable})
	if err != nil {
		return 0, xerrors.Errorf("delete subscription: %w", err)
	}
	if deleted == 0 {
		return 1, nil
	}
	return 1 + others, nil
}

func (s *Store) ExpireDeliveries(ctx context.Context, now time.Time) (_ int64, err error) {
	defer func() { err = endedBy(ctx, err) }()
	deleted, err := s.q.ExpireDeliveries(ctx, now)
	if err != nil {
		return 0, xerrors.Errorf("expire deliveries: %w", err)
	}
	return deleted, nil
}

func toKeyHash(raw []byte) (faucet.SubscriptionKeyHash, error) {
	var key faucet.SubscriptionKeyHash
	if len(raw) != len(key) {
		return faucet.SubscriptionKeyHash{}, xerrors.Errorf("stored key hash has %d bytes, want %d", len(raw), len(key))
	}
	copy(key[:], raw)
	return key, nil
}
