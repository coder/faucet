// Package pgstore implements faucet.Store on Postgres.
package pgstore

import (
	"context"
	"database/sql"

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
