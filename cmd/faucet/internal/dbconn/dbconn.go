// Package dbconn opens the Postgres connection pool from a connection string.
package dbconn

import (
	"database/sql"

	"github.com/lib/pq"
	"golang.org/x/xerrors"
)

// Open returns a pool for the connection string url. It opens no connection.
//
// The error never quotes url. lib/pq returns parse errors that contain the
// connection string, password included, so Open replaces them.
func Open(url string) (*sql.DB, error) {
	connector, err := pq.NewConnector(url)
	if err != nil {
		return nil, xerrors.New("not a valid Postgres connection string")
	}
	return sql.OpenDB(connector), nil
}
