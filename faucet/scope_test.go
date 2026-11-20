package faucet_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"

	"github.com/coder/superblink-events/faucet"
)

// serviceWith is a service principal that holds only scopes.
func serviceWith(t *testing.T, scopes ...faucet.Scope) func(*faucet.Deps) {
	t.Helper()
	p, err := faucet.NewService(faucet.ServiceClient{ClientID: "reader"}, scopes)
	require.NoError(t, err)
	return func(d *faucet.Deps) {
		d.PrincipalFrom = func(context.Context) (faucet.Principal, error) { return p, nil }
	}
}

func waitOnce(h *eventsHarness) (*mcp.CallToolResult, error) {
	h.t.Helper()
	return h.session.CallTool(h.ctx, &mcp.CallToolParams{Name: "wait_for_github_events", Arguments: map[string]any{
		"repo": testRepo, "types": []string{"ci.run_completed"}, "cursor": "v2.0", "timeout_s": 1,
	}})
}

func TestReadScopeWaitsAndPollsButCannotSubscribe(t *testing.T) {
	t.Parallel()

	h := newWebhookHarness(t, serviceWith(t, faucet.ScopeEventsRead))
	h.insert(repoID, "ci.run_completed", "")

	res, err := waitOnce(h.eventsHarness)
	require.NoError(t, err)
	require.False(t, res.IsError, "events:read covers the wait tool: %v", res.Content)
	require.Len(t, h.poll(pollParams("ci.run_completed", repoArgs(), raw("v2.0"))).Events, 1, "events:read covers events/poll")
	calls := h.authz.callCount()

	rpcErr := h.subscribeError(subscribeTo(repoArgs(), callbackURL, newSecret(t, 32)))
	require.EqualValues(t, codeForbidden, rpcErr.Code)
	require.Contains(t, rpcErr.Message, "events:subscribe")
	require.EqualValues(t, codeForbidden, h.unsubscribe(repoArgs(), callbackURL).Code)
	requireNothingStored(t, h, "a token without events:subscribe stores no subscription")
	require.Equal(t, calls, h.authz.callCount(), "a missing scope is refused before any repository lookup")
	require.Zero(t, h.verifier.callCount(), "a missing scope is refused before the callback is verified")
}

func TestSubscribeScopeSubscribesButCannotWaitOrPoll(t *testing.T) {
	t.Parallel()

	h := newWebhookHarness(t, serviceWith(t, faucet.ScopeEventsSubscribe))
	h.subscribe(subscribeTo(repoArgs(), callbackURL, newSecret(t, 32)))
	require.Nil(t, h.unsubscribe(repoArgs(), callbackURL))
	calls := h.authz.callCount()

	res, err := waitOnce(h.eventsHarness)
	require.NoError(t, err)
	require.True(t, res.IsError)
	require.Contains(t, res.Content[0].(*mcp.TextContent).Text, "events:read")
	rpcErr := h.pollError(pollParams("ci.run_completed", repoArgs(), nil))
	require.EqualValues(t, codeForbidden, rpcErr.Code)
	require.Contains(t, rpcErr.Message, "events:read")
	require.Equal(t, calls, h.authz.callCount(), "a missing scope is refused before any repository lookup")
}

func TestAPrincipalOfNoKindIsNotAuthenticated(t *testing.T) {
	t.Parallel()

	cleared, err := faucet.NewPrincipal("octocat", testToken)
	require.NoError(t, err)
	cleared.Login = ""
	colon, err := faucet.NewPrincipal("octocat", testToken)
	require.NoError(t, err)
	colon.Login = "svc:ci-bot"
	renamed, err := faucet.NewService(faucet.ServiceClient{ClientID: "ci-bot"}, []faucet.Scope{faucet.ScopeEventsRead, faucet.ScopeEventsSubscribe})
	require.NoError(t, err)
	renamed.Login = "octocat"
	user, err := faucet.NewOAuthUser("alice", faucet.OAuthSession{SessionID: "s", ConnectionID: "c"}, []faucet.Scope{faucet.ScopeEventsRead, faucet.ScopeEventsSubscribe})
	require.NoError(t, err)
	user.Login = "a:b"
	for _, p := range []faucet.Principal{{}, {Login: "octocat"}, cleared, colon, renamed, user} {
		h := newWebhookHarness(t, func(d *faucet.Deps) {
			d.PrincipalFrom = func(context.Context) (faucet.Principal, error) { return p, nil }
		})
		res, err := waitOnce(h.eventsHarness)
		require.NoError(t, err)
		require.True(t, res.IsError)
		require.Equal(t, "authentication required", res.Content[0].(*mcp.TextContent).Text)
		for _, rpcErr := range []*jsonrpc.Error{
			h.pollError(pollParams("ci.run_completed", repoArgs(), nil)),
			h.subscribeError(subscribeTo(repoArgs(), callbackURL, newSecret(t, 32))),
			h.unsubscribe(repoArgs(), callbackURL),
		} {
			require.EqualValues(t, codeForbidden, rpcErr.Code)
			require.Equal(t, "authentication required", rpcErr.Message, "%+v is no kind of caller, as on the subscribe path before kinds", p)
		}
		require.Zero(t, h.authz.callCount())
		require.Zero(t, h.verifier.callCount(), "%+v is refused before the callback handshake", p)
	}
}

func TestEventsListNeedsNoScope(t *testing.T) {
	t.Parallel()

	h := newEventsHarness(t, func(d *faucet.Deps) { d.PrincipalFrom = failingPrincipal })
	res, err := h.call(methodList, &rawParams{})
	require.NoError(t, err, "events/list returns the catalog and reads no principal")
	require.Contains(t, res.fields, "events")
}

func TestACallerWithoutAGitHubTokenHasNoAccessThroughTheTokenAuthorizer(t *testing.T) {
	t.Parallel()

	stub := newGitHubStub(t, reply(http.StatusOK, `{"id": 1}`))
	h := newEventsHarness(t, serviceWith(t, faucet.ScopeEventsRead), func(d *faucet.Deps) {
		d.Authorizer = faucet.TokenAuthorizer{BaseURL: stub.URL, Client: stub.Client()}
	})
	rpcErr := h.pollError(pollParams("ci.run_completed", repoArgs(), nil))
	require.EqualValues(t, codeForbidden, rpcErr.Code, "a service is not a GitHub caller, so the answer is no access, not try again")
	require.Equal(t, faucet.ErrNoAccess.Error(), rpcErr.Message)
	require.Zero(t, stub.requests.Load())
}

func TestParseScope(t *testing.T) {
	t.Parallel()

	for _, s := range []faucet.Scope{faucet.ScopeEventsRead, faucet.ScopeEventsSubscribe} {
		got, err := faucet.ParseScope(string(s))
		require.NoError(t, err)
		require.Equal(t, s, got)
	}
	for _, bad := range []string{"", "events", "EVENTS:READ", "events:read "} {
		_, err := faucet.ParseScope(bad)
		require.Error(t, err, "%q", bad)
	}
}
