package redelivery_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/lib/pq"
	"github.com/stretchr/testify/require"

	"github.com/coder/quartz"
	"github.com/coder/superblink-events/faucet/internal/pgtest"
	"github.com/coder/superblink-events/faucet/pgstore"
	"github.com/coder/superblink-events/faucet/redelivery"
)

const (
	before000008EnsureRedeliveryScan = `INSERT INTO faucet_redelivery_scan (id, hook, epoch)
VALUES (true, $1, 0)
ON CONFLICT (id) DO NOTHING`
	before000008LockRedeliveryScan  = `SELECT hook, epoch, lease_until, top_at FROM faucet_redelivery_scan FOR UPDATE`
	before000008TakeRedeliveryLease = `UPDATE faucet_redelivery_scan
SET hook = $1,
    epoch = epoch + 1,
    lease_until = $2::timestamptz,
    top_at = CASE WHEN $3::boolean THEN NULL ELSE top_at END
RETURNING epoch, top_at`
	before000008RaiseRedeliveryTop     = `UPDATE faucet_redelivery_scan SET top_at = GREATEST(top_at, $1::timestamptz)`
	before000008ReleaseRedeliveryLease = `UPDATE faucet_redelivery_scan SET lease_until = NULL WHERE epoch = $1`
)

type buildBefore000008 struct {
	db   *sql.DB
	hook string
}

func (b buildBefore000008) acquire(ctx context.Context, now time.Time) (epoch int64, err error) {
	tx, err := b.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, before000008EnsureRedeliveryScan, b.hook); err != nil {
		return 0, err
	}
	if err := b.lock(ctx, tx, nil); err != nil {
		return 0, err
	}
	var top sql.NullTime
	if err := tx.QueryRowContext(ctx, before000008TakeRedeliveryLease, b.hook, now.Add(2*time.Minute), false).Scan(&epoch, &top); err != nil {
		return 0, err
	}
	return epoch, tx.Commit()
}

func (b buildBefore000008) raiseTop(ctx context.Context, epoch int64, top time.Time) error {
	tx, err := b.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var locked int64
	if err := b.lock(ctx, tx, &locked); err != nil {
		return err
	}
	if locked != epoch {
		return redelivery.ErrLeaseLost
	}
	if _, err := tx.ExecContext(ctx, before000008RaiseRedeliveryTop, top); err != nil {
		return err
	}
	return tx.Commit()
}

func (buildBefore000008) lock(ctx context.Context, tx *sql.Tx, epoch *int64) error {
	var (
		hook       string
		e          int64
		lease, top sql.NullTime
	)
	if err := tx.QueryRowContext(ctx, before000008LockRedeliveryScan).Scan(&hook, &e, &lease, &top); err != nil {
		return err
	}
	if epoch != nil {
		*epoch = e
	}
	return nil
}

func (b buildBefore000008) release(ctx context.Context, epoch int64) error {
	_, err := b.db.ExecContext(ctx, before000008ReleaseRedeliveryLease, epoch)
	return err
}

func migrate000008(t *testing.T, db *sql.DB, direction string) {
	t.Helper()
	statements, err := os.ReadFile("../pgstore/migrations/000008_redelivery_clear." + direction + ".sql")
	require.NoError(t, err)
	_, err = db.ExecContext(context.Background(), string(statements))
	require.NoError(t, err)
}

func scanRow(t *testing.T, db *sql.DB) string {
	t.Helper()
	var row string
	require.NoError(t, db.QueryRowContext(context.Background(), `SELECT to_jsonb(s)::text FROM faucet_redelivery_scan s`).Scan(&row))
	return row
}

func TestARunOfTheBuildBeforeTheClearMigrationFailsAfterItAndWritesNoPosition(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name                string
		hadTop, startsAfter bool
	}{
		{"it holds the lease during the migration, and the row has a top", true, false},
		{"it holds the lease during the migration, and it is the first run", false, false},
		{"an instance of that build starts it after the migration", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			env := newRedeliveryEnv(t)
			env.clock.WithLogger(quartz.NoOpLogger)
			db := pgtest.NewDB(t, pgstore.Migrate)
			env.use(pgstore.New(db), env.gh)
			old := buildBefore000008{db: db, hook: env.gh.Hook()}

			migrate000008(t, db, "down")
			if tc.hadTop {
				epoch, err := old.acquire(ctx, env.now())
				require.NoError(t, err)
				require.NoError(t, old.raiseTop(ctx, epoch, env.gh.now().Add(-time.Minute)))
				require.NoError(t, old.release(ctx, epoch))
				env.advance(cronEvery)
			}
			var inFlight int64
			if !tc.startsAfter {
				var err error
				inFlight, err = old.acquire(ctx, env.now())
				require.NoError(t, err)
			}
			migrate000008(t, db, "up")

			migrated := scanRow(t, db)
			epoch, oldErr := inFlight, error(nil)
			if tc.startsAfter {
				epoch, oldErr = old.acquire(ctx, env.now())
			}
			if oldErr == nil {
				oldErr = old.raiseTop(ctx, epoch, env.gh.now().Add(time.Hour))
			}
			require.ErrorContains(t, oldErr, `column "top_at" does not exist`)
			require.NoError(t, old.release(ctx, epoch))
			for _, statement := range []struct {
				sql  string
				args []any
			}{
				{before000008LockRedeliveryScan, nil},
				{before000008TakeRedeliveryLease, []any{old.hook, env.now(), false}},
				{before000008RaiseRedeliveryTop, []any{env.gh.now()}},
			} {
				_, err := db.ExecContext(ctx, statement.sql, statement.args...)
				require.ErrorContains(t, err, `column "top_at" does not exist`, "no statement of the old build reads or writes the position: %s", statement.sql)
			}
			require.Equal(t, migrated, scanRow(t, db), "the old build's run changed nothing")

			res := env.run()
			require.False(t, res.Busy, "a run of this build need not wait for the lease of a run that the migration fenced")
			var failures []ghAttempt
			for i := range 12 {
				env.advance(cronEvery)
				failures = append(failures, env.gh.record(fmt.Sprintf("f-%02d", i), env.gh.now().Add(-time.Second), 503))
				env.run()
			}
			guids := make([]string, len(failures))
			for i, f := range failures {
				guids[i] = f.guid
			}
			var tracked int
			require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM faucet_redeliveries WHERE guid = ANY($1)`, pq.Array(guids)).Scan(&tracked))
			require.Equal(t, len(failures), tracked, "every failure after the migration is tracked")
		})
	}
}
