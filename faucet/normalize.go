package faucet

import (
	"encoding/json"
	"strings"

	"golang.org/x/xerrors"
)

const sourceGitHub = "github"

type fields struct {
	prs     []int32
	headSHA string
	data    map[string]any
}

type rule struct {
	eventType string
	extract   func(payload []byte) (f fields, ok bool, err error)
}

var rules = map[string]rule{
	"workflow_run.completed":        {eventType: "ci.run_completed", extract: decode(workflowRunCompleted)},
	"check_run.completed":           {eventType: "ci.check_completed", extract: decode(checkRunCompleted)},
	"pull_request_review.submitted": {eventType: "pr.review_submitted", extract: decode(reviewSubmitted)},
	"pull_request.closed":           {eventType: "pr.closed", extract: decode(pullRequestClosed)},
	"issue_comment.created":         {eventType: "pr.comment_created", extract: decode(issueCommentCreated)},
}

type envelope struct {
	Action     string `json:"action"`
	Repository struct {
		ID int64 `json:"id"`
	} `json:"repository"`
}

// Normalize turns one GitHub webhook into an Event.
//
// It returns a nil Event and a nil error when faucet ignores the webhook: an
// event name or action it has no rule for, or a comment on a plain issue. It
// returns an error when the payload of a handled webhook is malformed.
func Normalize(eventName string, payload []byte, deliveryID string) (*Event, error) {
	ev, ok, err := buildEvent(eventName, payload, deliveryID)
	if err != nil || !ok {
		return nil, err
	}
	return &ev, nil
}

func buildEvent(eventName string, payload []byte, deliveryID string) (Event, bool, error) {
	if !handlesEvent(eventName) {
		return Event{}, false, nil
	}
	var env envelope
	if err := json.Unmarshal(payload, &env); err != nil {
		return Event{}, false, xerrors.Errorf("decode %s payload: %w", eventName, err)
	}
	if env.Action == "" {
		return Event{}, false, xerrors.Errorf("%s payload has no action", eventName)
	}
	key := eventName + "." + env.Action
	r, found := rules[key]
	if !found {
		return Event{}, false, nil
	}
	f, ok, err := r.extract(payload)
	if err != nil {
		return Event{}, false, xerrors.Errorf("extract %s: %w", key, err)
	}
	if !ok {
		return Event{}, false, nil
	}
	for _, n := range f.prs {
		if n <= 0 {
			return Event{}, false, xerrors.Errorf("%s payload has pull request number %d", key, n)
		}
	}
	data, err := json.Marshal(f.data)
	if err != nil {
		return Event{}, false, xerrors.Errorf("encode %s data: %w", key, err)
	}
	ev := Event{
		Source:     sourceGitHub,
		DeliveryID: deliveryID,
		Type:       r.eventType,
		RepoID:     env.Repository.ID,
		PRs:        f.prs,
		HeadSHA:    f.headSHA,
		Data:       data,
	}
	if err := ev.Validate(); err != nil {
		return Event{}, false, xerrors.Errorf("normalize %s: %w", key, err)
	}
	return ev, true, nil
}

func handlesEvent(eventName string) bool {
	prefix := eventName + "."
	for key := range rules {
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}
	return false
}

func decode[P any](f func(P) (fields, bool)) func([]byte) (fields, bool, error) {
	return func(payload []byte) (fields, bool, error) {
		var p P
		if err := json.Unmarshal(payload, &p); err != nil {
			return fields{}, false, xerrors.Errorf("decode payload: %w", err)
		}
		out, ok := f(p)
		return out, ok, nil
	}
}

type repoRef struct {
	ID int64 `json:"id"`
}

type prRef struct {
	Number int32 `json:"number"`
	Base   struct {
		Repo repoRef `json:"repo"`
	} `json:"base"`
}

// GitHub also lists pull requests whose base is another repository. Their
// numbers mean nothing here.
func prNumbers(repoID int64, refs []prRef) []int32 {
	nums := make([]int32, 0, len(refs))
	for _, r := range refs {
		if r.Base.Repo.ID == repoID {
			nums = append(nums, r.Number)
		}
	}
	return nums
}

type workflowRunPayload struct {
	Repository  repoRef `json:"repository"`
	WorkflowRun struct {
		ID           int64   `json:"id"`
		Name         string  `json:"name"`
		Conclusion   string  `json:"conclusion"`
		HeadBranch   string  `json:"head_branch"`
		HeadSHA      string  `json:"head_sha"`
		HTMLURL      string  `json:"html_url"`
		RunAttempt   int32   `json:"run_attempt"`
		PullRequests []prRef `json:"pull_requests"`
	} `json:"workflow_run"`
}

func workflowRunCompleted(p workflowRunPayload) (fields, bool) {
	r := p.WorkflowRun
	return fields{
		prs:     prNumbers(p.Repository.ID, r.PullRequests),
		headSHA: r.HeadSHA,
		data: map[string]any{
			"run_id":      r.ID,
			"name":        r.Name,
			"conclusion":  r.Conclusion,
			"head_branch": r.HeadBranch,
			"html_url":    r.HTMLURL,
			"run_attempt": r.RunAttempt,
		},
	}, true
}

type checkRunPayload struct {
	Repository repoRef `json:"repository"`
	CheckRun   struct {
		ID           int64   `json:"id"`
		Name         string  `json:"name"`
		Conclusion   string  `json:"conclusion"`
		HeadSHA      string  `json:"head_sha"`
		HTMLURL      string  `json:"html_url"`
		PullRequests []prRef `json:"pull_requests"`
		App          struct {
			Slug string `json:"slug"`
		} `json:"app"`
	} `json:"check_run"`
}

func checkRunCompleted(p checkRunPayload) (fields, bool) {
	c := p.CheckRun
	return fields{
		prs:     prNumbers(p.Repository.ID, c.PullRequests),
		headSHA: c.HeadSHA,
		data: map[string]any{
			"check_run_id": c.ID,
			"name":         c.Name,
			"conclusion":   c.Conclusion,
			"html_url":     c.HTMLURL,
			"app_slug":     c.App.Slug,
		},
	}, true
}

type user struct {
	Login string `json:"login"`
}

type reviewPayload struct {
	Review struct {
		ID      int64  `json:"id"`
		State   string `json:"state"`
		User    user   `json:"user"`
		HTMLURL string `json:"html_url"`
	} `json:"review"`
	PullRequest struct {
		Number int32 `json:"number"`
		Head   struct {
			SHA string `json:"sha"`
		} `json:"head"`
	} `json:"pull_request"`
}

func reviewSubmitted(p reviewPayload) (fields, bool) {
	return fields{
		prs:     []int32{p.PullRequest.Number},
		headSHA: p.PullRequest.Head.SHA,
		data: map[string]any{
			"review_id":  p.Review.ID,
			"state":      p.Review.State,
			"user_login": p.Review.User.Login,
			"html_url":   p.Review.HTMLURL,
		},
	}, true
}

type pullRequestPayload struct {
	Number      int32 `json:"number"`
	PullRequest struct {
		Merged         bool    `json:"merged"`
		MergeCommitSHA *string `json:"merge_commit_sha"`
		HTMLURL        string  `json:"html_url"`
		Head           struct {
			SHA string `json:"sha"`
		} `json:"head"`
	} `json:"pull_request"`
}

func pullRequestClosed(p pullRequestPayload) (fields, bool) {
	return fields{
		prs:     []int32{p.Number},
		headSHA: p.PullRequest.Head.SHA,
		data: map[string]any{
			"merged":           p.PullRequest.Merged,
			"merge_commit_sha": p.PullRequest.MergeCommitSHA,
			"html_url":         p.PullRequest.HTMLURL,
		},
	}, true
}

type issueCommentPayload struct {
	Issue struct {
		Number int32 `json:"number"`
		// PullRequest is present only when the issue is a pull request.
		PullRequest *struct{} `json:"pull_request"`
	} `json:"issue"`
	Comment struct {
		ID      int64  `json:"id"`
		User    user   `json:"user"`
		HTMLURL string `json:"html_url"`
	} `json:"comment"`
}

func issueCommentCreated(p issueCommentPayload) (fields, bool) {
	if p.Issue.PullRequest == nil {
		return fields{}, false
	}
	return fields{
		prs: []int32{p.Issue.Number},
		data: map[string]any{
			"comment_id": p.Comment.ID,
			"user_login": p.Comment.User.Login,
			"html_url":   p.Comment.HTMLURL,
		},
	}, true
}
