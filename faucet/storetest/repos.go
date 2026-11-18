package storetest

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/coder/faucet/faucet"
)

var repositorySubtests = []subtest{
	{"RecordRepositoryKeepsTheNamesAnIDWasFirstSeenUnder", recordRepositoryKeepsTheNamesAnIDWasFirstSeenUnder},
	{"RenameRepositoryMovesANameAndANewRepositoryCanTakeTheOldOne", renameRepositoryMovesANameAndANewRepositoryCanTakeTheOldOne},
	{"RenameRepositoryFromANameTheRowDoesNotHoldChangesNothing", renameRepositoryFromANameTheRowDoesNotHoldChangesNothing},
	{"RenameRepositoryOfAnUnknownIDRecordsIt", renameRepositoryOfAnUnknownIDRecordsIt},
	{"RepositoryByNameIgnoresCaseAndTakesTheLatestHolderOfAName", repositoryByNameIgnoresCaseAndTakesTheLatestHolderOfAName},
	{"RepositoryByNameOfAnUnseenNameIsUnknown", repositoryByNameOfAnUnseenNameIsUnknown},
	{"RecordAndRenameRepositoryValidateInput", recordAndRenameRepositoryValidateInput},
}

var repositoryTime = time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)

func repositoryNamed(id int64, fullName string) faucet.Repository {
	return faucet.Repository{ID: id, OwnerID: 7, OwnerLogin: "coder", FullName: fullName}
}

func requireRepositoryNamed(t *testing.T, s faucet.Store, fullName string, want faucet.Repository) {
	t.Helper()
	got, err := s.RepositoryByName(context.Background(), fullName)
	require.NoError(t, err, fullName)
	require.Equal(t, want, got, fullName)
}

func requireUnknownName(t *testing.T, s faucet.Store, fullName string) {
	t.Helper()
	_, err := s.RepositoryByName(context.Background(), fullName)
	require.ErrorIs(t, err, faucet.ErrUnknownRepository, fullName)
}

// A replayed or late delivery names a repository as it was then. Recording it
// must not move a name back to the repository.
func recordRepositoryKeepsTheNamesAnIDWasFirstSeenUnder(t *testing.T, s faucet.Store) {
	ctx := context.Background()
	require.NoError(t, s.RecordRepository(ctx, repositoryNamed(1, "coder/faucet"), repositoryTime))
	requireRepositoryNamed(t, s, "coder/faucet", repositoryNamed(1, "coder/faucet"))

	require.NoError(t, s.RecordRepository(ctx, faucet.Repository{ID: 1, OwnerID: 8, OwnerLogin: "acme", FullName: "acme/old"}, repositoryTime.Add(time.Hour)))
	requireRepositoryNamed(t, s, "coder/faucet", repositoryNamed(1, "coder/faucet"))
	requireUnknownName(t, s, "acme/old")
}

func renameRepositoryMovesANameAndANewRepositoryCanTakeTheOldOne(t *testing.T, s faucet.Store) {
	ctx := context.Background()
	require.NoError(t, s.RecordRepository(ctx, repositoryNamed(1, "coder/faucet"), repositoryTime))
	renamed := faucet.Repository{ID: 1, OwnerID: 8, OwnerLogin: "acme", FullName: "acme/tap"}
	require.NoError(t, s.RenameRepository(ctx, renamed, "Coder/Faucet", repositoryTime.Add(time.Minute)))
	requireRepositoryNamed(t, s, "acme/tap", renamed)
	requireUnknownName(t, s, "coder/faucet")

	// A new repository takes the old name, and then a replayed delivery of
	// the first repository names it under the old name too.
	require.NoError(t, s.RecordRepository(ctx, repositoryNamed(2, "coder/faucet"), repositoryTime.Add(2*time.Minute)))
	require.NoError(t, s.RecordRepository(ctx, repositoryNamed(1, "coder/faucet"), repositoryTime.Add(3*time.Minute)))
	requireRepositoryNamed(t, s, "coder/faucet", repositoryNamed(2, "coder/faucet"))
	requireRepositoryNamed(t, s, "acme/tap", renamed)
}

// GitHub can deliver an older rename again after a newer one. Its from
// names a name that the row no longer holds, so it changes nothing.
func renameRepositoryFromANameTheRowDoesNotHoldChangesNothing(t *testing.T, s faucet.Store) {
	ctx := context.Background()
	require.NoError(t, s.RecordRepository(ctx, repositoryNamed(1, "coder/x"), repositoryTime))
	require.NoError(t, s.RenameRepository(ctx, repositoryNamed(1, "coder/y"), "coder/x", repositoryTime.Add(time.Minute)))
	require.NoError(t, s.RenameRepository(ctx, repositoryNamed(1, "coder/z"), "coder/y", repositoryTime.Add(2*time.Minute)))
	require.NoError(t, s.RenameRepository(ctx, repositoryNamed(1, "coder/y"), "coder/x", repositoryTime.Add(3*time.Minute)))
	requireRepositoryNamed(t, s, "coder/z", repositoryNamed(1, "coder/z"))
	requireUnknownName(t, s, "coder/y")
}

func renameRepositoryOfAnUnknownIDRecordsIt(t *testing.T, s faucet.Store) {
	ctx := context.Background()
	require.NoError(t, s.RenameRepository(ctx, repositoryNamed(3, "coder/new"), "coder/old", repositoryTime))
	requireRepositoryNamed(t, s, "coder/new", repositoryNamed(3, "coder/new"))
}

func repositoryByNameIgnoresCaseAndTakesTheLatestHolderOfAName(t *testing.T, s faucet.Store) {
	ctx := context.Background()
	require.NoError(t, s.RecordRepository(ctx, repositoryNamed(1, "coder/faucet"), repositoryTime))
	requireRepositoryNamed(t, s, "Coder/Faucet", repositoryNamed(1, "coder/faucet"))

	// The first repository was deleted with no event that says so, and a new
	// one took its name.
	require.NoError(t, s.RecordRepository(ctx, repositoryNamed(2, "coder/Faucet"), repositoryTime.Add(time.Hour)))
	requireRepositoryNamed(t, s, "coder/faucet", repositoryNamed(2, "coder/Faucet"))
}

func repositoryByNameOfAnUnseenNameIsUnknown(t *testing.T, s faucet.Store) {
	ctx := context.Background()
	require.NoError(t, s.RecordRepository(ctx, repositoryNamed(1, "coder/faucet"), repositoryTime))
	for _, name := range []string{"coder/fauce", "coder/faucet2", "other/faucet", "coder/%", "coder/_aucet"} {
		requireUnknownName(t, s, name)
	}
}

func recordAndRenameRepositoryValidateInput(t *testing.T, s faucet.Store) {
	ctx := context.Background()
	for _, bad := range []faucet.Repository{
		{},
		{ID: 1, OwnerID: 7, OwnerLogin: "coder", FullName: "faucet"},
		{ID: 1, OwnerID: 7, OwnerLogin: "coder", FullName: "coder/"},
		{ID: 1, OwnerID: 7, OwnerLogin: "coder", FullName: "coder/a/b"},
		{ID: 1, OwnerID: 7, OwnerLogin: "", FullName: "coder/faucet"},
		{ID: 1, OwnerID: 0, OwnerLogin: "coder", FullName: "coder/faucet"},
		{ID: -1, OwnerID: 7, OwnerLogin: "coder", FullName: "coder/faucet"},
		{ID: 1, OwnerID: 7, OwnerLogin: "coder", FullName: "coder/fau\x00cet"},
	} {
		require.Error(t, s.RecordRepository(ctx, bad, repositoryTime), "%+v", bad)
		require.Error(t, s.RenameRepository(ctx, bad, "coder/faucet", repositoryTime), "%+v", bad)
	}
	requireUnknownName(t, s, "coder/faucet")
}
