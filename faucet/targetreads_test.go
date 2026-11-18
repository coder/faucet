package faucet_test

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// GitHub sends some events of an organization, such as a ruleset of the
// organization, with no repository. Such a delivery comes from the
// organization: it reaches the owner and all interests that want it, and
// no interest of a repository or of named repositories.
func TestADeliveryOfAnOrganizationReachesItsOwnerAndEveryRepository(t *testing.T) {
	t.Parallel()

	h := newRawPollHarness(t, asCaller(newServicePrincipal(t), &fakeGrants{owners: map[string]int64{"coder": ownerID}, all: true}))
	leases := map[string]map[string]any{
		"repo":        repoArgs(),
		"owner":       args("owner", "coder"),
		"all":         args("all", true),
		"owner repos": args("owner", "coder", "repos", []string{"coder/a"}),
	}
	opened := map[string]string{}
	for name, arguments := range leases {
		opened[name] = h.pollRaw("github.repository_ruleset", arguments, "").Cursor
	}

	ruleset := map[string]any{"id": 1, "name": "main", "source_type": "Organization"}
	require.Equal(t, "stored", h.deliver("repository_ruleset", "of-coder", []byte(raw(args(
		"action", "created", "repository_ruleset", ruleset, "organization", map[string]any{"id": ownerID, "login": "coder"})))))
	require.Equal(t, "stored", h.deliver("repository_ruleset", "of-octo", []byte(raw(args(
		"action", "created", "repository_ruleset", ruleset, "organization", map[string]any{"id": 9, "login": "octo"})))))
	require.Equal(t, "ignored", h.deliver("repository_ruleset", "of-nobody", []byte(raw(args(
		"action", "created", "repository_ruleset", ruleset)))), "a delivery that names no repository and no organization comes from nowhere")

	got := map[string][]string{}
	for name, arguments := range leases {
		got[name] = h.pollRaw("github.repository_ruleset", arguments, opened[name]).eventIDs()
	}
	require.Equal(t, map[string][]string{
		"repo":        {},
		"owner":       {"of-coder"},
		"all":         {"of-coder", "of-octo"},
		"owner repos": {},
	}, got)
}

// GitHub's list of deliveries names a delivery's repository and not its
// owner, so an owner or all interest wants every failed delivery of its
// event and actions, of any repository or none.
func TestAnOwnerOrAllInterestWantsEveryFailedDeliveryOfItsEvent(t *testing.T) {
	t.Parallel()

	h := newRawPollHarness(t, asCaller(newServicePrincipal(t), &fakeGrants{owners: map[string]int64{"coder": ownerID}, all: true}))
	h.pollRaw("github.star", repoArgs(), "")
	h.pollRaw("github.push", args("owner", "coder"), "")
	h.pollRaw("github.issues", args("all", true, "actions", []string{"opened"}), "")
	h.subscribe(&rawParams{Name: "github.release", Arguments: raw(args("owner", "coder", "repos", []string{"coder/a"})), Delivery: deliveryTo(callbackURL, newSecret(t, 32))})

	wants, err := h.svc.RawWants(h.ctx)
	require.NoError(t, err)
	for _, tc := range []struct {
		name          string
		event, action string
		repoID        int64
		want          bool
	}{
		{name: "a push of the repository", event: "push", repoID: repoID, want: true},
		{name: "a push of another repository", event: "push", repoID: otherRepoID, want: true},
		{name: "a push that names no repository", event: "push", want: true},
		{name: "an action that the all lease lists", event: "issues", action: "opened", repoID: otherRepoID, want: true},
		{name: "an action that it does not", event: "issues", action: "closed", repoID: otherRepoID},
		{name: "a subscription with repos still wants every repository", event: "release", action: "published", repoID: otherRepoID, want: true},
		{name: "a repository's interest", event: "star", repoID: repoID, want: true},
		{name: "another repository than a repository's interest", event: "star", repoID: otherRepoID},
		{name: "no repository for a repository's interest", event: "star"},
		{name: "an event that no interest names", event: "fork", repoID: repoID},
	} {
		require.Equal(t, tc.want, wants(tc.event, tc.action, tc.repoID), tc.name)
	}
}
