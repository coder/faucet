package faucet_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/coder/superblink-events/faucet"
)

// ownedRepo is a repository and the account that owns it.
type ownedRepo struct {
	id, ownerID int64
	fullName    string
}

var (
	coderA = ownedRepo{id: 101, ownerID: ownerID, fullName: "coder/a"}
	coderB = ownedRepo{id: 102, ownerID: ownerID, fullName: "coder/b"}
	octoC  = ownedRepo{id: 103, ownerID: 9, fullName: "octo/c"}
)

func (r ownedRepo) repository() map[string]any {
	owner, _, _ := strings.Cut(r.fullName, "/")
	return map[string]any{"id": r.id, "full_name": r.fullName, "owner": map[string]any{"id": r.ownerID, "login": owner}}
}

// runURL is the html_url of the run that runOf posts for r.
func (r ownedRepo) runURL() string { return "https://github.com/" + r.fullName + "/actions/runs/1" }

// runOf is the workflow_run fixture as a completed run of r.
func runOf(t *testing.T, r ownedRepo) []byte {
	t.Helper()
	var payload map[string]any
	require.NoError(t, json.Unmarshal(fixture(t, "workflow_run_completed"), &payload))
	payload["repository"] = r.repository()
	payload["workflow_run"].(map[string]any)["html_url"] = r.runURL()
	return raw(payload)
}

// post sends a signed delivery to the webhook handler, which must store it.
func (h *webhookHarness) post(event, delivery string, body []byte) {
	h.t.Helper()
	rec := httptest.NewRecorder()
	h.svc.WebhookHandler().ServeHTTP(rec, newRequest(http.MethodPost, body, event, delivery))
	require.Equal(h.t, "stored", strings.TrimSpace(rec.Body.String()), delivery)
}

// runsAt returns the html_url of each event delivered at path, sorted.
func (d *deliverHarness) runsAt(path string) []string {
	d.t.Helper()
	var runs []string
	for _, g := range d.rcv.requests(path) {
		if url, ok := g.body(d.t).Data["html_url"].(string); ok {
			runs = append(runs, url)
		}
	}
	slices.Sort(runs)
	return runs
}

// orgGrants cover the owners coder and octo, and every repository.
func orgGrants() *fakeGrants {
	return &fakeGrants{owners: map[string]int64{"coder": ownerID, "octo": octoC.ownerID}, all: true}
}

// GitHub sends some events of an organization, such as a classic project of
// the organization, with no repository. Such a delivery comes from the
// organization: it reaches the owner and all interests that want it, and
// no interest of a repository or of named repositories.
func TestADeliveryOfAnOrganizationReachesItsOwnerAndEveryRepository(t *testing.T) {
	t.Parallel()

	h := newRawPollHarness(t, asCaller(newServicePrincipal(t), orgGrants()))
	leases := map[string]map[string]any{
		"repo":        repoArgs(),
		"owner":       args("owner", "coder"),
		"all":         args("all", true),
		"owner repos": args("owner", "coder", "repos", []string{"coder/a"}),
	}
	opened := map[string]string{}
	for name, arguments := range leases {
		opened[name] = h.pollRaw("github.project", arguments, "").Cursor
	}

	project := map[string]any{"id": 1, "name": "roadmap"}
	require.Equal(t, "stored", h.deliver("project", "of-coder", []byte(raw(args(
		"action", "created", "project", project, "organization", map[string]any{"id": ownerID, "login": "coder"})))))
	require.Equal(t, "stored", h.deliver("project", "of-octo", []byte(raw(args(
		"action", "created", "project", project, "organization", map[string]any{"id": 9, "login": "octo"})))))
	require.Equal(t, "ignored", h.deliver("project", "of-nobody", []byte(raw(args(
		"action", "created", "project", project)))), "a delivery that names no repository and no organization comes from nowhere")

	got := map[string][]string{}
	for name, arguments := range leases {
		got[name] = h.pollRaw("github.project", arguments, opened[name]).eventIDs()
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

	h := newRawPollHarness(t, asCaller(newServicePrincipal(t), orgGrants()))
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

// One subscription of a service gets the events of every repository of an
// owner, or of every repository, as GitHub sends them.
func TestASubscriptionToAnOwnerGetsTheEventsOfEachOfItsRepositories(t *testing.T) {
	t.Parallel()

	d := newDeliverHarness(t, asCaller(newServicePrincipal(t), orgGrants()))
	for path, arguments := range map[string]map[string]any{
		"/owner":    args("owner", "coder"),
		"/all":      args("all", true),
		"/narrowed": args("owner", "coder", "repos", []string{"Coder/B"}),
	} {
		d.subscribe(subscribeTo(arguments, d.rcv.url(path), newSecret(t, 32)))
	}
	d.advance(time.Second)
	for i, r := range []ownedRepo{coderA, coderB, octoC} {
		d.post("workflow_run", "run-"+string(rune('a'+i)), runOf(t, r))
	}

	res := d.deliverDue()
	require.Equal(t, 6, res.Delivered)
	require.Empty(t, res.GaveUp)
	require.Equal(t, []string{coderA.runURL(), coderB.runURL()}, d.runsAt("/owner"))
	require.Equal(t, []string{coderA.runURL(), coderB.runURL(), octoC.runURL()}, d.runsAt("/all"))
	require.Equal(t, []string{coderB.runURL()}, d.runsAt("/narrowed"))
}

// A purge that deletes an event that an owner subscription had not read
// leaves it a gap, and the next run sends the gap envelope. A subscription
// of another owner lost nothing and gets none.
func TestAPurgeLeavesAGapForAnOwnerSubscriptionThatLostAnEvent(t *testing.T) {
	t.Parallel()

	d := newDeliverHarness(t, asCaller(newServicePrincipal(t), orgGrants()))
	d.subscribe(subscribeTo(args("owner", "coder"), d.rcv.url("/coder"), newSecret(t, 32)))
	d.subscribe(subscribeTo(args("owner", "octo"), d.rcv.url("/octo"), newSecret(t, 32)))
	d.advance(time.Second)
	d.post("workflow_run", "lost", runOf(t, coderB))
	d.advance(5 * time.Minute)

	purged, err := d.svc.Purge(d.ctx, d.clock.Now().Add(-time.Minute))
	require.NoError(t, err)
	require.EqualValues(t, 1, purged.Deleted)

	res := d.deliverDue()
	require.Equal(t, 1, res.GapsSent)
	require.Len(t, d.rcv.envelopesOf("/coder", "gap"), 1, "the subscription that lost the event of coder/b")
	require.Empty(t, d.rcv.requests("/octo"))
}

// A raw subscription of an owner gets the deliveries of each repository of
// the owner, and one of every repository gets them all.
func TestARawSubscriptionToAnOwnerGetsTheDeliveriesOfEachOfItsRepositories(t *testing.T) {
	t.Parallel()

	d := newRawDeliverHarness(t, asCaller(newServicePrincipal(t), orgGrants()))
	d.subscribeRaw("github.push", hookEndpoint{path: "/owner", secret: newSecret(t, 32), args: args("owner", "coder")})
	d.subscribeRaw("github.push", hookEndpoint{path: "/all", secret: newSecret(t, 32), args: args("all", true)})
	for _, r := range []ownedRepo{coderA, coderB, octoC} {
		d.deliverRaw("push", raw(map[string]any{"ref": "refs/heads/main", "repository": r.repository()}))
	}

	res := d.deliverDue()
	require.Equal(t, 5, res.Delivered)
	require.Empty(t, res.GaveUp)
	pushedTo := func(path string) []string {
		var names []string
		for _, g := range d.rcv.requests(path) {
			names = append(names, g.body(t).Data["repository"].(map[string]any)["full_name"].(string))
		}
		slices.Sort(names)
		return names
	}
	require.Equal(t, []string{"coder/a", "coder/b"}, pushedTo("/owner"))
	require.Equal(t, []string{"coder/a", "coder/b", "octo/c"}, pushedTo("/all"))
}

// A poll lease of an owner reads the deliveries of each of its repositories,
// in the order they arrived.
func TestAPollLeaseOfAnOwnerReadsEachOfItsRepositories(t *testing.T) {
	t.Parallel()

	h := newRawPollHarness(t, asCaller(newServicePrincipal(t), orgGrants()))
	opened := h.pollRaw("github.push", args("owner", "coder"), "")
	push := func(r ownedRepo) string {
		return h.deliver("push", "push-"+r.fullName, raw(map[string]any{"ref": "refs/heads/main", "repository": r.repository()}))
	}
	require.Equal(t, "stored", push(coderA))
	require.Equal(t, "ignored", push(octoC), "no interest wants the pushes of another owner")
	require.Equal(t, "stored", push(coderB))
	page := h.pollRaw("github.push", args("owner", "coder"), opened.Cursor)
	require.Equal(t, []string{"push-coder/a", "push-coder/b"}, page.eventIDs())
	require.False(t, page.Truncated)
}

// An event read through an owner or all target says in its data which
// repository it is of, because the data of some types, such as a check
// suite's, names none. An event read through a repo target is as before.
func TestAnEventOfAnOwnerOrAllTargetNamesItsRepository(t *testing.T) {
	t.Parallel()
	named := func(t *testing.T, data json.RawMessage) (repo, repoID any) {
		t.Helper()
		var members map[string]any
		require.NoError(t, json.Unmarshal(data, &members))
		return members["repo"], members["repo_id"]
	}

	d := newDeliverHarness(t, asCaller(newServicePrincipal(t), orgGrants()))
	for path, arguments := range map[string]map[string]any{"/owner": args("owner", "coder"), "/all": args("all", true)} {
		d.subscribe(subscribeTo(arguments, d.rcv.url(path), newSecret(t, 32)))
	}
	d.advance(time.Second)
	d.post("workflow_run", "run-a", runOf(t, coderA))
	require.Equal(t, 2, d.deliverDue().Delivered)
	for _, path := range []string{"/owner", "/all"} {
		requests := d.rcv.requests(path)
		require.Len(t, requests, 1, path)
		data := requests[0].body(t).Data
		require.Equal(t, coderA.fullName, data["repo"], path)
		require.EqualValues(t, coderA.id, data["repo_id"], path)
	}

	h := newRawPollHarness(t, asCaller(newServicePrincipal(t), orgGrants()))
	opened := h.poll(pollParams("ci.run_completed", args("owner", "coder"), nil))
	h.post("workflow_run", "run-a", runOf(t, coderA))
	polled := h.poll(pollParams("ci.run_completed", args("owner", "coder"), cursorOf(opened.Cursor)))
	require.Len(t, polled.Events, 1)
	require.Equal(t, coderA.fullName, polled.Events[0].Data["repo"])
	require.EqualValues(t, coderA.id, polled.Events[0].Data["repo_id"])

	w := newWaitHarness(t, asCaller(newServicePrincipal(t), orgGrants()))
	_, _, err := w.store.InsertEvent(w.ctx, faucet.Event{
		Source: "github", DeliveryID: "run-a", Type: "ci.run_completed", RepoID: coderA.id, OwnerID: ownerID, RepoName: coderA.fullName,
		Data: json.RawMessage(`{"conclusion":"success"}`),
	})
	require.NoError(t, err)
	waited := w.call(args("owner", "coder", "types", []string{"ci.run_completed"}, "cursor", "v2.0")).success(t)
	require.Len(t, waited.Events, 1)
	repo, id := named(t, waited.Events[0].Data)
	require.Equal(t, coderA.fullName, repo)
	require.EqualValues(t, coderA.id, id)

	w.insert(repoID, "ci.run_completed", "")
	waited = w.call(args("repo", testRepo, "types", []string{"ci.run_completed"}, "cursor", "v2.0")).success(t)
	require.Len(t, waited.Events, 1)
	require.JSONEq(t, `{"conclusion":"success"}`, string(waited.Events[0].Data), "a repo target's data is as before")
}
