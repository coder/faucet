package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"

	"github.com/coder/superblink-events/faucet"
)

// coderID is the account ID of the organization coder in these tests.
const coderID = 5

type deliveredRepo struct {
	id       int64
	fullName string
}

var (
	coderExisting = deliveredRepo{id: 501, fullName: "coder/existing"}
	coderNewRepo  = deliveredRepo{id: 502, fullName: "coder/new-repo"}
)

// deliverRunOf posts the workflow_run fixture as a run of repo, a repository
// of coder.
func (s *loginServer) deliverRunOf(t *testing.T, delivery string, repo deliveredRepo) {
	t.Helper()
	s.deliverRunOwnedBy(t, delivery, repo, coderID, "coder")
}

// deliverRunOwnedBy posts the workflow_run fixture as a run of repo, a
// repository of the account ownerID that the delivery names login.
func (s *loginServer) deliverRunOwnedBy(t *testing.T, delivery string, repo deliveredRepo, ownerID int64, login string) {
	t.Helper()
	body, err := os.ReadFile("../../faucet/testdata/workflow_run_completed.json")
	require.NoError(t, err)
	var payload map[string]any
	require.NoError(t, json.Unmarshal(body, &payload))
	_, name, _ := strings.Cut(repo.fullName, "/")
	payload["repository"] = map[string]any{
		"id": repo.id, "name": name, "full_name": repo.fullName,
		"owner": map[string]any{"id": ownerID, "login": login, "type": "Organization"},
	}
	payload["workflow_run"].(map[string]any)["html_url"] = "https://github.com/" + repo.fullName + "/actions/runs/1"
	body, err = json.Marshal(payload)
	require.NoError(t, err)
	s.post(t, "workflow_run", delivery, body)
}

// callbacks records the html_url of every event that faucet delivers, by
// the path of the callback URL.
type callbacks struct {
	*httptest.Server
	mu  sync.Mutex
	got map[string][]string
}

func newCallbacks(t *testing.T) *callbacks {
	t.Helper()
	c := &callbacks{got: map[string][]string{}}
	c.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Data struct {
				HTMLURL string `json:"html_url"`
			} `json:"data"`
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		c.mu.Lock()
		c.got[r.URL.Path] = append(c.got[r.URL.Path], body.Data.HTMLURL)
		c.mu.Unlock()
	}))
	t.Cleanup(c.Close)
	return c
}

func (c *callbacks) runsOf(path string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	runs := slices.Clone(c.got[path])
	slices.Sort(runs)
	return runs
}

func runOf(repo deliveredRepo) string {
	return "https://github.com/" + repo.fullName + "/actions/runs/1"
}

// waitFor calls the wait tool for ci.run_completed with arguments, and
// returns the repository IDs of the events and the cursor.
func waitFor(ctx context.Context, t *testing.T, session *mcp.ClientSession, arguments map[string]any) ([]int64, string) {
	t.Helper()
	arguments = maps.Clone(arguments)
	arguments["types"] = []string{"ci.run_completed"}
	arguments["timeout_s"] = 1
	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "wait_for_github_events", Arguments: arguments})
	require.NoError(t, err)
	require.False(t, res.IsError, toolText(t, res))
	var out struct {
		Events []struct {
			RepoID int64 `json:"repo_id"`
		} `json:"events"`
		Cursor string `json:"cursor"`
	}
	raw, err := json.Marshal(res.StructuredContent)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &out))
	repos := make([]int64, len(out.Events))
	for i, ev := range out.Events {
		repos[i] = ev.RepoID
	}
	slices.Sort(repos)
	return repos, out.Cursor
}

func subscribeOwner(ctx context.Context, t *testing.T, session *mcp.ClientSession, callbackURL string, arguments map[string]any) {
	t.Helper()
	_, err := mcp.CallCustomMethod[*subscribeParams, *eventsResult](ctx, session, "events/subscribe", &subscribeParams{
		Name: "ci.run_completed", Arguments: arguments,
		Delivery: map[string]any{"mode": "webhook", "url": callbackURL, "secret": "whsec_" + base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{2}, 32))},
	})
	require.NoError(t, err)
}

// A service granted an owner, or every repository, reads a repository of the
// owner that no delivery has named yet: faucet knows the owner from another
// of its repositories, and the first delivery of the new one reaches the
// waits and the subscriptions that the service opened before it. repos
// narrows them to the repositories it names.
func TestAServiceGrantedAnOwnerReadsARepositoryBeforeItsFirstDelivery(t *testing.T) {
	t.Parallel()
	for _, grant := range []string{"org:coder", "all"} {
		t.Run(grant, func(t *testing.T) {
			t.Parallel()
			hooks := newCallbacks(t)
			key, err := faucet.NewWebhookKey(bytes.Repeat([]byte{1}, 32))
			require.NoError(t, err)
			allow, err := faucet.NewCallbackAllowlist([]string{hooks.URL}, true)
			require.NoError(t, err)
			srv := newLoginServer(t, func(c *loginConfig) {
				c.webhook = &faucet.WebhookDeps{Key: key, Callbacks: allow, AllowInsecureLoopback: true}
			})
			srv.deliverRunOf(t, "existing-1", coderExisting)
			secret := srv.client(t, "org-watcher", "/github/mcp", bothScopes, grant)

			ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
			defer cancel()
			session := connectSubscriber(ctx, t, &testServer{Server: srv.Server, mcpPath: "/github/mcp"}, srv.token(t, "org-watcher", secret))
			owner := map[string]any{"owner": "coder"}
			narrowed := map[string]any{"owner": "coder", "repos": []string{"coder/new-repo"}}
			_, ownerCursor := waitFor(ctx, t, session, owner)
			_, narrowedCursor := waitFor(ctx, t, session, narrowed)
			subscribeOwner(ctx, t, session, hooks.URL+"/owner", owner)
			subscribeOwner(ctx, t, session, hooks.URL+"/narrowed", narrowed)

			srv.deliverRunOf(t, "new-1", coderNewRepo)
			srv.deliverRunOf(t, "existing-2", coderExisting)

			repos, _ := waitFor(ctx, t, session, map[string]any{"owner": "coder", "cursor": ownerCursor})
			require.Equal(t, []int64{coderExisting.id, coderNewRepo.id}, repos)
			repos, _ = waitFor(ctx, t, session, map[string]any{"owner": "coder", "repos": []string{"coder/new-repo"}, "cursor": narrowedCursor})
			require.Equal(t, []int64{coderNewRepo.id}, repos)

			_, err = srv.svc.DeliverDue(ctx)
			require.NoError(t, err)
			require.Equal(t, []string{runOf(coderExisting), runOf(coderNewRepo)}, hooks.runsOf("/owner"))
			require.Equal(t, []string{runOf(coderNewRepo)}, hooks.runsOf("/narrowed"))
		})
	}
}

// A person names one repository: GitHub answers for a person's access to a
// repository, and has no answer for an owner or every repository.
func TestAPersonIsToldThatOwnerAndAllTargetsAreForServices(t *testing.T) {
	t.Parallel()
	srv := newBrowserServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	session := connectSubscriber(ctx, t, &testServer{Server: srv.Server, mcpPath: "/github/mcp"}, srv.accessToken(ctx, t, "/github/mcp"))
	const want = "owner and all targets are for service clients; name one repository with repo: repository not found or not accessible"

	for _, target := range []map[string]any{{"owner": "o"}, {"all": true}} {
		arguments := maps.Clone(target)
		arguments["types"] = []string{"ci.run_completed"}
		res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "wait_for_github_events", Arguments: arguments})
		require.NoError(t, err)
		require.True(t, res.IsError)
		require.Equal(t, want, toolText(t, res))

		_, err = callEvents(ctx, session, "events/poll", &eventsParams{Name: "ci.run_completed", Arguments: target})
		var denied *jsonrpc.Error
		require.ErrorAs(t, err, &denied)
		require.EqualValues(t, -32024, denied.Code)
		require.Equal(t, want, denied.Message)
	}
	srv.github.mu.Lock()
	defer srv.github.mu.Unlock()
	require.NotContains(t, strings.Join(srv.github.apiCalls, " "), "/repos/", "no repository is looked up")
}

// An org grant covers each repository whose deliveries named the owner's
// account, and no repository of another owner.
func TestAnOrgGrantCoversTheRepositoriesOfItsOwner(t *testing.T) {
	t.Parallel()
	srv := newLoginServer(t)
	srv.deliverRunOf(t, "existing-1", coderExisting)
	srv.deliver(t, "octo-1")
	secret := srv.client(t, "org-watcher", "/github/mcp", bothScopes, "org:Coder")
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	session, err := srv.connectService(ctx, t, "/github/mcp", "org-watcher", secret)
	require.NoError(t, err)

	repos, _ := waitFor(ctx, t, session, map[string]any{"repo": "coder/existing", "cursor": "v2.0"})
	require.Equal(t, []int64{coderExisting.id}, repos)

	res, err := call(ctx, session, "octo-org/octo-repo", map[string]any{"timeout_s": 1})
	require.NoError(t, err)
	require.True(t, res.IsError)
	require.Contains(t, toolText(t, res), "no grant of this service names octo-org/octo-repo")
	res, err = call(ctx, session, "coder/new-repo", map[string]any{"timeout_s": 1})
	require.NoError(t, err)
	require.True(t, res.IsError)
	require.Contains(t, toolText(t, res), "faucet has received no webhook delivery from coder/new-repo yet")
}

// A service polls a raw event of an organization over the server as run()
// builds it: a classic project of the organization names no repository and
// reaches the owner's lease and the lease of every repository, beside a
// project of one of its repositories.
func TestAServicePollsTheRawEventsOfAnOrganization(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	srv := newLoginServer(t, func(c *loginConfig) { c.rawEvents = []string{"project", "push"} })
	srv.deliverRunOf(t, "existing-1", coderExisting)
	secret := srv.client(t, "org-watcher", "/github/mcp", []faucet.Scope{faucet.ScopeEventsRead}, "org:coder", "all")
	session := srv.connectServiceEvents(ctx, t, "org-watcher", secret)

	poll := func(arguments map[string]any, cursor *string) rawPollPage {
		t.Helper()
		fields, err := callEvents(ctx, session, "events/poll", &eventsParams{Name: "github.project", Arguments: arguments, Cursor: cursor})
		require.NoError(t, err)
		var page rawPollPage
		require.NoError(t, json.Unmarshal(fields["events"], &page.Events))
		require.NoError(t, json.Unmarshal(fields["cursor"], &page.Cursor))
		return page
	}
	ids := func(page rawPollPage) []string {
		var out []string
		for _, ev := range page.Events {
			out = append(out, ev.EventID)
		}
		return out
	}
	owner, all := map[string]any{"owner": "coder"}, map[string]any{"all": true}
	ownerOpened, allOpened := poll(owner, nil), poll(all, nil)

	project := map[string]any{"id": 1, "name": "roadmap"}
	body, err := json.Marshal(map[string]any{"action": "created", "project": project, "organization": map[string]any{"id": coderID, "login": "coder"}})
	require.NoError(t, err)
	srv.post(t, "project", "of-the-org", body)
	body, err = json.Marshal(map[string]any{"action": "created", "project": project, "repository": map[string]any{
		"id": coderNewRepo.id, "full_name": coderNewRepo.fullName, "owner": map[string]any{"id": coderID, "login": "coder"},
	}})
	require.NoError(t, err)
	srv.post(t, "project", "of-new-repo", body)
	body, err = json.Marshal(map[string]any{"action": "created", "project": project, "organization": map[string]any{"id": 9, "login": "octo"}})
	require.NoError(t, err)
	srv.post(t, "project", "of-another-org", body)

	require.Equal(t, []string{"of-the-org", "of-new-repo"}, ids(poll(owner, &ownerOpened.Cursor)))
	require.Equal(t, []string{"of-the-org", "of-new-repo", "of-another-org"}, ids(poll(all, &allOpened.Cursor)))
}

// waitError calls the wait tool for ci.run_completed with arguments and
// returns the text of the error that it answers.
func waitError(ctx context.Context, t *testing.T, session *mcp.ClientSession, arguments map[string]any) string {
	t.Helper()
	arguments = maps.Clone(arguments)
	arguments["types"] = []string{"ci.run_completed"}
	arguments["timeout_s"] = 1
	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "wait_for_github_events", Arguments: arguments})
	require.NoError(t, err)
	require.True(t, res.IsError, "the wait is refused: %s", toolText(t, res))
	return toolText(t, res)
}

// An org grant names the account that held its login when the client was
// made. It follows that account through a rename, and neither the account
// that takes the old login next nor a delivery from before the rename that
// a hook admin sends again by hand points it at another account.
func TestAnOrgGrantFollowsItsAccountAndNoOtherHolderOfItsLogin(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	srv := newLoginServer(t)
	srv.deliverRunOf(t, "existing-1", coderExisting)
	coderSecret := srv.client(t, "coder-watcher", "/github/mcp", bothScopes, "org:coder")

	renamed, err := json.Marshal(map[string]any{
		"action": "renamed", "organization": map[string]any{"id": coderID, "login": "coder-legacy"},
		"changes": map[string]any{"login": map[string]any{"from": "coder"}},
	})
	require.NoError(t, err)
	srv.postAnswered(t, "organization", "renamed-1", renamed, http.StatusAccepted)
	srv.deliverRunOwnedBy(t, "existing-2", deliveredRepo{id: coderExisting.id, fullName: "coder-legacy/existing"}, coderID, "coder-legacy")
	watcher := connectSubscriber(ctx, t, &testServer{Server: srv.Server, mcpPath: "/github/mcp"}, srv.token(t, "coder-watcher", coderSecret))
	repos, _ := waitFor(ctx, t, watcher, map[string]any{"owner": "coder-legacy", "cursor": "v2.0"})
	require.Equal(t, []int64{coderExisting.id, coderExisting.id}, repos, "the grant follows its account to the new login")
	waitFor(ctx, t, watcher, map[string]any{"repo": "coder-legacy/existing"})
	require.Contains(t, waitError(ctx, t, watcher, map[string]any{"owner": "coder"}), "the grant org:coder names an account that holds another login now")

	takeover := deliveredRepo{id: 601, fullName: "coder/takeover"}
	srv.deliverRunOwnedBy(t, "takeover-1", takeover, 99, "coder")
	require.Contains(t, waitError(ctx, t, watcher, map[string]any{"owner": "coder"}), "the grant org:coder names an account that holds another login now")
	require.Contains(t, waitError(ctx, t, watcher, map[string]any{"repo": "coder/takeover"}), "no grant of this service names coder/takeover")
	takerSecret := srv.client(t, "taker-watcher", "/github/mcp", bothScopes, "org:coder")
	taker := connectSubscriber(ctx, t, &testServer{Server: srv.Server, mcpPath: "/github/mcp"}, srv.token(t, "taker-watcher", takerSecret))
	repos, _ = waitFor(ctx, t, taker, map[string]any{"owner": "coder", "cursor": "v2.0"})
	require.Equal(t, []int64{takeover.id}, repos, "a grant made now names the account that holds the login now")

	srv.deliverRunOf(t, "existing-1", coderExisting)
	held, err := srv.store.OwnerByLogin(ctx, "coder")
	require.NoError(t, err)
	require.Equal(t, faucet.Owner{ID: coderID, Login: "coder"}, held, "the delivery sent again by hand gives the renamed account its old login back")
	require.Contains(t, waitError(ctx, t, taker, map[string]any{"owner": "coder"}), "the grant org:coder names an account that holds another login now")
	require.Contains(t, waitError(ctx, t, taker, map[string]any{"repo": "coder/existing"}), "no grant of this service names coder/existing")
	repos, _ = waitFor(ctx, t, watcher, map[string]any{"owner": "coder", "cursor": "v2.0"})
	require.Equal(t, []int64{coderExisting.id, coderExisting.id}, repos, "the first grant reads its own account under either login")
}

// An organization that gave up a login and the one that took it never mix:
// after the one that took it moves its last repository away, an org grant
// of the login still names it, or nothing, and never the organization that
// held the login before, even on a renewal.
func TestAnOrgGrantNeverSnapsBackToTheAccountThatGaveItsLoginUp(t *testing.T) {
	t.Parallel()
	hooks := newCallbacks(t)
	key, err := faucet.NewWebhookKey(bytes.Repeat([]byte{1}, 32))
	require.NoError(t, err)
	allow, err := faucet.NewCallbackAllowlist([]string{hooks.URL}, true)
	require.NoError(t, err)
	srv := newLoginServer(t, func(c *loginConfig) {
		c.webhook = &faucet.WebhookDeps{Key: key, Callbacks: allow, AllowInsecureLoopback: true}
	})
	tool := deliveredRepo{id: 44, fullName: "old-coder/tool"}
	faucetRepo := deliveredRepo{id: 42, fullName: "coder/faucet"}
	secretRepo := deliveredRepo{id: 49, fullName: "coder/secret"}
	sandbox := deliveredRepo{id: 50, fullName: "old-coder/sandbox"}
	srv.deliverRunOwnedBy(t, "tool-1", tool, 7, "old-coder")
	srv.deliverRunOwnedBy(t, "faucet-1", faucetRepo, 7, "coder")
	srv.deliverRunOwnedBy(t, "secret-1", secretRepo, 7, "coder")
	srv.deliverRunOwnedBy(t, "sandbox-1", sandbox, 99, "old-coder")
	secret := srv.client(t, "taker", "/github/mcp", bothScopes, "org:old-coder")

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	session := connectSubscriber(ctx, t, &testServer{Server: srv.Server, mcpPath: "/github/mcp"}, srv.token(t, "taker", secret))
	owner := map[string]any{"owner": "old-coder"}
	subscribeOwner(ctx, t, session, hooks.URL+"/taker", owner)
	_, cursor := waitFor(ctx, t, session, owner)

	transferred, err := json.Marshal(map[string]any{
		"action":     "transferred",
		"repository": map[string]any{"id": sandbox.id, "full_name": "acme/sandbox", "owner": map[string]any{"id": 88, "login": "acme"}},
		"changes":    map[string]any{"owner": map[string]any{"from": map[string]any{"organization": map[string]any{"id": 99, "login": "old-coder"}}}},
	})
	require.NoError(t, err)
	srv.postAnswered(t, "repository", "transferred-1", transferred, http.StatusAccepted)
	subscribeOwner(ctx, t, session, hooks.URL+"/taker", owner)
	got, err := srv.store.OwnerByLogin(ctx, "old-coder")
	require.NoError(t, err)
	require.Equal(t, faucet.Owner{ID: 99, Login: "old-coder"}, got, "the login stays with the account that took it")

	srv.deliverRunOwnedBy(t, "faucet-2", faucetRepo, 7, "coder")
	srv.deliverRunOwnedBy(t, "secret-2", secretRepo, 7, "coder")
	repos, _ := waitFor(ctx, t, session, map[string]any{"owner": "old-coder", "cursor": cursor})
	require.Empty(t, repos, "a wait of the login reads no repository of the account that gave it up")
	_, err = srv.svc.DeliverDue(ctx)
	require.NoError(t, err)
	require.Empty(t, hooks.runsOf("/taker"), "the renewed subscription gets no event of that account")
	require.Contains(t, waitError(ctx, t, session, map[string]any{"repo": "old-coder/tool"}), "faucet has received no webhook delivery from old-coder/tool yet")
}

// GitHub sends no repository event when it renames an organization, so the
// rows of its repositories keep the old login. The organization's next
// delivery gives faucet the new login: an org grant and a repo grant of it
// work, and a later rename of the repository, which GitHub writes under the
// new login, moves the repository.
func TestARepositoryOfARenamedOrganizationIsGrantedAndRenamedUnderItsNewLogin(t *testing.T) {
	t.Parallel()
	srv := newLoginServer(t)
	repoSecret := srv.client(t, "repo-grant", "/github/mcp", bothScopes, "repo:coder/faucet", "repo:coder/tap")
	srv.deliverRunOwnedBy(t, "old-1", deliveredRepo{id: 5, fullName: "old-coder/faucet"}, 7, "old-coder")
	srv.deliverRunOwnedBy(t, "new-1", deliveredRepo{id: 5, fullName: "coder/faucet"}, 7, "coder")
	orgSecret := srv.client(t, "org-grant", "/github/mcp", bothScopes, "org:coder")

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	org := connectSubscriber(ctx, t, &testServer{Server: srv.Server, mcpPath: "/github/mcp"}, srv.token(t, "org-grant", orgSecret))
	repos, _ := waitFor(ctx, t, org, map[string]any{"owner": "coder", "cursor": "v2.0"})
	require.Equal(t, []int64{5, 5}, repos, "the org grant covers the repository of both deliveries")
	repo := connectSubscriber(ctx, t, &testServer{Server: srv.Server, mcpPath: "/github/mcp"}, srv.token(t, "repo-grant", repoSecret))
	waitFor(ctx, t, repo, map[string]any{"repo": "coder/faucet"})

	renamed, err := json.Marshal(map[string]any{
		"action":     "renamed",
		"repository": map[string]any{"id": 5, "full_name": "coder/tap", "owner": map[string]any{"id": 7, "login": "coder"}},
		"changes":    map[string]any{"repository": map[string]any{"name": map[string]any{"from": "faucet"}}},
	})
	require.NoError(t, err)
	srv.postAnswered(t, "repository", "renamed-1", renamed, http.StatusAccepted)
	waitFor(ctx, t, repo, map[string]any{"repo": "coder/tap"})
	require.Contains(t, waitError(ctx, t, repo, map[string]any{"repo": "coder/faucet"}), "faucet has received no webhook delivery from coder/faucet yet")
}
