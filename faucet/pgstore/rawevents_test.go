package pgstore_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/coder/faucet/faucet"
	"github.com/coder/faucet/faucet/pgstore"
)

const rawTestRepo = 42

var rawTestLimits = faucet.RawLimits{Live: 100, PerTarget: 100, PerPrincipalTarget: 100}

func rawTestSubscription(n int, principal string, expiresAt time.Time) faucet.Subscription {
	url := fmt.Sprintf("https://hooks.example.test/raw/%d", n)
	args := json.RawMessage(`{"repo":"o/r"}`)
	return faucet.Subscription{
		KeyHash:     faucet.NewSubscriptionKeyHash(principal, url, "github.push", args),
		Principal:   principal,
		CallbackURL: url,
		EventType:   "github.push",
		EventTypes:  []string{"github.push"},
		Arguments:   args,
		RepoID:      rawTestRepo,
		SecretEnc:   []byte("sealed"),
		Cursor:      math.MaxInt64,
		ExpiresAt:   expiresAt,
	}
}

func rawTestLease(n byte, principal string, expiresAt time.Time) faucet.RawLease {
	lease := faucet.RawLease{
		Key:       faucet.NewSubscriptionKeyHash(principal, "", "github.push", json.RawMessage(`{"repo":"o/r"}`)),
		Principal: principal,
		Event:     "push",
		RepoID:    rawTestRepo,
		ExpiresAt: expiresAt,
	}
	lease.ID[15] = n
	return lease
}

// serveEvery gives the event to every interest that matches it.
func serveEvery(interests []faucet.RawInterest) faucet.RawPlan {
	plan := faucet.RawPlan{Body: []byte(`{"ref":"refs/heads/main"}`)}
	for _, in := range interests {
		if in.IsLease {
			plan.Unread = append(plan.Unread, in.Lease)
		} else {
			plan.Deliveries = append(plan.Deliveries, faucet.RawOutbox{Key: in.Subscription, Incarnation: in.Incarnation})
		}
	}
	return plan
}

func lockTables(t *testing.T, db *sql.DB, tables ...string) *sql.Tx {
	t.Helper()
	tx, err := db.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })
	for _, table := range tables {
		_, err = tx.Exec("LOCK TABLE " + table + " IN ACCESS EXCLUSIVE MODE") //nolint:gosec // The names are constants.
		require.NoError(t, err)
	}
	return tx
}

func countRows(t *testing.T, db *sql.DB, table string) (n int) {
	t.Helper()
	require.NoError(t, db.QueryRow("SELECT count(*) FROM "+table).Scan(&n)) //nolint:gosec // The names are constants.
	return n
}

// A delivery that ExpireRawDeliveries removes is counted by the call that
// removed it, even when the call fails after that, so that DeliverDue and
// Purge account for every delivery.
func TestExpireRawDeliveriesCountsEveryDeliveryThatLeavesTheQueue(t *testing.T) {
	t.Parallel()
	db := newDatabase(t, testDatabaseURL(t))
	store := pgstore.New(db)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	_, err := store.UpsertRawSubscription(ctx, rawTestSubscription(1, "octocat", now.Add(time.Minute)), now, rawTestLimits)
	require.NoError(t, err)
	_, err = store.IngestRaw(ctx, faucet.RawDelivery{DeliveryID: "d-1", Event: "push", RepoID: rawTestRepo}, now, serveEvery)
	require.NoError(t, err)
	require.Equal(t, 1, countRows(t, db, "faucet_raw_deliveries"))

	blocker := lockTables(t, db, "faucet_raw_gaps")
	later := now.Add(2 * time.Minute)
	cut, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	first, err := store.ExpireRawDeliveries(cut, later)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.NoError(t, blocker.Rollback())

	second, err := store.ExpireRawDeliveries(ctx, later)
	require.NoError(t, err)
	require.EqualValues(t, 1, first+second, "the delivery that left the queue is counted once")
	require.Zero(t, countRows(t, db, "faucet_raw_deliveries"))
}
