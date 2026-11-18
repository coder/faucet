// Package pgtest gives a test its own Postgres database.
package pgtest

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

// NewDB creates a database on the server that FAUCET_TEST_DATABASE_URL names,
// runs migrate on it and drops it when the test ends. Without the variable
// the test is skipped, and in CI it fails.
func NewDB(t *testing.T, migrate func(context.Context, *sql.DB) error) *sql.DB {
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
	// The handle creates the database and drops it, and keeps no connection
	// in between. One idle connection for each test that runs can pass the
	// server's 100 connections.
	admin.SetMaxIdleConns(0)
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
	require.NoError(t, migrate(ctx, db))
	return db
}
