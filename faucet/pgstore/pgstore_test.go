package pgstore_test

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

	"github.com/coder/faucet/faucet"
	"github.com/coder/faucet/faucet/pgstore"
	"github.com/coder/faucet/faucet/storetest"
)

const databaseURLEnv = "FAUCET_TEST_DATABASE_URL"

func TestStore(t *testing.T) {
	t.Parallel()
	adminURL := os.Getenv(databaseURLEnv)
	if adminURL == "" {
		if os.Getenv("CI") != "" {
			t.Fatalf("%s must be set in CI", databaseURLEnv)
		}
		t.Skipf("set %s to a Postgres URL, for example postgres://postgres@127.0.0.1:5432/postgres?sslmode=disable", databaseURLEnv)
	}

	storetest.Run(t, func(t *testing.T) faucet.Store {
		return pgstore.New(newDatabase(t, adminURL))
	})
}

func newDatabase(t *testing.T, adminURL string) *sql.DB {
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
	t.Cleanup(func() {
		_ = db.Close()
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer dropCancel()
		_, err := admin.ExecContext(dropCtx, "DROP DATABASE "+pq.QuoteIdentifier(name)+" WITH (FORCE)")
		require.NoError(t, err)
	})

	require.NoError(t, pgstore.Migrate(ctx, db))
	return db
}
