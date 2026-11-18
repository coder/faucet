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
