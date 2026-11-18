package pgstore

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"golang.org/x/xerrors"

	"github.com/coder/faucet/faucet"
	"github.com/coder/faucet/faucet/pgstore/database"
)

// RecordRepository implements faucet.Store. A row of the same ID makes it
// write nothing but the owner's login.
func (s *Store) RecordRepository(ctx context.Context, repo faucet.Repository, seenAt time.Time) (err error) {
	defer func() { err = endedBy(ctx, err) }()
	if err := repo.Validate(); err != nil {
		return xerrors.Errorf("validate repository: %w", err)
	}
	return s.inRepositoryTx(ctx, faucet.Owner{ID: repo.OwnerID, Login: repo.OwnerLogin}, seenAt, func(q *database.Queries) error {
		if err := q.RecordRepository(ctx, database.RecordRepositoryParams{
			RepoID: repo.ID, OwnerID: repo.OwnerID, OwnerLogin: repo.OwnerLogin, FullName: repo.FullName, SeenAt: seenAt,
		}); err != nil {
			return xerrors.Errorf("record repository: %w", err)
		}
		return nil
	})
}

// RenameRepository implements faucet.Store.
func (s *Store) RenameRepository(ctx context.Context, repo faucet.Repository, from string, seenAt time.Time) (err error) {
	defer func() { err = endedBy(ctx, err) }()
	if err := repo.Validate(); err != nil {
		return xerrors.Errorf("validate repository: %w", err)
	}
	return s.inRepositoryTx(ctx, faucet.Owner{ID: repo.OwnerID, Login: repo.OwnerLogin}, seenAt, func(q *database.Queries) error {
		if err := q.RenameRepository(ctx, database.RenameRepositoryParams{
			RepoID: repo.ID, OwnerID: repo.OwnerID, OwnerLogin: repo.OwnerLogin, FullName: repo.FullName, SeenAt: seenAt,
			PreviousFullName: from,
		}); err != nil {
			return xerrors.Errorf("rename repository: %w", err)
		}
		return nil
	})
}

// RecordOwner implements faucet.Store.
func (s *Store) RecordOwner(ctx context.Context, owner faucet.Owner, seenAt time.Time) (err error) {
	defer func() { err = endedBy(ctx, err) }()
	if err := owner.Validate(); err != nil {
		return xerrors.Errorf("validate owner: %w", err)
	}
	return s.inRepositoryTx(ctx, owner, seenAt, func(*database.Queries) error { return nil })
}

// inRepositoryTx records the login of owner and then runs write, in one
// transaction. Another account that held the login gives it up first, so
// the unique login of faucet_owners never refuses the write.
func (s *Store) inRepositoryTx(ctx context.Context, owner faucet.Owner, seenAt time.Time, write func(*database.Queries) error) error {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return xerrors.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	q := s.q.WithTx(tx)
	if err := q.ReleaseOwnerLogin(ctx, database.ReleaseOwnerLoginParams{Login: owner.Login, OwnerID: owner.ID}); err != nil {
		return xerrors.Errorf("release the login of other owners: %w", err)
	}
	if err := q.RecordOwner(ctx, database.RecordOwnerParams{OwnerID: owner.ID, Login: owner.Login, SeenAt: seenAt}); err != nil {
		return xerrors.Errorf("record owner: %w", err)
	}
	if err := write(q); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return xerrors.Errorf("commit: %w", err)
	}
	return nil
}

// RepositoryByName implements faucet.Store.
func (s *Store) RepositoryByName(ctx context.Context, fullName string) (_ faucet.Repository, err error) {
	defer func() { err = endedBy(ctx, err) }()
	owner, name, ok := strings.Cut(fullName, "/")
	if !ok {
		return faucet.Repository{}, faucet.ErrUnknownRepository
	}
	row, err := s.q.GetRepositoryByName(ctx, database.GetRepositoryByNameParams{OwnerLogin: owner, Name: name})
	switch {
	case xerrors.Is(err, sql.ErrNoRows):
		return faucet.Repository{}, faucet.ErrUnknownRepository
	case err != nil:
		return faucet.Repository{}, xerrors.Errorf("read repository: %w", err)
	}
	return faucet.Repository{ID: row.RepoID, OwnerID: row.OwnerID, OwnerLogin: row.OwnerLogin, FullName: row.FullName}, nil
}

// OwnerByLogin implements faucet.Store.
func (s *Store) OwnerByLogin(ctx context.Context, login string) (_ faucet.Owner, err error) {
	defer func() { err = endedBy(ctx, err) }()
	row, err := s.q.GetOwnerByLogin(ctx, login)
	switch {
	case xerrors.Is(err, sql.ErrNoRows):
		return faucet.Owner{}, faucet.ErrUnknownOwner
	case err != nil:
		return faucet.Owner{}, xerrors.Errorf("read owner: %w", err)
	}
	return faucet.Owner{ID: row.OwnerID, Login: row.Login}, nil
}
