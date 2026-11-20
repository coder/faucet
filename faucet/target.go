package faucet

import (
	"strings"

	"golang.org/x/xerrors"

	"cdr.dev/slog/v3"
	"github.com/coder/superblink-events/faucet/internal/githubapi"
)

// TargetKind says what a read covers.
type TargetKind string

const (
	// TargetRepo is one repository, by GitHub repository ID.
	TargetRepo TargetKind = "repo"
	// TargetOwner is every repository of one owner, a user or an
	// organization, by GitHub account ID.
	TargetOwner TargetKind = "owner"
	// TargetAll is every repository and owner that faucet hears from.
	TargetAll TargetKind = "all"
)

// Target is what a subscription, a poll or a lease reads. Build one with
// RepoTarget, OwnerTarget or AllTarget, or with ParseTarget from what a store
// holds. A repo target has a positive repository ID and no owner ID, an owner
// target a positive owner ID and no repository ID, and an all target neither.
// The zero Target is invalid.
type Target struct {
	kind    TargetKind
	repoID  int64
	ownerID int64
}

// RepoTarget is the target of one repository.
func RepoTarget(repoID int64) Target { return Target{kind: TargetRepo, repoID: repoID} }

// OwnerTarget is the target of every repository of one owner.
func OwnerTarget(ownerID int64) Target { return Target{kind: TargetOwner, ownerID: ownerID} }

// AllTarget is the target of every repository.
func AllTarget() Target { return Target{kind: TargetAll} }

// ParseTarget builds the target that a store row holds, and fails when the
// three values break the invariant of Target.
func ParseTarget(kind string, repoID, ownerID int64) (Target, error) {
	t := Target{kind: TargetKind(kind), repoID: repoID, ownerID: ownerID}
	if err := t.Validate(); err != nil {
		return Target{}, err
	}
	return t, nil
}

// Validate reports whether t keeps the invariant of its kind.
func (t Target) Validate() error {
	switch t.kind {
	case TargetRepo:
		if t.repoID <= 0 || t.ownerID != 0 {
			return xerrors.New("a repo target needs a positive repo ID and no owner ID")
		}
	case TargetOwner:
		if t.ownerID <= 0 || t.repoID != 0 {
			return xerrors.New("an owner target needs a positive owner ID and no repo ID")
		}
	case TargetAll:
		if t.repoID != 0 || t.ownerID != 0 {
			return xerrors.New("an all target has no repo ID and no owner ID")
		}
	default:
		return xerrors.Errorf("unknown target kind %q", t.kind)
	}
	return nil
}

// Kind is what t covers.
func (t Target) Kind() TargetKind { return t.kind }

// RepoID is the repository of a repo target, and 0 for the other kinds.
func (t Target) RepoID() int64 { return t.repoID }

// OwnerID is the owner of an owner target, and 0 for the other kinds.
func (t Target) OwnerID() int64 { return t.ownerID }

// Covers reports whether an event of this repository and owner is one that
// t reads. An owner ID of 0, which an event stored before owners were
// recorded has, is no owner's. An invalid target covers nothing.
func (t Target) Covers(repoID, ownerID int64) bool {
	if t.Validate() != nil {
		return false
	}
	switch t.kind {
	case TargetRepo:
		return repoID == t.repoID
	case TargetOwner:
		return ownerID == t.ownerID
	default:
		return true
	}
}

// targetArguments are the arguments that name what a read covers: exactly
// one of repo, owner and all. repos narrows an owner or all target to the
// repositories it names.
type targetArguments struct {
	Repo  string   `json:"repo,omitempty" jsonschema:"the repository, written owner/name"`
	Owner string   `json:"owner,omitempty" jsonschema:"every repository of this owner, a GitHub user or organization login; for service clients only"`
	All   *bool    `json:"all,omitempty" jsonschema:"true for every repository that faucet receives events from; for service clients only"`
	Repos []string `json:"repos,omitempty" jsonschema:"with owner or all, only the events of these repositories, each written owner/name"`
}

// all reports whether a names every repository.
func (a targetArguments) all() bool { return a.All != nil && *a.All }

// check reports whether a names one target in the form of its kind. It
// refuses all that is false and repos that is empty, as the inputSchema of
// every event name does, so the wait tool, whose schema the SDK builds,
// refuses them too.
func (a targetArguments) check() error {
	switch {
	case a.All != nil && !*a.All:
		return xerrors.New("all must be true; leave it out to name repo or owner")
	case a.Repos != nil && len(a.Repos) == 0:
		return xerrors.New("repos must name at least one repository; leave it out to read every repository of the target")
	}
	named := 0
	for _, set := range []bool{a.Repo != "", a.Owner != "", a.all()} {
		if set {
			named++
		}
	}
	if named != 1 {
		return xerrors.New("name one target: repo, owner or all")
	}
	switch {
	case a.Repo != "":
		if _, _, err := githubapi.ParseRepo(a.Repo); err != nil {
			return err
		}
		if len(a.Repos) > 0 {
			return xerrors.New("repos narrows an owner or all target, so it goes without repo")
		}
	case a.Owner != "":
		if !githubapi.ValidOwner(a.Owner) {
			return xerrors.New("owner must be a GitHub login")
		}
	}
	for i, repo := range a.Repos {
		owner, _, err := githubapi.ParseRepo(repo)
		if err != nil {
			return xerrors.Errorf("repos[%d] must be written owner/name", i)
		}
		if a.Owner != "" && !strings.EqualFold(owner, a.Owner) {
			return xerrors.Errorf("repos[%d] %s is not a repository of %s", i, repo, a.Owner)
		}
	}
	return nil
}

// normalized is a with repos in its one spelling.
func (a targetArguments) normalized() targetArguments {
	a.Repos = a.repoNames()
	return a
}

// repoNames are the names of repos in lower case, sorted once each, as a
// store compares them, and nil without repos.
func (a targetArguments) repoNames() []string {
	if len(a.Repos) == 0 {
		return nil
	}
	lower := make([]string, len(a.Repos))
	for i, repo := range a.Repos {
		lower[i] = strings.ToLower(repo)
	}
	return sortedSet(lower)
}

// describe names the target in an error that a caller reads.
func (a targetArguments) describe() string {
	switch {
	case a.Owner != "":
		return "every repository of " + a.Owner
	case a.all():
		return "every repository"
	}
	return a.Repo
}

// logField names the target in a log line.
func (a targetArguments) logField() slog.Field {
	switch {
	case a.Owner != "":
		return slog.F("owner", a.Owner)
	case a.all():
		return slog.F("all", true)
	}
	return slog.F("repo", a.Repo)
}
