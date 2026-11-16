package pgstore_test

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
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
		return newGapRecordingStore(newDatabase(t, adminURL))
	})
}

// gapRecordingStore lets storetest read the gaps that pgstore keeps.
type gapRecordingStore struct {
	*pgstore.Store
	db *sql.DB
}

var _ storetest.GapRecords = gapRecordingStore{}

func newGapRecordingStore(db *sql.DB) gapRecordingStore {
	return gapRecordingStore{Store: pgstore.New(db), db: db}
}

func (s gapRecordingStore) RecordedRedeliveryGaps(ctx context.Context) ([]faucet.RedeliveryGap, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT older_at, newer_at FROM faucet_redelivery_ranges WHERE state = 'gap'`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var gaps []faucet.RedeliveryGap
	for rows.Next() {
		var gap faucet.RedeliveryGap
		if err := rows.Scan(&gap.From, &gap.To); err != nil {
			return nil, err
		}
		gaps = append(gaps, gap)
	}
	return gaps, rows.Err()
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
		return newGapRecordingStore(newDatabase(t, adminURL))
	})
}

// CursorAt(t) is the highest ID received before t, which is only a prefix of
// the log if a later ID never carries an earlier time. A time taken when the
// transaction starts breaks that for an insert that then waits for the lock.
func TestInsertEventIsStampedWhenItIsWrittenNotWhenItsTransactionStarts(t *testing.T) {
	t.Parallel()
	db := newDatabase(t, testDatabaseURL(t))
	store := pgstore.New(db)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	blocker, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = blocker.Rollback() }()
	_, err = blocker.ExecContext(ctx, `LOCK TABLE faucet_events IN ACCESS EXCLUSIVE MODE`)
	require.NoError(t, err)

	type inserted struct {
		ev  faucet.Event
		err error
	}
	done := make(chan inserted, 1)
	go func() {
		ev, _, err := store.InsertEvent(ctx, faucet.Event{
			Source: "github", DeliveryID: "stamp-1", Type: "ci.run_completed", RepoID: 1, Data: json.RawMessage(`{}`),
		})
		done <- inserted{ev, err}
	}()
	time.Sleep(400 * time.Millisecond)
	released := time.Now()
	require.NoError(t, blocker.Commit())

	got := <-done
	require.NoError(t, got.err)
	require.True(t, got.ev.ReceivedAt.After(released.Add(-100*time.Millisecond)),
		"received_at %s is from before the insert could run, which was released at %s", got.ev.ReceivedAt, released)
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
	require.EqualValues(t, 8, faucetVersion)
}

func TestTheClearMigrationMovesTheOldTopToTheClearAndBack(t *testing.T) {
	t.Parallel()
	db := newDatabase(t, testDatabaseURL(t))
	store := pgstore.New(db)
	ctx := context.Background()
	const hook = "/orgs/acme/hooks/7"
	migrate := func(direction string) {
		t.Helper()
		statements, err := os.ReadFile("migrations/000008_redelivery_clear." + direction + ".sql")
		require.NoError(t, err)
		_, err = db.ExecContext(ctx, string(statements))
		require.NoError(t, err)
	}
	storedTop := func() (top sql.NullTime) {
		t.Helper()
		require.NoError(t, db.QueryRowContext(ctx, `SELECT top_at FROM faucet_redelivery_scan`).Scan(&top))
		return top
	}
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	oldTop := now.Add(-10 * time.Minute)

	lease, ok, err := store.AcquireRedeliveryLease(ctx, hook, now, time.Minute)
	require.NoError(t, err)
	require.True(t, ok)
	_, err = store.ApplyRedeliveryScan(ctx, faucet.ScanStep{
		Epoch: lease.Epoch, Now: now, Top: oldTop,
		Open: &faucet.RedeliveryRange{NewerAt: now.Add(-time.Hour), OlderAt: now.Add(-48 * time.Hour), Cursor: "cursor-1"},
	})
	require.NoError(t, err)
	require.NoError(t, store.ReleaseRedeliveryLease(ctx, lease.Epoch))
	migrate("down")
	require.True(t, oldTop.Equal(storedTop().Time), "this is the row of a build before the migration")

	migrate("up")
	migrated, ok, err := store.AcquireRedeliveryLease(ctx, hook, now.Add(time.Minute), time.Minute)
	require.NoError(t, err)
	require.True(t, ok)
	require.True(t, migrated.Top.IsZero(), "the next run is a first run: %s", migrated.Top)
	require.True(t, oldTop.Equal(migrated.Clear), "that knows when the old build last saw the top of the list: %s", migrated.Clear)
	require.False(t, migrated.HookChanged)
	require.Len(t, migrated.Ranges, 1, "an open range stays open")

	migrate("down")
	require.True(t, oldTop.Equal(storedTop().Time), "a scan whose head no run has read since the migration gets its position back")

	migrate("up")
	emptyListDate := now.Add(30 * time.Minute)
	_, err = store.ApplyRedeliveryScan(ctx, faucet.ScanStep{Epoch: migrated.Epoch, Now: now.Add(30 * time.Minute), Clear: emptyListDate})
	require.ErrorIs(t, err, faucet.ErrRedeliveryLeaseLost, "the migration ends a lease that a run took before it")
	emptyList, ok, err := store.AcquireRedeliveryLease(ctx, hook, now.Add(30*time.Minute), time.Minute)
	require.NoError(t, err)
	require.True(t, ok)
	_, err = store.ApplyRedeliveryScan(ctx, faucet.ScanStep{Epoch: emptyList.Epoch, Now: now.Add(30 * time.Minute), Clear: emptyListDate})
	require.NoError(t, err)
	migrate("down")
	require.True(t, emptyListDate.Equal(storedTop().Time), "a scan whose runs read only an empty list since the migration gets the Date of the last one")

	migrate("up")
	read, ok, err := store.AcquireRedeliveryLease(ctx, hook, now.Add(time.Hour), time.Minute)
	require.NoError(t, err)
	require.True(t, ok)
	attempt, date := now.Add(time.Hour), now.Add(time.Hour+time.Second)
	_, err = store.ApplyRedeliveryScan(ctx, faucet.ScanStep{Epoch: read.Epoch, Now: now.Add(time.Hour), Top: attempt, Clear: date})
	require.NoError(t, err)
	migrate("down")
	require.True(t, attempt.Equal(storedTop().Time), "a scan that has read an attempt keeps that attempt's time, not the Date")
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

// A purge that waits for another purge must hold no event row yet. The test
// holds the purge-mark row as the running purge, and a second transaction
// asks for an event that the waiting purge would delete. A purge that deleted
// before it waited would hold that row, and the inserts behind it would wait
// for two purges.
func TestPurgeThatWaitsForAnotherPurgeHoldsNoEventRow(t *testing.T) {
	t.Parallel()
	db := newDatabase(t, testDatabaseURL(t))
	store := pgstore.New(db)
	ctx := context.Background()

	stored, inserted, err := store.InsertEvent(ctx, faucet.Event{
		Source: "github", DeliveryID: "purge-wait-1", Type: "ci.run_completed", RepoID: 42, Data: []byte(`{}`),
	})
	require.NoError(t, err)
	require.True(t, inserted)

	running, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = running.Rollback() })
	_, err = running.ExecContext(ctx, `SELECT purged_through FROM faucet_purge_mark FOR UPDATE`)
	require.NoError(t, err)

	type outcome struct {
		res faucet.PurgeResult
		err error
	}
	waiting := make(chan outcome, 1)
	go func() {
		res, err := store.Purge(ctx, time.Now().Add(time.Hour))
		waiting <- outcome{res, err}
	}()
	require.Eventually(t, func() bool {
		var blocked int
		err := db.QueryRowContext(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&blocked)
		return err == nil && blocked == 1
	}, 10*time.Second, 20*time.Millisecond, "the purge never waited for the held purge mark")

	probe, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	var id int64
	err = probe.QueryRowContext(ctx, `SELECT id FROM faucet_events FOR UPDATE NOWAIT`).Scan(&id)
	require.NoError(t, probe.Rollback())
	require.NoError(t, err, "the waiting purge already holds an event row")
	require.Equal(t, stored.ID, id)

	select {
	case got := <-waiting:
		t.Fatalf("the purge finished while the purge mark was held: %+v", got)
	default:
	}
	require.NoError(t, running.Commit())
	got := <-waiting
	require.NoError(t, got.err)
	require.Equal(t, faucet.PurgeResult{Deleted: 1, PurgedThrough: stored.ID}, got.res)
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

func TestRedeliveryStoreCallThatItsContextCutShortWrapsTheContextError(t *testing.T) {
	t.Parallel()
	db := newDatabase(t, testDatabaseURL(t))
	store := pgstore.New(db)
	tx, err := db.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })
	for _, table := range []string{"faucet_redeliveries", "faucet_redelivery_scan", "faucet_redelivery_ranges"} {
		_, err = tx.Exec("LOCK TABLE " + table + " IN ACCESS EXCLUSIVE MODE") //nolint:gosec // The names are constants.
		require.NoError(t, err)
	}

	now := time.Now()
	for name, call := range map[string]func(context.Context) error{
		"AcquireRedeliveryLease": func(ctx context.Context) error {
			_, _, err := store.AcquireRedeliveryLease(ctx, "hook", now, time.Minute)
			return err
		},
		"ReleaseRedeliveryLease": func(ctx context.Context) error {
			return store.ReleaseRedeliveryLease(ctx, 1)
		},
		"ApplyRedeliveryScan": func(ctx context.Context) error {
			_, err := store.ApplyRedeliveryScan(ctx, faucet.ScanStep{Epoch: 1, Now: now})
			return err
		},
		"SettleRedeliveries": func(ctx context.Context) error {
			_, err := store.SettleRedeliveries(ctx, faucet.SettleRequest{Now: now, Retention: time.Hour, MaxAsks: 3})
			return err
		},
		"ClaimRedelivery": func(ctx context.Context) error {
			_, _, err := store.ClaimRedelivery(ctx, faucet.ClaimRequest{Now: now, Retention: time.Hour, MaxAsks: 1, Schedule: []time.Duration{time.Minute}})
			return err
		},
		"PurgeRedeliveries": func(ctx context.Context) error {
			_, err := store.PurgeRedeliveries(ctx, now)
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
