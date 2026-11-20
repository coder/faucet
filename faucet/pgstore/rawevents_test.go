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

	"github.com/coder/superblink-events/faucet"
	"github.com/coder/superblink-events/faucet/pgstore"
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
		Target:      faucet.RepoTarget(rawTestRepo),
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
		Target:    faucet.RepoTarget(rawTestRepo),
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

// A poll holds its lease's row until it commits. An open that needs to end a
// lease to keep a limit waits for it rather than pass it over, so the limit
// holds while a lease is being polled, and it then ends the lease polled
// longest ago as the poll left them.
func TestOpenRawLeaseKeepsItsLimitsAndEndsTheOldestLeaseWhileOneIsPolled(t *testing.T) {
	t.Parallel()
	for name, limits := range map[string]faucet.RawLimits{
		"the leases of a principal":             {Live: 2, PerTarget: 100, PerPrincipalTarget: 100},
		"the leases of a principal of an event": {Live: 100, PerTarget: 100, PerPrincipalTarget: 2},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			db := newDatabase(t, testDatabaseURL(t))
			store := pgstore.New(db)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			now := time.Now().UTC()
			first, second := rawTestLease(1, "octocat", now.Add(time.Hour)), rawTestLease(2, "octocat", now.Add(time.Hour))
			for i, lease := range []faucet.RawLease{first, second} {
				_, err := store.OpenRawLease(ctx, lease, now.Add(time.Duration(i)*time.Second), limits)
				require.NoError(t, err)
			}

			poll, err := db.BeginTx(ctx, nil)
			require.NoError(t, err)
			defer func() { _ = poll.Rollback() }()
			_, err = poll.ExecContext(ctx, `UPDATE faucet_raw_leases SET polled_at = $1 WHERE id = $2`, now.Add(5*time.Second), first.ID[:])
			require.NoError(t, err)
			opened := make(chan error, 1)
			third := rawTestLease(3, "octocat", now.Add(time.Hour))
			go func() {
				_, err := store.OpenRawLease(ctx, third, now.Add(6*time.Second), limits)
				opened <- err
			}()
			time.Sleep(300 * time.Millisecond)
			require.NoError(t, poll.Commit())
			require.NoError(t, <-opened)

			rows, err := db.QueryContext(ctx, `SELECT id FROM faucet_raw_leases ORDER BY id`)
			require.NoError(t, err)
			defer func() { _ = rows.Close() }()
			var kept [][]byte
			for rows.Next() {
				var id []byte
				require.NoError(t, rows.Scan(&id))
				kept = append(kept, id)
			}
			require.NoError(t, rows.Err())
			require.Equal(t, [][]byte{first.ID[:], third.ID[:]}, kept,
				"the open waited for the poll, then ended the second lease, which the poll left as the one polled longest ago")
		})
	}
}

func TestRawStoreCallThatItsContextCutShortWrapsTheContextError(t *testing.T) {
	t.Parallel()
	db := newDatabase(t, testDatabaseURL(t))
	store := pgstore.New(db)
	lockTables(t, db, "faucet_subscriptions", "faucet_raw_events", "faucet_raw_deliveries", "faucet_raw_gaps", "faucet_raw_leases", "faucet_raw_unread")

	now := time.Now().UTC()
	sub := rawTestSubscription(1, "octocat", now.Add(time.Hour))
	lease := rawTestLease(1, "octocat", now.Add(time.Hour))
	for name, call := range map[string]func(context.Context) error{
		"IngestRaw": func(ctx context.Context) error {
			_, err := store.IngestRaw(ctx, faucet.RawDelivery{DeliveryID: "d", Event: "push", RepoID: rawTestRepo}, now, serveEvery)
			return err
		},
		"RawHead": func(ctx context.Context) error {
			_, err := store.RawHead(ctx)
			return err
		},
		"UpsertRawSubscription": func(ctx context.Context) error {
			_, err := store.UpsertRawSubscription(ctx, sub, now, rawTestLimits)
			return err
		},
		"OpenRawLease": func(ctx context.Context) error {
			_, err := store.OpenRawLease(ctx, lease, now, rawTestLimits)
			return err
		},
		"ReadRawLease": func(ctx context.Context) error {
			_, _, err := store.ReadRawLease(ctx, faucet.RawRead{
				Lease: lease.ID, Key: lease.Key, Target: faucet.RepoTarget(rawTestRepo), Limit: 10, MaxBytes: 1 << 20, Now: now, ExpiresAt: now.Add(time.Hour),
			})
			return err
		},
		"ClaimRawDelivery": func(ctx context.Context) error {
			_, _, err := store.ClaimRawDelivery(ctx, now, time.Minute, nil)
			return err
		},
		"FinishRawDelivery": func(ctx context.Context) error {
			_, err := store.FinishRawDelivery(ctx, faucet.DeliveryRef{Key: sub.KeyHash, EventID: 1, Attempt: 1, Incarnation: now}, now, faucet.Finish{Kind: faucet.FinishDelivered})
			return err
		},
		"ClaimRawGap": func(ctx context.Context) error {
			_, _, err := store.ClaimRawGap(ctx, now, time.Minute, nil)
			return err
		},
		"FinishRawGap": func(ctx context.Context) error {
			return store.FinishRawGap(ctx, faucet.GapRef{Key: sub.KeyHash, Incarnation: now, Through: 1})
		},
		"ExpireRawDeliveries": func(ctx context.Context) error {
			_, err := store.ExpireRawDeliveries(ctx, now)
			return err
		},
		"PurgeRawEvents": func(ctx context.Context) error {
			_, err := store.PurgeRawEvents(ctx, faucet.RawPurge{Now: now})
			return err
		},
		"RawWants": func(ctx context.Context) error {
			_, err := store.RawWants(ctx, now)
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
			defer cancel()
			require.ErrorIs(t, call(ctx), context.DeadlineExceeded, "the statement waits on the lock until the context ends it")
		})
	}
}

// Raw IDs become visible in commit order, and the received_at of a raw event
// is taken at its insert, under the lock, so a higher ID never carries an
// earlier time even when its ingest waited for the lock.
func TestIngestRawIsStampedWhenItIsWrittenNotWhenItsTransactionStarts(t *testing.T) {
	t.Parallel()
	db := newDatabase(t, testDatabaseURL(t))
	store := pgstore.New(db)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	now := time.Now().UTC()
	_, err := store.OpenRawLease(ctx, rawTestLease(1, "octocat", now.Add(time.Hour)), now, rawTestLimits)
	require.NoError(t, err)

	blocker := lockTables(t, db, "faucet_raw_events")
	ingested := make(chan error, 2)
	for _, delivery := range []string{"first", "second"} {
		go func() {
			_, err := store.IngestRaw(ctx, faucet.RawDelivery{DeliveryID: delivery, Event: "push", RepoID: rawTestRepo}, now, serveEvery)
			ingested <- err
		}()
	}
	time.Sleep(400 * time.Millisecond)
	released := time.Now()
	require.NoError(t, blocker.Commit())
	for range 2 {
		require.NoError(t, <-ingested)
	}

	rows, err := db.QueryContext(ctx, `SELECT id, received_at FROM faucet_raw_events ORDER BY id`)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	var stamps []time.Time
	for rows.Next() {
		var id int64
		var at time.Time
		require.NoError(t, rows.Scan(&id, &at))
		require.True(t, at.After(released.Add(-100*time.Millisecond)),
			"received_at %s of raw event %d is from before the insert could run, which was released at %s", at, id, released)
		stamps = append(stamps, at)
	}
	require.NoError(t, rows.Err())
	require.Len(t, stamps, 2)
	require.False(t, stamps[1].Before(stamps[0]), "the higher raw ID carries the later time")
}
