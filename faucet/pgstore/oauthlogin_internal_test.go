package pgstore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"testing"
	"time"

	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
	"golang.org/x/xerrors"

	"github.com/coder/superblink-events/faucet/internal/pgtest"
	"github.com/coder/superblink-events/faucet/pgstore/database"
)

// Only the check that a refresh token is not both used and retired means
// that a rotation lost to another one. Any other violation is a failure.
func TestOnlyTheUsedOrRetiredCheckIsARotationThatLost(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		err  error
		lost bool
	}{
		"the check":                  {&pq.Error{Code: "23514", Constraint: usedOrRetiredConstraint}, true},
		"the check, wrapped":         {xerrors.Errorf("rotate session: %w", &pq.Error{Code: "23514", Constraint: usedOrRetiredConstraint}), true},
		"another check":              {&pq.Error{Code: "23514", Constraint: "faucet_refresh_tokens_token_hash_check"}, false},
		"another error of the check": {&pq.Error{Code: "23505", Constraint: usedOrRetiredConstraint}, false},
		"an error of no constraint":  {&pq.Error{Code: "23514"}, false},
		"an error of no driver":      {xerrors.New("violates check constraint \"" + usedOrRetiredConstraint + "\""), false},
		"no error":                   {nil, false},
	} {
		require.Equal(t, c.lost, violatesUsedOrRetired(c.err), name)
	}
}

// A rotation of a refresh token that an earlier rotation retired finds no
// row. It does not lean on the check that no token is both used and retired:
// only a rotation that waited for another one of its session needs that
// check, and through the store both end as oauth.ErrUnknownSession, so this
// test reads the statement's own answer.
func TestARotationOfARetiredRefreshTokenFindsNoRow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := pgtest.NewDB(t, Migrate)
	now := time.Now().Truncate(time.Microsecond)
	_, err := db.ExecContext(ctx, `INSERT INTO faucet_connections
		(id, provider, upstream_user_id, login, access_token_enc, refresh_token_enc, key_id, access_expires_at, refresh_expires_at, updated_at)
		VALUES ('github:1', 'github', 1, 'octocat', '\x01', '\x01', 'k1', $1, $1, $1)`, now)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO faucet_user_sessions (id, client_id, client_name, connection_id, resource, scopes, refreshed_at)
		VALUES ('session-1', 'https://app.example.com/client.json', 'app', 'github:1', 'https://faucet.example.com/github/mcp', '{events:read}', $1)`, now)
	require.NoError(t, err)
	retired := sha256.Sum256([]byte("fcr_retired"))
	_, err = db.ExecContext(ctx, `INSERT INTO faucet_refresh_tokens (token_hash, session_id, issued_at, retired_at) VALUES ($1, 'session-1', $2, $2)`, retired[:], now)
	require.NoError(t, err)

	next, access := sha256.Sum256([]byte("fcr_next")), sha256.Sum256([]byte("fct_next"))
	_, err = database.New(db).RotateSession(ctx, database.RotateSessionParams{
		Now: now.Add(time.Minute), PresentedHash: retired[:], UsedAfter: now.Add(30 * time.Second), Scopes: []string{"events:read"},
		NextHash: next[:], AccessHash: access[:], AccessExpiresAt: now.Add(16 * time.Minute),
	})
	require.ErrorIs(t, err, sql.ErrNoRows, "the statement finds no row, and fails no check")
	var tokens int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM faucet_refresh_tokens`).Scan(&tokens))
	require.Equal(t, 1, tokens, "the statement stores no next token")
}
