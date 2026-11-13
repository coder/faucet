package dbconn_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/coder/faucet/cmd/faucet/internal/dbconn"
)

const fakePassword = "fake-db-password-7c1f4e"

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
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			db, err := dbconn.Open(tt.url)
			require.Error(t, err)
			require.Nil(t, db)
			require.Zero(t, strings.Count(err.Error(), fakePassword), "password in error text")
			require.Zero(t, strings.Count(fmt.Sprintf("%+v", err), fakePassword), "password in error chain")
		})
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
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			db, err := dbconn.Open(tt.url)
			require.NoError(t, err)
			require.NotNil(t, db)
			require.NoError(t, db.Close())
		})
	}
}
