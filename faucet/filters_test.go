package faucet_test

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/coder/superblink-events/faucet"
)

var (
	ciFailedOnMain = []any{"actions", []string{"completed"}, "head_branch", []string{"main"}, "conclusion", []string{"failure", "timed_out"}}
	prsIntoMain    = []any{"actions", []string{"opened", "reopened", "ready_for_review", "synchronize"}, "base_branch", []string{"main"}}
)

// The recorded samples run CI on master and open the pull request from
// changes into master. runOn and prInto edit a sample to the branch and the
// conclusion that a case needs, and leave the rest as GitHub sent it.
func runOn(t *testing.T, branch, conclusion string) func(doc map[string]any) {
	return func(doc map[string]any) {
		run := member(t, doc, "workflow_run")
		run["head_branch"], run["conclusion"] = branch, conclusion
	}
}

func prInto(t *testing.T, base, head string) func(doc map[string]any) {
	return func(doc map[string]any) {
		member(t, doc, "pull_request", "base")["ref"] = base
		member(t, doc, "pull_request", "head")["ref"] = head
	}
}

type matched struct{ first, second faucet.Event }

func storeRuns(ctx context.Context, t *testing.T, store faucet.Store) matched {
	t.Helper()
	run := func(guid string, edits ...func(map[string]any)) faucet.Event {
		return storePayload(ctx, t, store, "workflow_run", mutate(t, "workflow_run_completed", edits...), guid)
	}
	failed := run("failed-on-main", runOn(t, "main", "failure"))
	run("passed-on-main", runOn(t, "main", "success"))
	run("failed-on-dev", runOn(t, "dev", "failure"))
	timedOut := run("timed-out-on-main", runOn(t, "main", "timed_out"))
	run("failed-on-main-of-a-fork", runOn(t, "main", "failure"), headIn(t, "workflow_run", forkRepo))
	storePayload(ctx, t, store, "pull_request", mutate(t, "pull_request_opened", prInto(t, "main", "feature")), "opened-into-main")
	return matched{failed, timedOut}
}

func storePullRequests(ctx context.Context, t *testing.T, store faucet.Store) matched {
	t.Helper()
	pr := func(fixtureName, guid string, edits ...func(map[string]any)) faucet.Event {
		return storePayload(ctx, t, store, "pull_request", mutate(t, fixtureName, edits...), guid)
	}
	opened := pr("pull_request_opened", "opened-into-main", prInto(t, "main", "feature"))
	pr("pull_request_opened", "opened-into-master")
	pr("pull_request_labeled", "labeled-into-main", prInto(t, "main", "feature"))
	pushed := pr("pull_request_synchronize", "pushed-into-main", prInto(t, "main", "feature"))
	pr("pull_request_closed", "closed-into-main", prInto(t, "main", "feature"))
	storePayload(ctx, t, store, "workflow_run", mutate(t, "workflow_run_completed", runOn(t, "main", "failure")), "failed-on-main")
	return matched{opened, pushed}
}

func TestPollWithFieldFiltersReturnsOnlyTheMatchingEventsPageByPage(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		family    string
		arguments []any
		store     func(context.Context, *testing.T, faucet.Store) matched
	}{
		"CI failed on main":       {"ci.runs", ciFailedOnMain, storeRuns},
		"pull requests into main": {"pr.activity", prsIntoMain, storePullRequests},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			h := newEventsHarness(t)
			start := h.headCursor(repoArgs())
			want := tc.store(h.ctx, t, h.store)
			arguments := repoArgs(tc.arguments...)

			all := h.poll(pollParams(tc.family, arguments, cursorOf(start)))
			require.Equal(t, []string{want.first.DeliveryID, want.second.DeliveryID}, all.eventIDs())
			require.False(t, all.HasMore)

			params := pollParams(tc.family, arguments, cursorOf(start))
			params.MaxEvents = raw(1)
			first := h.poll(params)
			require.Equal(t, []string{want.first.DeliveryID}, first.eventIDs())
			require.True(t, first.HasMore, "a page of one says that a match is left")
			params.Cursor = cursorOf(first.Cursor)
			second := h.poll(params)
			require.Equal(t, []string{want.second.DeliveryID}, second.eventIDs())
			require.False(t, second.HasMore, "and none after the second, though events that do not match follow it")
			params.Cursor = cursorOf(second.Cursor)
			require.Empty(t, h.poll(params).Events)
		})
	}
}

func TestASubscriptionWithFieldFiltersIsDeliveredOnlyTheMatchingEvents(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		family    string
		arguments []any
		store     func(context.Context, *testing.T, faucet.Store) matched
		field     string
	}{
		"CI failed on main":       {"ci.runs", ciFailedOnMain, storeRuns, "conclusion"},
		"pull requests into main": {"pr.activity", prsIntoMain, storePullRequests, "action"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			d := newDeliverHarness(t)
			ep := d.endpoint("/hook", tc.arguments...)
			d.subscribe(&rawParams{Name: tc.family, Arguments: raw(ep.args), Delivery: deliveryTo(d.rcv.url(ep.path), ep.secret)})
			d.advance(time.Second)
			want := tc.store(d.ctx, t, d.store)

			require.Equal(t, 2, d.deliverDue().Delivered)
			var got []any
			for _, r := range d.rcv.requests(ep.path) {
				body := r.body(t)
				require.Equal(t, tc.family, body.Name)
				got = append(got, body.Data[tc.field])
			}
			require.ElementsMatch(t, []any{fieldOf(t, want.first, tc.field), fieldOf(t, want.second, tc.field)}, got)
		})
	}
}

// fieldOf reads a field of an event as the webhook body carries it.
func fieldOf(t *testing.T, ev faucet.Event, field string) any {
	t.Helper()
	var data map[string]any
	require.NoError(t, json.Unmarshal(ev.Data, &data))
	if field == "action" {
		return map[string]string{"pr.opened": "opened", "pr.synchronize": "synchronize"}[ev.Type]
	}
	return data[field]
}

func TestAFilteredSubscriptionGetsItsEventPastMoreThanAPageOfOthers(t *testing.T) {
	t.Parallel()

	d := newDeliverHarness(t)
	ep := d.endpoint("/hook", ciFailedOnMain...)
	d.subscribe(&rawParams{Name: "ci.runs", Arguments: raw(ep.args), Delivery: deliveryTo(d.rcv.url(ep.path), ep.secret)})
	d.advance(time.Second)
	passed := mutate(t, "workflow_run_completed", runOn(t, "main", "success"))
	for i := range faucet.MaxPendingDeliveries + 1 {
		storePayload(d.ctx, t, d.store, "workflow_run", passed, fmt.Sprintf("passed-%d", i))
	}
	storePayload(d.ctx, t, d.store, "workflow_run", mutate(t, "workflow_run_completed", runOn(t, "main", "failure")), "failed")

	require.Equal(t, 1, d.deliverDue().Delivered, "the store filters before the page limit, so one run reads past every passing run")
	require.Equal(t, "failure", d.rcv.requests(ep.path)[0].body(t).Data["conclusion"])
}

func TestAHeadBranchFilterNeverMatchesABranchOfAFork(t *testing.T) {
	t.Parallel()

	h := newEventsHarness(t)
	start := h.headCursor(repoArgs())
	ours := storePayload(h.ctx, t, h.store, "workflow_run", mutate(t, "workflow_run_completed", runOn(t, "main", "failure")), "run-on-main")
	storePayload(h.ctx, t, h.store, "workflow_run", mutate(t, "workflow_run_completed", runOn(t, "main", "failure"), headIn(t, "workflow_run", forkRepo)), "run-edited-to-a-forks-main")
	ourPR := storePayload(h.ctx, t, h.store, "pull_request", mutate(t, "pull_request_opened", prInto(t, "master", "main")), "pr-from-main")
	storePayload(h.ctx, t, h.store, "pull_request", mutate(t, "pull_request_opened", prInto(t, "master", "main"), headIn(t, "pull_request", forkRepo)), "pr-edited-to-come-from-a-forks-main")
	storePayload(h.ctx, t, h.store, "pull_request", mutate(t, "pull_request_opened", prInto(t, "master", "main"), headIn(t, "pull_request", nil)), "pr-edited-to-come-from-a-deleted-forks-main")
	h.insert(repoID, "ci.run_completed", "")

	onMain := repoArgs("head_branch", []string{"main"})
	require.Equal(t, []string{ours.DeliveryID}, h.poll(pollParams("ci.runs", onMain, cursorOf(start))).eventIDs(),
		"a fork's main is not ours, and an event stored before from_fork existed does not match either")
	require.Equal(t, []string{ourPR.DeliveryID}, h.poll(pollParams("pr.activity", onMain, cursorOf(start))).eventIDs())
}

func TestReorderedOrRepeatedFilterValuesAddressTheSameSubscription(t *testing.T) {
	t.Parallel()

	h := newWebhookHarness(t)
	secret := newSecret(t, 32)
	subscribeWith := func(arguments map[string]any) subscribed {
		t.Helper()
		return h.subscribe(&rawParams{Name: "ci.runs", Arguments: raw(arguments), Delivery: deliveryTo(callbackURL, secret)})
	}

	sorted := subscribeWith(repoArgs("head_branch", []string{"main", "release"}, "conclusion", []string{"failure", "timed_out"}))
	shuffled := repoArgs("conclusion", []string{"timed_out", "failure", "timed_out"}, "head_branch", []string{"release", "main", "main"})
	require.Equal(t, sorted.ID, subscribeWith(shuffled).ID)
	require.Len(t, h.rows(), 1)

	other := subscribeWith(repoArgs("head_branch", []string{"main"}, "conclusion", []string{"failure", "timed_out"}))
	require.NotEqual(t, sorted.ID, other.ID, "other values are another subscription")
	require.Len(t, h.rows(), 2)

	_, err := h.call(methodUnsubscribe, &rawParams{Name: "ci.runs", Arguments: raw(shuffled), Delivery: raw(map[string]any{"url": callbackURL})})
	require.NoError(t, err)
	rows := h.rows()
	require.Len(t, rows, 1, "unsubscribe reads the filters as subscribe does")
	require.Equal(t, other.ID, rows[0].KeyHash.ID())
}

func TestASubscriptionWithoutFieldFiltersKeepsItsKey(t *testing.T) {
	t.Parallel()

	h := newWebhookHarness(t)
	secret := newSecret(t, 32)
	for spelled, arguments := range map[string]map[string]any{
		`{"repo":"octo/repo"}`:                         repoArgs(),
		`{"repo":"octo/repo","pr":7}`:                  repoArgs("pr", 7),
		`{"repo":"octo/repo","actions":["completed"]}`: repoArgs("actions", []string{"completed"}),
	} {
		sub := h.subscribe(&rawParams{Name: "ci.runs", Arguments: raw(arguments), Delivery: deliveryTo(callbackURL, secret)})
		require.Equal(t, faucet.NewSubscriptionKeyHash("octocat", callbackURL, "ci.runs", json.RawMessage(spelled)).ID(), sub.ID,
			"arguments without field filters are spelled as before them")
	}
}

func TestAFieldFilterThatTheNameDoesNotOfferIsInvalidParams(t *testing.T) {
	t.Parallel()

	h := newWebhookHarness(t)
	secret := newSecret(t, 32)
	for name, params := range map[string]struct {
		event     string
		arguments map[string]any
	}{
		"an unknown filter":          {"ci.runs", repoArgs("branch", []string{"main"})},
		"a filter of another family": {"ci.runs", repoArgs("base_branch", []string{"main"})},
		"a head branch of check runs, which name no head repository": {"ci.checks", repoArgs("head_branch", []string{"main"})},
		"a head branch of jobs":                        {"ci.jobs", repoArgs("head_branch", []string{"main"})},
		"a head branch of check suites":                {"ci.suites", repoArgs("head_branch", []string{"main"})},
		"a conclusion of statuses, which have a state": {"ci.statuses", repoArgs("conclusion", []string{"failure"})},
		"a conclusion for pull requests":               {"pr.activity", repoArgs("conclusion", []string{"failure"})},
		"a filter of a family with no filters":         {"pr.reviews", repoArgs("head_branch", []string{"main"})},
		"a filter for an exact type":                   {"ci.run_completed", repoArgs("head_branch", []string{"main"})},
		"no value":                                     {"ci.runs", repoArgs("head_branch", []string{})},
		"a value that is not a list":                   {"ci.runs", repoArgs("head_branch", "main")},
		"values that are not strings":                  {"ci.runs", repoArgs("conclusion", []int{1})},
		"a known filter beside an unknown one":         {"ci.runs", repoArgs("conclusion", []string{"failure"}, "status", []string{"completed"})},
	} {
		require.EqualValues(t, codeInvalidParams, h.pollError(pollParams(params.event, params.arguments, nil)).Code, "poll with %s", name)
		require.EqualValues(t, codeInvalidParams, h.subscribeError(&rawParams{
			Name: params.event, Arguments: raw(params.arguments), Delivery: deliveryTo(callbackURL, secret),
		}).Code, "subscribe with %s", name)
	}
	requireNothingStored(t, h, "a refused subscribe")
}

type inputProperties struct {
	Name        string `json:"name"`
	InputSchema struct {
		Properties map[string]struct {
			Items struct {
				Type string `json:"type"`
			} `json:"items"`
		} `json:"properties"`
	} `json:"inputSchema"`
	PayloadSchema struct {
		Properties map[string]json.RawMessage `json:"properties"`
	} `json:"payloadSchema"`
}

func TestEventsListOffersEachFieldFilterOnlyWhereTheDataHasTheField(t *testing.T) {
	t.Parallel()

	h := newEventsHarness(t)
	res, err := h.call(methodList, nil)
	require.NoError(t, err)
	var events []inputProperties
	require.NoError(t, json.Unmarshal(res.fields["events"], &events))
	want := map[string][]string{
		"ci.runs":     {"conclusion", "head_branch"},
		"ci.jobs":     {"conclusion"},
		"ci.checks":   {"conclusion"},
		"ci.suites":   {"conclusion"},
		"pr.activity": {"base_branch", "head_branch"},
	}
	fieldFilters := []string{"base_branch", "conclusion", "head_branch"}
	for _, e := range events {
		var offered []string
		for property := range e.InputSchema.Properties {
			if slices.Contains(fieldFilters, property) {
				offered = append(offered, property)
				require.Contains(t, e.PayloadSchema.Properties, property, "%s offers %s, a field of its data", e.Name, property)
				require.Equal(t, "string", e.InputSchema.Properties[property].Items.Type)
			}
		}
		require.ElementsMatch(t, want[e.Name], offered, e.Name)
	}
}

func waitIDs(t *testing.T, h *waitHarness, types []string, filters ...any) []int64 {
	t.Helper()
	res := h.call(args(append([]any{"repo", testRepo, "types", types, "cursor", "v2.0"}, filters...)...)).success(t)
	ids := make([]int64, len(res.Events))
	for i, ev := range res.Events {
		ids[i] = ev.ID
	}
	return ids
}

func TestWaitWithFieldFiltersReturnsOnlyTheMatchingEvents(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		family    string
		arguments []any
		store     func(context.Context, *testing.T, faucet.Store) matched
	}{
		"CI failed on main":       {"ci.runs", ciFailedOnMain, storeRuns},
		"pull requests into main": {"pr.activity", prsIntoMain, storePullRequests},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			h := newWaitHarness(t)
			want := tc.store(h.ctx, t, h.store)
			require.Equal(t, []int64{want.first.ID, want.second.ID}, waitIDs(t, h, []string{tc.family}, tc.arguments...))
		})
	}
}

func TestWaitWithAFieldFilterLeavesTheNamesThatDoNotOfferItWhole(t *testing.T) {
	t.Parallel()

	h := newWaitHarness(t)
	failed := storePayload(h.ctx, t, h.store, "workflow_run", mutate(t, "workflow_run_completed", runOn(t, "dev", "failure")), "failed")
	storePayload(h.ctx, t, h.store, "workflow_run", mutate(t, "workflow_run_completed", runOn(t, "dev", "success")), "passed")
	opened := storeDelivery(h.ctx, t, h.store, "pull_request", "pull_request_opened", "opened")
	closed := storeDelivery(h.ctx, t, h.store, "pull_request", "pull_request_closed", "closed")

	require.Equal(t, []int64{failed.ID, opened.ID, closed.ID}, waitIDs(t, h, []string{"ci.runs", "pr.activity"}, "conclusion", []string{"failure"}),
		"conclusion narrows ci.runs, and pr.activity, which has no conclusion, stays whole")
	require.Equal(t, []int64{closed.ID}, waitIDs(t, h, []string{"pr.activity", "pr.closed"}, "base_branch", []string{"release"}),
		"base_branch narrows pr.activity, and pr.closed, an exact type, offers no field filter and stays whole")

	for name, filters := range map[string][]any{
		"a filter of another family":    {"types", []string{"ci.runs"}, "base_branch", []string{"main"}},
		"a filter and only exact types": {"types", []string{"ci.run_completed"}, "head_branch", []string{"main"}},
		"a filter of none of several":   {"types", []string{"pr.activity", "pr.reviews"}, "conclusion", []string{"failure"}},
	} {
		require.Contains(t, h.call(args(append([]any{"repo", testRepo, "cursor", "v2.0"}, filters...)...)).failure(t), "is a filter of none of the names in types", name)
	}
}

func TestAConclusionFilterMatchesOnlyTheCIEventsThatEndedSo(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		family, event, unfinished, finished, conclusion string
	}{
		{"ci.runs", "workflow_run", "workflow_run_requested", "workflow_run_completed", "success"},
		{"ci.jobs", "workflow_job", "workflow_job_queued", "workflow_job_completed", "failure"},
		{"ci.checks", "check_run", "check_run_created", "check_run_completed", "success"},
		{"ci.suites", "check_suite", "", "check_suite_completed", "success"},
	} {
		t.Run(tc.family, func(t *testing.T) {
			t.Parallel()

			h := newEventsHarness(t)
			start := h.headCursor(repoArgs())
			if tc.unfinished != "" {
				storeDelivery(h.ctx, t, h.store, tc.event, tc.unfinished, "unfinished")
			}
			finished := storeDelivery(h.ctx, t, h.store, tc.event, tc.finished, "finished")

			conclusion := func(values ...string) []string {
				t.Helper()
				return h.poll(pollParams(tc.family, repoArgs("conclusion", values), cursorOf(start))).eventIDs()
			}
			require.Equal(t, []string{finished.DeliveryID}, conclusion(tc.conclusion, "null", ""),
				"an event whose conclusion is still null matches no value")
			require.Empty(t, conclusion("neutral"))
		})
	}
}
