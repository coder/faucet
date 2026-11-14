// Package dbconn opens the Postgres connection pool from a connection string.
package dbconn

import (
	"database/sql"
	"strings"

	"github.com/lib/pq"
	"golang.org/x/xerrors"
)

var errInvalid = xerrors.New("not a valid Postgres connection string")

// Open returns a pool for the connection string url. It opens no connection.
//
// The error never quotes url. lib/pq returns parse errors that contain the
// connection string, password included, so Open replaces them.
func Open(url string) (*sql.DB, error) {
	connector, err := pq.NewConnector(url)
	if err != nil {
		return nil, errInvalid
	}
	return sql.OpenDB(connector), nil
}

// OpenPooled is Open for a URL that can go through a transaction-mode pooler,
// such as Neon's pooled URL. It sets binary_parameters=yes. By default lib/pq
// parses a statement and binds it in two batches, and a pooler that changes
// backend between them fails the second with "unnamed prepared statement does
// not exist". The mode sends each statement in one batch.
func OpenPooled(url string) (*sql.DB, error) {
	if strings.HasPrefix(url, "postgres://") || strings.HasPrefix(url, "postgresql://") {
		keywords, err := pq.ParseURL(url)
		if err != nil {
			return nil, errInvalid
		}
		url = keywords
	}
	return Open(url + " binary_parameters=yes")
}
