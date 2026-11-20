package faucet_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/coder/superblink-events/faucet"
)

// refusedRawEvents are the GitHub events whose payload GitHub shows only to
// people who can do more than read a repository.
var refusedRawEvents = []string{
	"branch_protection_configuration", "branch_protection_rule", "code_scanning_alert", "dependabot_alert",
	"deploy_key", "member", "repository_advisory", "repository_dispatch", "repository_ruleset",
	"repository_vulnerability_alert", "secret_scanning_alert", "secret_scanning_alert_location",
	"secret_scanning_scan", "security_and_analysis", "team_add",
}

func offering(events ...string) func(*faucet.Deps) {
	return func(d *faucet.Deps) { d.RawEvents = events }
}

// offeredIn returns the names that an unknown event error lists.
func offeredIn(t *testing.T, message string) []string {
	t.Helper()
	_, list, ok := strings.Cut(message, "the types are: ")
	require.True(t, ok, message)
	return strings.Split(list, ", ")
}

func TestNewRefusesARawEventThatNoServiceOffers(t *testing.T) {
	t.Parallel()

	newWith := func(store faucet.RawStore, events ...string) error {
		deps := testDeps(&fakeStore{})
		deps.RawStore, deps.RawEvents = store, events
		_, err := faucet.New(deps)
		return err
	}
	store := struct{ faucet.RawStore }{}

	require.NoError(t, newWith(store, "push", "release"))
	require.NoError(t, newWith(store), "a RawStore without events offers no raw name")
	require.NoError(t, newWith(store, faucet.RawEventNames()...))
	for _, event := range refusedRawEvents {
		err := newWith(store, "push", event)
		require.ErrorContains(t, err, event)
		require.ErrorContains(t, err, "more than read access")
		require.ErrorContains(t, faucet.CheckRawEvents([]string{event}), event)
	}
	for _, event := range []string{"pull_request", "github.push", "pushes", ""} {
		require.ErrorContains(t, newWith(store, event), "not a GitHub event that faucet can offer raw", "%q", event)
	}
	require.ErrorContains(t, newWith(nil, "push"), "RawStore")

	require.Equal(t, refusedRawEvents, faucet.RefusedRawEvents())
	for _, event := range faucet.RawEventNames() {
		require.NotContains(t, refusedRawEvents, event)
	}
}

func TestAServiceOffersOnlyTheRawEventsThatItsHostLists(t *testing.T) {
	t.Parallel()

	none := newRawPollHarness(t, offering())
	for _, e := range listEvents(t, none.eventsHarness) {
		require.False(t, strings.HasPrefix(e.Name, "github."), "events/list offers %s", e.Name)
	}
	rpcErr := none.pollError(pollParams("github.push", repoArgs(), nil))
	require.EqualValues(t, codeNotFound, rpcErr.Code)
	require.Equal(t, allNames, offeredIn(t, rpcErr.Message))
	require.Equal(t, "ignored", none.deliver("push", "nobody-may-ask", rawPayload(repoID, "", nil)))
	require.EqualValues(t, codeNotFound, none.subscribeError(rawSubscribeTo("github.push", repoArgs(), callbackURL, newSecret(t, 32))).Code)

	pushes := newRawPollHarness(t, offering("push"))
	var names []string
	for _, e := range listEvents(t, pushes.eventsHarness) {
		if strings.HasPrefix(e.Name, "github.") {
			names = append(names, e.Name)
		}
	}
	require.Equal(t, []string{"github.push"}, names)
	rpcErr = pushes.pollError(pollParams("github.release", repoArgs(), nil))
	require.EqualValues(t, codeNotFound, rpcErr.Code)
	require.Equal(t, append(slices.Clone(allNames), "github.push"), offeredIn(t, rpcErr.Message))
	pushes.open()
}

func TestNoServiceServesARepositoryAdvisory(t *testing.T) {
	t.Parallel()

	h := newRawPollHarness(t, offering(faucet.RawEventNames()...))
	rpcErr := h.pollError(pollParams("github.repository_advisory", repoArgs(), nil))
	require.EqualValues(t, codeNotFound, rpcErr.Code)
	require.Equal(t, slices.Concat(allNames, rawNames), offeredIn(t, rpcErr.Message))

	advisory := rawPayload(repoID, "reported", map[string]any{"repository_advisory": map[string]any{"ghsa_id": "GHSA-xxxx-xxxx-xxxx", "state": "triage"}})
	require.Equal(t, "ignored", h.deliver("repository_advisory", "private-report", advisory))
	require.False(t, slices.Contains(faucet.RawEventNames(), "repository_advisory"))
}
