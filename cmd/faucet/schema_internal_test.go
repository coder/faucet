package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"net/url"
	"os"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/coder/faucet/cmd/faucet/internal/dbconn"
	"github.com/coder/faucet/faucet/pgstore"
)

const testDatabaseURLEnv = "FAUCET_TEST_DATABASE_URL"

// A Vercel preview runs the code of any branch and can reach the same database
// as production. Only production may change the schema, so a deployment of an
// unmerged migration cannot break the instances that serve production.
func TestPrepareSchemaMigratesOnlyInProduction(t *testing.T) {
	t.Parallel()
	adminURL := os.Getenv(testDatabaseURLEnv)
	if adminURL == "" {
		if os.Getenv("CI") != "" {
			t.Fatalf("%s must be set in CI", testDatabaseURLEnv)
		}
		t.Skipf("set %s to a Postgres URL", testDatabaseURLEnv)
	}
	ctx := context.Background()
	hasMigrationsTable := func(t *testing.T, db *sql.DB) (exists bool) {
		t.Helper()
		require.NoError(t, db.QueryRowContext(ctx, `SELECT to_regclass($1::text) IS NOT NULL`, pgstore.MigrationsTable).Scan(&exists))
		return exists
	}

	for _, vercelEnv := range []string{"", "production"} {
		t.Run("Migrates/"+vercelEnv, func(t *testing.T) {
			t.Parallel()
			db := newScratchDatabase(t, adminURL)
			require.NoError(t, prepareSchema(ctx, db, db, vercelEnv))
			require.NoError(t, pgstore.CheckSchema(ctx, db))
		})
	}
	for _, vercelEnv := range []string{"preview", "development"} {
		t.Run("RefusesToMigrate/"+vercelEnv, func(t *testing.T) {
			t.Parallel()
			db := newScratchDatabase(t, adminURL)
			err := prepareSchema(ctx, db, db, vercelEnv)
			require.ErrorIs(t, err, pgstore.ErrSchemaNotCurrent)
			require.ErrorContains(t, err, envVercel+"="+vercelEnv)
			require.False(t, hasMigrationsTable(t, db), "the process changed the schema")
		})
	}
	t.Run("ServesAMigratedDatabase/preview", func(t *testing.T) {
		t.Parallel()
		db := newScratchDatabase(t, adminURL)
		require.NoError(t, pgstore.Migrate(ctx, db))
		require.NoError(t, prepareSchema(ctx, db, db, "preview"))
	})
}

// The requests of a deployment go through a pooler, and the migrations go
// around it, so the schema must land in the database that migrateURL names.
func TestMigrateSchemaMigratesOverTheMigrationURL(t *testing.T) {
	t.Parallel()
	adminURL := os.Getenv(testDatabaseURLEnv)
	if adminURL == "" {
		if os.Getenv("CI") != "" {
			t.Fatalf("%s must be set in CI", testDatabaseURLEnv)
		}
		t.Skipf("set %s to a Postgres URL", testDatabaseURLEnv)
	}
	ctx := context.Background()
	served := newScratchDatabase(t, adminURL)
	migrateURL := newScratchDatabaseURL(t, adminURL)

	require.NoError(t, migrateSchema(ctx, config{migrateURL: migrateURL, migrateVar: envDatabaseURLUnpooled}, served))

	migrated, err := dbconn.Open(migrateURL)
	require.NoError(t, err)
	t.Cleanup(func() { _ = migrated.Close() })
	require.NoError(t, pgstore.CheckSchema(ctx, migrated))
	require.ErrorIs(t, pgstore.CheckSchema(ctx, served), pgstore.ErrSchemaNotCurrent, "the migration ran over the serving connection")
}

func newScratchDatabase(t *testing.T, adminURL string) *sql.DB {
	t.Helper()
	db, err := dbconn.Open(newScratchDatabaseURL(t, adminURL))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func newScratchDatabaseURL(t *testing.T, adminURL string) string {
	t.Helper()
	ctx := context.Background()

	suffix := make([]byte, 8)
	_, err := rand.Read(suffix)
	require.NoError(t, err)
	name := "faucet_test_" + hex.EncodeToString(suffix)

	admin, err := dbconn.Open(adminURL)
	require.NoError(t, err)
	t.Cleanup(func() { _ = admin.Close() })
	_, err = admin.ExecContext(ctx, "CREATE DATABASE "+name)
	require.NoError(t, err)

	u, err := url.Parse(adminURL)
	require.NoError(t, err)
	u.Path = "/" + name
	t.Cleanup(func() {
		_, err := admin.ExecContext(ctx, "DROP DATABASE "+name+" WITH (FORCE)")
		require.NoError(t, err)
	})
	return u.String()
}
