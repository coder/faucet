package faucet_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/coder/superblink-events/faucet"
	"github.com/coder/superblink-events/faucet/internal/pgtest"
	"github.com/coder/superblink-events/faucet/pgstore"
)

func postgresDB(t *testing.T) *sql.DB {
	t.Helper()
	return pgtest.NewDB(t, pgstore.Migrate)
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
