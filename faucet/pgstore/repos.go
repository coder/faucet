package pgstore

import (
	"context"
	"database/sql"
	"time"

	"golang.org/x/xerrors"

	"github.com/coder/faucet/faucet"
	"github.com/coder/faucet/faucet/pgstore/database"
)

// RecordRepository implements faucet.Store. A row of the same ID makes it
// write nothing.
func (s *Store) RecordRepository(ctx context.Context, repo faucet.Repository, seenAt time.Time) (err error) {
	defer func() { err = endedBy(ctx, err) }()
	if err := repo.Validate(); err != nil {
		return xerrors.Errorf("validate repository: %w", err)
	}
	if err := s.q.RecordRepository(ctx, database.RecordRepositoryParams(repositoryRow(repo, seenAt))); err != nil {
		return xerrors.Errorf("record repository: %w", err)
	}
	return nil
}

// RenameRepository implements faucet.Store.
func (s *Store) RenameRepository(ctx context.Context, repo faucet.Repository, seenAt time.Time) (err error) {
	defer func() { err = endedBy(ctx, err) }()
	if err := repo.Validate(); err != nil {
		return xerrors.Errorf("validate repository: %w", err)
	}
	if err := s.q.RenameRepository(ctx, repositoryRow(repo, seenAt)); err != nil {
		return xerrors.Errorf("rename repository: %w", err)
	}
	return nil
}

func repositoryRow(repo faucet.Repository, seenAt time.Time) database.RenameRepositoryParams {
	return database.RenameRepositoryParams{
		RepoID: repo.ID, OwnerID: repo.OwnerID, OwnerLogin: repo.OwnerLogin, FullName: repo.FullName, SeenAt: seenAt,
	}
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
