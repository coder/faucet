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
// unmerged migration cannot break the instances that serve production. Vercel
// sets VERCEL on every deployment, but VERCEL_ENV only when the project
// exposes its system environment variables, so a process on Vercel without
// VERCEL_ENV cannot tell that it is in production and must not migrate.
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
	deploymentOf := func(t *testing.T, vercel, vercelEnv string) deployment {
		t.Helper()
		cfg, err := loadConfig(func(name string) string {
			return map[string]string{
				envDatabaseURL: "postgres://localhost/faucet", envSecret: testSecret,
				envVercel: vercel, envVercelEnv: vercelEnv,
			}[name]
		})
		require.NoError(t, err)
		return cfg.deployment
	}

	for _, c := range []struct{ name, vercel, vercelEnv string }{
		{"NotOnVercel", "", ""},
		{"Production", "1", "production"},
	} {
		t.Run("Migrates/"+c.name, func(t *testing.T) {
			t.Parallel()
			db := newScratchDatabase(t, adminURL)
			require.NoError(t, prepareSchema(ctx, db, db, deploymentOf(t, c.vercel, c.vercelEnv)))
			require.NoError(t, pgstore.CheckSchema(ctx, db))
		})
	}
	for _, c := range []struct{ name, vercel, vercelEnv, want string }{
		{"Preview", "1", "preview", "VERCEL_ENV=preview"},
		{"Development", "1", "development", "VERCEL_ENV=development"},
		{"PreviewWithoutVercel", "", "preview", "VERCEL_ENV=preview"},
		{"OnVercelWithoutVercelEnv", "1", "", "VERCEL is set and VERCEL_ENV is not"},
	} {
		t.Run("RefusesToMigrate/"+c.name, func(t *testing.T) {
			t.Parallel()
			db := newScratchDatabase(t, adminURL)
			err := prepareSchema(ctx, db, db, deploymentOf(t, c.vercel, c.vercelEnv))
			require.ErrorIs(t, err, pgstore.ErrSchemaNotCurrent)
			require.ErrorContains(t, err, c.want)
			require.False(t, hasMigrationsTable(t, db), "the process changed the schema")
		})
	}
	t.Run("ServesAMigratedDatabase/OnVercelWithoutVercelEnv", func(t *testing.T) {
		t.Parallel()
		db := newScratchDatabase(t, adminURL)
		require.NoError(t, pgstore.Migrate(ctx, db))
		require.NoError(t, prepareSchema(ctx, db, db, deploymentOf(t, "1", "")))
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
