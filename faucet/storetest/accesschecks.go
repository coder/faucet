package storetest

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/coder/faucet/faucet/oauth"
)

// accessCheckSubtests is the contract of oauth.AccessCheckStore, one
// behavior per name.
var accessCheckSubtests = []struct {
	name string
	run  func(t *testing.T, s oauth.Store)
}{
	{"ACheckThatStartedAtOrAfterTheRefreshOfTheSessionIsFresh", aCheckThatStartedAtOrAfterTheRefreshOfTheSessionIsFresh},
	{"EachSessionOfAConnectionJudgesACheckByItsOwnRefresh", eachSessionOfAConnectionJudgesACheckByItsOwnRefresh},
	{"ARotationMakesAnOlderCheckStale", aRotationMakesAnOlderCheckStale},
	{"AFreshCheckMovesItsLastUseForwardOnly", aFreshCheckMovesItsLastUseForwardOnly},
	{"AMissChangesNothing", aMissChangesNothing},
	{"AnOlderCheckNeverReplacesALaterOne", anOlderCheckNeverReplacesALaterOne},
	{"StoreAccessCheckOfAnUnknownConnectionIsUnknown", storeAccessCheckOfAnUnknownConnectionIsUnknown},
	{"StoreAccessCheckValidatesInput", storeAccessCheckValidatesInput},
	{"DeleteAccessCheckDeletesThatCheckOnly", deleteAccessCheckDeletesThatCheckOnly},
	{"RecentAccessChecksAreTheMostRecentlyUsedOfTheConnection", recentAccessChecksAreTheMostRecentlyUsedOfTheConnection},
	{"DeleteUnusedConnectionsDeletesTheirAccessChecks", deleteUnusedConnectionsDeletesTheirAccessChecks},
}

// runAccessChecks runs the oauth.AccessCheckStore contract. A check belongs
// to a connection and is judged by a session, which a login stores.
func runAccessChecks(t *testing.T, newStore func(t *testing.T) oauth.Store) {
	for _, st := range accessCheckSubtests {
		t.Run(st.name, func(t *testing.T) {
			t.Parallel()
			st.run(t, newStore(t))
		})
	}
}

// checkOf is the check of repo by c, which found the repository ID id, whose
// lookup started at checked, and which was last used then.
func checkOf(c oauth.Connection, repo string, id int64, checked time.Time) oauth.AccessCheck {
	return oauth.AccessCheck{ConnectionID: c.ID, Repo: repo, RepoID: id, CheckedAt: checked, LastUsedAt: checked}
}

func storeCheck(t *testing.T, s oauth.Store, c oauth.AccessCheck) {
	t.Helper()
	require.NoError(t, s.StoreAccessCheck(context.Background(), c))
}

// requireChecks requires that the checks of the connection with this ID,
// the most recently used first, are want.
func requireChecks(t *testing.T, s oauth.Store, connectionID string, want []oauth.AccessCheck, msgAndArgs ...any) {
	t.Helper()
	got, err := s.RecentAccessChecks(context.Background(), connectionID, len(want)+10)
	require.NoError(t, err, msgAndArgs...)
	requireSameChecks(t, want, got, msgAndArgs...)
}

func requireSameChecks(t *testing.T, want, got []oauth.AccessCheck, msgAndArgs ...any) {
	t.Helper()
	require.Len(t, got, len(want), msgAndArgs...)
	if len(want) == 0 {
		return
	}
	for i := range want {
		require.True(t, want[i].CheckedAt.Equal(got[i].CheckedAt), "check %d checked_at: want %v, got %v", i, want[i].CheckedAt, got[i].CheckedAt)
		require.True(t, want[i].LastUsedAt.Equal(got[i].LastUsedAt), "check %d last_used_at: want %v, got %v", i, want[i].LastUsedAt, got[i].LastUsedAt)
		got[i].CheckedAt, got[i].LastUsedAt = want[i].CheckedAt, want[i].LastUsedAt
	}
	require.Equal(t, want, got, msgAndArgs...)
}

// requireFresh requires that the session with this ID takes its
// connection's check of repo at now, with the repository ID want.
func requireFresh(t *testing.T, s oauth.Store, sessionID, repo string, now time.Time, want int64) {
	t.Helper()
	id, ok, err := s.FreshAccessCheck(context.Background(), sessionID, repo, now)
	require.NoError(t, err)
	require.True(t, ok, "%s takes the check of %s", sessionID, repo)
	require.Equal(t, want, id)
}

// requireStale requires that the session with this ID takes no check of
// repo at now.
func requireStale(t *testing.T, s oauth.Store, sessionID, repo string, now time.Time) {
	t.Helper()
	id, ok, err := s.FreshAccessCheck(context.Background(), sessionID, repo, now)
	require.NoError(t, err)
	require.False(t, ok, "%s takes no check of %s", sessionID, repo)
	require.Zero(t, id)
}

func aCheckThatStartedAtOrAfterTheRefreshOfTheSessionIsFresh(t *testing.T, s oauth.Store) {
	now := loginNow()
	c := connectionAt(1, now)
	session := newSession(t, s, "req-1", c, now)
	storeCheck(t, s, checkOf(c, "o/at", 11, now))
	storeCheck(t, s, checkOf(c, "o/after", 12, now.Add(time.Microsecond)))
	storeCheck(t, s, checkOf(c, "o/before", 13, now.Add(-time.Microsecond)))

	requireFresh(t, s, session.ID, "o/at", now, 11)
	requireFresh(t, s, session.ID, "o/after", now, 12)
	requireStale(t, s, session.ID, "o/before", now)
}

// The sessions of one user share the connection's checks, and each takes
// only those that started at or after its own refresh.
func eachSessionOfAConnectionJudgesACheckByItsOwnRefresh(t *testing.T, s oauth.Store) {
	now := loginNow()
	c := connectionAt(1, now)
	early := newSession(t, s, "req-early", c, now)
	late := newSession(t, s, "req-late", c, now.Add(time.Hour))
	storeCheck(t, s, checkOf(c, "o/r", 11, now.Add(30*time.Minute)))

	requireFresh(t, s, early.ID, "o/r", now.Add(time.Hour), 11)
	requireStale(t, s, late.ID, "o/r", now.Add(time.Hour))
}

func aRotationMakesAnOlderCheckStale(t *testing.T, s oauth.Store) {
	ctx := context.Background()
	now := loginNow()
	c := connectionAt(1, now)
	session := newSession(t, s, "req-1", c, now)
	storeCheck(t, s, checkOf(c, "o/r", 11, now))
	requireFresh(t, s, session.ID, "o/r", now, 11)

	later := now.Add(time.Minute)
	_, err := s.RotateSession(ctx, rotationOf(refreshOf(session.ID), "next", later))
	require.NoError(t, err)
	requireStale(t, s, session.ID, "o/r", later)
	storeCheck(t, s, checkOf(c, "o/r", 11, later))
	requireFresh(t, s, session.ID, "o/r", later, 11)
}

func aFreshCheckMovesItsLastUseForwardOnly(t *testing.T, s oauth.Store) {
	now := loginNow()
	c := connectionAt(1, now)
	session := newSession(t, s, "req-1", c, now)
	check := checkOf(c, "o/r", 11, now)
	check.LastUsedAt = now.Add(10 * time.Minute)
	storeCheck(t, s, check)

	requireFresh(t, s, session.ID, "o/r", now.Add(5*time.Minute), 11)
	requireChecks(t, s, c.ID, []oauth.AccessCheck{check}, "an earlier use leaves the last use")
	requireFresh(t, s, session.ID, "o/r", now.Add(20*time.Minute), 11)
	check.LastUsedAt = now.Add(20 * time.Minute)
	requireChecks(t, s, c.ID, []oauth.AccessCheck{check}, "a later use moves it")
}

// A stale check, a repository that the connection has no check of, another
// connection's check and an unknown session are all misses.
func aMissChangesNothing(t *testing.T, s oauth.Store) {
	now := loginNow()
	c := connectionAt(1, now)
	session := newSession(t, s, "req-1", c, now)
	stale := checkOf(c, "o/stale", 11, now.Add(-time.Second))
	storeCheck(t, s, stale)
	other := connectionAt(2, now)
	newSession(t, s, "req-2", other, now)
	theirs := checkOf(other, "o/theirs", 12, now)
	storeCheck(t, s, theirs)

	later := now.Add(time.Hour)
	requireStale(t, s, session.ID, "o/stale", later)
	requireStale(t, s, session.ID, "o/unknown", later)
	requireStale(t, s, session.ID, "o/theirs", later)
	requireStale(t, s, "no-such-session", "o/stale", later)
	requireChecks(t, s, c.ID, []oauth.AccessCheck{stale})
	requireChecks(t, s, other.ID, []oauth.AccessCheck{theirs})
}

// Two lookups of one repository can answer in either order. The check whose
// lookup started later stands, and the last use is the later of the two.
func anOlderCheckNeverReplacesALaterOne(t *testing.T, s oauth.Store) {
	now := loginNow()
	c := connectionAt(1, now)
	approvedRequest(t, s, "req-1", c, now)
	later := checkOf(c, "o/r", 12, now.Add(time.Minute))
	storeCheck(t, s, later)
	older := checkOf(c, "o/r", 11, now)
	older.LastUsedAt = now.Add(time.Hour)
	storeCheck(t, s, older)

	want := later
	want.LastUsedAt = older.LastUsedAt
	requireChecks(t, s, c.ID, []oauth.AccessCheck{want}, "the older check keeps the later one's ID and time")
	newer := checkOf(c, "o/r", 13, now.Add(2*time.Minute))
	storeCheck(t, s, newer)
	want = newer
	want.LastUsedAt = older.LastUsedAt
	requireChecks(t, s, c.ID, []oauth.AccessCheck{want}, "a newer check replaces the ID and the time, not a later use")
}

func storeAccessCheckOfAnUnknownConnectionIsUnknown(t *testing.T, s oauth.Store) {
	now := loginNow()
	c := connectionAt(1, now)

	err := s.StoreAccessCheck(context.Background(), checkOf(c, "o/r", 11, now))
	require.ErrorIs(t, err, oauth.ErrUnknownConnection)
	requireChecks(t, s, c.ID, nil)
}

func storeAccessCheckValidatesInput(t *testing.T, s oauth.Store) {
	now := loginNow()
	c := connectionAt(1, now)
	approvedRequest(t, s, "req-1", c, now)
	valid := checkOf(c, "o/r", 11, now)

	for name, change := range map[string]func(*oauth.AccessCheck){
		"no connection":         func(a *oauth.AccessCheck) { a.ConnectionID = "" },
		"no repository":         func(a *oauth.AccessCheck) { a.Repo = "" },
		"a capital letter":      func(a *oauth.AccessCheck) { a.Repo = "o/R" },
		"no repository ID":      func(a *oauth.AccessCheck) { a.RepoID = 0 },
		"a negative repository": func(a *oauth.AccessCheck) { a.RepoID = -1 },
		"no lookup time":        func(a *oauth.AccessCheck) { a.CheckedAt = time.Time{} },
		"no last use":           func(a *oauth.AccessCheck) { a.LastUsedAt = time.Time{} },
	} {
		bad := valid
		change(&bad)
		require.Error(t, s.StoreAccessCheck(context.Background(), bad), name)
	}
	requireChecks(t, s, c.ID, nil, "no invalid check is stored")
	storeCheck(t, s, valid)
}

func deleteAccessCheckDeletesThatCheckOnly(t *testing.T, s oauth.Store) {
	ctx := context.Background()
	now := loginNow()
	c, other := connectionAt(1, now), connectionAt(2, now)
	approvedRequest(t, s, "req-1", c, now)
	approvedRequest(t, s, "req-2", other, now)
	storeCheck(t, s, checkOf(c, "o/a", 11, now.Add(time.Minute)))
	kept := checkOf(c, "o/b", 12, now)
	storeCheck(t, s, kept)
	theirs := checkOf(other, "o/a", 11, now)
	storeCheck(t, s, theirs)

	require.NoError(t, s.DeleteAccessCheck(ctx, c.ID, "o/a"))
	requireChecks(t, s, c.ID, []oauth.AccessCheck{kept})
	requireChecks(t, s, other.ID, []oauth.AccessCheck{theirs}, "another connection keeps its check of the repository")
	require.NoError(t, s.DeleteAccessCheck(ctx, c.ID, "o/a"), "a check that is gone")
	require.NoError(t, s.DeleteAccessCheck(ctx, connectionAt(3, now).ID, "o/a"), "an unknown connection")
}

func recentAccessChecksAreTheMostRecentlyUsedOfTheConnection(t *testing.T, s oauth.Store) {
	ctx := context.Background()
	now := loginNow()
	c, other := connectionAt(1, now), connectionAt(2, now)
	approvedRequest(t, s, "req-1", c, now)
	approvedRequest(t, s, "req-2", other, now)
	oldest := checkOf(c, "o/oldest", 11, now)
	tieB := checkOf(c, "o/b", 12, now.Add(2*time.Minute))
	tieA := checkOf(c, "o/a", 13, now.Add(2*time.Minute))
	newest := checkOf(c, "o/newest", 14, now.Add(3*time.Minute))
	for _, check := range []oauth.AccessCheck{oldest, tieB, tieA, newest, checkOf(other, "o/theirs", 15, now.Add(time.Hour))} {
		storeCheck(t, s, check)
	}

	requireChecks(t, s, c.ID, []oauth.AccessCheck{newest, tieA, tieB, oldest})
	got, err := s.RecentAccessChecks(ctx, c.ID, 2)
	require.NoError(t, err)
	requireSameChecks(t, []oauth.AccessCheck{newest, tieA}, got, "the limit keeps the most recently used")
	got, err = s.RecentAccessChecks(ctx, c.ID, 0)
	require.NoError(t, err)
	require.Empty(t, got)
}

func deleteUnusedConnectionsDeletesTheirAccessChecks(t *testing.T, s oauth.Store) {
	ctx := context.Background()
	now := loginNow()
	before := now.Add(-24 * time.Hour)
	old := before.Add(-time.Hour)
	unused := connectionAt(1, old)
	newSession(t, s, "req-1", unused, old)
	require.NoError(t, s.EndSession(ctx, "req-1", old))
	used := connectionAt(2, old)
	newSession(t, s, "req-2", used, old)
	storeCheck(t, s, checkOf(unused, "o/r", 11, old))
	kept := checkOf(used, "o/r", 11, old)
	storeCheck(t, s, kept)
	_, err := s.DeleteEndedSessions(ctx, now, now.Add(-180*24*time.Hour))
	require.NoError(t, err)
	_, err = s.DeleteExpiredAuthRequests(ctx, now, before)
	require.NoError(t, err)

	n, err := s.DeleteUnusedConnections(ctx, before)
	require.NoError(t, err)
	require.EqualValues(t, 1, n)
	requireChecks(t, s, unused.ID, nil, "the checks went with their connection")
	requireChecks(t, s, used.ID, []oauth.AccessCheck{kept})
}
