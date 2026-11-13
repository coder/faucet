package pgstore

import (
	"context"
	"database/sql"
	"embed"
	"errors"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"golang.org/x/xerrors"
)

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
	driver, err := postgres.WithConnection(ctx, conn, &postgres.Config{})
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
