package oauth

import (
	"context"
	"strings"
	"time"

	"golang.org/x/xerrors"
)

// AccessCheck is the answer that GitHub gave when faucet asked whether the
// GitHub token of a connection could read a repository: a pass, with the ID
// of the repository, or a denial, without one. A denial is kept only so that
// a pass whose lookup started before it never takes its place.
type AccessCheck struct {
	ConnectionID string
	// Repo is the repository as callers write it, owner/name, in lower case:
	// GitHub ignores the case of a name.
	Repo string
	// RepoID is the ID of the repository for a pass, and 0 for a denial.
	RepoID int64
	// CheckedAt is when the lookup that GitHub answered started, never when
	// the answer came.
	CheckedAt  time.Time
	LastUsedAt time.Time
}

// Denied reports whether c is a denial.
func (c AccessCheck) Denied() bool { return c.RepoID == 0 }

// Validate reports whether a store may keep c.
func (c AccessCheck) Validate() error {
	switch {
	case c.ConnectionID == "":
		return xerrors.New("an access check needs its connection")
	case c.Repo == "" || c.Repo != strings.ToLower(c.Repo):
		return xerrors.New("an access check needs its repository, in lower case")
	case c.RepoID < 0:
		return xerrors.New("an access check needs the ID of its repository, or 0 for a denial")
	case c.CheckedAt.IsZero() || c.LastUsedAt.IsZero():
		return xerrors.New("an access check needs the time of its lookup and of its last use")
	}
	return nil
}

// AccessCheckStore keeps the repositories that each connection could read,
// and for a while those that GitHub denied it.
// storetest.RunOAuth checks an implementation.
type AccessCheckStore interface {
	// FreshAccessCheck looks for the pass of repo by the connection of the
	// live session with this ID that started at or after the session's
	// refreshed time. When it finds one, it returns its repository ID and
	// true, and when the check's last use came before usedBefore, it sets
	// the last use to now, never earlier than it was. Otherwise it returns 0
	// and false and changes nothing, as for a denial, an ended session or an
	// unknown one.
	FreshAccessCheck(ctx context.Context, sessionID, repo string, now, usedBefore time.Time) (int64, bool, error)
	// StoreAccessCheck stores c, a pass or a denial, in place of the
	// connection's check of c.Repo. A stored check that started later stays
	// as it is, pass or denial, and the last use becomes the later of the
	// two. A denial also turns into denials, as of c's start, the
	// connection's checks of the repository that the stored check of c.Repo
	// held, under its other names, which a renamed repository has, when
	// their lookup started at or before c's.
	// It returns ErrUnknownConnection when there is no such connection, and
	// fails when c is not valid.
	StoreAccessCheck(ctx context.Context, c AccessCheck) error
	// RecentAccessChecks returns up to limit passes of the connection whose
	// last use came at or after usedSince, the most recently used first,
	// and those used at the same time in the order of the bytes of their
	// repository. It leaves the denials out.
	RecentAccessChecks(ctx context.Context, connectionID string, usedSince time.Time, limit int) ([]AccessCheck, error)
	// DeleteUnusedAccessChecks deletes every check, of every connection,
	// whose last use came before usedBefore, and every denial whose lookup
	// started before deniedBefore, and returns how many.
	DeleteUnusedAccessChecks(ctx context.Context, usedBefore, deniedBefore time.Time) (int64, error)
}
