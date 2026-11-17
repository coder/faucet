package faucet_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/coder/faucet/faucet"
)

var (
	allFamilies = []string{"ci.runs", "ci.checks", "pr.activity", "pr.reviews", "pr.comments"}
	allNames    = append(slices.Clone(allFamilies), allTypes...)
	familyOf    = map[string]string{
		"ci.check_completed":  "ci.checks",
		"ci.run_completed":    "ci.runs",
		"pr.closed":           "pr.activity",
		"pr.comment_created":  "pr.comments",
		"pr.review_submitted": "pr.reviews",
	}
)

// storeDelivery stores what Normalize makes of a recorded GitHub webhook, as
// an event of repository repoID.
func storeDelivery(ctx context.Context, t *testing.T, store faucet.Store, event, fixtureName, guid string) faucet.Event {
	t.Helper()
	ev, err := faucet.Normalize(event, fixture(t, fixtureName), guid)
	require.NoError(t, err)
	require.NotNil(t, ev, fixtureName)
	ev.RepoID = repoID
	stored, inserted, err := store.InsertEvent(ctx, *ev)
	require.NoError(t, err)
	require.True(t, inserted)
	return stored
}

func TestPollOfAFamilyNamesEachEventByTheFamilyAndCarriesItsExactType(t *testing.T) {
	t.Parallel()

	h := newEventsHarness(t)
	start := h.headCursor(repoArgs())
	closed := storeDelivery(h.ctx, t, h.store, "pull_request", "pull_request_closed", "closed-guid")
	storeDelivery(h.ctx, t, h.store, "workflow_run", "workflow_run_completed", "run-guid")

	family := h.poll(pollParams("pr.activity", repoArgs(), cursorOf(start)))
	require.Equal(t, []string{closed.DeliveryID}, family.eventIDs(), "a family without actions returns every type of it and nothing else")
	got := family.Events[0]
	require.Equal(t, "pr.activity", got.Name, "an occurrence is named by the name that was polled")
	require.Equal(t, "pr.closed", got.Data["event"])
	require.Equal(t, "closed", got.Data["action"])
	require.Equal(t, false, got.Data["merged"])

	legacy := h.poll(pollParams("pr.closed", repoArgs(), cursorOf(start)))
	require.Equal(t, family.eventIDs(), legacy.eventIDs())
	require.Equal(t, "pr.closed", legacy.Events[0].Name, "a name from before families names its events as before")
	require.Equal(t, got.Data, legacy.Events[0].Data, "and carries the same data")
}

func TestASubscriptionToAFamilyIsDeliveredUnderTheFamilyName(t *testing.T) {
	t.Parallel()

	d := newDeliverHarness(t)
	ep := d.endpoint("/activity")
	sub := d.subscribe(&rawParams{Name: "pr.activity", Arguments: raw(ep.args), Delivery: deliveryTo(d.rcv.url(ep.path), ep.secret)})
	d.advance(time.Second)
	storeDelivery(d.ctx, t, d.store, "pull_request", "pull_request_merged", "merged-guid")
	storeDelivery(d.ctx, t, d.store, "workflow_run", "workflow_run_completed", "run-guid")

	require.Equal(t, 1, d.deliverDue().Delivered)
	got := d.rcv.requests(ep.path)
	require.Len(t, got, 1, "the subscription gets the events of its family and no other")
	require.True(t, got[0].signatureValid(ep.secret))
	require.Equal(t, sub.ID, got[0].Header.Get("X-MCP-Subscription-Id"))
	body := got[0].body(t)
	require.Equal(t, "pr.activity", body.Name, "the body names the subscribed name and not the stored type")
	require.Equal(t, "pr.closed", body.Data["event"])
	require.Equal(t, "closed", body.Data["action"])
	require.Equal(t, true, body.Data["merged"])
}

func TestWaitForAFamilyReturnsTheExactTypeOfEachEvent(t *testing.T) {
	t.Parallel()

	h := newWaitHarness(t)
	closed := storeDelivery(h.ctx, t, h.store, "pull_request", "pull_request_closed", "closed-guid")
	storeDelivery(h.ctx, t, h.store, "issue_comment", "issue_comment_created_on_pr", "comment-guid")

	res := h.call(args("repo", testRepo, "types", []string{"pr.activity"}, "cursor", 0)).success(t)
	require.Len(t, res.Events, 1)
	require.Equal(t, closed.ID, res.Events[0].ID)
	require.Equal(t, "pr.closed", res.Events[0].Type, "the wait tool reports the stored type")
}
