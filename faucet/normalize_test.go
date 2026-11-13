package faucet_test

import (
	"encoding/json"
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
			wantData: `{"merged":false,"merge_commit_sha":"c4295bd74fb0f4fda03689c3df3f2803b658fd85","html_url":"https://github.com/Codertocat/Hello-World/pull/2"}`,
		},
		{
			name:     "PullRequestMerged",
			event:    "pull_request",
			fixture:  "pull_request_merged",
			wantType: "pr.closed",
			wantRepo: 186853002,
			wantPRs:  []int32{2},
			wantSHA:  "ec26c3e57ca3a959ca5aad62de7213c562f8c821",
			wantData: `{"merged":true,"merge_commit_sha":"e0e8f3b7c8b2d6a9f5c3a1b7d4e2f6a8c0b9d1e3","html_url":"https://github.com/Codertocat/Hello-World/pull/2"}`,
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
		{"PullRequestOpened", "pull_request", func(t *testing.T) []byte {
			return mutate(t, "pull_request_closed", withAction("opened"))
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
