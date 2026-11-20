package pgstore_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/coder/superblink-events/faucet/oauth"
	"github.com/coder/superblink-events/faucet/pgstore"
)

// A use of a fresh access check that moves no last use writes no row, so
// that the calls of a busy agent cost no write each. A use that moves the
// last use writes the row.
func TestAUseOfAnAccessCheckThatMovesNoLastUseWritesNoRow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := newDatabase(t, testDatabaseURL(t))
	store := pgstore.New(db)
	now := time.Now().Truncate(time.Microsecond)
	for _, stmt := range []string{
		`INSERT INTO faucet_connections (id, provider, upstream_user_id, login, access_token_enc, refresh_token_enc, key_id, access_expires_at, refresh_expires_at, updated_at)
		VALUES ('github:1', 'github', 1, 'octocat', '\x01', '\x01', 'k1', $1, $1, $1)`,
		`INSERT INTO faucet_user_sessions (id, client_id, client_name, connection_id, resource, scopes, refreshed_at)
		VALUES ('session-1', 'https://agent.example.com/client.json', 'Agent', 'github:1', 'https://faucet.example.com/github/mcp', '{events:read}', $1)`,
	} {
		_, err := db.ExecContext(ctx, stmt, now)
		require.NoError(t, err)
	}
	require.NoError(t, store.StoreAccessCheck(ctx, oauth.AccessCheck{ConnectionID: "github:1", Repo: "o/r", RepoID: 11, CheckedAt: now, LastUsedAt: now}))
	stored := rowVersion(t, db)

	_, ok, err := store.FreshAccessCheck(ctx, "session-1", "o/r", now.Add(30*time.Second), now)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, stored, rowVersion(t, db), "a use that moves no last use writes no row")
	_, ok, err = store.FreshAccessCheck(ctx, "session-1", "o/r", now.Add(time.Minute), now.Add(time.Second))
	require.NoError(t, err)
	require.True(t, ok)
	require.NotEqual(t, stored, rowVersion(t, db), "a use that moves the last use writes the row")
}

// rowVersion is the ID of the transaction that wrote the access check of
// o/r, which every write of the row changes.
func rowVersion(t *testing.T, db *sql.DB) string {
	t.Helper()
	var xmin string
	require.NoError(t, db.QueryRowContext(context.Background(), `SELECT xmin::text FROM faucet_access_checks WHERE connection_id = 'github:1' AND repo = 'o/r'`).Scan(&xmin))
	return xmin
}
