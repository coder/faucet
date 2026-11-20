package pgstore_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/coder/superblink-events/faucet"
	"github.com/coder/superblink-events/faucet/pgstore"
)

// The statements of the build before the target lines migration, which keeps
// inserting, purging and subscribing during a rolling deploy. They name no
// position.
const (
	insertBeforeLines = `INSERT INTO faucet_events (source, delivery_id, type, repo_id, owner_id, repo_name, prs, head_sha, data, received_at)
VALUES ('github', $1, 'ci.run_completed', $2, $3, '', '{}', NULL, '{}'::jsonb, clock_timestamp())
ON CONFLICT (delivery_id) DO NOTHING`
	purgeBeforeLines = `WITH deleted AS (
  DELETE FROM faucet_events WHERE received_at < $1 RETURNING id
)
UPDATE faucet_purge_mark SET purged_through = GREATEST(purged_through, (SELECT COALESCE(MAX(id), 0) FROM deleted))`
	subscribeBeforeLines = `INSERT INTO faucet_subscriptions (key_hash, principal, callback_url, event_type, event_types, arguments, target_kind, repo_id, owner_id, secret_enc, cursor, scanned_through, created_at, updated_at, expires_at)
VALUES ($1, 'octocat', $2, 'ci.run_completed', '{ci.run_completed}', $3::text::jsonb, 'repo', 42, 0, '\x01', $4, $4, $5, $5, $6)`
)

func TestTheBuildBeforeTargetLinesKeepsTheLinesWhole(t *testing.T) {
	t.Parallel()
	db := newDatabase(t, testDatabaseURL(t))
	store := pgstore.New(db)
	ctx := context.Background()
	insertAsBefore := func(delivery string, repoID, ownerID int64) {
		t.Helper()
		tx, err := db.BeginTx(ctx, nil)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback() }()
		_, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(1664114494957747203)`)
		require.NoError(t, err)
		_, err = tx.ExecContext(ctx, insertBeforeLines, delivery, repoID, ownerID)
		require.NoError(t, err)
		require.NoError(t, tx.Commit())
		time.Sleep(2 * time.Millisecond)
	}
	insert := func(delivery string, repoID, ownerID int64) faucet.Event {
		t.Helper()
		ev, inserted, err := store.InsertEvent(ctx, faucet.Event{
			Source: "github", DeliveryID: delivery, Type: "ci.run_completed", RepoID: repoID, OwnerID: ownerID, Data: json.RawMessage(`{}`),
		})
		require.NoError(t, err)
		require.True(t, inserted)
		time.Sleep(2 * time.Millisecond)
		return ev
	}
	line := func(target faucet.Target) faucet.PollPage {
		t.Helper()
		page, err := store.PollEvents(ctx, faucet.PollQuery{EventFilter: faucet.EventFilter{Target: target}, Limit: 10})
		require.NoError(t, err)
		return page
	}
	positions := func(target faucet.Target) map[string]faucet.Position {
		t.Helper()
		got := map[string]faucet.Position{}
		for _, ev := range line(target).Events {
			got[ev.DeliveryID] = target.PositionOf(ev)
		}
		return got
	}
	repo, owner := faucet.RepoTarget(42), faucet.OwnerTarget(7)

	insert("new-1", 42, 7)
	insertAsBefore("old-1", 42, 7)
	insertAsBefore("old-2", 43, 7)
	insertAsBefore("old-3", 42, 0)
	insertAsBefore("old-1", 42, 7)
	require.Equal(t, map[string]faucet.Position{"new-1": 1, "old-1": 2, "old-3": 3}, positions(repo),
		"the trigger numbers an event that the old build inserts, and its retry takes no position")
	require.Equal(t, map[string]faucet.Position{"new-1": 1, "old-1": 2, "old-2": 3}, positions(owner))

	var cut time.Time
	require.NoError(t, db.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&cut))
	time.Sleep(2 * time.Millisecond)
	kept := insert("new-2", 42, 7)
	_, err := db.ExecContext(ctx, purgeBeforeLines, cut)
	require.NoError(t, err)
	for _, tt := range []struct {
		target     faucet.Target
		mark, head faucet.Position
	}{{repo, 3, 4}, {owner, 3, 4}, {faucet.RepoTarget(43), 1, 1}} {
		page := line(tt.target)
		require.Equal(t, tt.mark, page.PurgedThrough, "a purge by the old build raises the mark of the line of %s %d", tt.target.Kind(), tt.target.RepoID()+tt.target.OwnerID())
		require.Equal(t, tt.head, page.Next)
	}
	require.EqualValues(t, 4, kept.RepoPosition)
	next := insert("new-3", 43, 7)
	require.EqualValues(t, 2, next.RepoPosition, "a line whose events the old build purged goes on from its head")

	now := time.Now().Truncate(time.Second)
	arguments := json.RawMessage(`{"repo":"octo/repo"}`)
	const callbackURL = "https://hooks.example.test/before"
	key := faucet.NewSubscriptionKeyHash("octocat", callbackURL, "ci.run_completed", arguments)
	_, err = db.ExecContext(ctx, subscribeBeforeLines, key[:], callbackURL, string(arguments), kept.ID, now, now.Add(time.Hour))
	require.NoError(t, err)
	renewed, err := store.UpsertSubscription(ctx, faucet.Subscription{
		KeyHash: key, Principal: "octocat", CallbackURL: callbackURL, EventType: "ci.run_completed", EventTypes: []string{"ci.run_completed"},
		Arguments: arguments, Target: repo, SecretEnc: []byte{1}, ExpiresAt: now.Add(2 * time.Hour),
	}, now.Add(time.Minute), 10)
	require.NoError(t, err)
	require.Equal(t, kept.ID, renewed.Cursor)
	require.Equal(t, kept.RepoPosition, renewed.Position, "a subscription that the old build stored gets the position of its cursor")
}

// The migration numbers the events that exist from the oldest that a purge
// left, and gives each subscription the position of its cursor.
func TestTheTargetLinesMigrationNumbersTheEventsAndSubscriptionsThatExist(t *testing.T) {
	t.Parallel()
	db := newDatabase(t, testDatabaseURL(t))
	ctx := context.Background()
	runMigrationFile(t, db, "000018_target_lines.down.sql")
	_, err := db.ExecContext(ctx, `SELECT setval('faucet_events_id_seq', 100)`)
	require.NoError(t, err)

	var ids []int64
	for _, ev := range []struct {
		delivery        string
		repoID, ownerID int64
	}{{"a", 42, 7}, {"b", 43, 7}, {"c", 42, 0}, {"d", 44, 9}, {"e", 42, 7}} {
		var id int64
		require.NoError(t, db.QueryRowContext(ctx, `INSERT INTO faucet_events (source, delivery_id, type, repo_id, owner_id, data) VALUES ('github', $1, 'ci.run_completed', $2, $3, '{}') RETURNING id`,
			ev.delivery, ev.repoID, ev.ownerID).Scan(&id))
		ids = append(ids, id)
	}
	_, err = db.ExecContext(ctx, `UPDATE faucet_purge_mark SET purged_through = $1`, ids[0]-1)
	require.NoError(t, err)
	now := time.Now().Truncate(time.Second)
	for i, cursor := range []int64{0, ids[2], ids[3], ids[0] - 2} {
		arguments := json.RawMessage(`{"repo":"octo/repo","n":` + strconv.Itoa(i) + `}`)
		key := faucet.NewSubscriptionKeyHash("octocat", "https://hooks.example.test/m", "ci.run_completed", arguments)
		_, err := db.ExecContext(ctx, subscribeBeforeLines, key[:], "https://hooks.example.test/m", string(arguments), cursor, now, now.Add(time.Hour))
		require.NoError(t, err)
	}
	runMigrationFile(t, db, "000018_target_lines.up.sql")

	rows, err := db.QueryContext(ctx, `SELECT delivery_id, repo_seq, owner_seq FROM faucet_events ORDER BY id`)
	require.NoError(t, err)
	type numbered struct {
		delivery string
		repo     int64
		owner    sql.NullInt64
	}
	var got []numbered
	for rows.Next() {
		var n numbered
		require.NoError(t, rows.Scan(&n.delivery, &n.repo, &n.owner))
		got = append(got, n)
	}
	require.NoError(t, rows.Err())
	some := func(n int64) sql.NullInt64 { return sql.NullInt64{Int64: n, Valid: true} }
	require.Equal(t, []numbered{{"a", 1, some(1)}, {"b", 1, some(2)}, {"c", 2, sql.NullInt64{}}, {"d", 1, some(1)}, {"e", 3, some(3)}}, got)

	var positions []int64
	rows, err = db.QueryContext(ctx, `SELECT position FROM faucet_subscriptions ORDER BY cursor`)
	require.NoError(t, err)
	for rows.Next() {
		var p int64
		require.NoError(t, rows.Scan(&p))
		positions = append(positions, p)
	}
	require.NoError(t, rows.Err())
	require.Equal(t, []int64{0, 0, 2, 2}, positions,
		"cursors 0 and below the purge mark start before every event that the migration numbered, c is the second event of repository 42, and d, of another repository, lies after it")
}

// TestAPurgeOfAnEventWithNoValidRepositoryIDStillRuns: the build before
// target lines stored an event of a negative repository ID, which no line
// can have. Its purge must not fail the purge of every other event.
func TestAPurgeOfAnEventWithNoValidRepositoryIDStillRuns(t *testing.T) {
	t.Parallel()
	db := newDatabase(t, testDatabaseURL(t))
	store := pgstore.New(db)
	ctx := context.Background()
	_, err := db.ExecContext(ctx, insertBeforeLines, "bad", -5, 0)
	require.NoError(t, err)
	kept, inserted, err := store.InsertEvent(ctx, faucet.Event{
		Source: "github", DeliveryID: "kept", Type: "ci.run_completed", RepoID: 42, OwnerID: 7, Data: json.RawMessage(`{}`),
	})
	require.NoError(t, err)
	require.True(t, inserted)

	res, err := store.Purge(ctx, kept.ReceivedAt)
	require.NoError(t, err)
	require.EqualValues(t, 1, res.Deleted)
	page, err := store.PollEvents(ctx, faucet.PollQuery{EventFilter: faucet.EventFilter{Target: faucet.RepoTarget(42)}, Limit: 10})
	require.NoError(t, err)
	require.Len(t, page.Events, 1)
	require.Zero(t, page.PurgedThrough, "the line of repository 42 lost nothing")
}
