package pgstore_test

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/lib/pq"
	"github.com/stretchr/testify/require"

	"cdr.dev/slog/v3"
	"github.com/coder/faucet/faucet"
	"github.com/coder/faucet/faucet/oauth"
	"github.com/coder/faucet/faucet/pgstore"
	"github.com/coder/faucet/faucet/redelivery"
	"github.com/coder/faucet/faucet/storetest"
	"github.com/coder/quartz"
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

func TestRawStore(t *testing.T) {
	t.Parallel()
	adminURL := testDatabaseURL(t)

	storetest.RunRaw(t, func(t *testing.T) storetest.RawStore {
		return pgstore.New(newDatabase(t, adminURL))
	})
}

func TestOAuthStore(t *testing.T) {
	t.Parallel()
	adminURL := testDatabaseURL(t)

	storetest.RunOAuth(t, func(t *testing.T) oauth.Store {
		return pgstore.New(newDatabase(t, adminURL))
	})
}

func TestOAuthStoreOverBinaryParameters(t *testing.T) {
	t.Parallel()
	adminURL := withBinaryParameters(t, testDatabaseURL(t))

	storetest.RunOAuth(t, func(t *testing.T) oauth.Store {
		return pgstore.New(newDatabase(t, adminURL))
	})
}

// A row that a later build wrote can hold a scope this build does not know.
// The store returns it as it is, and the verifier rejects it like an unknown
// token, not as a check that could not run.
func TestAnAccessTokenWithAnUnknownScopeIsRejectedNotAFailure(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := newDatabase(t, testDatabaseURL(t))
	store := pgstore.New(db)
	token := "fct_" + strings.Repeat("A", 43)
	hash := oauth.HashToken(token)
	_, err := db.ExecContext(ctx, `INSERT INTO faucet_access_tokens (token_hash, expires_at, scopes, resource, subject_kind, subject)
		VALUES ($1, now() + interval '1 hour', ARRAY['events:read', 'events:admin'], 'https://faucet.example.com/mcp', 'service', 'ci-bot')`, hash[:])
	require.NoError(t, err)

	got, err := store.AccessToken(ctx, hash)
	require.NoError(t, err)
	require.Equal(t, []faucet.Scope{faucet.ScopeEventsRead, "events:admin"}, got.Scopes)

	verifier, err := oauth.NewVerifier(store, quartz.NewReal(), map[oauth.SubjectKind]oauth.SubjectResolver{
		oauth.SubjectService: func(_ context.Context, id string, scopes []faucet.Scope) (faucet.Principal, error) {
			return faucet.NewService(faucet.ServiceClient{ClientID: id}, scopes)
		},
	})
	require.NoError(t, err)
	_, err = verifier.Verify(ctx, token, "https://faucet.example.com/mcp")
	require.ErrorIs(t, err, faucet.ErrInvalidToken)
}

type gapRecordingStore struct {
	*pgstore.Store
	db *sql.DB
}

var _ storetest.GapRecords = gapRecordingStore{}

func newGapRecordingStore(db *sql.DB) gapRecordingStore {
	return gapRecordingStore{Store: pgstore.New(db), db: db}
}

func (s gapRecordingStore) RecordedRedeliveryGaps(ctx context.Context) ([]redelivery.Gap, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT older_at, newer_at FROM faucet_redelivery_ranges WHERE state = 'gap'`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var gaps []redelivery.Gap
	for rows.Next() {
		var gap redelivery.Gap
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

func TestRawStoreOverBinaryParameters(t *testing.T) {
	t.Parallel()
	adminURL := withBinaryParameters(t, testDatabaseURL(t))

	storetest.RunRaw(t, func(t *testing.T) storetest.RawStore {
		return pgstore.New(newDatabase(t, adminURL))
	})
}

// PositionAt(t) is the highest position received before t, which is only a
// prefix of the line if a later ID never carries an earlier time. A time taken when the
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
	require.EqualValues(t, 19, faucetVersion)
}

// The raw events migration adds tables of its own and one index. A down
// leaves the event log as a build from before it reads it, with the raw
// subscriptions as rows that such a build never scans, and an up starts raw
// events empty.
func TestTheRawEventsMigrationGoesDownAndUpAroundTheEventLog(t *testing.T) {
	t.Parallel()
	db := newDatabase(t, testDatabaseURL(t))
	store := pgstore.New(db)
	ctx := context.Background()
	// The owner targets migration adds columns to the raw tables, so it goes
	// down before them and up after them, and the target lines migration
	// reads the owner of an event, so it goes down first and up last.
	migrate := func(direction string) {
		t.Helper()
		files := []string{"000018_target_lines", "000014_owner_targets", "000012_raw_events"}
		if direction == "up" {
			slices.Reverse(files)
		}
		for _, file := range files {
			runMigrationFile(t, db, file+"."+direction+".sql")
		}
	}
	exists := func(relation string) (found bool) {
		t.Helper()
		require.NoError(t, db.QueryRowContext(ctx, `SELECT to_regclass($1::text) IS NOT NULL`, relation).Scan(&found))
		return found
	}
	relations := []string{
		"faucet_raw_events", "faucet_raw_deliveries", "faucet_raw_gaps", "faucet_raw_leases", "faucet_raw_unread", "faucet_subscriptions_raw",
	}
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	arguments := json.RawMessage(`{"repo":"octo/repo"}`)
	const callbackURL = "https://hooks.example.test/raw"
	subscription := faucet.Subscription{
		KeyHash:   faucet.NewSubscriptionKeyHash("octocat", callbackURL, "github.push", arguments),
		Principal: "octocat", CallbackURL: callbackURL, EventType: "github.push", EventTypes: []string{"github.push"},
		Arguments: arguments, Target: faucet.RepoTarget(42), SecretEnc: []byte("sealed"), Cursor: math.MaxInt64, ExpiresAt: now.Add(time.Hour),
	}
	push := faucet.RawDelivery{DeliveryID: "push-1", Event: "push", RepoID: 42}
	serve := func(interests []faucet.RawInterest) faucet.RawPlan {
		plan := faucet.RawPlan{Body: []byte(`{"ref":"refs/heads/main"}`)}
		for _, in := range interests {
			plan.Deliveries = append(plan.Deliveries, faucet.RawOutbox{Key: in.Subscription, Incarnation: in.Incarnation})
		}
		return plan
	}

	row, err := store.UpsertSubscription(ctx, subscription, now, 100)
	require.NoError(t, err)
	stored, err := store.IngestRaw(ctx, push, now, serve)
	require.NoError(t, err)
	require.Equal(t, faucet.RawIngest{Matched: true, ID: 1}, stored)

	migrate("down")
	for _, relation := range relations {
		require.False(t, exists(relation), "%s is gone", relation)
	}
	// This build reads the columns of later migrations, so the build before
	// the raw events is played by its own statements.
	_, err = db.ExecContext(ctx, `INSERT INTO faucet_events (source, delivery_id, type, repo_id, data) VALUES ('github', 'run-1', 'ci.run_completed', 42, '{}')`)
	require.NoError(t, err, "the event log stores as before")
	var scanned int64
	require.NoError(t, db.QueryRowContext(ctx, `SELECT scanned_through FROM faucet_subscriptions WHERE key_hash = $1`, row.KeyHash[:]).Scan(&scanned))
	require.EqualValues(t, math.MaxInt64, scanned, "the raw subscription is a row that the event log never scans")

	migrate("up")
	for _, relation := range relations {
		require.True(t, exists(relation), "%s is back", relation)
	}
	targets, err := store.ScanTargets(ctx, now, 100, 100)
	require.NoError(t, err)
	require.Empty(t, targets, "the raw subscription is still no scan target")
	renewed, err := store.UpsertSubscription(ctx, subscription, now.Add(time.Minute), 100)
	require.NoError(t, err)
	require.True(t, row.CreatedAt.Equal(renewed.CreatedAt), "and its subscriber can still renew it")
	head, err := store.RawHead(ctx)
	require.NoError(t, err)
	require.Zero(t, head, "raw events start empty")
	again, err := store.IngestRaw(ctx, push, now.Add(time.Minute), serve)
	require.NoError(t, err)
	require.Equal(t, faucet.RawIngest{Matched: true, ID: 1}, again, "the delivery is no duplicate, and the subscription that outlived the down gets it")
	claim, ok, err := store.ClaimRawDelivery(ctx, now.Add(time.Minute), time.Minute, nil)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, row.KeyHash, claim.Ref.Key)
}

func runMigrationFile(t *testing.T, db *sql.DB, name string) {
	t.Helper()
	statements, err := os.ReadFile("migrations/" + name)
	require.NoError(t, err)
	_, err = db.ExecContext(context.Background(), string(statements))
	require.NoError(t, err)
}

// A down of the owner targets migration deletes the subscriptions, leases and
// raw events that a build from before it cannot hold, keeps the rows of one
// repository, and an up reads those as repo targets.
func TestTheOwnerTargetsMigrationGoesDownToRepositoryRowsAndUpAgain(t *testing.T) {
	t.Parallel()
	db := newDatabase(t, testDatabaseURL(t))
	store := pgstore.New(db)
	ctx := context.Background()
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	subscription := func(callbackURL string, target faucet.Target) faucet.Subscription {
		arguments := json.RawMessage(`{"repo":"octo/repo"}`)
		return faucet.Subscription{
			KeyHash:   faucet.NewSubscriptionKeyHash("octocat", callbackURL, "ci.run_completed", arguments),
			Principal: "octocat", CallbackURL: callbackURL, EventType: "ci.run_completed", EventTypes: []string{"ci.run_completed"},
			Arguments: arguments, Target: target, SecretEnc: []byte("sealed"), ExpiresAt: now.Add(time.Hour),
		}
	}
	repo := subscription("https://hooks.example.test/repo", faucet.RepoTarget(42))
	for _, sub := range []faucet.Subscription{repo, subscription("https://hooks.example.test/owner", faucet.OwnerTarget(7)), subscription("https://hooks.example.test/all", faucet.AllTarget())} {
		_, err := store.UpsertSubscription(ctx, sub, now, 100)
		require.NoError(t, err)
	}
	for _, lease := range []struct {
		kind            string
		repoID, ownerID int64
	}{{"repo", 42, 0}, {"owner", 0, 7}, {"all", 0, 0}} {
		_, err := db.ExecContext(ctx, `INSERT INTO faucet_raw_leases (id, key_hash, principal, event, target_kind, repo_id, owner_id, acked_through, created_at, polled_at, expires_at)
			VALUES (decode(md5($1 || random()::text), 'hex'), sha256(random()::text::bytea), 'octocat', 'push', $1, $2, $3, 0, $4, $4, $5)`, lease.kind, lease.repoID, lease.ownerID, now, now.Add(time.Hour))
		require.NoError(t, err)
	}
	for _, raw := range []struct {
		deliveryID      string
		repoID, ownerID int64
	}{{"push-1", 42, 7}, {"project-1", 0, 7}} {
		_, err := db.ExecContext(ctx, `INSERT INTO faucet_raw_events (delivery_id, event, action, repo_id, owner_id, received_at) VALUES ($1, 'push', '', $2, $3, $4)`,
			raw.deliveryID, raw.repoID, raw.ownerID, now)
		require.NoError(t, err)
	}
	ev, _, err := store.InsertEvent(ctx, faucet.Event{Source: "github", DeliveryID: "run-1", Type: "ci.run_completed", RepoID: 42, OwnerID: 7, RepoName: "octo/repo", Data: json.RawMessage(`{}`)})
	require.NoError(t, err)
	count := func(query string) (n int) {
		t.Helper()
		require.NoError(t, db.QueryRowContext(ctx, query).Scan(&n))
		return n
	}

	runMigrationFile(t, db, "000014_owner_targets.down.sql")
	require.Equal(t, 1, count(`SELECT COUNT(*) FROM faucet_subscriptions WHERE repo_id = 42`), "the repository's subscription stays")
	require.Equal(t, 1, count(`SELECT COUNT(*) FROM faucet_subscriptions`), "the owner and all subscriptions are gone")
	require.Equal(t, 1, count(`SELECT COUNT(*) FROM faucet_raw_leases`), "so are the owner and all leases")
	require.Equal(t, 1, count(`SELECT COUNT(*) FROM faucet_raw_events WHERE delivery_id = 'push-1'`))
	require.Equal(t, 1, count(`SELECT COUNT(*) FROM faucet_raw_events`), "a raw event of no repository is gone")
	require.Equal(t, 1, count(`SELECT COUNT(*) FROM faucet_events`), "the event log keeps its rows")
	_, err = db.ExecContext(ctx, `UPDATE faucet_subscriptions SET repo_id = 0`)
	require.Error(t, err, "a subscription needs a repository again")

	runMigrationFile(t, db, "000014_owner_targets.up.sql")
	targets, err := store.ScanTargets(ctx, now, 100, 100)
	require.NoError(t, err)
	require.Len(t, targets, 1)
	require.Equal(t, repo.KeyHash, targets[0].KeyHash)
	require.Equal(t, faucet.RepoTarget(42), targets[0].Target, "a row from before the up is a repo target")
	page, err := store.PollEvents(ctx, faucet.PollQuery{EventFilter: faucet.EventFilter{Target: faucet.AllTarget()}, Limit: 10})
	require.NoError(t, err)
	require.Len(t, page.Events, 1)
	require.Equal(t, ev.ID, page.Events[0].ID)
	require.Zero(t, page.Events[0].OwnerID, "an event stored before the up has no owner")
	require.Empty(t, page.Events[0].RepoName)
	for _, bad := range []struct{ repoID, ownerID int64 }{{0, 0}, {-1, 7}, {42, -1}} {
		_, err := db.ExecContext(ctx, `INSERT INTO faucet_raw_events (delivery_id, event, action, repo_id, owner_id, received_at) VALUES ('bad', 'push', '', $1, $2, $3)`,
			bad.repoID, bad.ownerID, now)
		require.Error(t, err, "a raw event after the up names a repository or an owner, and no negative ID: %+v", bad)
	}
}

// The org grants migration binds each org grant of a client made before it to
// the account that holds its login, under the login that the account holds,
// and keeps a grant whose login no account holds, which then reads nothing.
// Its down turns each bound grant back into the login that its account holds
// at the down, so a build that resolves logins reads the same account. A
// grant whose account holds no login keeps its bound form, which that build
// reads as no grant.
func TestTheOrgGrantsMigrationBindsEachLoginToTheAccountThatHoldsItAndBack(t *testing.T) {
	t.Parallel()
	db := newDatabase(t, testDatabaseURL(t))
	store := pgstore.New(db)
	ctx := context.Background()
	require.NoError(t, store.RecordOwner(ctx, faucet.Owner{ID: 7, Login: "Coder"}))
	require.NoError(t, store.RecordOwner(ctx, faucet.Owner{ID: 9, Login: "octo"}))
	grantsOf := func(id string) []string {
		t.Helper()
		var grants []string
		require.NoError(t, db.QueryRowContext(ctx, `SELECT grants FROM faucet_service_clients WHERE client_id = $1`, id).Scan(pq.Array(&grants)))
		return grants
	}

	runMigrationFile(t, db, "000016_org_grant_accounts.down.sql")
	for id, grants := range map[string][]string{
		"orgs":    {"repo:coder/faucet", "org:coder", "all", "org:OCTO", "org:gone"},
		"repos":   {"repo:coder/faucet"},
		"nothing": {},
	} {
		_, err := db.ExecContext(ctx, `INSERT INTO faucet_service_clients (client_id, secret_hash, resource, grants, scopes, created_at)
			VALUES ($1::text, sha256(convert_to($1::text, 'UTF8')), 'https://faucet.example.com/github/mcp', $2, '{events:read}', now())`, id, pq.Array(grants))
		require.NoError(t, err)
	}
	runMigrationFile(t, db, "000016_org_grant_accounts.up.sql")
	require.Equal(t, []string{"repo:coder/faucet", "org:Coder#7", "all", "org:octo#9", "org:gone"}, grantsOf("orgs"))
	require.Equal(t, []string{"repo:coder/faucet"}, grantsOf("repos"))
	require.Empty(t, grantsOf("nothing"))
	c, err := store.Client(ctx, "orgs")
	require.NoError(t, err)
	require.Error(t, c.Validate(), "org:gone names no account, so it is no grant that a new client can hold")

	require.NoError(t, store.RecordOwner(ctx, faucet.Owner{ID: 7, Login: "coder-legacy"}))
	require.NoError(t, store.RecordOwner(ctx, faucet.Owner{ID: 99, Login: "coder"}))
	require.NoError(t, store.RecordOwner(ctx, faucet.Owner{ID: 10, Login: "octo"}))
	runMigrationFile(t, db, "000016_org_grant_accounts.down.sql")
	require.Equal(t, []string{"repo:coder/faucet", "org:coder-legacy", "all", "org:octo#9", "org:gone"}, grantsOf("orgs"),
		"a renamed account's grant takes its new login, not the old one that account 99 holds now, and account 9 lost its login to 10")
}

// The workload trusts migration lets a client have no secret. Its down drops
// the trust rules and gives each client with no secret an all-zero hash,
// which no known secret hashes to, as the build before needs a hash, and its
// up takes that hash back to no secret. No secret logs such a client in at
// any step.
func TestTheWorkloadTrustsMigrationKeepsAClientWithNoSecretWithoutOneDownAndUp(t *testing.T) {
	t.Parallel()
	db := newDatabase(t, testDatabaseURL(t))
	store := pgstore.New(db)
	ctx := context.Background()
	const resource = "https://faucet.example.com/github/mcp"
	server, err := oauth.NewServer(oauth.ServerDeps{Store: store, Clock: quartz.NewReal(), Logger: slog.Make(), Issuer: "https://faucet.example.com", Resources: []string{resource}})
	require.NoError(t, err)
	logIn := func(id, secret string) int {
		t.Helper()
		req := httptest.NewRequestWithContext(ctx, http.MethodPost, oauth.TokenPath, strings.NewReader(url.Values{"grant_type": {"client_credentials"}}.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.SetBasicAuth(url.QueryEscape(id), url.QueryEscape(secret))
		reply := httptest.NewRecorder()
		server.TokenHandler().ServeHTTP(reply, req)
		return reply.Code
	}
	secretHashOf := func(id string) (hash []byte) {
		t.Helper()
		require.NoError(t, db.QueryRowContext(ctx, `SELECT secret_hash FROM faucet_service_clients WHERE client_id = $1`, id).Scan(&hash))
		return hash
	}
	scopes, grants, now := []faucet.Scope{faucet.ScopeEventsRead}, []string{"repo:coder/faucet"}, time.Now()
	withSecret, secret, err := oauth.NewClient("with-secret", resource, scopes, grants, now)
	require.NoError(t, err)
	require.NoError(t, store.InsertClient(ctx, withSecret))
	workloads := []string{"workload-a", "workload-b"}
	for _, id := range workloads {
		c, err := oauth.NewSecretlessClient(id, resource, scopes, grants, now)
		require.NoError(t, err)
		require.NoError(t, store.InsertClient(ctx, c))
	}
	_, err = store.AddTrustRule(ctx, "workload-a", oauth.TrustRule{Issuer: "https://k8s.wi-test.invalid", Claims: map[string]string{"sub": "system:serviceaccount:apps:digest-bot"}}, now)
	require.NoError(t, err)
	requireNoLogIn := func(step string) {
		t.Helper()
		require.Equal(t, http.StatusOK, logIn("with-secret", secret), "%s: the client with a secret logs in", step)
		for _, id := range workloads {
			for _, attempt := range []string{"", secret, "fcs_" + strings.Repeat("A", 43)} {
				require.Equal(t, http.StatusUnauthorized, logIn(id, attempt), "%s: %s logs in with %q", step, id, attempt)
			}
		}
	}
	requireNoLogIn("before the down")

	runMigrationFile(t, db, "000017_workload_trusts.down.sql")
	requireNoLogIn("after the down")
	require.Equal(t, withSecret.SecretHash[:], secretHashOf("with-secret"))
	for _, id := range workloads {
		require.Equal(t, make([]byte, 32), secretHashOf(id), "%s gets an all-zero hash", id)
	}
	_, err = db.ExecContext(ctx, `UPDATE faucet_service_clients SET secret_hash = NULL WHERE client_id = 'workload-a'`)
	require.Error(t, err, "a client needs a secret hash again")
	var trusts *string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT to_regclass('faucet_workload_trusts')::text`).Scan(&trusts))
	require.Nil(t, trusts, "the trust rules are gone")

	runMigrationFile(t, db, "000017_workload_trusts.up.sql")
	requireNoLogIn("after the up")
	for _, id := range workloads {
		c, err := store.Client(ctx, id)
		require.NoError(t, err)
		require.Nil(t, c.SecretHash, "%s has no secret again", id)
	}
	c, err := store.Client(ctx, "with-secret")
	require.NoError(t, err)
	require.Equal(t, withSecret.SecretHash, c.SecretHash)
	workload, err := oauth.NewSecretlessClient("workload-c", resource, scopes, grants, now)
	require.NoError(t, err)
	require.NoError(t, store.InsertClient(ctx, workload), "a client with no secret is taken again")
}

// The claims of a trust rule are an object of strings, as oauth.TrustRule
// holds them. A lax path would unwrap an array and pass it.
func TestATrustRuleHoldsClaimsOfStringsOnly(t *testing.T) {
	t.Parallel()
	db := newDatabase(t, testDatabaseURL(t))
	ctx := context.Background()
	_, err := db.ExecContext(ctx, `INSERT INTO faucet_service_clients (client_id, secret_hash, resource, grants, scopes, created_at)
		VALUES ('digest-bot', NULL, 'https://faucet.example.com/github/mcp', '{repo:coder/faucet}', '{events:read}', now())`)
	require.NoError(t, err)
	insert := func(claims string) error {
		_, err := db.ExecContext(ctx, `INSERT INTO faucet_workload_trusts (client_id, issuer, claims, rule_hash, created_at)
			VALUES ('digest-bot', 'https://k8s.wi-test.invalid', $1::text::jsonb, sha256(convert_to($1::text, 'UTF8')), now())`, claims)
		return err
	}
	for _, claims := range []string{`{"sub":["x"]}`, `{"sub":[]}`, `{"sub":1}`, `{"sub":null}`, `{"sub":{"a":"x"}}`, `{}`, `["x"]`} {
		require.Error(t, insert(claims), claims)
	}
	require.NoError(t, insert(`{"sub":"system:serviceaccount:apps:digest-bot","kubernetes.io/namespace":"apps"}`))
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
	_, err = store.ApplyRedeliveryScan(ctx, redelivery.ScanStep{
		Epoch: lease.Epoch, Now: now, Top: oldTop,
		Open: &redelivery.Range{NewerAt: now.Add(-time.Hour), OlderAt: now.Add(-48 * time.Hour), Cursor: "cursor-1"},
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
	_, err = store.ApplyRedeliveryScan(ctx, redelivery.ScanStep{Epoch: migrated.Epoch, Now: now.Add(30 * time.Minute), Clear: emptyListDate})
	require.ErrorIs(t, err, redelivery.ErrLeaseLost, "the migration ends a lease that a run took before it")
	emptyList, ok, err := store.AcquireRedeliveryLease(ctx, hook, now.Add(30*time.Minute), time.Minute)
	require.NoError(t, err)
	require.True(t, ok)
	_, err = store.ApplyRedeliveryScan(ctx, redelivery.ScanStep{Epoch: emptyList.Epoch, Now: now.Add(30 * time.Minute), Clear: emptyListDate})
	require.NoError(t, err)
	migrate("down")
	require.True(t, emptyListDate.Equal(storedTop().Time), "a scan whose runs read only an empty list since the migration gets the Date of the last one")

	migrate("up")
	read, ok, err := store.AcquireRedeliveryLease(ctx, hook, now.Add(time.Hour), time.Minute)
	require.NoError(t, err)
	require.True(t, ok)
	attempt, date := now.Add(time.Hour), now.Add(time.Hour+time.Second)
	_, err = store.ApplyRedeliveryScan(ctx, redelivery.ScanStep{Epoch: read.Epoch, Now: now.Add(time.Hour), Top: attempt, Clear: date})
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
	// The handle creates the database and drops it, and keeps no connection
	// in between. One idle connection for each test that runs can pass the
	// server's 100 connections.
	admin.SetMaxIdleConns(0)
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
			_, err := store.ApplyRedeliveryScan(ctx, redelivery.ScanStep{Epoch: 1, Now: now})
			return err
		},
		"SettleRedeliveries": func(ctx context.Context) error {
			_, err := store.SettleRedeliveries(ctx, redelivery.SettleRequest{Now: now, Retention: time.Hour, MaxAsks: 3})
			return err
		},
		"ClaimRedelivery": func(ctx context.Context) error {
			_, _, err := store.ClaimRedelivery(ctx, redelivery.ClaimRequest{Now: now, Retention: time.Hour, MaxAsks: 1, Schedule: []time.Duration{time.Minute}})
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

func TestOAuthStoreCallThatItsContextCutShortWrapsTheContextError(t *testing.T) {
	t.Parallel()
	db := newDatabase(t, testDatabaseURL(t))
	store := pgstore.New(db)
	tx, err := db.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })
	for _, table := range []string{"faucet_access_tokens", "faucet_service_clients", "faucet_workload_trusts"} {
		_, err = tx.Exec("LOCK TABLE " + table + " IN ACCESS EXCLUSIVE MODE") //nolint:gosec // The names are constants.
		require.NoError(t, err)
	}

	client, _, err := oauth.NewClient("ci-bot", "https://faucet.example.com/github/mcp", []faucet.Scope{faucet.ScopeEventsRead}, []string{"repo:coder/faucet"}, time.Now())
	require.NoError(t, err)
	token := oauth.AccessToken{
		Hash: oauth.HashToken("fct_locked"), ExpiresAt: time.Now().Add(time.Minute), Scopes: []faucet.Scope{faucet.ScopeEventsRead},
		Resource: "https://faucet.example.com/github/mcp", SubjectKind: oauth.SubjectService, Subject: "ci-bot",
	}
	for name, call := range map[string]func(context.Context) error{
		"InsertAccessToken": func(ctx context.Context) error { return store.InsertAccessToken(ctx, token) },
		"AccessToken": func(ctx context.Context) error {
			_, err := store.AccessToken(ctx, token.Hash)
			return err
		},
		"DeleteExpiredAccessTokens": func(ctx context.Context) error {
			_, err := store.DeleteExpiredAccessTokens(ctx, time.Now())
			return err
		},
		"InsertClient": func(ctx context.Context) error { return store.InsertClient(ctx, client) },
		"Client": func(ctx context.Context) error {
			_, err := store.Client(ctx, client.ID)
			return err
		},
		"Clients": func(ctx context.Context) error {
			_, err := store.Clients(ctx)
			return err
		},
		"DisableClient": func(ctx context.Context) error {
			_, err := store.DisableClient(ctx, client.ID, time.Now())
			return err
		},
		"AddTrustRule": func(ctx context.Context) error {
			_, err := store.AddTrustRule(ctx, client.ID, oauth.TrustRule{Issuer: "https://k8s.wi-test.invalid", Claims: map[string]string{"sub": "system:serviceaccount:apps:digest-bot"}}, time.Now())
			return err
		},
		"TrustRules": func(ctx context.Context) error {
			_, err := store.TrustRules(ctx, client.ID)
			return err
		},
		"RemoveTrustRule": func(ctx context.Context) error { return store.RemoveTrustRule(ctx, client.ID, 1) },
		"CountTrustRules": func(ctx context.Context) error {
			_, err := store.CountTrustRules(ctx)
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

func TestRepositoryCallThatItsContextCutShortWrapsTheContextError(t *testing.T) {
	t.Parallel()
	db := newDatabase(t, testDatabaseURL(t))
	store := pgstore.New(db)
	tx, err := db.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })
	for _, table := range []string{"faucet_repos", "faucet_subscriptions"} {
		_, err = tx.Exec("LOCK TABLE " + table + " IN ACCESS EXCLUSIVE MODE") //nolint:gosec // The names are constants.
		require.NoError(t, err)
	}

	repo := faucet.Repository{ID: 1, OwnerID: 7, OwnerLogin: "coder", FullName: "coder/faucet"}
	for name, call := range map[string]func(context.Context) error{
		"RecordRepository": func(ctx context.Context) error { return store.RecordRepository(ctx, repo, time.Now()) },
		"RenameRepository": func(ctx context.Context) error {
			_, err := store.RenameRepository(ctx, faucet.RepositoryRename{Repo: repo, From: "coder/tap"}, time.Now())
			return err
		},
		"RepositoryByName": func(ctx context.Context) error {
			_, err := store.RepositoryByName(ctx, repo.FullName)
			return err
		},
		"DeleteSubscriptionsOf": func(ctx context.Context) error {
			_, err := store.DeleteSubscriptionsOf(ctx, "svc:ci-bot")
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
