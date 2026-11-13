// Package pgstore implements faucet.Store on Postgres.
package pgstore

import (
	"context"
	"database/sql"
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

// InsertEvent stores ev in one transaction that holds eventsLockKey until it
// commits. Sequence values are therefore handed out and committed in the same
// order, so a reader that sees ID n has seen every lower ID that will commit.
func (s *Store) InsertEvent(ctx context.Context, ev faucet.Event) (faucet.Event, bool, error) {
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

func (s *Store) EventsAfter(ctx context.Context, afterID int64, limit int32) ([]faucet.Event, error) {
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
func (s *Store) PollEvents(ctx context.Context, q faucet.PollQuery) (faucet.PollPage, error) {
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

func (s *Store) CursorAt(ctx context.Context, t time.Time) (int64, error) {
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
// not take eventsLockKey, so a large delete never holds up a webhook. It locks
// the mark row first, so concurrent purges queue up instead of deadlocking.
func (s *Store) Purge(ctx context.Context, before time.Time) (faucet.PurgeResult, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return faucet.PurgeResult{}, xerrors.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	q := s.q.WithTx(tx)

	if err := q.LockPurgeMark(ctx); err != nil {
		return faucet.PurgeResult{}, xerrors.Errorf("lock purge mark: %w", err)
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

func (s *Store) StoredDeliveries(ctx context.Context, deliveryIDs []string) ([]string, error) {
	// pq.Array encodes a nil slice as NULL, which matches nothing.
	if len(deliveryIDs) == 0 {
		return []string{}, nil
	}
	stored, err := s.q.StoredDeliveries(ctx, deliveryIDs)
	if err != nil {
		return nil, xerrors.Errorf("select deliveries: %w", err)
	}
	return stored, nil
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
