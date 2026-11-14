package pgstore

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"io/fs"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"golang.org/x/xerrors"
)

// MigrationsTable records faucet's schema version. It is not golang-migrate's
// default name, so faucet can share a database with a host such as coderd,
// which already uses schema_migrations.
const MigrationsTable = "faucet_schema_migrations"

//go:embed migrations/*.sql
var migrations embed.FS

// Migrate applies every pending schema migration to db.
//
// ctx covers only the setup. golang-migrate cannot stop a running migration.
func Migrate(ctx context.Context, db *sql.DB) (err error) {
	src, err := iofs.New(migrations, "migrations")
	if err != nil {
		return xerrors.Errorf("open migrations: %w", err)
	}

	// A dedicated connection keeps migrate from closing db when it finishes.
	conn, err := db.Conn(ctx)
	if err != nil {
		_ = src.Close()
		return xerrors.Errorf("acquire connection: %w", err)
	}
	driver, err := postgres.WithConnection(ctx, conn, &postgres.Config{MigrationsTable: MigrationsTable})
	if err != nil {
		_ = conn.Close()
		_ = src.Close()
		return xerrors.Errorf("create migrate driver: %w", err)
	}

	m, err := migrate.NewWithInstance("iofs", src, "postgres", driver)
	if err != nil {
		_ = driver.Close()
		_ = src.Close()
		return xerrors.Errorf("create migrator: %w", err)
	}
	defer func() {
		srcErr, dbErr := m.Close()
		err = errors.Join(err, srcErr, dbErr)
	}()

	if err := m.Up(); err != nil && !xerrors.Is(err, migrate.ErrNoChange) {
		return xerrors.Errorf("apply migrations: %w", err)
	}
	return nil
}

// ErrSchemaNotCurrent means that the database is not at the newest migration
// that this build embeds: it is behind, ahead, or marked dirty by a migration
// that failed.
var ErrSchemaNotCurrent = errors.New("database schema is not at the version of this build")

// CheckSchema returns ErrSchemaNotCurrent unless db is at the newest embedded
// migration. It only reads: unlike Migrate it creates no table, so a process
// that must not change the schema can call it.
func CheckSchema(ctx context.Context, db *sql.DB) error {
	want, err := newestVersion()
	if err != nil {
		return err
	}

	var exists bool
	if err := db.QueryRowContext(ctx, `SELECT to_regclass($1::text) IS NOT NULL`, MigrationsTable).Scan(&exists); err != nil {
		return xerrors.Errorf("look for the migrations table: %w", err)
	}
	var have int64
	var dirty bool
	if exists {
		err := db.QueryRowContext(ctx, `SELECT version, dirty FROM `+MigrationsTable).Scan(&have, &dirty)
		if err != nil && !xerrors.Is(err, sql.ErrNoRows) {
			return xerrors.Errorf("read the schema version: %w", err)
		}
	}
	switch {
	case dirty:
		return xerrors.Errorf("%w: version %d is dirty, a migration failed", ErrSchemaNotCurrent, have)
	case have != int64(want):
		return xerrors.Errorf("%w: it is at version %d and this build needs %d", ErrSchemaNotCurrent, have, want)
	}
	return nil
}

func newestVersion() (uint, error) {
	src, err := iofs.New(migrations, "migrations")
	if err != nil {
		return 0, xerrors.Errorf("open migrations: %w", err)
	}
	defer func() { _ = src.Close() }()
	version, err := src.First()
	if err != nil {
		return 0, xerrors.Errorf("read the first migration: %w", err)
	}
	for {
		next, err := src.Next(version)
		if xerrors.Is(err, fs.ErrNotExist) {
			return version, nil
		}
		if err != nil {
			return 0, xerrors.Errorf("read the migration after %d: %w", version, err)
		}
		version = next
	}
}
