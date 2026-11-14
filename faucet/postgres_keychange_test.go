package faucet_test

import (
	"bytes"
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
)

func postgresStore(t *testing.T) faucet.Store {
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
	return pgstore.New(db)
}

func onPostgres(t *testing.T) (d *deliverHarness, store faucet.Store) {
	t.Helper()
	store = postgresStore(t)
	d = newDeliverHarness(t, func(deps *faucet.Deps) { deps.Store = store })
	d.ledgerSkip = true
	return d, store
}

func (d *deliverHarness) restartedUnderNewKey(store faucet.Store) *deliverHarness {
	d.t.Helper()
	key, err := faucet.NewWebhookKey(bytes.Repeat([]byte{0x5c}, 32))
	require.NoError(d.t, err)
	next := newDeliverHarness(d.t, func(deps *faucet.Deps) {
		deps.Store = store
		deps.Clock = d.clock
		deps.Webhook.Key = key
	})
	next.ledgerSkip = true
	return next
}

func TestKeyChangeOnPostgresStillSendsTheGapEnvelopeWithTheRenewedSecret(t *testing.T) {
	t.Parallel()

	d, store := onPostgres(t)
	ep := d.endpoint("/hook")
	sub := d.subscribeEndpoint(ep)
	d.advance(time.Second)
	for i := range 2 {
		_, _, err := store.InsertEvent(d.ctx, faucet.Event{
			Source: "github", DeliveryID: "lost-" + string(rune('a'+i)), Type: "ci.run_completed", RepoID: repoID, Data: json.RawMessage(`{}`),
		})
		require.NoError(t, err)
	}
	purged, err := store.Purge(d.ctx, time.Now().Add(time.Hour))
	require.NoError(t, err)
	require.EqualValues(t, 2, purged.Deleted)

	next := d.restartedUnderNewKey(store)
	renewed := newSecret(t, 32)
	next.subscribe(subscribeTo(ep.args, d.rcv.url(ep.path), renewed))

	require.Equal(t, 1, next.deliverDue().GapsSent, "the previous secret is sealed under the old key and unreadable, and the gap goes out anyway")
	gaps := d.rcv.envelopesOf("/hook", "gap")
	require.Len(t, gaps, 1)
	d.requireEnvelope(gaps[0], "gap", renewed, sub.ID)
	require.Equal(t, 1, gaps[0].signatureCount(), "the renewed secret signs alone")
	d.requireUnreadablePreviousSkipped(next, ep.secret, renewed)
}

func TestKeyChangeOnPostgresStillSendsTheTerminatedEnvelopeWithTheRenewedSecret(t *testing.T) {
	t.Parallel()

	d, store := onPostgres(t)
	ep := d.endpoint("/hook")
	sub := d.subscribeEndpoint(ep)
	d.advance(time.Second)

	next := d.restartedUnderNewKey(store)
	renewed := newSecret(t, 32)
	next.subscribe(subscribeTo(ep.args, d.rcv.url(ep.path), renewed))
	next.revoke()
	next.subscribeError(subscribeTo(ep.args, d.rcv.url(ep.path), renewed))

	terminated := d.rcv.envelopesOf("/hook", "terminated")
	require.Len(t, terminated, 1)
	d.requireEnvelope(terminated[0], "terminated", renewed, sub.ID)
	require.Equal(t, 1, terminated[0].signatureCount())
	d.requireUnreadablePreviousSkipped(next, ep.secret, renewed)
}
