package faucet

import (
	"golang.org/x/xerrors"
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
