package oauth

import (
	"context"
	"strings"
	"time"

	"golang.org/x/xerrors"
)

// AccessCheck is a repository that the GitHub token of a connection could
// read when faucet asked GitHub.
type AccessCheck struct {
	ConnectionID string
	// Repo is the repository as callers write it, owner/name, in lower case:
	// GitHub ignores the case of a name.
	Repo   string
	RepoID int64
	// CheckedAt is when the lookup that GitHub answered started, never when
	// the answer came.
	CheckedAt  time.Time
	LastUsedAt time.Time
}

// Validate reports whether a store may keep c.
func (c AccessCheck) Validate() error {
	switch {
	case c.ConnectionID == "":
		return xerrors.New("an access check needs its connection")
	case c.Repo == "" || c.Repo != strings.ToLower(c.Repo):
		return xerrors.New("an access check needs its repository, in lower case")
	case c.RepoID <= 0:
		return xerrors.New("an access check needs the ID of its repository")
	case c.CheckedAt.IsZero() || c.LastUsedAt.IsZero():
		return xerrors.New("an access check needs the time of its lookup and of its last use")
	}
	return nil
}

// AccessCheckStore keeps the repositories that each connection could read.
// storetest.RunOAuth checks an implementation.
type AccessCheckStore interface {
	// FreshAccessCheck looks for the check of repo by the connection of the
	// session with this ID that started at or after the session's refreshed
	// time. When it finds one, it returns its repository ID and true, and
	// when the check's last use came before usedBefore, it sets the last use
	// to now, never earlier than it was. Otherwise it returns 0 and false
	// and changes nothing, as for an unknown session.
	FreshAccessCheck(ctx context.Context, sessionID, repo string, now, usedBefore time.Time) (int64, bool, error)
	// StoreAccessCheck stores c in place of the connection's check of c.Repo.
	// A stored check that started later keeps its repository ID and its
	// time, and the last use becomes the later of the two. It returns
	// ErrUnknownConnection when there is no such connection, and fails when
	// c is not valid.
	StoreAccessCheck(ctx context.Context, c AccessCheck) error
	// DeleteAccessCheck deletes the connection's check of repo. No such check
	// is no error.
	DeleteAccessCheck(ctx context.Context, connectionID, repo string) error
	// RecentAccessChecks returns up to limit checks of the connection whose
	// last use came at or after usedSince, the most recently used first,
	// and those used at the same time by repository.
	RecentAccessChecks(ctx context.Context, connectionID string, usedSince time.Time, limit int) ([]AccessCheck, error)
	// DeleteUnusedAccessChecks deletes every check, of every connection,
	// whose last use came before usedBefore, and returns how many.
	DeleteUnusedAccessChecks(ctx context.Context, usedBefore time.Time) (int64, error)
}
