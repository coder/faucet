package pgstore_test

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/lib/pq"
	"github.com/stretchr/testify/require"

	"github.com/coder/faucet/faucet"
	"github.com/coder/faucet/faucet/pgstore"
	"github.com/coder/faucet/faucet/storetest"
)

const (
	databaseURLEnv = "FAUCET_TEST_DATABASE_URL"
	maxTestConns   = 10
)

func TestStore(t *testing.T) {
	t.Parallel()
	adminURL := testDatabaseURL(t)

	storetest.Run(t, func(t *testing.T) faucet.Store {
		return pgstore.New(newDatabase(t, adminURL))
	})
}

func TestRedeliveryStore(t *testing.T) {
	t.Parallel()
	adminURL := testDatabaseURL(t)

	storetest.RunRedelivery(t, func(t *testing.T) storetest.RedeliveryStore {
		return pgstore.New(newDatabase(t, adminURL))
	})
}

// A transaction-mode pooler such as Neon's pooled URL breaks lib/pq's default
// parse-then-bind pair of batches, and binary_parameters=yes avoids it by
// sending each query in one batch. That mode sends every []byte parameter in
// binary, which a jsonb parameter cannot take, so the whole suite runs in it.
func TestStoreOverBinaryParameters(t *testing.T) {
	t.Parallel()
	adminURL := withBinaryParameters(t, testDatabaseURL(t))

	storetest.Run(t, func(t *testing.T) faucet.Store {
		return pgstore.New(newDatabase(t, adminURL))
	})
}

func TestRedeliveryStoreOverBinaryParameters(t *testing.T) {
	t.Parallel()
	adminURL := withBinaryParameters(t, testDatabaseURL(t))

	storetest.RunRedelivery(t, func(t *testing.T) storetest.RedeliveryStore {
		return pgstore.New(newDatabase(t, adminURL))
	})
}

func TestMigrateLeavesHostMigrationsTableAlone(t *testing.T) {
	t.Parallel()
	adminURL := testDatabaseURL(t)
	db := createDatabase(t, adminURL)
	ctx := context.Background()

	_, err := db.ExecContext(ctx, `CREATE TABLE schema_migrations (version bigint NOT NULL PRIMARY KEY, dirty boolean NOT NULL)`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO schema_migrations VALUES (400, false)`)
	require.NoError(t, err)

	require.NoError(t, pgstore.Migrate(ctx, db))

	var hostVersion int64
	require.NoError(t, db.QueryRowContext(ctx, `SELECT version FROM schema_migrations`).Scan(&hostVersion))
	require.EqualValues(t, 400, hostVersion)
	var faucetVersion int64
	require.NoError(t, db.QueryRowContext(ctx, `SELECT version FROM `+pq.QuoteIdentifier(pgstore.MigrationsTable)).Scan(&faucetVersion))
	require.EqualValues(t, 7, faucetVersion)
}

func testDatabaseURL(t *testing.T) string {
	t.Helper()
	adminURL := os.Getenv(databaseURLEnv)
	if adminURL == "" {
		if os.Getenv("CI") != "" {
			t.Fatalf("%s must be set in CI", databaseURLEnv)
		}
		t.Skipf("set %s to a Postgres URL, for example postgres://postgres@127.0.0.1:5432/postgres?sslmode=disable", databaseURLEnv)
	}
	return adminURL
}

func withBinaryParameters(t *testing.T, rawURL string) string {
	t.Helper()
	u, err := url.Parse(rawURL)
	require.NoError(t, err)
	q := u.Query()
	q.Set("binary_parameters", "yes")
	u.RawQuery = q.Encode()
	return u.String()
}

func newDatabase(t *testing.T, adminURL string) *sql.DB {
	t.Helper()
	db := createDatabase(t, adminURL)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	require.NoError(t, pgstore.Migrate(ctx, db))
	return db
}

func createDatabase(t *testing.T, adminURL string) *sql.DB {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	suffix := make([]byte, 8)
	_, err := rand.Read(suffix)
	require.NoError(t, err)
	name := "faucet_test_" + hex.EncodeToString(suffix)

	u, err := url.Parse(adminURL)
	require.NoError(t, err)
	admin, err := sql.Open("postgres", adminURL)
	require.NoError(t, err)
	t.Cleanup(func() { _ = admin.Close() })

	_, err = admin.ExecContext(ctx, "CREATE DATABASE "+pq.QuoteIdentifier(name))
	require.NoError(t, err)

	u.Path = "/" + name
	db, err := sql.Open("postgres", u.String())
	require.NoError(t, err)
	// The subtests run in parallel and several start a dozen callers. Without a cap they can pass the server's 100 connections.
	db.SetMaxOpenConns(maxTestConns)
	t.Cleanup(func() {
		_ = db.Close()
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer dropCancel()
		_, err := admin.ExecContext(dropCtx, "DROP DATABASE "+pq.QuoteIdentifier(name)+" WITH (FORCE)")
		require.NoError(t, err)
	})

	return db
}

func TestCheckSchemaAcceptsOnlyTheNewestVersionAndWritesNothing(t *testing.T) {
	t.Parallel()
	adminURL := testDatabaseURL(t)
	ctx := context.Background()
	setVersion := func(t *testing.T, db *sql.DB, version int, dirty bool) {
		t.Helper()
		_, err := db.ExecContext(ctx, `DELETE FROM `+pgstore.MigrationsTable) //nolint:gosec // The table name is a package constant.
		require.NoError(t, err)
		_, err = db.ExecContext(ctx, `INSERT INTO `+pgstore.MigrationsTable+` VALUES ($1, $2)`, version, dirty) //nolint:gosec // The table name is a package constant.
		require.NoError(t, err)
	}
	hasTable := func(t *testing.T, db *sql.DB) (exists bool) {
		t.Helper()
		require.NoError(t, db.QueryRowContext(ctx, `SELECT to_regclass($1::text) IS NOT NULL`, pgstore.MigrationsTable).Scan(&exists))
		return exists
	}

	t.Run("NoDatabaseSchema", func(t *testing.T) {
		t.Parallel()
		db := createDatabase(t, adminURL)
		require.ErrorIs(t, pgstore.CheckSchema(ctx, db), pgstore.ErrSchemaNotCurrent)
		require.False(t, hasTable(t, db), "the check created the migrations table")
	})
	t.Run("Migrated", func(t *testing.T) {
		t.Parallel()
		db := newDatabase(t, adminURL)
		require.NoError(t, pgstore.CheckSchema(ctx, db))
	})
	t.Run("Behind", func(t *testing.T) {
		t.Parallel()
		db := newDatabase(t, adminURL)
		setVersion(t, db, 5, false)
		require.ErrorIs(t, pgstore.CheckSchema(ctx, db), pgstore.ErrSchemaNotCurrent)
	})
	t.Run("Ahead", func(t *testing.T) {
		t.Parallel()
		db := newDatabase(t, adminURL)
		setVersion(t, db, 99, false)
		require.ErrorIs(t, pgstore.CheckSchema(ctx, db), pgstore.ErrSchemaNotCurrent)
	})
	t.Run("FailedMigration", func(t *testing.T) {
		t.Parallel()
		db := newDatabase(t, adminURL)
		setVersion(t, db, 6, true)
		require.ErrorIs(t, pgstore.CheckSchema(ctx, db), pgstore.ErrSchemaNotCurrent)
	})
}

func TestStoreCallThatItsContextCutShortWrapsTheContextError(t *testing.T) {
	t.Parallel()
	db := newDatabase(t, testDatabaseURL(t))
	store := pgstore.New(db)
	tx, err := db.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })
	_, err = tx.Exec("LOCK TABLE faucet_subscriptions IN ACCESS EXCLUSIVE MODE")
	require.NoError(t, err)

	for name, c := range map[string]struct {
		newContext func() (context.Context, context.CancelFunc)
		want       error
	}{
		"canceled": {func() (context.Context, context.CancelFunc) {
			ctx, cancel := context.WithCancel(context.Background())
			time.AfterFunc(300*time.Millisecond, cancel)
			return ctx, cancel
		}, context.Canceled},
		"deadline": {func() (context.Context, context.CancelFunc) {
			return context.WithTimeout(context.Background(), 300*time.Millisecond)
		}, context.DeadlineExceeded},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := c.newContext()
			defer cancel()
			_, err := store.ScanTargets(ctx, time.Now(), 10, 10)
			require.ErrorIs(t, err, c.want, "the statement waits on the lock until the context ends it")
		})
	}
}
