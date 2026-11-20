package pgstore

import (
	"context"
	"database/sql"
	"time"

	"golang.org/x/xerrors"

	"github.com/coder/superblink-events/faucet/oauth"
	"github.com/coder/superblink-events/faucet/pgstore/database"
)

var _ oauth.AccessCheckStore = (*Store)(nil)

// FreshAccessCheck implements oauth.AccessCheckStore in one statement, which
// reads the session's refreshed time and the check, and writes the check
// only when its last use moves.
func (s *Store) FreshAccessCheck(ctx context.Context, sessionID, repo string, now, usedBefore time.Time) (_ int64, _ bool, err error) {
	defer func() { err = endedBy(ctx, err) }()
	id, err := s.q.FreshAccessCheck(ctx, database.FreshAccessCheckParams{SessionID: sessionID, Repo: repo, Now: now, UsedBefore: usedBefore})
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

// RecentAccessChecks implements oauth.AccessCheckStore.
func (s *Store) RecentAccessChecks(ctx context.Context, connectionID string, usedSince time.Time, limit int) (_ []oauth.AccessCheck, err error) {
	defer func() { err = endedBy(ctx, err) }()
	rows, err := s.q.RecentAccessChecks(ctx, database.RecentAccessChecksParams{ConnectionID: connectionID, UsedSince: usedSince, MaxChecks: int64(max(limit, 0))})
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

// DeleteUnusedAccessChecks implements oauth.AccessCheckStore.
func (s *Store) DeleteUnusedAccessChecks(ctx context.Context, usedBefore, deniedBefore time.Time) (_ int64, err error) {
	defer func() { err = endedBy(ctx, err) }()
	n, err := s.q.DeleteUnusedAccessChecks(ctx, database.DeleteUnusedAccessChecksParams{UsedBefore: usedBefore, DeniedBefore: deniedBefore})
	if err != nil {
		return 0, xerrors.Errorf("delete unused access checks: %w", err)
	}
	return n, nil
}
