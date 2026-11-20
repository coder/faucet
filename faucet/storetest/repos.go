package storetest

import (
	"context"
	"crypto/sha256"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/coder/faucet/faucet"
)

var repositorySubtests = []subtest{
	{"RecordRepositoryKeepsTheNamesAnIDWasFirstSeenUnder", recordRepositoryKeepsTheNamesAnIDWasFirstSeenUnder},
	{"RenameRepositoryMovesANameAndANewRepositoryCanTakeTheOldOne", renameRepositoryMovesANameAndANewRepositoryCanTakeTheOldOne},
	{"ARenameFromANameTheRowDoesNotHoldQuarantinesItUnlessLate", aRenameFromANameTheRowDoesNotHoldQuarantinesItUnlessLate},
	{"AKeptOldRenameGivesNoNameBack", aKeptOldRenameGivesNoNameBack},
	{"KeptRenamesThatChainGiveNoNameBack", keptRenamesThatChainGiveNoNameBack},
	{"RenameRepositoryJudgesEachBodyOnce", renameRepositoryJudgesEachBodyOnce},
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
// as a repository event that GitHub sent at at and that renamed it does.
func renameDelivered(t *testing.T, s faucet.Store, repo faucet.Repository, from string, at time.Time) faucet.RenameOutcome {
	t.Helper()
	return renameAs(t, s, renameOf(repo, from, at), at)
}

// renameOf is the rename of a repository event that GitHub sent at at. Its
// Body stands for the signed body, which differs between any two events.
func renameOf(repo faucet.Repository, from string, at time.Time) faucet.RepositoryRename {
	body := sha256.Sum256(fmt.Appendf(nil, "%d %s %s %s", repo.ID, from, repo.FullName, at.Format(time.RFC3339Nano)))
	return faucet.RepositoryRename{Repo: repo, From: from, Body: body}
}

func renameAs(t *testing.T, s faucet.Store, rename faucet.RepositoryRename, at time.Time) faucet.RenameOutcome {
	t.Helper()
	require.NoError(t, s.RecordOwner(context.Background(), faucet.Owner{ID: rename.Repo.OwnerID, Login: rename.Repo.OwnerLogin}))
	outcome, err := s.RenameRepository(context.Background(), rename, at)
	require.NoError(t, err)
	return outcome
}

// late is rename as a delivery that faucet's redelivery asked for.
func late(rename faucet.RepositoryRename) faucet.RepositoryRename {
	rename.Late = true
	return rename
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
	require.Equal(t, faucet.Renamed, renameDelivered(t, s, renamed, "Coder/Faucet", repositoryTime.Add(time.Minute)))
	requireRepositoryNamed(t, s, "acme/tap", renamed)
	requireUnknownName(t, s, "coder/faucet")

	// A new repository takes the old name, and then a replayed delivery of
	// the first repository names it under the old name too.
	recordDelivered(t, s, repositoryNamed(2, "coder/faucet"), repositoryTime.Add(2*time.Minute))
	recordDelivered(t, s, repositoryNamed(1, "coder/faucet"), repositoryTime.Add(3*time.Minute))
	requireRepositoryNamed(t, s, "coder/faucet", repositoryNamed(2, "coder/faucet"))
	requireRepositoryNamed(t, s, "acme/tap", renamed)
}

// A rename that never reached the store, or whose write failed, leaves the
// row with the name from before it. A fresh rename or transfer after it names
// the name that it moved the repository from, which the row does not hold.
// The store cannot tell such a rename from an old one that someone kept and
// sent again, so it takes neither name: the row resolves no name
// (RenameQuarantined). A rename from the name that the rename gave cannot
// lift it either, since kept bodies can chain. When redelivery brings the
// missed rename late, it changes nothing, and it is not judged again when it
// comes fresh.
func aRenameFromANameTheRowDoesNotHoldQuarantinesItUnlessLate(t *testing.T, s faucet.Store) {
	recordDelivered(t, s, repositoryNamed(1, "coder/r"), repositoryTime)
	missed := renameOf(repositoryNamed(1, "coder/s"), "coder/r", repositoryTime.Add(time.Minute))
	moved := faucet.Repository{ID: 1, OwnerID: 8, OwnerLogin: "acme", FullName: "acme/s"}
	require.Equal(t, faucet.RenameQuarantined, renameDelivered(t, s, moved, "coder/s", repositoryTime.Add(2*time.Minute)))
	for _, name := range []string{"coder/r", "coder/s", "acme/s"} {
		requireUnknownName(t, s, name)
	}

	require.Equal(t, faucet.RenameLate, renameAs(t, s, late(missed), repositoryTime.Add(3*time.Minute)))
	require.Equal(t, faucet.RenameSeenBefore, renameAs(t, s, missed, repositoryTime.Add(4*time.Minute)))
	recordDelivered(t, s, moved, repositoryTime.Add(5*time.Minute))
	requireUnknownName(t, s, "acme/s")
	requireUnknownName(t, s, "coder/r")

	next := faucet.Repository{ID: 1, OwnerID: 8, OwnerLogin: "acme", FullName: "acme/t"}
	require.Equal(t, faucet.RenameQuarantined, renameDelivered(t, s, next, "acme/s", repositoryTime.Add(6*time.Minute)))
	for _, name := range []string{"coder/r", "acme/s", "acme/t"} {
		requireUnknownName(t, s, name)
	}
}

// Two kept bodies that chain, a rename to a name and a rename from it, give
// the repository no name back: the second names a name that the row does
// not hold either.
func keptRenamesThatChainGiveNoNameBack(t *testing.T, s faucet.Store) {
	atNew := faucet.Repository{ID: 1, OwnerID: 20, OwnerLogin: "new", FullName: "new/s"}
	recordDelivered(t, s, atNew, repositoryTime)
	old := func(name string) faucet.Repository {
		return faucet.Repository{ID: 1, OwnerID: 10, OwnerLogin: "old", FullName: name}
	}
	require.Equal(t, faucet.RenameQuarantined, renameAs(t, s, renameOf(old("old/r"), "old/a", repositoryTime), repositoryTime.Add(time.Minute)))
	require.Equal(t, faucet.RenameQuarantined, renameAs(t, s, renameOf(old("old/s"), "old/r", repositoryTime), repositoryTime.Add(2*time.Minute)))
	for _, name := range []string{"old/r", "old/s", "new/s"} {
		requireUnknownName(t, s, name)
	}
}

// A kept body that the store never judged, such as one signed before the
// store kept bodies, can come again fresh. When its from is not the name
// that the row holds, it gives the repository no name back: the name that
// the row held stops resolving, and the body's name does not start.
func aKeptOldRenameGivesNoNameBack(t *testing.T, s faucet.Store) {
	atNew := faucet.Repository{ID: 1, OwnerID: 20, OwnerLogin: "new", FullName: "new/r"}
	recordDelivered(t, s, atNew, repositoryTime)
	kept := renameOf(faucet.Repository{ID: 1, OwnerID: 10, OwnerLogin: "old", FullName: "old/r"}, "old/q", repositoryTime)
	require.Equal(t, faucet.RenameQuarantined, renameAs(t, s, kept, repositoryTime.Add(time.Minute)))
	requireUnknownName(t, s, "old/r")
	requireUnknownName(t, s, "new/r")

	// GitHub's next rename names the name that the row held before.
	next := faucet.Repository{ID: 1, OwnerID: 20, OwnerLogin: "new", FullName: "new/s"}
	require.Equal(t, faucet.Renamed, renameDelivered(t, s, next, "new/r", repositoryTime.Add(2*time.Minute)))
	requireRepositoryNamed(t, s, "new/s", next)
	requireUnknownName(t, s, "old/r")
}

// GitHub signs only the body of a delivery, so a body that the store judged
// can come again under any delivery GUID. It changes nothing, fresh or late,
// even after the repository returned to the name that it moved the
// repository from.
func renameRepositoryJudgesEachBodyOnce(t *testing.T, s faucet.Store) {
	atNew := faucet.Repository{ID: 1, OwnerID: 20, OwnerLogin: "new", FullName: "new/r"}
	atOld := faucet.Repository{ID: 1, OwnerID: 10, OwnerLogin: "old", FullName: "old/r"}
	recordDelivered(t, s, atNew, repositoryTime)
	away := renameOf(atOld, "new/r", repositoryTime.Add(time.Minute))
	require.Equal(t, faucet.Renamed, renameAs(t, s, away, repositoryTime.Add(time.Minute)))
	require.Equal(t, faucet.Renamed, renameDelivered(t, s, atNew, "old/r", repositoryTime.Add(2*time.Minute)))

	for _, replay := range []faucet.RepositoryRename{away, late(away)} {
		require.Equal(t, faucet.RenameSeenBefore, renameAs(t, s, replay, repositoryTime.Add(3*time.Minute)))
		requireRepositoryNamed(t, s, "new/r", atNew)
		requireUnknownName(t, s, "old/r")
	}
}

func renameRepositoryOfAnUnknownIDRecordsIt(t *testing.T, s faucet.Store) {
	require.Equal(t, faucet.Renamed, renameAs(t, s, late(renameOf(repositoryNamed(3, "coder/new"), "coder/old", repositoryTime)), repositoryTime))
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
	refused := renameOf(repositoryNamed(1, "coder/tap"), "coder/faucet", repositoryTime)
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
		_, err := s.RenameRepository(ctx, faucet.RepositoryRename{Repo: bad, From: "coder/faucet", Body: refused.Body}, repositoryTime)
		require.Error(t, err, "%+v", bad)
	}
	requireUnknownName(t, s, "coder/faucet")
	require.Equal(t, faucet.Renamed, renameAs(t, s, refused, repositoryTime), "a refused rename records no body")
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
	require.Equal(t, faucet.Renamed, renameDelivered(t, s, tap, "coder/faucet", repositoryTime.Add(2*time.Hour)))
	requireRepositoryNamed(t, s, "coder/tap", tap)
	requireUnknownName(t, s, "coder/faucet")

	older := renameOf(faucet.Repository{ID: 5, OwnerID: 7, OwnerLogin: "coder", FullName: "coder/faucet"}, "old-coder/x", repositoryTime)
	require.Equal(t, faucet.RenameLate, renameAs(t, s, late(older), repositoryTime.Add(3*time.Hour)))
	requireRepositoryNamed(t, s, "coder/tap", tap)
}
