package faucet_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/stretchr/testify/require"
)

func TestARawEventOfARepositoryHoldsAtMostOneHundredInterests(t *testing.T) {
	t.Parallel()

	h := newRawPollHarness(t)
	secret := newSecret(t, 32)
	var first subscribed
	for login := range 10 {
		h.as(fmt.Sprintf("login-%d", login))
		for n := range 5 {
			sub := h.subscribe(rawSubscribeTo("github.push", repoArgs(), fmt.Sprintf("%s/login-%d/%d", callbackURL, login, n), secret))
			if login == 0 && n == 0 {
				first = sub
			}
			h.open()
		}
	}

	h.as("login-10")
	for name, rpcErr := range map[string]*jsonrpc.Error{
		"poll":      h.pollError(pollParams("github.push", repoArgs(), nil)),
		"subscribe": h.subscribeError(rawSubscribeTo("github.push", repoArgs(), callbackURL+"/login-10", secret)),
	} {
		require.EqualValues(t, codeResourceExhausted, rpcErr.Code, name)
		require.Contains(t, rpcErr.Message, "github.push of octo/repo has 100 live subscriptions and poll leases", name)
		require.JSONEq(t, `{"limit":"raw_interests","max":100}`, string(rpcErr.Data), name)
	}
	require.Equal(t, "ignored", h.deliver("release", "nobody-asked", rawPayload(repoID, "published", nil)))
	h.pollRaw("github.release", repoArgs(), "")

	h.as("login-0")
	renewed := h.subscribe(rawSubscribeTo("github.push", repoArgs(), callbackURL+"/login-0/0", secret))
	require.Equal(t, first.ID, renewed.ID, "a renewal adds no interest")

	h.clock.Advance(61 * time.Minute)
	h.as("login-10")
	require.Regexp(t, rawPollCursor, h.open().Cursor, "the leases that ended left room")
}

func TestALoginHoldsAtMostTenInterestsOfOneRawEvent(t *testing.T) {
	t.Parallel()

	h := newRawPollHarness(t)
	oldest := h.open()
	for range 9 {
		h.clock.Advance(time.Second)
		h.open()
	}
	h.clock.Advance(time.Second)
	h.open()
	require.True(t, h.pollRaw("github.push", repoArgs(), oldest.Cursor).Truncated,
		"an eleventh lease of the event ended the login's lease of it polled longest ago")

	secret := newSecret(t, 32)
	for n := range 10 {
		h.subscribe(rawSubscribeTo("github.issues", repoArgs(), fmt.Sprintf("%s/%d", callbackURL, n), secret))
	}
	rpcErr := h.subscribeError(rawSubscribeTo("github.issues", repoArgs(), callbackURL+"/10", secret))
	require.EqualValues(t, codeResourceExhausted, rpcErr.Code)
	require.Contains(t, rpcErr.Message, "this login has 10 live subscriptions of github.issues of octo/repo")
	require.JSONEq(t, `{"limit":"raw_subscriptions","max":10}`, string(rpcErr.Data))
	h.subscribe(rawSubscribeTo("github.issues", repoArgs(), callbackURL+"/9", secret))

	h.as("mallory")
	h.subscribe(rawSubscribeTo("github.issues", repoArgs(), callbackURL+"/10", secret))
}
