package faucet_test

import (
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/coder/superblink-events/faucet"
)

// Fixtures in testdata are trimmed samples from github.com/octokit/webhooks.
func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name+".json"))
	require.NoError(t, err)
	return b
}

// helloWorldPR is the data of a pull_request fixture: the pull request that
// every octokit sample of the event shows, and the extras of its action.
func helloWorldPR(state string, merged bool, extras map[string]any) string {
	data := map[string]any{
		"html_url":     "https://github.com/Codertocat/Hello-World/pull/2",
		"title":        "Update the README with new information.",
		"state":        state,
		"draft":        false,
		"merged":       merged,
		"base_branch":  "master",
		"head_branch":  "changes",
		"from_fork":    false,
		"author_login": "Codertocat",
		"sender_login": "Codertocat",
		"sender_id":    21031067,
	}
	maps.Copy(data, extras)
	b, err := json.Marshal(data)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func mutate(t *testing.T, name string, edits ...func(doc map[string]any)) []byte {
	t.Helper()
	var doc map[string]any
	require.NoError(t, json.Unmarshal(fixture(t, name), &doc))
	for _, edit := range edits {
		edit(doc)
	}
	b, err := json.Marshal(doc)
	require.NoError(t, err)
	return b
}

func TestNormalize(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		event    string
		fixture  string
		wantType string
		wantRepo int64
		wantPRs  []int32
		wantSHA  string
		wantData string
	}{
		{
			name:     "WorkflowRunWithoutPRs",
			event:    "workflow_run",
			fixture:  "workflow_run_completed",
			wantType: "ci.run_completed",
			wantRepo: 300029405,
			wantPRs:  []int32{},
			wantSHA:  "3484a3fb816e0859fd6e1cea078d76385ff50625",
			wantData: `{"run_id":289782451,"name":"CI","status":"completed","conclusion":"success","head_branch":"master","from_fork":false,"trigger_event":"repository_dispatch","html_url":"https://github.com/octo-org/octo-repo/actions/runs/289782451","run_attempt":1,"workflow_id":2823525,"sender_login":"Codertocat","sender_id":15669918}`,
		},
		{
			name:     "WorkflowRunWithTwoPRs",
			event:    "workflow_run",
			fixture:  "workflow_run_completed_two_prs",
			wantType: "ci.run_completed",
			wantRepo: 300029405,
			wantPRs:  []int32{2, 3},
			wantSHA:  "3484a3fb816e0859fd6e1cea078d76385ff50625",
			wantData: `{"run_id":289782451,"name":"CI","status":"completed","conclusion":"success","head_branch":"master","from_fork":false,"trigger_event":"repository_dispatch","html_url":"https://github.com/octo-org/octo-repo/actions/runs/289782451","run_attempt":1,"workflow_id":2823525,"sender_login":"Codertocat","sender_id":15669918}`,
		},
		{
			name:     "WorkflowRunRequested",
			event:    "workflow_run",
			fixture:  "workflow_run_requested",
			wantType: "ci.run_requested",
			wantRepo: 300029405,
			wantPRs:  []int32{},
			wantSHA:  "3484a3fb816e0859fd6e1cea078d76385ff50625",
			wantData: `{"run_id":289782451,"name":"","status":"queued","conclusion":null,"head_branch":"master","from_fork":false,"trigger_event":"repository_dispatch","html_url":"https://github.com/octo-org/octo-repo/actions/runs/289782451","run_attempt":1,"workflow_id":2823525,"sender_login":"Codertocat","sender_id":15669918}`,
		},
		{
			name:     "WorkflowJobQueued",
			event:    "workflow_job",
			fixture:  "workflow_job_queued",
			wantType: "ci.job_queued",
			wantRepo: 186853002,
			wantPRs:  []int32{},
			wantSHA:  "3484a3fb816e0859fd6e1cea078d76385ff50625",
			wantData: `{"job_id":289782451,"run_id":2202229078,"run_attempt":1,"name":"update","workflow_name":"CodeQL","status":"queued","conclusion":null,"head_branch":"main","runner_name":"GitHub Actions 5","labels":["ubuntu-latest"],"html_url":"https://github.com/octo-org/octo-repo/runs/1291536064","sender_login":"Codertocat","sender_id":21031067}`,
		},
		{
			name:     "WorkflowJobWaitingWithoutRunner",
			event:    "workflow_job",
			fixture:  "workflow_job_waiting",
			wantType: "ci.job_waiting",
			wantRepo: 591427149,
			wantPRs:  []int32{},
			wantSHA:  "16c5286e8d9a0629956a28938386b36608707a71",
			wantData: `{"job_id":12877621891,"run_id":4747967848,"run_attempt":1,"name":"test","workflow_name":"Env Test","status":"waiting","conclusion":null,"head_branch":"main","runner_name":null,"labels":["self-hosted","k8s"],"html_url":"https://github.com/lineville/elastic-machines-testing/actions/runs/4747967848/jobs/8433573014","sender_login":"lineville","sender_id":25349044}`,
		},
		{
			name:     "WorkflowJobCompleted",
			event:    "workflow_job",
			fixture:  "workflow_job_completed",
			wantType: "ci.job_completed",
			wantRepo: 186853002,
			wantPRs:  []int32{},
			wantSHA:  "3484a3fb816e0859fd6e1cea078d76385ff50625",
			wantData: `{"job_id":289782451,"run_id":2202229078,"run_attempt":1,"name":"linters","workflow_name":"CodeQL","status":"completed","conclusion":"failure","head_branch":"main","runner_name":"GitHub Actions 5","labels":["ubuntu-latest"],"html_url":"https://github.com/octo-org/octo-repo/runs/1291536064","sender_login":"Codertocat","sender_id":21031067}`,
		},
		{
			name:     "CheckRun",
			event:    "check_run",
			fixture:  "check_run_completed",
			wantType: "ci.check_completed",
			wantRepo: 186853002,
			wantPRs:  []int32{2},
			wantSHA:  "ec26c3e57ca3a959ca5aad62de7213c562f8c821",
			wantData: `{"check_run_id":128620228,"name":"Octocoders-linter","status":"completed","conclusion":"success","html_url":"https://github.com/Codertocat/Hello-World/runs/128620228","app_slug":"octoapp","check_suite_id":118578147,"head_branch":"changes","sender_login":"Codertocat","sender_id":21031067}`,
		},
		{
			name:     "CheckRunCreated",
			event:    "check_run",
			fixture:  "check_run_created",
			wantType: "ci.check_created",
			wantRepo: 186853002,
			wantPRs:  []int32{2},
			wantSHA:  "ec26c3e57ca3a959ca5aad62de7213c562f8c821",
			wantData: `{"check_run_id":128620228,"name":"Octocoders-linter","status":"queued","conclusion":null,"html_url":"https://github.com/Codertocat/Hello-World/runs/128620228","app_slug":"","check_suite_id":118578147,"head_branch":"changes","sender_login":"Codertocat","sender_id":21031067}`,
		},
		{
			name:     "CheckSuiteCompleted",
			event:    "check_suite",
			fixture:  "check_suite_completed",
			wantType: "ci.suite_completed",
			wantRepo: 186853002,
			wantPRs:  []int32{2},
			wantSHA:  "ec26c3e57ca3a959ca5aad62de7213c562f8c821",
			wantData: `{"check_suite_id":118578147,"status":"completed","conclusion":"success","head_branch":"changes","app_slug":"","sender_login":"Codertocat","sender_id":21031067}`,
		},
		{
			name:     "StatusWithNoActionIsReadByItsState",
			event:    "status",
			fixture:  "status_success",
			wantType: "ci.status_success",
			wantRepo: 186853002,
			wantPRs:  []int32{},
			wantSHA:  "6113728f27ae82c7b1a177c8d03f9e96e0adf246",
			wantData: `{"state":"success","context":"default","target_url":null,"branches":["master","changes","gh-pages"],"html_url":"https://github.com/Codertocat/Hello-World/commit/6113728f27ae82c7b1a177c8d03f9e96e0adf246","sender_login":"Codertocat","sender_id":21031067}`,
		},
		{
			name:     "ReviewSubmitted",
			event:    "pull_request_review",
			fixture:  "pull_request_review_submitted",
			wantType: "pr.review_submitted",
			wantRepo: 186853002,
			wantPRs:  []int32{2},
			wantSHA:  "ec26c3e57ca3a959ca5aad62de7213c562f8c821",
			wantData: `{"review_id":237895671,"state":"approved","user_login":"Codertocat","html_url":"https://github.com/Codertocat/Hello-World/pull/2#pullrequestreview-237895671","sender_login":"Codertocat","sender_id":21031067}`,
		},
		{
			name:     "ReviewDismissed",
			event:    "pull_request_review",
			fixture:  "pull_request_review_dismissed",
			wantType: "pr.review_dismissed",
			wantRepo: 186853002,
			wantPRs:  []int32{2},
			wantSHA:  "ec26c3e57ca3a959ca5aad62de7213c562f8c821",
			wantData: `{"review_id":237895671,"state":"dismissed","user_login":"Codertocat","html_url":"https://github.com/Codertocat/Hello-World/pull/2#pullrequestreview-237895671","sender_login":"Codertocat","sender_id":21031067}`,
		},
		{
			name:     "PullRequestClosedUnmerged",
			event:    "pull_request",
			fixture:  "pull_request_closed",
			wantType: "pr.closed",
			wantRepo: 186853002,
			wantPRs:  []int32{2},
			wantSHA:  "ec26c3e57ca3a959ca5aad62de7213c562f8c821",
			wantData: helloWorldPR("closed", false, map[string]any{"merge_commit_sha": "c4295bd74fb0f4fda03689c3df3f2803b658fd85"}),
		},
		{
			name:     "PullRequestMerged",
			event:    "pull_request",
			fixture:  "pull_request_merged",
			wantType: "pr.closed",
			wantRepo: 186853002,
			wantPRs:  []int32{2},
			wantSHA:  "ec26c3e57ca3a959ca5aad62de7213c562f8c821",
			wantData: helloWorldPR("closed", true, map[string]any{"merge_commit_sha": "e0e8f3b7c8b2d6a9f5c3a1b7d4e2f6a8c0b9d1e3"}),
		},
		{
			name:     "PullRequestOpened",
			event:    "pull_request",
			fixture:  "pull_request_opened",
			wantType: "pr.opened",
			wantRepo: 186853002,
			wantPRs:  []int32{2},
			wantSHA:  "ec26c3e57ca3a959ca5aad62de7213c562f8c821",
			wantData: helloWorldPR("open", false, nil),
		},
		{
			name:     "PullRequestSynchronize",
			event:    "pull_request",
			fixture:  "pull_request_synchronize",
			wantType: "pr.synchronize",
			wantRepo: 186853002,
			wantPRs:  []int32{2},
			wantSHA:  "ec26c3e57ca3a959ca5aad62de7213c562f8c821",
			wantData: helloWorldPR("open", false, map[string]any{
				"before": "f95f852bd8fca8fcc58a9a2d6c842781e32a215e",
				"after":  "ec26c3e57ca3a959ca5aad62de7213c562f8c821",
			}),
		},
		{
			name:     "PullRequestLabeledWithoutItsMergeCommit",
			event:    "pull_request",
			fixture:  "pull_request_labeled",
			wantType: "pr.labeled",
			wantRepo: 186853002,
			wantPRs:  []int32{2},
			wantSHA:  "ec26c3e57ca3a959ca5aad62de7213c562f8c821",
			wantData: helloWorldPR("open", false, map[string]any{"label_name": "bug"}),
		},
		{
			name:     "PullRequestReviewRequested",
			event:    "pull_request",
			fixture:  "pull_request_review_requested",
			wantType: "pr.review_requested",
			wantRepo: 186853002,
			wantPRs:  []int32{2},
			wantSHA:  "ec26c3e57ca3a959ca5aad62de7213c562f8c821",
			wantData: helloWorldPR("open", false, map[string]any{"requested_reviewer_login": "octocat"}),
		},
		{
			name:     "PullRequestAssigned",
			event:    "pull_request",
			fixture:  "pull_request_assigned",
			wantType: "pr.assigned",
			wantRepo: 186853002,
			wantPRs:  []int32{2},
			wantSHA:  "ec26c3e57ca3a959ca5aad62de7213c562f8c821",
			wantData: helloWorldPR("open", false, map[string]any{"assignee_login": "Codertocat"}),
		},
		{
			name:     "PullRequestReadyForReview",
			event:    "pull_request",
			fixture:  "pull_request_ready_for_review",
			wantType: "pr.ready_for_review",
			wantRepo: 186853002,
			wantPRs:  []int32{2},
			wantSHA:  "ec26c3e57ca3a959ca5aad62de7213c562f8c821",
			wantData: helloWorldPR("open", false, nil),
		},
		{
			name:     "CommentOnPullRequest",
			event:    "issue_comment",
			fixture:  "issue_comment_created_on_pr",
			wantType: "pr.comment_created",
			wantRepo: 186853002,
			wantPRs:  []int32{1},
			wantSHA:  "",
			wantData: `{"comment_id":492700400,"user_login":"Codertocat","html_url":"https://github.com/Codertocat/Hello-World/issues/1#issuecomment-492700400","sender_login":"Codertocat","sender_id":21031067}`,
		},
		{
			name:     "CommentDeletedOnPullRequest",
			event:    "issue_comment",
			fixture:  "issue_comment_deleted_on_pr",
			wantType: "pr.comment_deleted",
			wantRepo: 186853002,
			wantPRs:  []int32{1},
			wantSHA:  "",
			wantData: `{"comment_id":492700400,"user_login":"Codertocat","html_url":"https://github.com/Codertocat/Hello-World/issues/1#issuecomment-492700400","sender_login":"Codertocat","sender_id":21031067}`,
		},
		{
			name:     "ReviewCommentCreated",
			event:    "pull_request_review_comment",
			fixture:  "pull_request_review_comment_created",
			wantType: "pr.review_comment_created",
			wantRepo: 186853002,
			wantPRs:  []int32{2},
			wantSHA:  "ec26c3e57ca3a959ca5aad62de7213c562f8c821",
			wantData: `{"comment_id":284312630,"review_id":237895671,"user_login":"Codertocat","html_url":"https://github.com/Codertocat/Hello-World/pull/2#discussion_r284312630","sender_login":"Codertocat","sender_id":21031067}`,
		},
		{
			name:     "ReviewThreadResolved",
			event:    "pull_request_review_thread",
			fixture:  "pull_request_review_thread_resolved",
			wantType: "pr.review_thread_resolved",
			wantRepo: 186853002,
			wantPRs:  []int32{2},
			wantSHA:  "ec26c3e57ca3a959ca5aad62de7213c562f8c821",
			wantData: `{"thread_node_id":"PRRT_kwDOFd42Pc4rQOUv","comment_ids":[284312630],"user_login":"Codertocat","html_url":"https://github.com/Codertocat/Hello-World/pull/2#discussion_r284312630","sender_login":"Codertocat","sender_id":21031067}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ev, err := faucet.Normalize(tt.event, fixture(t, tt.fixture), "delivery-1")
			require.NoError(t, err)
			require.NotNil(t, ev)

			require.Equal(t, "github", ev.Source)
			require.Equal(t, "delivery-1", ev.DeliveryID)
			require.Equal(t, tt.wantType, ev.Type)
			require.Equal(t, tt.wantRepo, ev.RepoID)
			require.Equal(t, tt.wantPRs, ev.PRs)
			require.Equal(t, tt.wantSHA, ev.HeadSHA)
			require.JSONEq(t, tt.wantData, string(ev.Data))
			require.NoError(t, ev.Validate())

			var named struct {
				Repository struct {
					FullName string `json:"full_name"`
					Owner    struct {
						ID int64 `json:"id"`
					} `json:"owner"`
				} `json:"repository"`
			}
			require.NoError(t, json.Unmarshal(fixture(t, tt.fixture), &named))
			require.Positive(t, named.Repository.Owner.ID)
			require.Equal(t, named.Repository.Owner.ID, ev.OwnerID, "the account that owns the repository")
			require.Equal(t, named.Repository.FullName, ev.RepoName, "the full name as the delivery wrote it")
		})
	}
}

// octokit/webhooks records no milestoned event and no review request for a
// team, so these cases add the field that GitHub's schema documents to a
// recorded payload: a milestone in milestoned.schema.json, and a team in
// review_requested.schema.json.
func TestNormalizeReadsTheDocumentedFieldsThatNoSampleRecords(t *testing.T) {
	t.Parallel()

	milestoned := mutate(t, "pull_request_labeled", func(doc map[string]any) {
		pr, ok := doc["pull_request"].(map[string]any)
		require.True(t, ok)
		doc["action"] = "milestoned"
		doc["milestone"] = pr["milestone"]
		delete(doc, "label")
	})
	teamRequested := mutate(t, "pull_request_review_requested", func(doc map[string]any) {
		delete(doc, "requested_reviewer")
		doc["requested_team"] = map[string]any{"name": "Justice League", "id": 1, "slug": "justice-league", "privacy": "closed", "permission": "admin"}
	})
	for name, tc := range map[string]struct {
		payload  []byte
		wantType string
		want     map[string]any
	}{
		"Milestoned":    {milestoned, "pr.milestoned", map[string]any{"milestone_title": "v1.0"}},
		"TeamRequested": {teamRequested, "pr.review_requested", map[string]any{"requested_team_slug": "justice-league"}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			ev, err := faucet.Normalize("pull_request", tc.payload, "delivery-1")
			require.NoError(t, err)
			require.NotNil(t, ev)
			require.Equal(t, tc.wantType, ev.Type)
			require.JSONEq(t, helloWorldPR("open", false, tc.want), string(ev.Data))
		})
	}
}

func member(t *testing.T, doc map[string]any, path ...string) map[string]any {
	t.Helper()
	for _, name := range path {
		next, ok := doc[name].(map[string]any)
		require.True(t, ok, "payload has no object %s", name)
		doc = next
	}
	return doc
}

// octokit/webhooks records no payload whose head is in a fork, so headIn
// edits a recorded one: its head repository becomes another repository, or
// null, which GitHub sends for a pull request whose fork was deleted (head.repo
// in payload-schemas/api.github.com/common/pull-request.schema.json is a
// repository or null).
func headIn(t *testing.T, event string, headRepo any) func(doc map[string]any) {
	return func(doc map[string]any) {
		switch event {
		case "workflow_run":
			member(t, doc, "workflow_run")["head_repository"] = headRepo
		case "pull_request":
			member(t, doc, "pull_request", "head")["repo"] = headRepo
		}
	}
}

func editedToFork(t *testing.T, event, fixtureName string, headRepo any) []byte {
	t.Helper()
	return mutate(t, fixtureName, headIn(t, event, headRepo))
}

var forkRepo = map[string]any{"id": 999999, "name": "Hello-World", "full_name": "forker/Hello-World", "fork": true}

func TestNormalizeTellsAHeadInAForkInRecordedPayloadsEditedToOne(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		event    string
		payload  []byte
		fromFork bool
	}{
		"WorkflowRunOfTheRepository":              {"workflow_run", fixture(t, "workflow_run_completed"), false},
		"WorkflowRunEditedToComeFromAFork":        {"workflow_run", editedToFork(t, "workflow_run", "workflow_run_completed", forkRepo), true},
		"PullRequestOfTheRepository":              {"pull_request", fixture(t, "pull_request_opened"), false},
		"PullRequestEditedToComeFromAFork":        {"pull_request", editedToFork(t, "pull_request", "pull_request_opened", forkRepo), true},
		"PullRequestEditedToComeFromADeletedFork": {"pull_request", editedToFork(t, "pull_request", "pull_request_opened", nil), true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			ev, err := faucet.Normalize(tc.event, tc.payload, "delivery-1")
			require.NoError(t, err)
			require.NotNil(t, ev)
			var data map[string]any
			require.NoError(t, json.Unmarshal(ev.Data, &data))
			require.Equal(t, tc.fromFork, data["from_fork"])
			require.NotEmpty(t, data["head_branch"], "the branch name stays as GitHub sent it")
		})
	}
}

func TestNormalizeKeepsOnlyPullRequestsOfTheRepository(t *testing.T) {
	t.Parallel()

	const repoID, otherRepoID = 1001, 2002
	pr := func(number, baseRepoID, headRepoID int) map[string]any {
		return map[string]any{
			"number": number,
			"base":   map[string]any{"repo": map[string]any{"id": baseRepoID}},
			"head":   map[string]any{"repo": map[string]any{"id": headRepoID}},
		}
	}
	events := []struct {
		event   string
		fixture string
	}{
		{"workflow_run", "workflow_run_completed"},
		{"check_run", "check_run_completed"},
		{"check_suite", "check_suite_completed"},
	}
	cases := []struct {
		name string
		prs  []any
		want []int32
	}{
		{"PullRequestOfTheRepository", []any{pr(2, repoID, repoID)}, []int32{2}},
		{"PullRequestFromAForkIntoTheRepository", []any{pr(5, repoID, otherRepoID)}, []int32{5}},
		{"PullRequestIntoAnotherRepository", []any{pr(1, otherRepoID, repoID)}, []int32{}},
		{"PullRequestWithoutBaseRepository", []any{map[string]any{"number": 4}}, []int32{}},
		{"MixedKeepsOrder", []any{pr(2, repoID, repoID), pr(1, otherRepoID, repoID), pr(3, repoID, otherRepoID)}, []int32{2, 3}},
	}
	for _, e := range events {
		t.Run(e.event, func(t *testing.T) {
			t.Parallel()

			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					t.Parallel()

					payload := mutate(t, e.fixture, func(doc map[string]any) {
						doc["repository"] = map[string]any{"id": repoID}
						run, ok := doc[e.event].(map[string]any)
						require.True(t, ok)
						run["pull_requests"] = tc.prs
					})
					ev, err := faucet.Normalize(e.event, payload, "delivery-1")
					require.NoError(t, err)
					require.NotNil(t, ev)
					require.Equal(t, int64(repoID), ev.RepoID)
					require.Equal(t, tc.want, ev.PRs)
				})
			}
		})
	}
}

func TestNormalizeIgnores(t *testing.T) {
	t.Parallel()

	withAction := func(action string) func(map[string]any) {
		return func(doc map[string]any) { doc["action"] = action }
	}
	tests := []struct {
		name    string
		event   string
		payload func(t *testing.T) []byte
	}{
		{"CheckRunRerequested", "check_run", func(t *testing.T) []byte {
			return mutate(t, "check_run_completed", withAction("rerequested"))
		}},
		{"CheckSuiteRequested", "check_suite", func(t *testing.T) []byte {
			return mutate(t, "check_suite_completed", withAction("requested"))
		}},
		{"PullRequestActionThatGitHubDoesNotDocument", "pull_request", func(t *testing.T) []byte {
			return mutate(t, "pull_request_opened", withAction("exploded"))
		}},
		{"CommentOnPlainIssue", "issue_comment", func(t *testing.T) []byte {
			return fixture(t, "issue_comment_created")
		}},
		{"CommentDeletedOnPlainIssue", "issue_comment", func(t *testing.T) []byte {
			return mutate(t, "issue_comment_deleted_on_pr", func(doc map[string]any) {
				issue, ok := doc["issue"].(map[string]any)
				require.True(t, ok)
				delete(issue, "pull_request")
			})
		}},
		{"StatusOfAStateGitHubNeverSends", "status", func(t *testing.T) []byte {
			return mutate(t, "status_success", func(doc map[string]any) { doc["state"] = "exploded" })
		}},
		{"UnhandledEventIsNotParsed", "push", func(*testing.T) []byte {
			return []byte("not json")
		}},
		{"UnhandledEventWithAction", "star", func(*testing.T) []byte {
			return []byte(`{"action":"created","repository":{"id":1}}`)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ev, err := faucet.Normalize(tt.event, tt.payload(t), "delivery-1")
			require.NoError(t, err)
			require.Nil(t, ev)
		})
	}
}

func TestNormalizeRejects(t *testing.T) {
	t.Parallel()

	handled := []struct {
		event        string
		fixture      string
		actionMember string
	}{
		{"workflow_run", "workflow_run_completed", "action"},
		{"check_run", "check_run_completed", "action"},
		{"workflow_job", "workflow_job_queued", "action"},
		{"check_suite", "check_suite_completed", "action"},
		{"status", "status_success", "state"},
		{"pull_request_review", "pull_request_review_submitted", "action"},
		{"pull_request", "pull_request_closed", "action"},
		{"issue_comment", "issue_comment_created_on_pr", "action"},
		{"pull_request_review_comment", "pull_request_review_comment_created", "action"},
		{"pull_request_review_thread", "pull_request_review_thread_resolved", "action"},
	}
	for _, h := range handled {
		t.Run(h.event, func(t *testing.T) {
			t.Parallel()

			cases := map[string][]byte{
				"MalformedJSON":      []byte(`{"action":`),
				"NotAnObject":        []byte(`[]`),
				"NoActionWord":       mutate(t, h.fixture, func(doc map[string]any) { delete(doc, h.actionMember) }),
				"ActionWordNotText":  mutate(t, h.fixture, func(doc map[string]any) { doc[h.actionMember] = 1 }),
				"NoRepository":       mutate(t, h.fixture, func(doc map[string]any) { delete(doc, "repository") }),
				"ZeroRepositoryID":   mutate(t, h.fixture, func(doc map[string]any) { doc["repository"] = map[string]any{"id": 0} }),
				"StringRepositoryID": mutate(t, h.fixture, func(doc map[string]any) { doc["repository"] = map[string]any{"id": "1"} }),
			}
			for name, payload := range cases {
				t.Run(name, func(t *testing.T) {
					t.Parallel()

					ev, err := faucet.Normalize(h.event, payload, "delivery-1")
					require.Error(t, err)
					require.Nil(t, ev)
				})
			}
		})
	}

	t.Run("EmptyDeliveryID", func(t *testing.T) {
		t.Parallel()

		ev, err := faucet.Normalize("workflow_run", fixture(t, "workflow_run_completed"), "")
		require.Error(t, err)
		require.Nil(t, ev)
	})

	t.Run("PullRequestNumberZero", func(t *testing.T) {
		t.Parallel()

		payload := mutate(t, "pull_request_closed", func(doc map[string]any) { doc["number"] = 0 })
		ev, err := faucet.Normalize("pull_request", payload, "delivery-1")
		require.Error(t, err)
		require.Nil(t, ev)
	})
}
