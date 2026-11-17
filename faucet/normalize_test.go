package faucet_test

import (
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/coder/faucet/faucet"
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

func mutate(t *testing.T, name string, edit func(doc map[string]any)) []byte {
	t.Helper()
	var doc map[string]any
	require.NoError(t, json.Unmarshal(fixture(t, name), &doc))
	edit(doc)
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
			wantData: `{"run_id":289782451,"name":"CI","conclusion":"success","head_branch":"master","html_url":"https://github.com/octo-org/octo-repo/actions/runs/289782451","run_attempt":1}`,
		},
		{
			name:     "WorkflowRunWithTwoPRs",
			event:    "workflow_run",
			fixture:  "workflow_run_completed_two_prs",
			wantType: "ci.run_completed",
			wantRepo: 300029405,
			wantPRs:  []int32{2, 3},
			wantSHA:  "3484a3fb816e0859fd6e1cea078d76385ff50625",
			wantData: `{"run_id":289782451,"name":"CI","conclusion":"success","head_branch":"master","html_url":"https://github.com/octo-org/octo-repo/actions/runs/289782451","run_attempt":1}`,
		},
		{
			name:     "CheckRun",
			event:    "check_run",
			fixture:  "check_run_completed",
			wantType: "ci.check_completed",
			wantRepo: 186853002,
			wantPRs:  []int32{2},
			wantSHA:  "ec26c3e57ca3a959ca5aad62de7213c562f8c821",
			wantData: `{"check_run_id":128620228,"name":"Octocoders-linter","conclusion":"success","html_url":"https://github.com/Codertocat/Hello-World/runs/128620228","app_slug":"octoapp"}`,
		},
		{
			name:     "ReviewSubmitted",
			event:    "pull_request_review",
			fixture:  "pull_request_review_submitted",
			wantType: "pr.review_submitted",
			wantRepo: 186853002,
			wantPRs:  []int32{2},
			wantSHA:  "ec26c3e57ca3a959ca5aad62de7213c562f8c821",
			wantData: `{"review_id":237895671,"state":"approved","user_login":"Codertocat","html_url":"https://github.com/Codertocat/Hello-World/pull/2#pullrequestreview-237895671"}`,
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
			wantData: `{"comment_id":492700400,"user_login":"Codertocat","html_url":"https://github.com/Codertocat/Hello-World/issues/1#issuecomment-492700400"}`,
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
		{"WorkflowRunRequested", "workflow_run", func(t *testing.T) []byte {
			return mutate(t, "workflow_run_completed", withAction("requested"))
		}},
		{"WorkflowRunInProgress", "workflow_run", func(t *testing.T) []byte {
			return mutate(t, "workflow_run_completed", withAction("in_progress"))
		}},
		{"CheckRunCreated", "check_run", func(t *testing.T) []byte {
			return mutate(t, "check_run_completed", withAction("created"))
		}},
		{"ReviewDismissed", "pull_request_review", func(t *testing.T) []byte {
			return mutate(t, "pull_request_review_submitted", withAction("dismissed"))
		}},
		{"PullRequestActionThatGitHubDoesNotDocument", "pull_request", func(t *testing.T) []byte {
			return mutate(t, "pull_request_opened", withAction("exploded"))
		}},
		{"CommentEdited", "issue_comment", func(t *testing.T) []byte {
			return mutate(t, "issue_comment_created_on_pr", withAction("edited"))
		}},
		{"CommentOnPlainIssue", "issue_comment", func(t *testing.T) []byte {
			return fixture(t, "issue_comment_created")
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
		event   string
		fixture string
	}{
		{"workflow_run", "workflow_run_completed"},
		{"check_run", "check_run_completed"},
		{"pull_request_review", "pull_request_review_submitted"},
		{"pull_request", "pull_request_closed"},
		{"issue_comment", "issue_comment_created_on_pr"},
	}
	for _, h := range handled {
		t.Run(h.event, func(t *testing.T) {
			t.Parallel()

			cases := map[string][]byte{
				"MalformedJSON":      []byte(`{"action":`),
				"NotAnObject":        []byte(`[]`),
				"NoAction":           mutate(t, h.fixture, func(doc map[string]any) { delete(doc, "action") }),
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
