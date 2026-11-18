package pgstore

import (
	"context"
	"database/sql"
	"time"

	"golang.org/x/xerrors"

	"github.com/coder/faucet/faucet/oauth"
	"github.com/coder/faucet/faucet/pgstore/database"
)

var _ oauth.AccessCheckStore = (*Store)(nil)

// FreshAccessCheck implements oauth.AccessCheckStore in one statement, which
// reads the session's refreshed time and moves the check's last use
// together.
func (s *Store) FreshAccessCheck(ctx context.Context, sessionID, repo string, now time.Time) (_ int64, _ bool, err error) {
	defer func() { err = endedBy(ctx, err) }()
	id, err := s.q.FreshAccessCheck(ctx, database.FreshAccessCheckParams{Now: now, SessionID: sessionID, Repo: repo})
	switch {
	case xerrors.Is(err, sql.ErrNoRows):
		return 0, false, nil
	case err != nil:
		return 0, false, xerrors.Errorf("use access check: %w", err)
	}
	return id, true, nil
}

// StoreAccessCheck implements oauth.AccessCheckStore in one statement.
func (s *Store) StoreAccessCheck(ctx context.Context, c oauth.AccessCheck) (err error) {
	defer func() { err = endedBy(ctx, err) }()
	if err := c.Validate(); err != nil {
		return xerrors.Errorf("validate access check: %w", err)
	}
	n, err := s.q.StoreAccessCheck(ctx, database.StoreAccessCheckParams{
		Repo:         c.Repo,
		RepoID:       c.RepoID,
		CheckedAt:    c.CheckedAt,
		LastUsedAt:   c.LastUsedAt,
		ConnectionID: c.ConnectionID,
	})
	switch {
	case err != nil:
		return xerrors.Errorf("store access check: %w", err)
	case n == 0:
		return oauth.ErrUnknownConnection
	}
	return nil
}

// DeleteAccessCheck implements oauth.AccessCheckStore.
func (s *Store) DeleteAccessCheck(ctx context.Context, connectionID, repo string) (err error) {
	defer func() { err = endedBy(ctx, err) }()
	if err := s.q.DeleteAccessCheck(ctx, database.DeleteAccessCheckParams{ConnectionID: connectionID, Repo: repo}); err != nil {
		return xerrors.Errorf("delete access check: %w", err)
	}
	return nil
}

// RecentAccessChecks implements oauth.AccessCheckStore.
func (s *Store) RecentAccessChecks(ctx context.Context, connectionID string, limit int) (_ []oauth.AccessCheck, err error) {
	defer func() { err = endedBy(ctx, err) }()
	rows, err := s.q.RecentAccessChecks(ctx, database.RecentAccessChecksParams{ConnectionID: connectionID, MaxChecks: int64(max(limit, 0))})
	if err != nil {
		return nil, xerrors.Errorf("read access checks: %w", err)
	}
	checks := make([]oauth.AccessCheck, len(rows))
	for i, row := range rows {
		checks[i] = oauth.AccessCheck{
			ConnectionID: row.ConnectionID,
			Repo:         row.Repo,
			RepoID:       row.RepoID,
			CheckedAt:    row.CheckedAt,
			LastUsedAt:   row.LastUsedAt,
		}
	}
	return checks, nil
}
