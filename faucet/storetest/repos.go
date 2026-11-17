package storetest

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/coder/faucet/faucet"
)

var repositorySubtests = []subtest{
	{"RepositoryByNameReadsBackTheLatestNamesOfAnID", repositoryByNameReadsBackTheLatestNamesOfAnID},
	{"RepositoryByNameIgnoresCaseAndTakesTheLatestHolderOfAName", repositoryByNameIgnoresCaseAndTakesTheLatestHolderOfAName},
	{"RepositoryByNameOfAnUnseenNameIsUnknown", repositoryByNameOfAnUnseenNameIsUnknown},
	{"UpsertRepositoryValidatesInput", upsertRepositoryValidatesInput},
}

func repositoryNamed(id int64, fullName string) faucet.Repository {
	return faucet.Repository{ID: id, OwnerID: 7, OwnerLogin: "coder", FullName: fullName}
}

func repositoryByNameReadsBackTheLatestNamesOfAnID(t *testing.T, s faucet.Store) {
	ctx := context.Background()
	at := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	require.NoError(t, s.UpsertRepository(ctx, repositoryNamed(1, "coder/faucet"), at))
	got, err := s.RepositoryByName(ctx, "coder/faucet")
	require.NoError(t, err)
	require.Equal(t, repositoryNamed(1, "coder/faucet"), got)

	renamed := faucet.Repository{ID: 1, OwnerID: 8, OwnerLogin: "acme", FullName: "acme/tap"}
	require.NoError(t, s.UpsertRepository(ctx, renamed, at.Add(time.Minute)))
	got, err = s.RepositoryByName(ctx, "acme/tap")
	require.NoError(t, err)
	require.Equal(t, renamed, got)
	_, err = s.RepositoryByName(ctx, "coder/faucet")
	require.ErrorIs(t, err, faucet.ErrUnknownRepository, "a rename frees the old name")
}

func repositoryByNameIgnoresCaseAndTakesTheLatestHolderOfAName(t *testing.T, s faucet.Store) {
	ctx := context.Background()
	at := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	require.NoError(t, s.UpsertRepository(ctx, repositoryNamed(1, "coder/faucet"), at))
	got, err := s.RepositoryByName(ctx, "Coder/Faucet")
	require.NoError(t, err)
	require.Equal(t, int64(1), got.ID)

	// The first repository was deleted without a delivery that says so, and a
	// new one took its name.
	require.NoError(t, s.UpsertRepository(ctx, repositoryNamed(2, "coder/Faucet"), at.Add(time.Hour)))
	got, err = s.RepositoryByName(ctx, "coder/faucet")
	require.NoError(t, err)
	require.Equal(t, repositoryNamed(2, "coder/Faucet"), got)
}

func repositoryByNameOfAnUnseenNameIsUnknown(t *testing.T, s faucet.Store) {
	ctx := context.Background()
	require.NoError(t, s.UpsertRepository(ctx, repositoryNamed(1, "coder/faucet"), time.Now()))
	for _, name := range []string{"coder/fauce", "coder/faucet2", "other/faucet", "coder/%", "coder/_aucet"} {
		_, err := s.RepositoryByName(ctx, name)
		require.ErrorIs(t, err, faucet.ErrUnknownRepository, name)
	}
}

func upsertRepositoryValidatesInput(t *testing.T, s faucet.Store) {
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
		require.Error(t, s.UpsertRepository(ctx, bad, time.Now()), "%+v", bad)
	}
	_, err := s.RepositoryByName(ctx, "coder/faucet")
	require.ErrorIs(t, err, faucet.ErrUnknownRepository, "a refused repository is not stored")
}
