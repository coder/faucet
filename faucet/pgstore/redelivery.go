package pgstore

import (
	"context"
	"database/sql"
	"slices"
	"time"

	"golang.org/x/xerrors"

	"github.com/coder/faucet/faucet"
	"github.com/coder/faucet/faucet/pgstore/database"
)

var _ faucet.RedeliveryStore = (*Store)(nil)

// AcquireRedeliveryLease locks the scan row, so two runs that start together
// are decided one after the other, and the second sees the first one's lease.
func (s *Store) AcquireRedeliveryLease(ctx context.Context, hook string, now time.Time, ttl time.Duration) (_ faucet.RedeliveryScan, _ bool, err error) {
	defer func() { err = endedBy(ctx, err) }()
	if hook == "" || now.IsZero() || ttl <= 0 {
		return faucet.RedeliveryScan{}, false, xerrors.New("a lease needs a hook, a time and a ttl")
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return faucet.RedeliveryScan{}, false, xerrors.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	q := s.q.WithTx(tx)

	if err := q.EnsureRedeliveryScan(ctx, hook); err != nil {
		return faucet.RedeliveryScan{}, false, xerrors.Errorf("create scan row: %w", err)
	}
	row, err := q.LockRedeliveryScan(ctx)
	if err != nil {
		return faucet.RedeliveryScan{}, false, xerrors.Errorf("lock scan row: %w", err)
	}
	if row.LeaseUntil.Valid && row.LeaseUntil.Time.After(now) {
		return faucet.RedeliveryScan{}, false, nil
	}

	var scan faucet.RedeliveryScan
	scan.HookChanged = row.Epoch > 0 && row.Hook != hook
	if scan.HookChanged {
		closed, err := q.CloseOpenRedeliveryRanges(ctx, now)
		if err != nil {
			return faucet.RedeliveryScan{}, false, xerrors.Errorf("close ranges of the old hook: %w", err)
		}
		for _, r := range closed {
			scan.ClosedGaps = append(scan.ClosedGaps, faucet.RedeliveryGap{From: r.OlderAt.UTC(), To: laterOf(r.NewerAt, r.OlderAt).UTC()})
		}
	}
	taken, err := q.TakeRedeliveryLease(ctx, database.TakeRedeliveryLeaseParams{Hook: hook, LeaseUntil: now.Add(ttl), ResetTop: scan.HookChanged})
	if err != nil {
		return faucet.RedeliveryScan{}, false, xerrors.Errorf("take lease: %w", err)
	}
	scan.Epoch = taken.Epoch
	if taken.TopAt.Valid {
		scan.Top = taken.TopAt.Time.UTC()
	}
	ranges, err := q.ListOpenRedeliveryRanges(ctx)
	if err != nil {
		return faucet.RedeliveryScan{}, false, xerrors.Errorf("list ranges: %w", err)
	}
	for _, r := range ranges {
		scan.Ranges = append(scan.Ranges, faucet.RedeliveryRange{ID: r.ID, NewerAt: r.NewerAt.UTC(), OlderAt: r.OlderAt.UTC(), Cursor: r.Cursor})
	}
	if err := tx.Commit(); err != nil {
		return faucet.RedeliveryScan{}, false, xerrors.Errorf("commit: %w", err)
	}
	return scan, true, nil
}

func laterOf(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func (s *Store) ReleaseRedeliveryLease(ctx context.Context, epoch int64) (err error) {
	defer func() { err = endedBy(ctx, err) }()
	if err := s.q.ReleaseRedeliveryLease(ctx, epoch); err != nil {
		return xerrors.Errorf("release lease: %w", err)
	}
	return nil
}

// ApplyRedeliveryScan writes a step's attempts and its position move in one
// transaction, after it has locked the scan row and checked the epoch. A run
// whose lease was replaced therefore writes nothing.
func (s *Store) ApplyRedeliveryScan(ctx context.Context, step faucet.ScanStep) (_ faucet.ScanResult, err error) {
	defer func() { err = endedBy(ctx, err) }()
	if err := step.Validate(); err != nil {
		return faucet.ScanResult{}, xerrors.Errorf("validate scan step: %w", err)
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return faucet.ScanResult{}, xerrors.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	q := s.q.WithTx(tx)

	row, err := q.LockRedeliveryScan(ctx)
	if err != nil {
		return faucet.ScanResult{}, xerrors.Errorf("lock scan row: %w", err)
	}
	if row.Epoch != step.Epoch {
		return faucet.ScanResult{}, faucet.ErrRedeliveryLeaseLost
	}

	var res faucet.ScanResult
	if len(step.Failed) > 0 {
		guids := make([]string, len(step.Failed))
		attemptIDs := make([]int64, len(step.Failed))
		deliveredAts := make([]time.Time, len(step.Failed))
		for i, f := range step.Failed {
			guids[i], attemptIDs[i], deliveredAts[i] = f.GUID, f.AttemptID, f.DeliveredAt
		}
		tracked, err := q.TrackFailedAttempts(ctx, database.TrackFailedAttemptsParams{
			Guids: guids, AttemptIds: attemptIDs, DeliveredAts: deliveredAts, Now: step.Now,
		})
		if err != nil {
			return faucet.ScanResult{}, xerrors.Errorf("track failed attempts: %w", err)
		}
		res.Tracked = int(tracked)
	}
	if len(step.Succeeded) > 0 {
		res.Recovered, err = q.RecoverSucceededDeliveries(ctx, database.RecoverSucceededDeliveriesParams{Now: step.Now, Guids: step.Succeeded})
		if err != nil {
			return faucet.ScanResult{}, xerrors.Errorf("recover deliveries: %w", err)
		}
		slices.Sort(res.Recovered)
	}
	if !step.Top.IsZero() {
		if err := q.RaiseRedeliveryTop(ctx, step.Top); err != nil {
			return faucet.ScanResult{}, xerrors.Errorf("raise top: %w", err)
		}
	}
	if res.OpenedID, err = moveRange(ctx, q, step); err != nil {
		return faucet.ScanResult{}, err
	}
	if step.Gap != nil {
		err := q.InsertRedeliveryGap(ctx, database.InsertRedeliveryGapParams{FromAt: step.Gap.From, ToAt: step.Gap.To, Now: step.Now})
		if err != nil {
			return faucet.ScanResult{}, xerrors.Errorf("record gap: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return faucet.ScanResult{}, xerrors.Errorf("commit: %w", err)
	}
	return res, nil
}

// moveRange applies the step's one range move. It returns the ID of a range
// that the move created.
func moveRange(ctx context.Context, q *database.Queries, step faucet.ScanStep) (int64, error) {
	var (
		rows int64
		err  error
		id   int64
	)
	switch {
	case step.Open != nil:
		id, err = q.InsertRedeliveryRange(ctx, database.InsertRedeliveryRangeParams{
			NewerAt: step.Open.NewerAt, OlderAt: step.Open.OlderAt, Cursor: step.Open.Cursor,
		})
		return id, wrapRange(err, "open range")
	case step.Advance != nil:
		rows, err = q.AdvanceRedeliveryRange(ctx, database.AdvanceRedeliveryRangeParams{
			ID: step.Advance.ID, NewerAt: step.Advance.NewerAt, Cursor: step.Advance.Cursor,
		})
		id = step.Advance.ID
	case step.CloseID != 0:
		rows, err = q.FinishRedeliveryRange(ctx, step.CloseID)
		id = step.CloseID
	default:
		return 0, nil
	}
	if err := wrapRange(err, "move range"); err != nil {
		return 0, err
	}
	if rows != 1 {
		return 0, xerrors.Errorf("range %d is not open", id)
	}
	return 0, nil
}

func wrapRange(err error, what string) error {
	if err != nil {
		return xerrors.Errorf("%s: %w", what, err)
	}
	return nil
}

func (s *Store) SettleRedeliveries(ctx context.Context, req faucet.SettleRequest) (_ faucet.SettleResult, err error) {
	defer func() { err = endedBy(ctx, err) }()
	if err := req.Validate(); err != nil {
		return faucet.SettleResult{}, xerrors.Errorf("validate settle request: %w", err)
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return faucet.SettleResult{}, xerrors.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	q := s.q.WithTx(tx)

	var res faucet.SettleResult
	if res.Recovered, err = q.SettleRecoveredRedeliveries(ctx, req.Now); err != nil {
		return faucet.SettleResult{}, xerrors.Errorf("settle recovered: %w", err)
	}
	slices.Sort(res.Recovered)
	expired, err := q.SettleExpiredRedeliveries(ctx, database.SettleExpiredRedeliveriesParams{Now: req.Now, ExpiredBefore: req.Now.Add(-req.Retention)})
	if err != nil {
		return faucet.SettleResult{}, xerrors.Errorf("settle expired: %w", err)
	}
	for _, r := range expired {
		res.GaveUp = append(res.GaveUp, faucet.GaveUp{GUID: r.Guid, Reason: faucet.GiveUpExpired, Asks: int(r.Asks), FirstDeliveredAt: r.FirstAttemptAt.UTC()})
	}
	limited, err := q.SettleLimitedRedeliveries(ctx, database.SettleLimitedRedeliveriesParams{Now: req.Now, MaxAsks: int32(req.MaxAsks)}) //nolint:gosec // Validate bounds MaxAsks.
	if err != nil {
		return faucet.SettleResult{}, xerrors.Errorf("settle limited: %w", err)
	}
	for _, r := range limited {
		res.GaveUp = append(res.GaveUp, faucet.GaveUp{GUID: r.Guid, Reason: faucet.GiveUpLimit, Asks: int(r.Asks), FirstDeliveredAt: r.FirstAttemptAt.UTC()})
	}
	slices.SortFunc(res.GaveUp, func(a, b faucet.GaveUp) int { return compareStrings(a.GUID, b.GUID) })
	pending, err := q.CountPendingRedeliveries(ctx)
	if err != nil {
		return faucet.SettleResult{}, xerrors.Errorf("count pending: %w", err)
	}
	res.Pending = int(pending)
	if err := tx.Commit(); err != nil {
		return faucet.SettleResult{}, xerrors.Errorf("commit: %w", err)
	}
	return res, nil
}

func compareStrings(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// ClaimRedelivery picks a delivery and raises its ask count in one statement,
// so a run that crashes right after it has spent an ask and no two runs share
// one. SKIP LOCKED lets a second claim take the next delivery instead of
// waiting.
func (s *Store) ClaimRedelivery(ctx context.Context, req faucet.ClaimRequest) (_ faucet.RedeliveryClaim, _ bool, err error) {
	defer func() { err = endedBy(ctx, err) }()
	if err := req.Validate(); err != nil {
		return faucet.RedeliveryClaim{}, false, xerrors.Errorf("validate claim request: %w", err)
	}
	schedule := make([]float64, len(req.Schedule))
	for i, wait := range req.Schedule {
		schedule[i] = wait.Seconds()
	}
	row, err := s.q.ClaimRedelivery(ctx, database.ClaimRedeliveryParams{
		Now: req.Now, NotBefore: req.Now.Add(-req.Retention), MaxAsks: int32(req.MaxAsks), ScheduleSecs: schedule, //nolint:gosec // Validate bounds MaxAsks.
	})
	if xerrors.Is(err, sql.ErrNoRows) {
		return faucet.RedeliveryClaim{}, false, nil
	}
	if err != nil {
		return faucet.RedeliveryClaim{}, false, xerrors.Errorf("claim redelivery: %w", err)
	}
	return faucet.RedeliveryClaim{GUID: row.Guid, AttemptID: row.LastAttemptID, Ask: int(row.Asks)}, true, nil
}

func (s *Store) PurgeRedeliveries(ctx context.Context, before time.Time) (_ int64, err error) {
	defer func() { err = endedBy(ctx, err) }()
	deleted, err := s.q.PurgeRedeliveries(ctx, before)
	if err != nil {
		return 0, xerrors.Errorf("purge redeliveries: %w", err)
	}
	return deleted, nil
}
