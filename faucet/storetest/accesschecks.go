package storetest

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/coder/superblink-events/faucet/oauth"
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
	{"AFreshCheckMovesItsLastUseOnlyWhenItCameBeforeTheBound", aFreshCheckMovesItsLastUseOnlyWhenItCameBeforeTheBound},
	{"AMissChangesNothing", aMissChangesNothing},
	{"AnEndedSessionTakesNoCheck", anEndedSessionTakesNoCheck},
	{"AnOlderCheckNeverReplacesALaterOne", anOlderCheckNeverReplacesALaterOne},
	{"AnOlderPassNeverReplacesALaterDenial", anOlderPassNeverReplacesALaterDenial},
	{"ALaterPassReplacesAnOlderDenial", aLaterPassReplacesAnOlderDenial},
	{"AnOlderDenialNeverReplacesALaterPass", anOlderDenialNeverReplacesALaterPass},
	{"ADenialWinsATieWithAPass", aDenialWinsATieWithAPass},
	{"ADenialIsNeverFreshNorRecent", aDenialIsNeverFreshNorRecent},
	{"StoreAccessCheckOfAnUnknownConnectionIsUnknown", storeAccessCheckOfAnUnknownConnectionIsUnknown},
	{"StoreAccessCheckValidatesInput", storeAccessCheckValidatesInput},
	{"ADenialTakesAwayThatCheckOnly", aDenialTakesAwayThatCheckOnly},
	{"ADenialTakesAwayTheOlderChecksOfTheOtherNamesOfTheRepository", aDenialTakesAwayTheOlderChecksOfTheOtherNamesOfTheRepository},
	{"RecentAccessChecksAreTheMostRecentlyUsedOfTheConnection", recentAccessChecksAreTheMostRecentlyUsedOfTheConnection},
	{"RecentAccessChecksBreakATieByTheBytesOfTheName", recentAccessChecksBreakATieByTheBytesOfTheName},
	{"RecentAccessChecksLeaveOutTheChecksUsedBeforeTheBound", recentAccessChecksLeaveOutTheChecksUsedBeforeTheBound},
	{"DeleteUnusedAccessChecksDeletesTheChecksUsedBeforeTheBound", deleteUnusedAccessChecksDeletesTheChecksUsedBeforeTheBound},
	{"DeleteUnusedAccessChecksDeletesTheDenialsMadeBeforeTheirBound", deleteUnusedAccessChecksDeletesTheDenialsMadeBeforeTheirBound},
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

// denialOf is the denial of repo to c by a lookup that started at checked,
// and which was last used then.
func denialOf(c oauth.Connection, repo string, checked time.Time) oauth.AccessCheck {
	return checkOf(c, repo, 0, checked)
}

func storeCheck(t *testing.T, s oauth.Store, c oauth.AccessCheck) {
	t.Helper()
	require.NoError(t, s.StoreAccessCheck(context.Background(), c))
}

// requireChecks requires that the checks of the connection with this ID,
// the most recently used first, are want.
func requireChecks(t *testing.T, s oauth.Store, connectionID string, want []oauth.AccessCheck, msgAndArgs ...any) {
	t.Helper()
	got, err := s.RecentAccessChecks(context.Background(), connectionID, time.Time{}, len(want)+10)
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
	id, ok, err := s.FreshAccessCheck(context.Background(), sessionID, repo, now, now)
	require.NoError(t, err)
	require.True(t, ok, "%s takes the check of %s", sessionID, repo)
	require.Equal(t, want, id)
}

// requireStale requires that the session with this ID takes no check of
// repo at now.
func requireStale(t *testing.T, s oauth.Store, sessionID, repo string, now time.Time) {
	t.Helper()
	id, ok, err := s.FreshAccessCheck(context.Background(), sessionID, repo, now, now)
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

// A use moves the last use of a check only when the last use came before
// the bound, so that a use soon after another needs no write.
func aFreshCheckMovesItsLastUseOnlyWhenItCameBeforeTheBound(t *testing.T, s oauth.Store) {
	ctx := context.Background()
	now := loginNow()
	c := connectionAt(1, now)
	session := newSession(t, s, "req-1", c, now)
	check := checkOf(c, "o/r", 11, now)
	storeCheck(t, s, check)

	id, ok, err := s.FreshAccessCheck(ctx, session.ID, "o/r", now.Add(30*time.Second), now)
	require.NoError(t, err)
	require.True(t, ok)
	require.EqualValues(t, 11, id)
	requireChecks(t, s, c.ID, []oauth.AccessCheck{check}, "a last use at the bound stays")
	id, ok, err = s.FreshAccessCheck(ctx, session.ID, "o/r", now.Add(time.Minute), now.Add(time.Microsecond))
	require.NoError(t, err)
	require.True(t, ok)
	require.EqualValues(t, 11, id)
	check.LastUsedAt = now.Add(time.Minute)
	requireChecks(t, s, c.ID, []oauth.AccessCheck{check}, "a last use before the bound moves to the time of the use")
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

// A session that ended takes no check of its connection, and changes none.
func anEndedSessionTakesNoCheck(t *testing.T, s oauth.Store) {
	now := loginNow()
	c := connectionAt(1, now)
	session := newSession(t, s, "req-1", c, now)
	check := checkOf(c, "o/r", 11, now)
	storeCheck(t, s, check)
	requireFresh(t, s, session.ID, "o/r", now, 11)
	require.NoError(t, s.EndSession(context.Background(), session.ID, now.Add(time.Minute)))

	requireStale(t, s, session.ID, "o/r", now.Add(2*time.Minute))
	requireChecks(t, s, c.ID, []oauth.AccessCheck{check}, "the miss moved no last use")
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

// A pass whose lookup started before a denial's, and which GitHub answered
// after it, is not stored: the denial stands, and no session takes the pass.
func anOlderPassNeverReplacesALaterDenial(t *testing.T, s oauth.Store) {
	now := loginNow()
	c := connectionAt(1, now)
	session := newSession(t, s, "req-1", c, now)
	storeCheck(t, s, denialOf(c, "o/r", now.Add(2*time.Minute)))
	storeCheck(t, s, checkOf(c, "o/r", 11, now.Add(time.Minute)))

	requireStale(t, s, session.ID, "o/r", now.Add(3*time.Minute))
	requireChecks(t, s, c.ID, nil, "the denial is no check")
}

func aLaterPassReplacesAnOlderDenial(t *testing.T, s oauth.Store) {
	now := loginNow()
	c := connectionAt(1, now)
	session := newSession(t, s, "req-1", c, now)
	storeCheck(t, s, denialOf(c, "o/r", now.Add(time.Minute)))
	pass := checkOf(c, "o/r", 11, now.Add(2*time.Minute))
	storeCheck(t, s, pass)

	requireFresh(t, s, session.ID, "o/r", now.Add(3*time.Minute), 11)
	pass.LastUsedAt = now.Add(3 * time.Minute)
	requireChecks(t, s, c.ID, []oauth.AccessCheck{pass})
}

// A denial whose lookup started before a pass's, and which GitHub answered
// after it, leaves the pass.
// A pass and a denial whose lookups started at the same time end as the
// denial, in either order.
func aDenialWinsATieWithAPass(t *testing.T, s oauth.Store) {
	now := loginNow()
	c := connectionAt(1, now)
	session := newSession(t, s, "req-1", c, now)
	at := now.Add(time.Minute)

	storeCheck(t, s, denialOf(c, "o/denied-first", at))
	storeCheck(t, s, checkOf(c, "o/denied-first", 11, at))
	requireStale(t, s, session.ID, "o/denied-first", at)
	storeCheck(t, s, checkOf(c, "o/passed-first", 12, at))
	storeCheck(t, s, denialOf(c, "o/passed-first", at))
	requireStale(t, s, session.ID, "o/passed-first", at)
	requireChecks(t, s, c.ID, nil, "both ended as denials")
}

func anOlderDenialNeverReplacesALaterPass(t *testing.T, s oauth.Store) {
	now := loginNow()
	c := connectionAt(1, now)
	session := newSession(t, s, "req-1", c, now)
	pass := checkOf(c, "o/r", 11, now.Add(2*time.Minute))
	storeCheck(t, s, pass)
	storeCheck(t, s, denialOf(c, "o/r", now.Add(time.Minute)))

	requireChecks(t, s, c.ID, []oauth.AccessCheck{pass}, "the pass stands")
	requireFresh(t, s, session.ID, "o/r", now.Add(2*time.Minute), 11)
}

// A denial is kept only to order the answers. No session takes it, and the
// checks of a connection leave it out.
func aDenialIsNeverFreshNorRecent(t *testing.T, s oauth.Store) {
	now := loginNow()
	c := connectionAt(1, now)
	session := newSession(t, s, "req-1", c, now)
	storeCheck(t, s, checkOf(c, "o/r", 11, now))
	storeCheck(t, s, denialOf(c, "o/r", now.Add(time.Minute)))
	storeCheck(t, s, denialOf(c, "o/new", now.Add(time.Minute)))

	requireStale(t, s, session.ID, "o/r", now.Add(2*time.Minute))
	requireStale(t, s, session.ID, "o/new", now.Add(2*time.Minute))
	requireChecks(t, s, c.ID, nil)
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
	storeCheck(t, s, denialOf(c, "o/denied", now))
}

// A denial takes away the connection's check of that repository, and no
// other check.
func aDenialTakesAwayThatCheckOnly(t *testing.T, s oauth.Store) {
	now := loginNow()
	c, other := connectionAt(1, now), connectionAt(2, now)
	session := newSession(t, s, "req-1", c, now)
	approvedRequest(t, s, "req-2", other, now)
	storeCheck(t, s, checkOf(c, "o/a", 11, now))
	kept := checkOf(c, "o/b", 12, now)
	storeCheck(t, s, kept)
	theirs := checkOf(other, "o/a", 11, now)
	storeCheck(t, s, theirs)

	storeCheck(t, s, denialOf(c, "o/a", now.Add(time.Minute)))
	requireStale(t, s, session.ID, "o/a", now.Add(time.Minute))
	requireChecks(t, s, c.ID, []oauth.AccessCheck{kept})
	requireChecks(t, s, other.ID, []oauth.AccessCheck{theirs}, "another connection keeps its check of the repository")
}

// A repository renamed since a check holds one check per name, with one ID.
// A denial of one name also takes away the connection's checks of the
// repository that the denied name's check held, under its other names, when
// their lookup started at or before the denial's.
func aDenialTakesAwayTheOlderChecksOfTheOtherNamesOfTheRepository(t *testing.T, s oauth.Store) {
	now := loginNow()
	c, other := connectionAt(1, now), connectionAt(2, now)
	session := newSession(t, s, "req-1", c, now)
	approvedRequest(t, s, "req-2", other, now)
	later := checkOf(c, "org/later", 11, now.Add(3*time.Minute))
	unrelated := checkOf(c, "org/unrelated", 12, now)
	theirs := checkOf(other, "org/new", 11, now)
	for _, check := range []oauth.AccessCheck{
		checkOf(c, "org/old", 11, now), checkOf(c, "org/new", 11, now.Add(time.Minute)),
		checkOf(c, "org/at", 11, now.Add(2*time.Minute)), later, unrelated, theirs,
	} {
		storeCheck(t, s, check)
	}

	storeCheck(t, s, denialOf(c, "org/old", now.Add(2*time.Minute)))
	requireStale(t, s, session.ID, "org/new", now.Add(3*time.Minute))
	requireStale(t, s, session.ID, "org/at", now.Add(3*time.Minute))
	requireChecks(t, s, c.ID, []oauth.AccessCheck{later, unrelated}, "a check that started after the denial stays, and so does one of another repository")
	requireChecks(t, s, other.ID, []oauth.AccessCheck{theirs}, "another connection keeps its checks")

	storeCheck(t, s, checkOf(c, "org/new", 11, now.Add(90*time.Second)))
	requireStale(t, s, session.ID, "org/new", now.Add(3*time.Minute))
	storeCheck(t, s, checkOf(c, "org/new", 11, now.Add(4*time.Minute)))
	requireFresh(t, s, session.ID, "org/new", now.Add(5*time.Minute), 11)
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
	got, err := s.RecentAccessChecks(ctx, c.ID, time.Time{}, 2)
	require.NoError(t, err)
	requireSameChecks(t, []oauth.AccessCheck{newest, tieA}, got, "the limit keeps the most recently used")
	got, err = s.RecentAccessChecks(ctx, c.ID, time.Time{}, 0)
	require.NoError(t, err)
	require.Empty(t, got)
}

// The order of names that tie does not depend on the collation of a
// database: a database in en_US puts org/new before o/x, which bytes order
// the other way.
func recentAccessChecksBreakATieByTheBytesOfTheName(t *testing.T, s oauth.Store) {
	now := loginNow()
	c := connectionAt(1, now)
	approvedRequest(t, s, "req-1", c, now)
	var checks []oauth.AccessCheck
	for i, repo := range []string{"org/new", "o/x", "o-z/a", "o.y/b"} {
		checks = append(checks, checkOf(c, repo, int64(11+i), now))
	}
	for _, check := range checks {
		storeCheck(t, s, check)
	}

	requireChecks(t, s, c.ID, []oauth.AccessCheck{checks[2], checks[3], checks[1], checks[0]}, "ties come in the order of the bytes of their names")
}

func recentAccessChecksLeaveOutTheChecksUsedBeforeTheBound(t *testing.T, s oauth.Store) {
	now := loginNow()
	c := connectionAt(1, now)
	approvedRequest(t, s, "req-1", c, now)
	since := now.Add(time.Hour)
	before := checkOf(c, "o/before", 11, since.Add(-time.Microsecond))
	at := checkOf(c, "o/at", 12, since)
	after := checkOf(c, "o/after", 13, since.Add(time.Minute))
	for _, check := range []oauth.AccessCheck{before, at, after} {
		storeCheck(t, s, check)
	}

	got, err := s.RecentAccessChecks(context.Background(), c.ID, since, 10)
	require.NoError(t, err)
	requireSameChecks(t, []oauth.AccessCheck{after, at}, got, "a check last used before the bound is left out")
}

// A check whose last use came before the bound goes, of every connection,
// and one used at the bound or later stays.
func deleteUnusedAccessChecksDeletesTheChecksUsedBeforeTheBound(t *testing.T, s oauth.Store) {
	ctx := context.Background()
	now := loginNow()
	c, other := connectionAt(1, now), connectionAt(2, now)
	approvedRequest(t, s, "req-1", c, now)
	approvedRequest(t, s, "req-2", other, now)
	bound := now.Add(time.Hour)
	at := checkOf(c, "o/at", 12, bound)
	for _, check := range []oauth.AccessCheck{
		checkOf(c, "o/before", 11, bound.Add(-time.Microsecond)), at,
		checkOf(other, "o/before", 11, bound.Add(-time.Hour)),
	} {
		storeCheck(t, s, check)
	}

	n, err := s.DeleteUnusedAccessChecks(ctx, bound, time.Time{})
	require.NoError(t, err)
	require.EqualValues(t, 2, n)
	requireChecks(t, s, c.ID, []oauth.AccessCheck{at})
	requireChecks(t, s, other.ID, nil)
	n, err = s.DeleteUnusedAccessChecks(ctx, bound, time.Time{})
	require.NoError(t, err)
	require.Zero(t, n, "nothing is left to delete")
}

// A denial whose lookup started before its bound goes, of every connection,
// and one that started at the bound or later stays, to keep the older passes
// out. A pass that started before the bound stays.
func deleteUnusedAccessChecksDeletesTheDenialsMadeBeforeTheirBound(t *testing.T, s oauth.Store) {
	ctx := context.Background()
	now := loginNow()
	c, other := connectionAt(1, now), connectionAt(2, now)
	approvedRequest(t, s, "req-1", c, now)
	approvedRequest(t, s, "req-2", other, now)
	bound := now.Add(time.Hour)
	pass := checkOf(c, "o/pass", 11, bound.Add(-time.Hour))
	pass.LastUsedAt = bound
	for _, check := range []oauth.AccessCheck{
		pass, denialOf(c, "o/before", bound.Add(-time.Microsecond)), denialOf(c, "o/at", bound),
		denialOf(other, "o/before", bound.Add(-time.Minute)),
	} {
		storeCheck(t, s, check)
	}

	n, err := s.DeleteUnusedAccessChecks(ctx, now, bound)
	require.NoError(t, err)
	require.EqualValues(t, 2, n)
	requireChecks(t, s, c.ID, []oauth.AccessCheck{pass}, "a pass stays")
	storeCheck(t, s, checkOf(c, "o/before", 12, bound.Add(-time.Minute)))
	storeCheck(t, s, checkOf(c, "o/at", 13, bound.Add(-time.Minute)))
	got, err := s.RecentAccessChecks(ctx, c.ID, time.Time{}, 10)
	require.NoError(t, err)
	var repos []string
	for _, check := range got {
		repos = append(repos, check.Repo)
	}
	require.ElementsMatch(t, []string{"o/pass", "o/before"}, repos, "the denial at the bound stays and keeps the older pass out")
	n, err = s.DeleteUnusedAccessChecks(ctx, now, bound)
	require.NoError(t, err)
	require.Zero(t, n, "nothing is left to delete")
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
