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

	"github.com/coder/faucet/faucet"
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
	body, err := os.ReadFile("../../faucet/testdata/workflow_run_completed.json")
	require.NoError(t, err)
	var payload map[string]any
	require.NoError(t, json.Unmarshal(body, &payload))
	_, name, _ := strings.Cut(repo.fullName, "/")
	payload["repository"] = map[string]any{
		"id": repo.id, "name": name, "full_name": repo.fullName,
		"owner": map[string]any{"id": coderID, "login": "coder", "type": "Organization"},
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
func waitFor(ctx context.Context, t *testing.T, session *mcp.ClientSession, arguments map[string]any) ([]int64, int64) {
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
		Cursor int64 `json:"cursor"`
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
			secret := srv.client(t, "org-watcher", "/github/mcp", bothScopes, grant)
			srv.deliverRunOf(t, "existing-1", coderExisting)

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
			paths, _ := srv.github.seen()
			require.Empty(t, paths, "a service never reaches GitHub")
		})
	}
}

// A person names one repository: GitHub answers for a person's access to a
// repository, and has no answer for an owner or every repository.
func TestAPersonIsToldThatOwnerAndAllTargetsAreForServices(t *testing.T) {
	t.Parallel()
	srv := newLoginServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	session := connectSubscriber(ctx, t, &testServer{Server: srv.Server, mcpPath: "/github/mcp"}, aliceToken)
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
	paths, _ := srv.github.seen()
	require.NotContains(t, strings.Join(paths, " "), "/repos/", "no repository is looked up")
}

// An org grant covers each repository whose deliveries named the owner's
// account, and no repository of another owner.
func TestAnOrgGrantCoversTheRepositoriesOfItsOwner(t *testing.T) {
	t.Parallel()
	srv := newLoginServer(t)
	secret := srv.client(t, "org-watcher", "/github/mcp", bothScopes, "org:Coder")
	srv.deliverRunOf(t, "existing-1", coderExisting)
	srv.deliver(t, "octo-1")
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	session, err := srv.connectService(ctx, t, "/github/mcp", "org-watcher", secret)
	require.NoError(t, err)

	repos, _ := waitFor(ctx, t, session, map[string]any{"repo": "coder/existing", "cursor": 0})
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
	secret := srv.client(t, "org-watcher", "/github/mcp", []faucet.Scope{faucet.ScopeEventsRead}, "org:coder", "all")
	srv.deliverRunOf(t, "existing-1", coderExisting)
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

// An organization that GitHub renamed gives up its old login, and whoever
// takes the login next at GitHub must not read the renamed organization's
// repositories through an org grant of it, by owner or by name. The
// renamed organization's own deliveries tell faucet the new login, and a
// delivery from before the rename that redelivery asked for again does not
// give the old login back.
func TestAnOrgGrantOfALoginThatItsAccountGaveUpReadsNothing(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	srv := newLoginServer(t)
	oldSecret := srv.client(t, "old-login", "/github/mcp", bothScopes, "org:coder")
	newSecret := srv.client(t, "new-login", "/github/mcp", bothScopes, "org:coder-legacy")
	srv.deliverRunOf(t, "existing-1", coderExisting)
	renamed, err := json.Marshal(map[string]any{
		"action": "renamed", "organization": map[string]any{"id": coderID, "login": "coder-legacy"},
		"changes": map[string]any{"login": map[string]any{"from": "coder"}},
	})
	require.NoError(t, err)
	srv.postAnswered(t, "organization", "renamed-1", renamed, http.StatusAccepted)
	_, err = srv.db.ExecContext(ctx, `INSERT INTO faucet_redeliveries (guid, hook, last_attempt_id, last_attempt_at, first_attempt_at, next_ask_at)
		VALUES ('existing-1', '/orgs/coder/hooks/1', 1, now(), now(), now())`)
	require.NoError(t, err)
	srv.deliverRunOf(t, "existing-1", coderExisting)

	old := connectSubscriber(ctx, t, &testServer{Server: srv.Server, mcpPath: "/github/mcp"}, srv.token(t, "old-login", oldSecret))
	require.Contains(t, waitError(ctx, t, old, map[string]any{"owner": "coder"}), "faucet has received no webhook delivery from a repository of coder yet")
	require.Contains(t, waitError(ctx, t, old, map[string]any{"repo": "coder/existing"}), "faucet has received no webhook delivery from coder/existing yet")

	renamedOrg := connectSubscriber(ctx, t, &testServer{Server: srv.Server, mcpPath: "/github/mcp"}, srv.token(t, "new-login", newSecret))
	waitFor(ctx, t, renamedOrg, map[string]any{"owner": "coder-legacy"})
	waitFor(ctx, t, renamedOrg, map[string]any{"repo": "coder-legacy/existing"})
}
