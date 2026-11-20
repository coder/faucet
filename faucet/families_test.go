package faucet_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/coder/superblink-events/faucet"
)

var (
	allFamilies = []string{"ci.runs", "ci.jobs", "ci.checks", "ci.suites", "ci.statuses", "pr.activity", "pr.reviews", "pr.comments", "pr.review_comments", "pr.review_threads"}
	allNames    = append(slices.Clone(allFamilies), allTypes...)
	// typePrefix is the naming rule of the exact types: the exact type of
	// action X of a family is its prefix followed by X.
	typePrefix = map[string]string{
		"ci.runs":            "ci.run_",
		"ci.jobs":            "ci.job_",
		"ci.checks":          "ci.check_",
		"ci.suites":          "ci.suite_",
		"ci.statuses":        "ci.status_",
		"pr.activity":        "pr.",
		"pr.reviews":         "pr.review_",
		"pr.comments":        "pr.comment_",
		"pr.review_comments": "pr.review_comment_",
		"pr.review_threads":  "pr.review_thread_",
	}
	// documentedActions are the actions of each event in GitHub's published
	// webhook schemas, which octokit/webhooks keeps in
	// payload-schemas/api.github.com/<event>/<action>.schema.json. Of
	// check_run and check_suite they are the actions that GitHub's docs say a
	// repository or organization webhook receives. A status has no action, and
	// its states stand in.
	documentedActions = map[string]struct {
		event   string
		actions []string
	}{
		"ci.runs":     {"workflow_run", []string{"completed", "in_progress", "requested"}},
		"ci.jobs":     {"workflow_job", []string{"completed", "in_progress", "queued", "waiting"}},
		"ci.checks":   {"check_run", []string{"completed", "created"}},
		"ci.suites":   {"check_suite", []string{"completed"}},
		"ci.statuses": {"status", []string{"error", "failure", "pending", "success"}},
		"pr.activity": {"pull_request", []string{
			"assigned", "auto_merge_disabled", "auto_merge_enabled", "closed", "converted_to_draft", "demilestoned", "dequeued",
			"edited", "enqueued", "labeled", "locked", "milestoned", "opened", "ready_for_review", "reopened",
			"review_request_removed", "review_requested", "synchronize", "unassigned", "unlabeled", "unlocked",
		}},
		"pr.reviews":         {"pull_request_review", []string{"dismissed", "edited", "submitted"}},
		"pr.comments":        {"issue_comment", []string{"created", "deleted", "edited"}},
		"pr.review_comments": {"pull_request_review_comment", []string{"created", "deleted", "edited"}},
		"pr.review_threads":  {"pull_request_review_thread", []string{"resolved", "unresolved"}},
	}
)

type familyDescriptor struct {
	Name        string `json:"name"`
	InputSchema struct {
		Properties struct {
			Actions struct {
				Items struct {
					Enum []string `json:"enum"`
				} `json:"items"`
			} `json:"actions"`
		} `json:"properties"`
	} `json:"inputSchema"`
	PayloadSchema struct {
		Properties struct {
			Event struct {
				Enum []string `json:"enum"`
			} `json:"event"`
			Action struct {
				Enum []string `json:"enum"`
			} `json:"action"`
		} `json:"properties"`
	} `json:"payloadSchema"`
}

func listFamilies(t *testing.T, h *eventsHarness) map[string]familyDescriptor {
	t.Helper()
	res, err := h.call(methodList, nil)
	require.NoError(t, err)
	var events []familyDescriptor
	require.NoError(t, json.Unmarshal(res.fields["events"], &events))
	families := map[string]familyDescriptor{}
	for _, e := range events {
		if slices.Contains(allFamilies, e.Name) {
			families[e.Name] = e
		}
	}
	require.Len(t, families, len(allFamilies))
	return families
}

func TestEachFamilyStoresEveryActionThatGitHubDocuments(t *testing.T) {
	t.Parallel()

	families := listFamilies(t, newEventsHarness(t))
	for name, documented := range documentedActions {
		require.ElementsMatch(t, documented.actions, families[name].InputSchema.Properties.Actions.Items.Enum, name)
		for _, action := range documented.actions {
			require.True(t, faucet.StoresDelivery(documented.event, action), "%s.%s is stored, so redelivery asks for it again", documented.event, action)
		}
	}
}

func TestTheExactTypeOfAnActionFollowsTheNamingRule(t *testing.T) {
	t.Parallel()

	for name, f := range listFamilies(t, newEventsHarness(t)) {
		actions := f.InputSchema.Properties.Actions.Items.Enum
		require.NotEmpty(t, actions, name)
		require.ElementsMatch(t, actions, f.PayloadSchema.Properties.Action.Enum, name)
		var want []string
		for _, action := range actions {
			want = append(want, typePrefix[name]+action)
		}
		require.ElementsMatch(t, want, f.PayloadSchema.Properties.Event.Enum, name)
	}
}

// storeDelivery stores what Normalize makes of a recorded GitHub webhook, as
// an event of repository repoID.
func storeDelivery(ctx context.Context, t *testing.T, store faucet.Store, event, fixtureName, guid string) faucet.Event {
	t.Helper()
	return storePayload(ctx, t, store, event, fixture(t, fixtureName), guid)
}

func storePayload(ctx context.Context, t *testing.T, store faucet.Store, event string, payload []byte, guid string) faucet.Event {
	t.Helper()
	ev, err := faucet.Normalize(event, payload, guid)
	require.NoError(t, err)
	require.NotNil(t, ev, guid)
	ev.RepoID = repoID
	stored, inserted, err := store.InsertEvent(ctx, *ev)
	require.NoError(t, err)
	require.True(t, inserted)
	return stored
}

// SEP-3415 lets the payloadSchema of a name change only by addition.
func TestAnExactTypeOnlyGainsOptionalPayloadProperties(t *testing.T) {
	t.Parallel()

	type payloadSchema struct {
		Properties map[string]json.RawMessage `json:"properties"`
		Required   []string                   `json:"required"`
	}
	golden, err := os.ReadFile(filepath.Join("testdata", "exact_type_payload_schemas.json"))
	require.NoError(t, err)
	var before map[string]payloadSchema
	require.NoError(t, json.Unmarshal(golden, &before))

	h := newEventsHarness(t)
	res, err := h.call(methodList, nil)
	require.NoError(t, err)
	var events []struct {
		Name          string        `json:"name"`
		PayloadSchema payloadSchema `json:"payloadSchema"`
	}
	require.NoError(t, json.Unmarshal(res.fields["events"], &events))
	seen := 0
	for _, e := range events {
		old, ok := before[e.Name]
		if !ok {
			continue
		}
		seen++
		require.Equal(t, old.Required, e.PayloadSchema.Required, e.Name)
		for name, property := range old.Properties {
			require.Contains(t, e.PayloadSchema.Properties, name, e.Name)
			require.Equal(t, withoutDescription(t, property), withoutDescription(t, e.PayloadSchema.Properties[name]),
				"%s keeps the property %s as it was, its description aside", e.Name, name)
		}
	}
	require.Equal(t, len(before), seen, "events/list offers every exact type of the golden file")
}

func withoutDescription(t *testing.T, property json.RawMessage) map[string]any {
	t.Helper()
	var p map[string]any
	require.NoError(t, json.Unmarshal(property, &p))
	delete(p, "description")
	return p
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

	res := h.call(args("repo", testRepo, "types", []string{"pr.activity"}, "cursor", "v2.0")).success(t)
	require.Len(t, res.Events, 1)
	require.Equal(t, closed.ID, res.Events[0].ID)
	require.Equal(t, "pr.closed", res.Events[0].Type, "the wait tool reports the stored type")
}

func TestPollOfAFamilyNarrowedByActionsReturnsTheEventsOfThoseActions(t *testing.T) {
	t.Parallel()

	h := newEventsHarness(t)
	start := h.headCursor(repoArgs())
	closed := storeDelivery(h.ctx, t, h.store, "pull_request", "pull_request_closed", "closed-guid")
	storeDelivery(h.ctx, t, h.store, "pull_request_review", "pull_request_review_submitted", "review-guid")

	out := h.poll(pollParams("pr.activity", repoArgs("actions", []string{"closed"}), cursorOf(start)))
	require.Equal(t, []string{closed.DeliveryID}, out.eventIDs())
	got := out.Events[0]
	require.Equal(t, "pr.activity", got.Name)
	require.Equal(t, "pr.closed", got.Data["event"])
	require.Equal(t, "closed", got.Data["action"])
}

func TestAnActionOutsideTheFamilyIsInvalidParams(t *testing.T) {
	t.Parallel()

	h := newWebhookHarness(t)
	secret := newSecret(t, 32)
	for name, params := range map[string]struct {
		event   string
		actions any
	}{
		"an action of another family":   {"pr.activity", []string{"completed"}},
		"an action GitHub never sends":  {"pr.activity", []string{"exploded"}},
		"no action":                     {"pr.activity", []string{}},
		"actions that are not a list":   {"pr.activity", "closed"},
		"actions for an exact type":     {"pr.closed", []string{"closed"}},
		"actions of numbers":            {"ci.runs", []int{1}},
		"a known and an unknown action": {"ci.runs", []string{"completed", "exploded"}},
	} {
		arguments := repoArgs("actions", params.actions)
		require.EqualValues(t, codeInvalidParams, h.pollError(pollParams(params.event, arguments, nil)).Code, "poll with %s", name)
		require.EqualValues(t, codeInvalidParams, h.subscribeError(&rawParams{
			Name: params.event, Arguments: raw(arguments), Delivery: deliveryTo(callbackURL, secret),
		}).Code, "subscribe with %s", name)
	}
	requireNothingStored(t, h, "a refused subscribe")
}

func TestRepeatedActionsAddressTheSameSubscription(t *testing.T) {
	t.Parallel()

	h := newWebhookHarness(t)
	secret := newSecret(t, 32)
	subscribeWith := func(arguments map[string]any) subscribed {
		t.Helper()
		return h.subscribe(&rawParams{Name: "pr.activity", Arguments: raw(arguments), Delivery: deliveryTo(callbackURL, secret)})
	}

	once := subscribeWith(repoArgs("actions", []string{"closed"}))
	twice := subscribeWith(repoArgs("actions", []string{"closed", "closed"}))
	require.Equal(t, once.ID, twice.ID, "an action listed twice is the same subscription")
	require.Len(t, h.rows(), 1)

	every := subscribeWith(repoArgs())
	require.NotEqual(t, once.ID, every.ID, "a family without actions is another subscription, because it takes the actions that later versions add")
	require.Len(t, h.rows(), 2)

	_, err := h.call(methodUnsubscribe, &rawParams{
		Name: "pr.activity", Arguments: raw(repoArgs("actions", []string{"closed", "closed"})), Delivery: raw(map[string]any{"url": callbackURL}),
	})
	require.NoError(t, err)
	rows := h.rows()
	require.Len(t, rows, 1, "unsubscribe reads the actions as subscribe does")
	require.Equal(t, every.ID, rows[0].KeyHash.ID())
}

func TestASubscriptionNarrowedByActionsIsDeliveredOnlyThoseActions(t *testing.T) {
	t.Parallel()

	d := newDeliverHarness(t)
	ep := d.endpoint("/closed", "actions", []string{"closed"})
	d.subscribe(&rawParams{Name: "pr.activity", Arguments: raw(ep.args), Delivery: deliveryTo(d.rcv.url(ep.path), ep.secret)})
	d.advance(time.Second)
	storeDelivery(d.ctx, t, d.store, "pull_request", "pull_request_closed", "closed-guid")
	storeDelivery(d.ctx, t, d.store, "pull_request_review", "pull_request_review_submitted", "review-guid")

	require.Equal(t, 1, d.deliverDue().Delivered)
	got := d.rcv.requests(ep.path)
	require.Len(t, got, 1)
	body := got[0].body(t)
	require.Equal(t, "pr.activity", body.Name)
	require.Equal(t, "closed", body.Data["action"])
}

func TestWaitWithActionsNarrowsOnlyTheFamiliesThatHaveThem(t *testing.T) {
	t.Parallel()

	h := newWaitHarness(t)
	closed := storeDelivery(h.ctx, t, h.store, "pull_request", "pull_request_closed", "closed-guid")
	run := storeDelivery(h.ctx, t, h.store, "workflow_run", "workflow_run_completed", "run-guid")
	storeDelivery(h.ctx, t, h.store, "pull_request_review", "pull_request_review_submitted", "review-guid")
	ids := func(types []string, actions []string) []int64 {
		t.Helper()
		res := h.call(args("repo", testRepo, "types", types, "actions", actions, "cursor", "v2.0")).success(t)
		got := make([]int64, len(res.Events))
		for i, ev := range res.Events {
			got[i] = ev.ID
		}
		return got
	}

	require.Equal(t, []int64{closed.ID, run.ID}, ids([]string{"pr.activity", "ci.runs"}, []string{"closed"}),
		"closed narrows pr.activity, and ci.runs, which has no such action, stays whole")
	require.Equal(t, []int64{closed.ID, run.ID}, ids([]string{"pr.activity", "ci.runs"}, []string{"closed", "completed"}))
	require.Equal(t, []int64{closed.ID, run.ID}, ids([]string{"pr.activity", "ci.run_completed"}, []string{"closed"}),
		"actions do not narrow an exact type")

	for name, call := range map[string]map[string]any{
		"an action of no family in types": args("repo", testRepo, "types", []string{"ci.runs"}, "actions", []string{"closed"}, "cursor", "v2.0"),
		"an action and only exact types":  args("repo", testRepo, "types", []string{"pr.closed"}, "actions", []string{"closed"}, "cursor", "v2.0"),
	} {
		require.Contains(t, h.call(call).failure(t), `action "closed"`, name)
	}
}

func TestEventNamesDescribeWhatEventsListOffers(t *testing.T) {
	t.Parallel()

	h := newEventsHarness(t)
	res, err := h.call(methodList, nil)
	require.NoError(t, err)
	var events []struct {
		Name        string `json:"name"`
		InputSchema struct {
			Properties map[string]struct {
				Items struct {
					Enum []string `json:"enum"`
				} `json:"items"`
			} `json:"properties"`
		} `json:"inputSchema"`
		PayloadSchema struct {
			Properties struct {
				Action struct {
					Enum []string `json:"enum"`
				} `json:"action"`
			} `json:"properties"`
		} `json:"payloadSchema"`
	}
	require.NoError(t, json.Unmarshal(res.fields["events"], &events))

	names := faucet.EventNames()
	require.Len(t, names, len(events))
	for i, n := range names {
		e := events[i]
		require.Equal(t, e.Name, n.Name)
		require.Equal(t, e.PayloadSchema.Properties.Action.Enum, n.Actions, n.Name)
		var filters []string
		for property := range e.InputSchema.Properties {
			if !slices.Contains([]string{"repo", "owner", "all", "repos", "pr", "sha", "actions"}, property) {
				filters = append(filters, property)
			}
		}
		require.ElementsMatch(t, filters, n.Filters, n.Name)
		if n.Exact {
			require.NotContains(t, e.InputSchema.Properties, "actions", n.Name)
			require.True(t, slices.ContainsFunc(names, func(f faucet.EventName) bool { return !f.Exact && f.Name == n.Family }), n.Name)
		} else {
			require.Equal(t, n.Name, n.Family)
			require.Equal(t, n.Actions, e.InputSchema.Properties["actions"].Items.Enum, n.Name)
		}
		for _, action := range n.Actions {
			require.True(t, faucet.StoresDelivery(n.GitHubEvent, action), "%s stores %s.%s", n.Name, n.GitHubEvent, action)
		}
		if !n.Exact {
			requireSamplesAreOfFamily(t, n)
		}
	}
}

// requireSamplesAreOfFamily normalizes each recorded sample of the family's
// GitHub event and requires every stored one to be a type of the family, so
// that a family that names the wrong GitHub event fails even where the two
// events share their actions.
func requireSamplesAreOfFamily(t *testing.T, family faucet.EventName) {
	t.Helper()
	samples, err := filepath.Glob(filepath.Join("testdata", family.GitHubEvent+"_*.json"))
	require.NoError(t, err)
	stored := 0
	for _, sample := range samples {
		if sampleEvent(filepath.Base(sample)) != family.GitHubEvent {
			continue
		}
		payload, err := os.ReadFile(sample)
		require.NoError(t, err)
		ev, err := faucet.Normalize(family.GitHubEvent, payload, "guid")
		require.NoError(t, err, sample)
		if ev == nil {
			continue
		}
		stored++
		action, ok := strings.CutPrefix(ev.Type, typePrefix[family.Name])
		require.True(t, ok && slices.Contains(family.Actions, action), "%s stores %s as %s, which is no type of %s", sample, family.GitHubEvent, ev.Type, family.Name)
	}
	require.NotZero(t, stored, "testdata holds no sample of %s that faucet stores", family.GitHubEvent)
}

// sampleEvent returns the GitHub event of a sample in testdata, which is
// named <event>_<action>.json: the longest event of a family that the name
// starts with, so pull_request_review_comment_created.json is not a
// pull_request.
func sampleEvent(name string) string {
	event := ""
	for _, n := range faucet.EventNames() {
		if strings.HasPrefix(name, n.GitHubEvent+"_") && len(n.GitHubEvent) > len(event) {
			event = n.GitHubEvent
		}
	}
	return event
}
