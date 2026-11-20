package faucet_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/coder/superblink-events/faucet"
)

func TestParseTargetKeepsTheInvariantOfEachKind(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		kind            string
		repoID, ownerID int64
		wantErr         bool
	}{
		{name: "repo", kind: "repo", repoID: 42},
		{name: "owner", kind: "owner", ownerID: 7},
		{name: "all", kind: "all"},
		{name: "repo without an ID", kind: "repo", wantErr: true},
		{name: "repo with a negative ID", kind: "repo", repoID: -1, wantErr: true},
		{name: "repo with an owner", kind: "repo", repoID: 42, ownerID: 7, wantErr: true},
		{name: "owner without an ID", kind: "owner", wantErr: true},
		{name: "owner with a repository", kind: "owner", repoID: 42, ownerID: 7, wantErr: true},
		{name: "all with a repository", kind: "all", repoID: 42, wantErr: true},
		{name: "all with an owner", kind: "all", ownerID: 7, wantErr: true},
		{name: "unknown kind", kind: "org", ownerID: 7, wantErr: true},
		{name: "no kind", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := faucet.ParseTarget(tt.kind, tt.repoID, tt.ownerID)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, faucet.TargetKind(tt.kind), got.Kind())
			require.Equal(t, tt.repoID, got.RepoID())
			require.Equal(t, tt.ownerID, got.OwnerID())
		})
	}
}

func TestTargetConstructorsBuildTheTargetsParseTargetReads(t *testing.T) {
	t.Parallel()

	for _, want := range []faucet.Target{faucet.RepoTarget(42), faucet.OwnerTarget(7), faucet.AllTarget()} {
		require.NoError(t, want.Validate())
		got, err := faucet.ParseTarget(string(want.Kind()), want.RepoID(), want.OwnerID())
		require.NoError(t, err)
		require.Equal(t, want, got)
	}
	require.Error(t, faucet.Target{}.Validate(), "the zero target is invalid")
	require.Error(t, faucet.RepoTarget(0).Validate())
	require.Error(t, faucet.OwnerTarget(0).Validate())
}

func TestTargetCoversTheEventsOfItsKind(t *testing.T) {
	t.Parallel()

	const repo, otherRepo, owner, otherOwner = 42, 43, 7, 8
	tests := []struct {
		name            string
		target          faucet.Target
		repoID, ownerID int64
		want            bool
	}{
		{"repo target, its repository", faucet.RepoTarget(repo), repo, owner, true},
		{"repo target, its repository with no owner recorded", faucet.RepoTarget(repo), repo, 0, true},
		{"repo target, another repository of the owner", faucet.RepoTarget(repo), otherRepo, owner, false},
		{"repo target, a delivery with no repository", faucet.RepoTarget(repo), 0, owner, false},
		{"owner target, a repository of the owner", faucet.OwnerTarget(owner), otherRepo, owner, true},
		{"owner target, a delivery of the owner with no repository", faucet.OwnerTarget(owner), 0, owner, true},
		{"owner target, another owner", faucet.OwnerTarget(owner), repo, otherOwner, false},
		{"owner target, an event with no owner recorded", faucet.OwnerTarget(owner), repo, 0, false},
		{"all target, any repository", faucet.AllTarget(), otherRepo, otherOwner, true},
		{"all target, an event with no owner recorded", faucet.AllTarget(), repo, 0, true},
		{"zero target", faucet.Target{}, 0, 0, false},
		{"repo target without an ID", faucet.RepoTarget(0), 0, owner, false},
		{"owner target without an ID", faucet.OwnerTarget(0), repo, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.want, tt.target.Covers(tt.repoID, tt.ownerID))
		})
	}
}
