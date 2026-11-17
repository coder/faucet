package pgstore

import (
	"context"
	"database/sql"
	"time"

	"golang.org/x/xerrors"

	"github.com/coder/faucet/faucet"
	"github.com/coder/faucet/faucet/pgstore/database"
)

// UpsertRepository implements faucet.Store.
func (s *Store) UpsertRepository(ctx context.Context, repo faucet.Repository, seenAt time.Time) (err error) {
	defer func() { err = endedBy(ctx, err) }()
	if err := repo.Validate(); err != nil {
		return xerrors.Errorf("validate repository: %w", err)
	}
	if err := s.q.UpsertRepository(ctx, database.UpsertRepositoryParams{
		RepoID: repo.ID, OwnerID: repo.OwnerID, OwnerLogin: repo.OwnerLogin, FullName: repo.FullName, SeenAt: seenAt,
	}); err != nil {
		return xerrors.Errorf("upsert repository: %w", err)
	}
	return nil
}

// RepositoryByName implements faucet.Store.
func (s *Store) RepositoryByName(ctx context.Context, fullName string) (_ faucet.Repository, err error) {
	defer func() { err = endedBy(ctx, err) }()
	row, err := s.q.GetRepositoryByName(ctx, fullName)
	switch {
	case xerrors.Is(err, sql.ErrNoRows):
		return faucet.Repository{}, faucet.ErrUnknownRepository
	case err != nil:
		return faucet.Repository{}, xerrors.Errorf("read repository: %w", err)
	}
	return faucet.Repository{ID: row.RepoID, OwnerID: row.OwnerID, OwnerLogin: row.OwnerLogin, FullName: row.FullName}, nil
}
