package pgstore

import (
	"io"
	"io/fs"
	"testing"

	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/stretchr/testify/require"
)

// golang-migrate applies only the migrations above a database's version, so
// a migration whose number lands below one that a database has run is never
// applied there, and CheckSchema passes all the same. The embedded
// migrations are so numbered 1 to N with no gap, each with its up and its
// down.
func TestTheMigrationsAreNumberedOneToNWithNoGap(t *testing.T) {
	t.Parallel()
	src, err := iofs.New(migrations, "migrations")
	require.NoError(t, err)
	t.Cleanup(func() { _ = src.Close() })

	var versions []uint
	version, err := src.First()
	for err == nil {
		versions = append(versions, version)
		version, err = src.Next(version)
	}
	require.ErrorIs(t, err, fs.ErrNotExist)
	for i, v := range versions {
		require.EqualValues(t, i+1, v, "migration %d comes after %d: the missing numbers belong to a migration that must land first, or this one must take the next free number", v, i)
		for name, read := range map[string]func(uint) (io.ReadCloser, string, error){"up": src.ReadUp, "down": src.ReadDown} {
			r, _, err := read(v)
			require.NoError(t, err, "migration %d has its %s", v, name)
			_ = r.Close()
		}
	}
}
