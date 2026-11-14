package dbconn_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/lib/pq"
	"github.com/stretchr/testify/require"

	"github.com/coder/faucet/cmd/faucet/internal/dbconn"
)

const fakePassword = "fake-db-password-7c1f4e"

var openers = map[string]func(string) (*sql.DB, error){"Open": dbconn.Open, "OpenPooled": dbconn.OpenPooled}

func TestOpenRejectsMalformedURLsWithoutTheirPassword(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		url  string
	}{
		{"PortNotANumber", "postgres://app:" + fakePassword + "@db.example.com:notaport/faucet"},
		{"BadEscapeAfterPassword", "postgres://app:" + fakePassword + "%zz@db.example.com/faucet"},
		{"SpaceInHost", "postgres://app:" + fakePassword + "@db exa.example.com/faucet"},
		{"ControlCharacter", "postgresql://app:" + fakePassword + "@db.example.com/faucet\x7f"},
		{"UnclosedIPv6Bracket", "postgresql://app:" + fakePassword + "@[::1/faucet"},
		{"PasswordWhereKeywordBelongs", fakePassword + " host=db.example.com"},
	}
	for opener, open := range openers {
		for _, tt := range tests {
			t.Run(opener+"/"+tt.name, func(t *testing.T) {
				t.Parallel()

				db, err := open(tt.url)
				require.Error(t, err)
				require.Nil(t, db)
				require.Zero(t, strings.Count(err.Error(), fakePassword), "password in error text")
				require.Zero(t, strings.Count(fmt.Sprintf("%+v", err), fakePassword), "password in error chain")
			})
		}
	}
}

func TestOpenAcceptsWellFormedConnectionStrings(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		url  string
	}{
		{"URL", "postgres://app:" + fakePassword + "@db.example.com:5432/faucet?sslmode=disable"},
		{"KeywordValue", "host=db.example.com user=app password=" + fakePassword + " dbname=faucet sslmode=disable"},
	}
	for opener, open := range openers {
		for _, tt := range tests {
			t.Run(opener+"/"+tt.name, func(t *testing.T) {
				t.Parallel()

				db, err := open(tt.url)
				require.NoError(t, err)
				require.NotNil(t, db)
				require.NoError(t, db.Close())
			})
		}
	}
}

// With binary_parameters=yes, lib/pq sends a []byte parameter in binary, so
// the server reads four bytes as an int4. Without it the bytes are text, and
// the server rejects them.
func TestOpenPooledSendsByteParametersInBinary(t *testing.T) {
	t.Parallel()
	const env = "FAUCET_TEST_DATABASE_URL"
	url := os.Getenv(env)
	if url == "" {
		if os.Getenv("CI") != "" {
			t.Fatalf("%s must be set in CI", env)
		}
		t.Skipf("set %s to a Postgres URL", env)
	}
	keywords, err := pq.ParseURL(url)
	require.NoError(t, err)

	bytesAsInt4 := func(open func(string) (*sql.DB, error), dsn string) (int, error) {
		db, err := open(dsn)
		require.NoError(t, err)
		defer func() { _ = db.Close() }()
		var got int
		err = db.QueryRowContext(context.Background(), `SELECT $1::int4`, []byte{0, 0, 0, 42}).Scan(&got)
		return got, err
	}
	for name, dsn := range map[string]string{"URL": url, "KeywordValue": keywords} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got, err := bytesAsInt4(dbconn.OpenPooled, dsn)
			require.NoError(t, err)
			require.Equal(t, 42, got)

			_, err = bytesAsInt4(dbconn.Open, dsn)
			require.Error(t, err, "without the option the bytes are text, so the control proves what the option changes")
		})
	}
}
