package faucet_test

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
)

func postgresDB(t *testing.T) *sql.DB {
	t.Helper()
	adminURL := os.Getenv("FAUCET_TEST_DATABASE_URL")
	if adminURL == "" {
		if os.Getenv("CI") != "" {
			t.Fatal("FAUCET_TEST_DATABASE_URL must be set in CI")
		}
		t.Skip("set FAUCET_TEST_DATABASE_URL to a Postgres URL")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	suffix := make([]byte, 8)
	_, err := rand.Read(suffix)
	require.NoError(t, err)
	name := "faucet_test_" + hex.EncodeToString(suffix)
	admin, err := sql.Open("postgres", adminURL)
	require.NoError(t, err)
	t.Cleanup(func() { _ = admin.Close() })
	_, err = admin.ExecContext(ctx, "CREATE DATABASE "+pq.QuoteIdentifier(name))
	require.NoError(t, err)
	u, err := url.Parse(adminURL)
	require.NoError(t, err)
	u.Path = "/" + name
	db, err := sql.Open("postgres", u.String())
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = db.Close()
		_, err := admin.ExecContext(context.Background(), "DROP DATABASE "+pq.QuoteIdentifier(name)+" WITH (FORCE)")
		require.NoError(t, err)
	})
	require.NoError(t, pgstore.Migrate(ctx, db))
	return db
}

// holdSubscriptionsLocked keeps faucet_subscriptions locked from another
// connection, so that a statement on it waits, and returns the function that
// reports whether a statement is waiting.
func holdSubscriptionsLocked(t *testing.T, db *sql.DB) (waiting func() bool) {
	t.Helper()
	tx, err := db.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })
	_, err = tx.Exec("LOCK TABLE faucet_subscriptions IN ACCESS EXCLUSIVE MODE")
	require.NoError(t, err)
	return func() bool {
		var n int
		require.NoError(t, db.QueryRow("SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock'").Scan(&n))
		return n > 0
	}
}

func TestDeliverDueOnPostgresIsIncompleteNotFailedWhenTheRunEndsDuringAStoreCall(t *testing.T) {
	t.Parallel()

	db := postgresDB(t)
	d := newDeliverHarness(t, func(deps *faucet.Deps) { deps.Store = pgstore.New(db) })
	d.ledgerSkip = true
	d.subscribeEndpoint(d.endpoint("/hook"))
	waiting := holdSubscriptionsLocked(t, db)
	ctx, cancel := context.WithCancel(d.ctx)
	defer cancel()
	type outcome struct {
		res faucet.DeliverResult
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := d.svc.DeliverDue(ctx)
		done <- outcome{res, err}
	}()
	require.Eventually(t, waiting, 5*time.Second, 10*time.Millisecond, "the run's first statement waits on the lock")

	cancel()
	got := <-done
	require.NoError(t, got.err, "a statement that the run's own end cut short is no store failure")
	require.True(t, got.res.Incomplete)
}
