package faucet_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/coder/superblink-events/faucet"
	"github.com/coder/superblink-events/faucet/pgstore"
)

const before000009UpsertSubscription = `INSERT INTO faucet_subscriptions (key_hash, principal, callback_url, event_type, arguments, repo_id, secret_enc, cursor, scanned_through, created_at, updated_at, expires_at)
VALUES ($1, $2, $3, $4, $5::text::jsonb, $6, $7, $8, $8, $9, $9, $10)
ON CONFLICT (key_hash) DO UPDATE SET
  repo_id = EXCLUDED.repo_id,
  previous_secret_enc = CASE
    WHEN faucet_subscriptions.secret_enc = EXCLUDED.secret_enc THEN faucet_subscriptions.previous_secret_enc
    WHEN $11::timestamptz IS NULL THEN NULL
    ELSE faucet_subscriptions.secret_enc
  END,
  previous_secret_until = CASE
    WHEN faucet_subscriptions.secret_enc = EXCLUDED.secret_enc THEN faucet_subscriptions.previous_secret_until
    ELSE $11::timestamptz
  END,
  secret_enc = EXCLUDED.secret_enc,
  updated_at = EXCLUDED.updated_at,
  expires_at = EXCLUDED.expires_at
RETURNING key_hash, principal, callback_url, event_type, arguments, repo_id, secret_enc, cursor, created_at, updated_at, expires_at, scanned_through, last_delivery_at, last_error, failed_since, previous_secret_enc, previous_secret_until, gap_through, gap_lease_until`

func runMigration(t *testing.T, db *sql.DB, name, direction string) {
	t.Helper()
	statements, err := os.ReadFile("pgstore/migrations/" + name + "." + direction + ".sql")
	require.NoError(t, err)
	_, err = db.ExecContext(context.Background(), string(statements))
	require.NoError(t, err)
}

func TestASubscriptionFromBeforeTheEventTypesMigrationKeepsItsKeyAndItsEvents(t *testing.T) {
	t.Parallel()

	db := postgresDB(t)
	store := pgstore.New(db)
	d := newDeliverHarness(t, func(deps *faucet.Deps) { deps.Store = store })
	d.ledgerSkip = true
	ep := d.endpoint("/hook", "pr", 7)
	url := d.rcv.url(ep.path)

	sub := d.subscribeEndpoint(ep)
	beforeFamilies := json.RawMessage(`{"repo":"octo/repo","pr":7}`)
	require.Equal(t, faucet.NewSubscriptionKeyHash("octocat", url, "ci.run_completed", beforeFamilies).ID(), sub.ID,
		"the arguments of a name without actions are spelled as they were before families")
	runMigration(t, db, "000009_subscription_event_types", "down")
	runMigration(t, db, "000009_subscription_event_types", "up")

	d.advance(time.Second)
	for i, typ := range []string{"pr.closed", "ci.run_completed"} {
		_, _, err := store.InsertEvent(d.ctx, faucet.Event{
			Source: "github", DeliveryID: fmt.Sprintf("after-%d", i), Type: typ, RepoID: repoID, PRs: []int32{7}, Data: json.RawMessage(`{}`),
		})
		require.NoError(t, err)
	}
	require.Equal(t, 1, d.deliverDue().Delivered)
	got := d.rcv.requests("/hook")
	require.Len(t, got, 1, "the migrated row matches the events of its name and no other")
	require.Equal(t, "ci.run_completed", got[0].body(t).Name)
	require.Equal(t, sub.ID, d.subscribeEndpoint(ep).ID, "a renewal finds the migrated row")
	require.Equal(t, sub.ID, got[0].Header.Get("X-MCP-Subscription-Id"))
}

func TestARowThatTheBuildBeforeTheEventTypesMigrationInsertsMatchesItsName(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db := postgresDB(t)
	store := pgstore.New(db)
	now := time.Now()
	arguments := `{"repo":"octo/repo"}`
	key := faucet.NewSubscriptionKeyHash("octocat", callbackURL, "pr.closed", json.RawMessage(arguments))
	for i, sealed := range []string{"sealed-1", "sealed-2"} {
		_, err := db.ExecContext(ctx, before000009UpsertSubscription,
			key[:], "octocat", callbackURL, "pr.closed", arguments, repoID, []byte(sealed), 0, now, now.Add(time.Hour), now.Add(time.Minute))
		require.NoError(t, err, "subscribe %d of the build before the migration", i+1)
	}
	_, _, err := store.InsertEvent(ctx, faucet.Event{Source: "github", DeliveryID: "closed", Type: "pr.closed", RepoID: repoID, Data: json.RawMessage(`{}`)})
	require.NoError(t, err)

	targets, err := store.ScanTargets(ctx, now, faucet.MaxPendingDeliveries, 10)
	require.NoError(t, err)
	require.Len(t, targets, 1)
	require.Equal(t, []string{"pr.closed"}, targets[0].EventTypes, "a row without stored types matches the type of its name")
}
