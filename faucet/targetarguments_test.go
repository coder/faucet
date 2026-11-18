package faucet_test

import (
	"encoding/json"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/stretchr/testify/require"
)

// Every way to name events checks the target in the same words: the typed
// and raw names of events/poll and events/subscribe, and the wait tool.
func TestEveryCallNamesOneTargetInTheFormOfItsKind(t *testing.T) {
	t.Parallel()

	bad := []struct {
		name      string
		arguments map[string]any
		want      string
	}{
		{"none", args(), "name one target: repo, owner or all"},
		{"repo and owner", args("repo", testRepo, "owner", "octo"), "name one target: repo, owner or all"},
		{"owner and all", args("owner", "octo", "all", true), "name one target: repo, owner or all"},
		{"repos with repo", args("repo", testRepo, "repos", []string{testRepo}), "repos narrows an owner or all target, so it goes without repo"},
		{"no login", args("owner", "octo/repo"), "owner must be a GitHub login"},
		{"repos of another owner", args("owner", "octo", "repos", []string{"other/repo"}), "repos[0] other/repo is not a repository of octo"},
		{"repos not a name", args("all", true, "repos", []string{"octo"}), "repos[0] must be written owner/name"},
	}
	h := newRawPollHarness(t)
	for _, tt := range bad {
		for _, name := range []string{"ci.run_completed", "github.push"} {
			rpcErr := h.pollError(pollParams(name, tt.arguments, nil))
			require.EqualValues(t, jsonrpc.CodeInvalidParams, rpcErr.Code, "%s %s", tt.name, name)
			require.Equal(t, tt.want, rpcErr.Message, "%s %s", tt.name, name)
			rpcErr = h.subscribeError(&rawParams{Name: name, Arguments: raw(tt.arguments), Delivery: deliveryTo(callbackURL, newSecret(t, 32))})
			require.EqualValues(t, jsonrpc.CodeInvalidParams, rpcErr.Code, "%s %s", tt.name, name)
			require.Equal(t, tt.want, rpcErr.Message, "%s %s", tt.name, name)
		}
	}
	for _, name := range []string{"ci.run_completed", "github.push"} {
		for _, arguments := range []json.RawMessage{nil, json.RawMessage("null")} {
			rpcErr := h.pollError(&rawParams{Name: name, Arguments: arguments})
			require.EqualValues(t, jsonrpc.CodeInvalidParams, rpcErr.Code, "no arguments %s %s", name, arguments)
			require.Equal(t, "name one target: repo, owner or all", rpcErr.Message, "no arguments %s %s", name, arguments)
			rpcErr = h.subscribeError(&rawParams{Name: name, Arguments: arguments, Delivery: deliveryTo(callbackURL, newSecret(t, 32))})
			require.EqualValues(t, jsonrpc.CodeInvalidParams, rpcErr.Code, "no arguments %s %s", name, arguments)
			require.Equal(t, "name one target: repo, owner or all", rpcErr.Message, "no arguments %s %s", name, arguments)
		}
	}
	require.Empty(t, h.rows(), "nothing is stored for a call that names no target")

	w := newWaitHarness(t)
	for _, tt := range bad {
		tt.arguments["types"] = []string{"ci.run_completed"}
		require.Equal(t, tt.want, w.call(tt.arguments).failure(t), tt.name)
	}

	// all is true or absent, and repos names at least one repository: the
	// wait tool refuses what the inputSchema of an event name refuses.
	for name, arguments := range map[string]map[string]any{
		"all false":           args("all", false),
		"all false and repo":  args("repo", testRepo, "all", false),
		"all false and owner": args("owner", "octo", "all", false),
		"repos empty":         args("owner", "octo", "repos", []string{}),
		"repos empty of all":  args("all", true, "repos", []string{}),
	} {
		for _, event := range []string{"ci.run_completed", "github.push"} {
			rpcErr := h.pollError(pollParams(event, arguments, nil))
			require.EqualValues(t, jsonrpc.CodeInvalidParams, rpcErr.Code, "%s %s", name, event)
			require.Contains(t, rpcErr.Message, "arguments do not match the inputSchema", "%s %s", name, event)
		}
		arguments["types"] = []string{"ci.run_completed"}
		failure := w.call(arguments).failure(t)
		require.NotEmpty(t, failure, name)
		require.NotContains(t, failure, "events", "%s is refused, not read: %s", name, failure)
	}
}

// Arguments that name a repository keep the spelling they had before owner
// and all targets existed, so every subscription made before keeps its key.
func TestTheArgumentsOfARepositoryKeepTheirSpelling(t *testing.T) {
	t.Parallel()

	h := newWebhookHarness(t)
	h.subscribe(&rawParams{
		Name: "ci.runs",
		Arguments: raw(args("conclusion", []string{"failure", "timed_out", "failure"}, "actions", []string{"completed"},
			"sha", testShaA, "pr", 7, "repo", testRepo)),
		Delivery: deliveryTo(callbackURL, newSecret(t, 32)),
	})
	rows := h.rows()
	require.Len(t, rows, 1)
	require.Equal(t, `{"repo":"octo/repo","sha":"`+testShaA+`","pr":7,"actions":["completed"],"conclusion":["failure","timed_out"]}`, string(rows[0].Arguments))
}
