// Package pgstore implements faucet.Store on Postgres.
package pgstore

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"golang.org/x/xerrors"

	"github.com/coder/faucet/faucet"
	"github.com/coder/faucet/faucet/pgstore/database"
)

// eventsLockKey is the FNV-1a 64-bit hash of "faucet_events", read as an int64.
// Every writer of faucet_events must take this advisory lock first.
const eventsLockKey int64 = 1664114494957747203

var _ faucet.Store = (*Store)(nil)

type Store struct {
	db *sql.DB
	q  *database.Queries
}

// New returns a Store that uses db. The schema must already be migrated.
func New(db *sql.DB) *Store {
	return &Store{db: db, q: database.New(db)}
}

// endedBy makes the error of a call that its context cut short wrap the context
// error. lib/pq ends such a query with the server's cancel error, which wraps
// neither context.Canceled nor context.DeadlineExceeded, and the callers tell
// the end of their own run from a store failure by those two.
func endedBy(ctx context.Context, err error) error {
	if err == nil || ctx.Err() == nil || xerrors.Is(err, ctx.Err()) {
		return err
	}
	return fmt.Errorf("%w: %w", ctx.Err(), err)
}

// InsertEvent stores ev in one transaction that holds eventsLockKey until it
// commits. Sequence values are therefore handed out and committed in the same
// order, so a reader that sees ID n has seen every lower ID that will commit.
func (s *Store) InsertEvent(ctx context.Context, ev faucet.Event) (_ faucet.Event, _ bool, err error) {
	defer func() { err = endedBy(ctx, err) }()
	if err := ev.Validate(); err != nil {
		return faucet.Event{}, false, xerrors.Errorf("validate event: %w", err)
	}
	// pq.Array encodes a nil slice as NULL, which the NOT NULL column rejects.
	prs := ev.PRs
	if prs == nil {
		prs = []int32{}
	}

	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return faucet.Event{}, false, xerrors.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	q := s.q.WithTx(tx)

	if err := q.LockEvents(ctx, eventsLockKey); err != nil {
		return faucet.Event{}, false, xerrors.Errorf("lock events: %w", err)
	}
	row, err := q.InsertEvent(ctx, database.InsertEventParams{
		Source:     ev.Source,
		DeliveryID: ev.DeliveryID,
		Type:       ev.Type,
		RepoID:     ev.RepoID,
		Prs:        prs,
		HeadSha:    ev.HeadSHA,
		Data:       ev.Data,
	})
	inserted := true
	if xerrors.Is(err, sql.ErrNoRows) {
		inserted = false
		row, err = q.GetEventByDelivery(ctx, ev.DeliveryID)
	}
	if err != nil {
		return faucet.Event{}, false, xerrors.Errorf("insert event: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return faucet.Event{}, false, xerrors.Errorf("commit: %w", err)
	}
	return toEvent(row), inserted, nil
}

func (s *Store) EventsAfter(ctx context.Context, afterID int64, limit int32) (_ []faucet.Event, err error) {
	defer func() { err = endedBy(ctx, err) }()
	if limit <= 0 {
		return nil, xerrors.Errorf("limit must be positive, got %d", limit)
	}
	rows, err := s.q.EventsAfter(ctx, database.EventsAfterParams{AfterID: afterID, RowLimit: limit})
	if err != nil {
		return nil, xerrors.Errorf("select events: %w", err)
	}
	events := make([]faucet.Event, len(rows))
	for i, row := range rows {
		events[i] = toEvent(row)
	}
	return events, nil
}

// PollEvents runs one statement, so the page and the head ID it reports share a
// snapshot. The head ID is read in the same statement as the page because
// IDs become visible in commit order: every ID at or below it that will ever
// exist is already visible, so a cursor set to it cannot skip an event.
func (s *Store) PollEvents(ctx context.Context, q faucet.PollQuery) (_ faucet.PollPage, err error) {
	defer func() { err = endedBy(ctx, err) }()
	if err := q.Validate(); err != nil {
		return faucet.PollPage{}, xerrors.Errorf("validate poll query: %w", err)
	}
	// pq.Array encodes a nil slice as NULL, which makes the type filter match nothing.
	types := q.Types
	if types == nil {
		types = []string{}
	}
	rows, err := s.q.PollEvents(ctx, database.PollEventsParams{
		RepoID:   q.RepoID,
		AfterID:  q.After,
		Types:    types,
		HeadSha:  q.HeadSHA,
		Pr:       q.PR,
		RowLimit: q.Limit,
	})
	if err != nil {
		return faucet.PollPage{}, xerrors.Errorf("poll events: %w", err)
	}
	if len(rows) == 0 {
		return faucet.PollPage{}, xerrors.New("poll events: statement returned no head row")
	}

	page := faucet.PollPage{Next: max(rows[0].MaxID, q.After), PurgedThrough: rows[0].PurgedThrough}
	for _, row := range rows {
		if row.ID.Valid {
			page.Events = append(page.Events, pollRowToEvent(row))
		}
	}
	if len(page.Events) == int(q.Limit) {
		page.Next = page.Events[len(page.Events)-1].ID
	}
	return page, nil
}

func (s *Store) CursorAt(ctx context.Context, t time.Time) (_ int64, err error) {
	defer func() { err = endedBy(ctx, err) }()
	id, err := s.q.CursorAt(ctx, t)
	if err != nil {
		return 0, xerrors.Errorf("select cursor: %w", err)
	}
	return id, nil
}

// Purge deletes the old events and raises the mark in one transaction. The
// mark is the highest ID the same statement deleted, so it is exact whatever
// inserts run alongside: an insert that the statement's snapshot cannot see is
// not deleted and gets an ID above every ID the snapshot can see. Purge does
// not take eventsLockKey, so an insert of a new delivery does not wait for a
// large delete. An insert of a delivery ID that the purge is deleting does wait
// for the purge to commit, and it holds eventsLockKey meanwhile, so the inserts
// behind it wait too. Only a redelivery of an event older than the retention
// does that. Purge locks the mark row first, so concurrent purges queue up
// instead of deadlocking.
func (s *Store) Purge(ctx context.Context, before time.Time) (_ faucet.PurgeResult, err error) {
	defer func() { err = endedBy(ctx, err) }()
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return faucet.PurgeResult{}, xerrors.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	q := s.q.WithTx(tx)

	if err := q.LockPurgeMark(ctx); err != nil {
		return faucet.PurgeResult{}, xerrors.Errorf("lock purge mark: %w", err)
	}
	if _, err := q.AdvanceSubscriptionsThatLostNothing(ctx, before); err != nil {
		return faucet.PurgeResult{}, xerrors.Errorf("advance subscriptions that lose nothing: %w", err)
	}
	deleted, err := q.DeleteEventsBefore(ctx, before)
	if err != nil {
		return faucet.PurgeResult{}, xerrors.Errorf("delete events: %w", err)
	}
	mark, err := q.RaisePurgeMark(ctx, deleted.MaxID)
	if err != nil {
		return faucet.PurgeResult{}, xerrors.Errorf("raise purge mark: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return faucet.PurgeResult{}, xerrors.Errorf("commit: %w", err)
	}
	return faucet.PurgeResult{Deleted: deleted.Deleted, PurgedThrough: mark}, nil
}

// UpsertSubscription takes a lock per principal, so two subscribes of one
// principal cannot both pass the limit. It deletes the principal's expired
// rows first, so a lapsed subscription is replaced whole and does not count
// against the limit.
func (s *Store) UpsertSubscription(ctx context.Context, sub faucet.Subscription, now time.Time, maxLive int) (_ faucet.Subscription, err error) {
	defer func() { err = endedBy(ctx, err) }()
	if maxLive <= 0 {
		return faucet.Subscription{}, xerrors.Errorf("max live subscriptions must be positive, got %d", maxLive)
	}
	if err := sub.Validate(now); err != nil {
		return faucet.Subscription{}, xerrors.Errorf("validate subscription: %w", err)
	}

	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return faucet.Subscription{}, xerrors.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	q := s.q.WithTx(tx)

	if err := q.LockSubscriptions(ctx, sub.Principal); err != nil {
		return faucet.Subscription{}, xerrors.Errorf("lock subscriptions: %w", err)
	}
	if err := q.DeleteExpiredSubscriptionsOf(ctx, database.DeleteExpiredSubscriptionsOfParams{Principal: sub.Principal, Now: now}); err != nil {
		return faucet.Subscription{}, xerrors.Errorf("delete expired subscriptions: %w", err)
	}
	others, err := q.CountOtherSubscriptions(ctx, database.CountOtherSubscriptionsParams{Principal: sub.Principal, KeyHash: sub.KeyHash[:]})
	if err != nil {
		return faucet.Subscription{}, xerrors.Errorf("count subscriptions: %w", err)
	}
	if others >= int64(maxLive) {
		return faucet.Subscription{}, faucet.ErrSubscriptionLimit
	}
	row, err := q.UpsertSubscription(ctx, database.UpsertSubscriptionParams{
		KeyHash:             sub.KeyHash[:],
		Principal:           sub.Principal,
		CallbackUrl:         sub.CallbackURL,
		EventType:           sub.EventType,
		Arguments:           sub.Arguments,
		RepoID:              sub.RepoID,
		SecretEnc:           sub.SecretEnc,
		Cursor:              sub.Cursor,
		Now:                 now,
		ExpiresAt:           sub.ExpiresAt,
		PreviousSecretUntil: sql.NullTime{Time: now.Add(sub.RotationGrace), Valid: sub.RotationGrace > 0},
	})
	if err != nil {
		return faucet.Subscription{}, xerrors.Errorf("upsert subscription: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return faucet.Subscription{}, xerrors.Errorf("commit: %w", err)
	}
	return toSubscription(row)
}

func (s *Store) DeleteSubscription(ctx context.Context, principal string, key faucet.SubscriptionKeyHash) (_ bool, err error) {
	defer func() { err = endedBy(ctx, err) }()
	deleted, err := s.q.DeleteSubscription(ctx, database.DeleteSubscriptionParams{KeyHash: key[:], Principal: principal})
	if err != nil {
		return false, xerrors.Errorf("delete subscription: %w", err)
	}
	return deleted > 0, nil
}

func (s *Store) DeleteLiveSubscription(ctx context.Context, principal string, key faucet.SubscriptionKeyHash, now time.Time) (_ faucet.Subscription, _ bool, err error) {
	defer func() { err = endedBy(ctx, err) }()
	row, err := s.q.DeleteLiveSubscription(ctx, database.DeleteLiveSubscriptionParams{KeyHash: key[:], Principal: principal, Now: now})
	if xerrors.Is(err, sql.ErrNoRows) {
		return faucet.Subscription{}, false, nil
	}
	if err != nil {
		return faucet.Subscription{}, false, xerrors.Errorf("delete live subscription: %w", err)
	}
	deleted, err := toSubscription(row)
	return deleted, err == nil, err
}

func (s *Store) DeleteExpiredSubscriptions(ctx context.Context, now time.Time) (_ int64, err error) {
	defer func() { err = endedBy(ctx, err) }()
	deleted, err := s.q.DeleteExpiredSubscriptions(ctx, now)
	if err != nil {
		return 0, xerrors.Errorf("delete expired subscriptions: %w", err)
	}
	return deleted, nil
}

func (s *Store) ClearExpiredPreviousSecrets(ctx context.Context, now time.Time) (_ int64, err error) {
	defer func() { err = endedBy(ctx, err) }()
	cleared, err := s.q.ClearExpiredPreviousSecrets(ctx, now)
	if err != nil {
		return 0, xerrors.Errorf("clear expired previous secrets: %w", err)
	}
	return cleared, nil
}

func toSubscription(row database.FaucetSubscription) (faucet.Subscription, error) {
	key, err := toKeyHash(row.KeyHash)
	if err != nil {
		return faucet.Subscription{}, err
	}
	return faucet.Subscription{
		KeyHash:             key,
		Principal:           row.Principal,
		CallbackURL:         row.CallbackUrl,
		EventType:           row.EventType,
		Arguments:           row.Arguments,
		RepoID:              row.RepoID,
		SecretEnc:           row.SecretEnc,
		PreviousSecretEnc:   row.PreviousSecretEnc,
		PreviousSecretUntil: row.PreviousSecretUntil.Time,
		Cursor:              row.Cursor,
		CreatedAt:           row.CreatedAt,
		UpdatedAt:           row.UpdatedAt,
		ExpiresAt:           row.ExpiresAt,
		Status: faucet.DeliveryStatus{
			LastDeliveryAt: row.LastDeliveryAt.Time,
			LastError:      faucet.CallbackReason(row.LastError.String),
			FailedSince:    row.FailedSince.Time,
		},
	}, nil
}

func pollRowToEvent(row database.PollEventsRow) faucet.Event {
	return faucet.Event{
		ID:         row.ID.Int64,
		Source:     row.Source.String,
		DeliveryID: row.DeliveryID.String,
		Type:       row.Type.String,
		RepoID:     row.RepoID.Int64,
		PRs:        row.Prs,
		HeadSHA:    row.HeadSha.String,
		Data:       row.Data,
		ReceivedAt: row.ReceivedAt.Time,
	}
}

func toEvent(row database.FaucetEvent) faucet.Event {
	return faucet.Event{
		ID:         row.ID,
		Source:     row.Source,
		DeliveryID: row.DeliveryID,
		Type:       row.Type,
		RepoID:     row.RepoID,
		PRs:        row.Prs,
		HeadSHA:    row.HeadSha.String,
		Data:       row.Data,
		ReceivedAt: row.ReceivedAt,
	}
}
