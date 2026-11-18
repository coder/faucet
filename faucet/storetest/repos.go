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
	{"OwnerByLoginIgnoresCaseAndTakesTheOwnerSeenLatest", ownerByLoginIgnoresCaseAndTakesTheOwnerSeenLatest},
	{"OwnerByLoginOfAnUnseenLoginIsUnknown", ownerByLoginOfAnUnseenLoginIsUnknown},
	{"AnOwnerKeepsOnlyTheLoginOfItsLatestDelivery", anOwnerKeepsOnlyTheLoginOfItsLatestDelivery},
	{"RecordOwnerNamesAnAccountWithoutARepository", recordOwnerNamesAnAccountWithoutARepository},
	{"AddOwnerAddsAnAccountAndMovesNoLogin", addOwnerAddsAnAccountAndMovesNoLogin},
	{"ARepositoryOfARenamedAccountIsFoundAndRenamedUnderItsNewLogin", aRepositoryOfARenamedAccountIsFoundAndRenamedUnderItsNewLogin},
}

var repositoryTime = time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)

func repositoryNamed(id int64, fullName string) faucet.Repository {
	return faucet.Repository{ID: id, OwnerID: 7, OwnerLogin: "coder", FullName: fullName}
}

// recordDelivered records repo and the login of its owner, as a delivery
// that names repo does.
func recordDelivered(t *testing.T, s faucet.Store, repo faucet.Repository, at time.Time) {
	t.Helper()
	require.NoError(t, s.RecordOwner(context.Background(), faucet.Owner{ID: repo.OwnerID, Login: repo.OwnerLogin}))
	require.NoError(t, s.RecordRepository(context.Background(), repo, at))
}

// renameDelivered renames repo from from and records the login of its owner,
// as a repository event that renamed it does.
func renameDelivered(t *testing.T, s faucet.Store, repo faucet.Repository, from string, at time.Time) {
	t.Helper()
	require.NoError(t, s.RecordOwner(context.Background(), faucet.Owner{ID: repo.OwnerID, Login: repo.OwnerLogin}))
	require.NoError(t, s.RenameRepository(context.Background(), repo, from, at))
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
	recordDelivered(t, s, repositoryNamed(1, "coder/faucet"), repositoryTime)
	requireRepositoryNamed(t, s, "coder/faucet", repositoryNamed(1, "coder/faucet"))

	recordDelivered(t, s, faucet.Repository{ID: 1, OwnerID: 8, OwnerLogin: "acme", FullName: "acme/old"}, repositoryTime.Add(time.Hour))
	requireRepositoryNamed(t, s, "coder/faucet", repositoryNamed(1, "coder/faucet"))
	requireUnknownName(t, s, "acme/old")
}

func renameRepositoryMovesANameAndANewRepositoryCanTakeTheOldOne(t *testing.T, s faucet.Store) {
	recordDelivered(t, s, repositoryNamed(1, "coder/faucet"), repositoryTime)
	renamed := faucet.Repository{ID: 1, OwnerID: 8, OwnerLogin: "acme", FullName: "acme/tap"}
	renameDelivered(t, s, renamed, "Coder/Faucet", repositoryTime.Add(time.Minute))
	requireRepositoryNamed(t, s, "acme/tap", renamed)
	requireUnknownName(t, s, "coder/faucet")

	// A new repository takes the old name, and then a replayed delivery of
	// the first repository names it under the old name too.
	recordDelivered(t, s, repositoryNamed(2, "coder/faucet"), repositoryTime.Add(2*time.Minute))
	recordDelivered(t, s, repositoryNamed(1, "coder/faucet"), repositoryTime.Add(3*time.Minute))
	requireRepositoryNamed(t, s, "coder/faucet", repositoryNamed(2, "coder/faucet"))
	requireRepositoryNamed(t, s, "acme/tap", renamed)
}

// GitHub can deliver an older rename again after a newer one. Its from
// names a name that the row no longer holds, so it changes nothing.
func renameRepositoryFromANameTheRowDoesNotHoldChangesNothing(t *testing.T, s faucet.Store) {
	recordDelivered(t, s, repositoryNamed(1, "coder/x"), repositoryTime)
	renameDelivered(t, s, repositoryNamed(1, "coder/y"), "coder/x", repositoryTime.Add(time.Minute))
	renameDelivered(t, s, repositoryNamed(1, "coder/z"), "coder/y", repositoryTime.Add(2*time.Minute))
	renameDelivered(t, s, repositoryNamed(1, "coder/y"), "coder/x", repositoryTime.Add(3*time.Minute))
	requireRepositoryNamed(t, s, "coder/z", repositoryNamed(1, "coder/z"))
	requireUnknownName(t, s, "coder/y")
}

func renameRepositoryOfAnUnknownIDRecordsIt(t *testing.T, s faucet.Store) {
	renameDelivered(t, s, repositoryNamed(3, "coder/new"), "coder/old", repositoryTime)
	requireRepositoryNamed(t, s, "coder/new", repositoryNamed(3, "coder/new"))
}

func repositoryByNameIgnoresCaseAndTakesTheLatestHolderOfAName(t *testing.T, s faucet.Store) {
	recordDelivered(t, s, repositoryNamed(1, "coder/faucet"), repositoryTime)
	requireRepositoryNamed(t, s, "Coder/Faucet", repositoryNamed(1, "coder/faucet"))

	// The first repository was deleted with no event that says so, and a new
	// one took its name.
	recordDelivered(t, s, repositoryNamed(2, "coder/Faucet"), repositoryTime.Add(time.Hour))
	requireRepositoryNamed(t, s, "coder/faucet", repositoryNamed(2, "coder/Faucet"))
}

func repositoryByNameOfAnUnseenNameIsUnknown(t *testing.T, s faucet.Store) {
	recordDelivered(t, s, repositoryNamed(1, "coder/faucet"), repositoryTime)
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

// A login can move to another account, as when an organization is renamed
// and another takes its old name. The account seen latest under a login
// holds it, and the one that held it before gets it back only from a
// delivery that names it so again.
func ownerByLoginIgnoresCaseAndTakesTheOwnerSeenLatest(t *testing.T, s faucet.Store) {
	ctx := context.Background()
	recordDelivered(t, s, faucet.Repository{ID: 1, OwnerID: 7, OwnerLogin: "Coder", FullName: "Coder/faucet"}, repositoryTime)
	recordDelivered(t, s, faucet.Repository{ID: 2, OwnerID: 7, OwnerLogin: "coder", FullName: "coder/tap"}, repositoryTime.Add(time.Minute))

	got, err := s.OwnerByLogin(ctx, "CODER")
	require.NoError(t, err)
	require.Equal(t, faucet.Owner{ID: 7, Login: "coder"}, got, "the login matches in any case, and the latest delivery gives the written login")

	recordDelivered(t, s, faucet.Repository{ID: 3, OwnerID: 9, OwnerLogin: "coder", FullName: "coder/new"}, repositoryTime.Add(time.Hour))
	got, err = s.OwnerByLogin(ctx, "coder")
	require.NoError(t, err)
	require.Equal(t, faucet.Owner{ID: 9, Login: "coder"}, got, "the account seen latest under the login holds it")
	requireRepositoryNamed(t, s, "coder/new", faucet.Repository{ID: 3, OwnerID: 9, OwnerLogin: "coder", FullName: "coder/new"})
	requireUnknownName(t, s, "coder/faucet")

	require.NoError(t, s.RecordOwner(ctx, faucet.Owner{ID: 9, Login: "acme"}))
	_, err = s.OwnerByLogin(ctx, "coder")
	require.ErrorIs(t, err, faucet.ErrUnknownOwner, "the account that gave the login up does not get it back")
	_, err = s.OwnerByID(ctx, 7)
	require.ErrorIs(t, err, faucet.ErrUnknownOwner, "the account that gave its login up holds none")
	requireUnknownName(t, s, "coder/tap")

	require.NoError(t, s.RecordOwner(ctx, faucet.Owner{ID: 7, Login: "coder"}))
	requireRepositoryNamed(t, s, "coder/tap", faucet.Repository{ID: 2, OwnerID: 7, OwnerLogin: "coder", FullName: "coder/tap"})
}

// An organization's own events name it with no repository.
func recordOwnerNamesAnAccountWithoutARepository(t *testing.T, s faucet.Store) {
	ctx := context.Background()
	require.NoError(t, s.RecordOwner(ctx, faucet.Owner{ID: 5, Login: "Acme"}))
	got, err := s.OwnerByLogin(ctx, "acme")
	require.NoError(t, err)
	require.Equal(t, faucet.Owner{ID: 5, Login: "Acme"}, got)
	requireUnknownName(t, s, "acme/tap")

	for _, bad := range []faucet.Owner{{}, {ID: 5}, {Login: "acme"}, {ID: -1, Login: "acme"}, {ID: 5, Login: "ac/me"}, {ID: 5, Login: "ac\x00me"}} {
		require.Error(t, s.RecordOwner(ctx, bad), "%+v", bad)
	}
	got, err = s.OwnerByLogin(ctx, "acme")
	require.NoError(t, err)
	require.Equal(t, faucet.Owner{ID: 5, Login: "Acme"}, got, "a refused owner changes nothing")
}

func ownerByLoginOfAnUnseenLoginIsUnknown(t *testing.T, s faucet.Store) {
	ctx := context.Background()
	recordDelivered(t, s, repositoryNamed(1, "coder/faucet"), repositoryTime)
	for _, login := range []string{"code", "coder2", "faucet", "c%", "_oder", ""} {
		_, err := s.OwnerByLogin(ctx, login)
		require.ErrorIs(t, err, faucet.ErrUnknownOwner, login)
	}
}

// An account that GitHub renamed sends no event for each of its
// repositories, so only its next delivery tells faucet the new login. From
// then on the old login names no account: whoever takes it at GitHub gets
// none of the renamed account's repositories, by an org grant or by name.
func anOwnerKeepsOnlyTheLoginOfItsLatestDelivery(t *testing.T, s faucet.Store) {
	ctx := context.Background()
	recordDelivered(t, s, faucet.Repository{ID: 100, OwnerID: 7, OwnerLogin: "acme", FullName: "acme/private"}, repositoryTime)
	renamed := faucet.Repository{ID: 100, OwnerID: 7, OwnerLogin: "acme-legacy", FullName: "acme-legacy/private"}
	recordDelivered(t, s, renamed, repositoryTime.Add(time.Hour))

	_, err := s.OwnerByLogin(ctx, "acme")
	require.ErrorIs(t, err, faucet.ErrUnknownOwner, "the old login names no account")
	got, err := s.OwnerByLogin(ctx, "acme-legacy")
	require.NoError(t, err)
	require.Equal(t, faucet.Owner{ID: 7, Login: "acme-legacy"}, got)
	requireUnknownName(t, s, "acme/private")
	requireRepositoryNamed(t, s, "ACME-legacy/private", renamed)
	got, err = s.OwnerByID(ctx, 7)
	require.NoError(t, err)
	require.Equal(t, faucet.Owner{ID: 7, Login: "acme-legacy"}, got)
	_, err = s.OwnerByID(ctx, 8)
	require.ErrorIs(t, err, faucet.ErrUnknownOwner)
}

// A delivery that GitHub sent again names an account as it was then: it may
// add an account that the store lacks, but it moves no login.
func addOwnerAddsAnAccountAndMovesNoLogin(t *testing.T, s faucet.Store) {
	ctx := context.Background()
	require.NoError(t, s.RecordOwner(ctx, faucet.Owner{ID: 7, Login: "acme"}))
	require.NoError(t, s.AddOwner(ctx, faucet.Owner{ID: 7, Login: "acme-old"}))
	require.NoError(t, s.AddOwner(ctx, faucet.Owner{ID: 9, Login: "ACME"}))
	got, err := s.OwnerByLogin(ctx, "acme")
	require.NoError(t, err)
	require.Equal(t, faucet.Owner{ID: 7, Login: "acme"}, got, "an account with a row keeps its login, and a login that one holds stays with it")
	_, err = s.OwnerByLogin(ctx, "acme-old")
	require.ErrorIs(t, err, faucet.ErrUnknownOwner)

	require.NoError(t, s.AddOwner(ctx, faucet.Owner{ID: 9, Login: "octo"}))
	got, err = s.OwnerByLogin(ctx, "octo")
	require.NoError(t, err)
	require.Equal(t, faucet.Owner{ID: 9, Login: "octo"}, got, "an account that the store lacks is added")
	require.Error(t, s.AddOwner(ctx, faucet.Owner{ID: 5, Login: "ac/me"}))
}

// GitHub sends no repository event when it renames an account, so the row of
// a repository keeps the old login. The account's next delivery gives it the
// new login, a lookup finds the repository under the new login, and a later
// rename of the repository, whose from GitHub writes under the new login,
// moves it.
func aRepositoryOfARenamedAccountIsFoundAndRenamedUnderItsNewLogin(t *testing.T, s faucet.Store) {
	recordDelivered(t, s, faucet.Repository{ID: 5, OwnerID: 7, OwnerLogin: "old-coder", FullName: "old-coder/faucet"}, repositoryTime)
	recordDelivered(t, s, faucet.Repository{ID: 5, OwnerID: 7, OwnerLogin: "coder", FullName: "coder/faucet"}, repositoryTime.Add(time.Hour))
	requireRepositoryNamed(t, s, "coder/faucet", faucet.Repository{ID: 5, OwnerID: 7, OwnerLogin: "coder", FullName: "coder/faucet"})
	requireUnknownName(t, s, "old-coder/faucet")

	tap := faucet.Repository{ID: 5, OwnerID: 7, OwnerLogin: "coder", FullName: "coder/tap"}
	renameDelivered(t, s, tap, "coder/faucet", repositoryTime.Add(2*time.Hour))
	requireRepositoryNamed(t, s, "coder/tap", tap)
	requireUnknownName(t, s, "coder/faucet")

	renameDelivered(t, s, faucet.Repository{ID: 5, OwnerID: 7, OwnerLogin: "coder", FullName: "coder/faucet"}, "old-coder/x", repositoryTime.Add(3*time.Hour))
	requireRepositoryNamed(t, s, "coder/tap", tap)
}
