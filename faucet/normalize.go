package faucet

import (
	"cmp"
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

// Repository and organization webhooks receive only the created and completed
// check_run actions and the completed check_suite action.
var rules = map[string]rule{
	"workflow_run.requested":   {eventType: "ci.run_requested", extract: decode(workflowRun)},
	"workflow_run.in_progress": {eventType: "ci.run_in_progress", extract: decode(workflowRun)},
	"workflow_run.completed":   {eventType: "ci.run_completed", extract: decode(workflowRun)},

	"workflow_job.queued":      {eventType: "ci.job_queued", extract: decode(workflowJob)},
	"workflow_job.waiting":     {eventType: "ci.job_waiting", extract: decode(workflowJob)},
	"workflow_job.in_progress": {eventType: "ci.job_in_progress", extract: decode(workflowJob)},
	"workflow_job.completed":   {eventType: "ci.job_completed", extract: decode(workflowJob)},

	"check_run.created":     {eventType: "ci.check_created", extract: decode(checkRun)},
	"check_run.completed":   {eventType: "ci.check_completed", extract: decode(checkRun)},
	"check_suite.completed": {eventType: "ci.suite_completed", extract: decode(checkSuite)},

	"status.pending": {eventType: "ci.status_pending", extract: decode(commitStatus)},
	"status.success": {eventType: "ci.status_success", extract: decode(commitStatus)},
	"status.failure": {eventType: "ci.status_failure", extract: decode(commitStatus)},
	"status.error":   {eventType: "ci.status_error", extract: decode(commitStatus)},

	"pull_request.assigned":               {eventType: "pr.assigned", extract: decode(pullRequest(withAssignee))},
	"pull_request.auto_merge_disabled":    {eventType: "pr.auto_merge_disabled", extract: decode(pullRequest())},
	"pull_request.auto_merge_enabled":     {eventType: "pr.auto_merge_enabled", extract: decode(pullRequest())},
	"pull_request.closed":                 {eventType: "pr.closed", extract: decode(pullRequest(withMergeCommit))},
	"pull_request.converted_to_draft":     {eventType: "pr.converted_to_draft", extract: decode(pullRequest())},
	"pull_request.demilestoned":           {eventType: "pr.demilestoned", extract: decode(pullRequest(withMilestone))},
	"pull_request.dequeued":               {eventType: "pr.dequeued", extract: decode(pullRequest())},
	"pull_request.edited":                 {eventType: "pr.edited", extract: decode(pullRequest())},
	"pull_request.enqueued":               {eventType: "pr.enqueued", extract: decode(pullRequest())},
	"pull_request.labeled":                {eventType: "pr.labeled", extract: decode(pullRequest(withLabel))},
	"pull_request.locked":                 {eventType: "pr.locked", extract: decode(pullRequest())},
	"pull_request.milestoned":             {eventType: "pr.milestoned", extract: decode(pullRequest(withMilestone))},
	"pull_request.opened":                 {eventType: "pr.opened", extract: decode(pullRequest())},
	"pull_request.ready_for_review":       {eventType: "pr.ready_for_review", extract: decode(pullRequest())},
	"pull_request.reopened":               {eventType: "pr.reopened", extract: decode(pullRequest())},
	"pull_request.review_request_removed": {eventType: "pr.review_request_removed", extract: decode(pullRequest(withRequestedReviewer))},
	"pull_request.review_requested":       {eventType: "pr.review_requested", extract: decode(pullRequest(withRequestedReviewer))},
	"pull_request.synchronize":            {eventType: "pr.synchronize", extract: decode(pullRequest(withPushedCommits))},
	"pull_request.unassigned":             {eventType: "pr.unassigned", extract: decode(pullRequest(withAssignee))},
	"pull_request.unlabeled":              {eventType: "pr.unlabeled", extract: decode(pullRequest(withLabel))},
	"pull_request.unlocked":               {eventType: "pr.unlocked", extract: decode(pullRequest())},

	"pull_request_review.dismissed": {eventType: "pr.review_dismissed", extract: decode(review)},
	"pull_request_review.edited":    {eventType: "pr.review_edited", extract: decode(review)},
	"pull_request_review.submitted": {eventType: "pr.review_submitted", extract: decode(review)},

	"issue_comment.created": {eventType: "pr.comment_created", extract: decode(issueComment)},
	"issue_comment.deleted": {eventType: "pr.comment_deleted", extract: decode(issueComment)},
	"issue_comment.edited":  {eventType: "pr.comment_edited", extract: decode(issueComment)},

	"pull_request_review_comment.created": {eventType: "pr.review_comment_created", extract: decode(reviewComment)},
	"pull_request_review_comment.deleted": {eventType: "pr.review_comment_deleted", extract: decode(reviewComment)},
	"pull_request_review_comment.edited":  {eventType: "pr.review_comment_edited", extract: decode(reviewComment)},

	"pull_request_review_thread.resolved":   {eventType: "pr.review_thread_resolved", extract: decode(reviewThread)},
	"pull_request_review_thread.unresolved": {eventType: "pr.review_thread_unresolved", extract: decode(reviewThread)},
}

// A status webhook has no action member, and its state takes that part.
var actionMemberOf = map[string]string{"status": "state"}

type envelope struct {
	repoID int64
	word   string
}

func readEnvelope(eventName string, payload []byte) (envelope, error) {
	var members map[string]json.RawMessage
	if err := json.Unmarshal(payload, &members); err != nil {
		return envelope{}, err
	}
	member := cmp.Or(actionMemberOf[eventName], "action")
	var repo repoRef
	var word string
	for name, into := range map[string]any{"repository": &repo, member: &word} {
		raw, ok := members[name]
		if !ok {
			continue
		}
		if err := json.Unmarshal(raw, into); err != nil {
			return envelope{}, xerrors.Errorf("%s: %w", name, err)
		}
	}
	if word == "" {
		return envelope{}, xerrors.Errorf("no %s", member)
	}
	return envelope{repoID: repo.ID, word: word}, nil
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
	env, err := readEnvelope(eventName, payload)
	if err != nil {
		return Event{}, false, xerrors.Errorf("decode %s payload: %w", eventName, err)
	}
	key := eventName + "." + env.word
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
		RepoID:     env.repoID,
		PRs:        f.prs,
		HeadSHA:    f.headSHA,
		Data:       data,
	}
	if err := ev.Validate(); err != nil {
		return Event{}, false, xerrors.Errorf("normalize %s: %w", key, err)
	}
	return ev, true, nil
}

// StoresDelivery says whether Normalize turns a webhook with this event name
// and action into a stored event, as far as the headers tell. action is the
// action member of the payload, as GitHub's list of deliveries gives it. That
// list names no other member, so for an event whose action word is another
// member, such as the state of a status, the event name alone decides. It
// reads the same rules, so the two cannot disagree. A webhook it accepts can
// still be ignored for its payload, such as a comment on a plain issue.
func StoresDelivery(eventName, action string) bool {
	if _, other := actionMemberOf[eventName]; other {
		return handlesEvent(eventName)
	}
	_, found := rules[eventName+"."+action]
	return found
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
	Sender      user    `json:"sender"`
	WorkflowRun struct {
		ID           int64   `json:"id"`
		Name         string  `json:"name"`
		Status       string  `json:"status"`
		Conclusion   *string `json:"conclusion"`
		HeadBranch   string  `json:"head_branch"`
		HeadSHA      string  `json:"head_sha"`
		Event        string  `json:"event"`
		HTMLURL      string  `json:"html_url"`
		RunAttempt   int32   `json:"run_attempt"`
		WorkflowID   int64   `json:"workflow_id"`
		PullRequests []prRef `json:"pull_requests"`
	} `json:"workflow_run"`
}

func workflowRun(p workflowRunPayload) (fields, bool) {
	r := p.WorkflowRun
	return fields{
		prs:     prNumbers(p.Repository.ID, r.PullRequests),
		headSHA: r.HeadSHA,
		data: map[string]any{
			"run_id":        r.ID,
			"name":          r.Name,
			"status":        r.Status,
			"conclusion":    r.Conclusion,
			"head_branch":   r.HeadBranch,
			"trigger_event": r.Event,
			"html_url":      r.HTMLURL,
			"run_attempt":   r.RunAttempt,
			"workflow_id":   r.WorkflowID,
			"sender_login":  p.Sender.Login,
			"sender_id":     p.Sender.ID,
		},
	}, true
}

// GitHub names no pull request in a workflow_job payload.
type workflowJobPayload struct {
	Sender      user `json:"sender"`
	WorkflowJob struct {
		ID           int64    `json:"id"`
		RunID        int64    `json:"run_id"`
		RunAttempt   int32    `json:"run_attempt"`
		Name         string   `json:"name"`
		WorkflowName *string  `json:"workflow_name"`
		Status       string   `json:"status"`
		Conclusion   *string  `json:"conclusion"`
		HeadBranch   *string  `json:"head_branch"`
		HeadSHA      string   `json:"head_sha"`
		HTMLURL      string   `json:"html_url"`
		RunnerName   *string  `json:"runner_name"`
		Labels       []string `json:"labels"`
	} `json:"workflow_job"`
}

func workflowJob(p workflowJobPayload) (fields, bool) {
	j := p.WorkflowJob
	labels := j.Labels
	if labels == nil {
		labels = []string{}
	}
	return fields{
		prs:     []int32{},
		headSHA: j.HeadSHA,
		data: map[string]any{
			"job_id":        j.ID,
			"run_id":        j.RunID,
			"run_attempt":   j.RunAttempt,
			"name":          j.Name,
			"workflow_name": j.WorkflowName,
			"status":        j.Status,
			"conclusion":    j.Conclusion,
			"head_branch":   j.HeadBranch,
			"runner_name":   j.RunnerName,
			"labels":        labels,
			"html_url":      j.HTMLURL,
			"sender_login":  p.Sender.Login,
			"sender_id":     p.Sender.ID,
		},
	}, true
}

type appRef struct {
	Slug string `json:"slug"`
}

type checkRunPayload struct {
	Repository repoRef `json:"repository"`
	Sender     user    `json:"sender"`
	CheckRun   struct {
		ID           int64   `json:"id"`
		Name         string  `json:"name"`
		Status       string  `json:"status"`
		Conclusion   *string `json:"conclusion"`
		HeadSHA      string  `json:"head_sha"`
		HTMLURL      string  `json:"html_url"`
		PullRequests []prRef `json:"pull_requests"`
		App          appRef  `json:"app"`
		CheckSuite   struct {
			ID         int64   `json:"id"`
			HeadBranch *string `json:"head_branch"`
		} `json:"check_suite"`
	} `json:"check_run"`
}

func checkRun(p checkRunPayload) (fields, bool) {
	c := p.CheckRun
	return fields{
		prs:     prNumbers(p.Repository.ID, c.PullRequests),
		headSHA: c.HeadSHA,
		data: map[string]any{
			"check_run_id":   c.ID,
			"name":           c.Name,
			"status":         c.Status,
			"conclusion":     c.Conclusion,
			"html_url":       c.HTMLURL,
			"app_slug":       c.App.Slug,
			"check_suite_id": c.CheckSuite.ID,
			"head_branch":    c.CheckSuite.HeadBranch,
			"sender_login":   p.Sender.Login,
			"sender_id":      p.Sender.ID,
		},
	}, true
}

// GitHub gives a check suite no html_url.
type checkSuitePayload struct {
	Repository repoRef `json:"repository"`
	Sender     user    `json:"sender"`
	CheckSuite struct {
		ID           int64   `json:"id"`
		HeadBranch   *string `json:"head_branch"`
		HeadSHA      string  `json:"head_sha"`
		Status       *string `json:"status"`
		Conclusion   *string `json:"conclusion"`
		PullRequests []prRef `json:"pull_requests"`
		App          appRef  `json:"app"`
	} `json:"check_suite"`
}

func checkSuite(p checkSuitePayload) (fields, bool) {
	c := p.CheckSuite
	return fields{
		prs:     prNumbers(p.Repository.ID, c.PullRequests),
		headSHA: c.HeadSHA,
		data: map[string]any{
			"check_suite_id": c.ID,
			"status":         c.Status,
			"conclusion":     c.Conclusion,
			"head_branch":    c.HeadBranch,
			"app_slug":       c.App.Slug,
			"sender_login":   p.Sender.Login,
			"sender_id":      p.Sender.ID,
		},
	}, true
}

// GitHub names no pull request in a status payload.
type statusPayload struct {
	Sender    user    `json:"sender"`
	SHA       string  `json:"sha"`
	State     string  `json:"state"`
	Context   string  `json:"context"`
	TargetURL *string `json:"target_url"`
	Commit    struct {
		HTMLURL string `json:"html_url"`
	} `json:"commit"`
	Branches []struct {
		Name string `json:"name"`
	} `json:"branches"`
}

func commitStatus(p statusPayload) (fields, bool) {
	branches := make([]string, 0, len(p.Branches))
	for _, b := range p.Branches {
		branches = append(branches, b.Name)
	}
	return fields{
		prs:     []int32{},
		headSHA: p.SHA,
		data: map[string]any{
			"state":        p.State,
			"context":      p.Context,
			"target_url":   p.TargetURL,
			"branches":     branches,
			"html_url":     p.Commit.HTMLURL,
			"sender_login": p.Sender.Login,
			"sender_id":    p.Sender.ID,
		},
	}, true
}

type user struct {
	Login string `json:"login"`
	ID    int64  `json:"id"`
}

// pullRequestHead is the pull request of a review, a review comment or a
// review thread.
type pullRequestHead struct {
	Number int32 `json:"number"`
	Head   struct {
		SHA string `json:"sha"`
	} `json:"head"`
}

func (pr pullRequestHead) fields(data map[string]any) fields {
	return fields{prs: []int32{pr.Number}, headSHA: pr.Head.SHA, data: data}
}

type reviewPayload struct {
	Review struct {
		ID      int64  `json:"id"`
		State   string `json:"state"`
		User    user   `json:"user"`
		HTMLURL string `json:"html_url"`
	} `json:"review"`
	PullRequest pullRequestHead `json:"pull_request"`
	Sender      user            `json:"sender"`
}

func review(p reviewPayload) (fields, bool) {
	return p.PullRequest.fields(withSender(map[string]any{
		"review_id":  p.Review.ID,
		"state":      p.Review.State,
		"user_login": p.Review.User.Login,
		"html_url":   p.Review.HTMLURL,
	}, p.Sender)), true
}

type pullRequestPayload struct {
	Number      int32 `json:"number"`
	PullRequest struct {
		HTMLURL        string    `json:"html_url"`
		Title          string    `json:"title"`
		State          string    `json:"state"`
		Draft          bool      `json:"draft"`
		Merged         bool      `json:"merged"`
		MergeCommitSHA *string   `json:"merge_commit_sha"`
		User           user      `json:"user"`
		Head           branchRef `json:"head"`
		Base           branchRef `json:"base"`
	} `json:"pull_request"`
	Sender user `json:"sender"`
	// The fields below come with some actions only.
	Before string `json:"before"`
	After  string `json:"after"`
	Label  struct {
		Name string `json:"name"`
	} `json:"label"`
	Assignee          user  `json:"assignee"`
	RequestedReviewer *user `json:"requested_reviewer"`
	RequestedTeam     *struct {
		Slug string `json:"slug"`
	} `json:"requested_team"`
	Milestone struct {
		Title string `json:"title"`
	} `json:"milestone"`
}

type branchRef struct {
	Ref string `json:"ref"`
	SHA string `json:"sha"`
}

// pullRequestExtra adds to the data the fields that GitHub sends with some
// actions only.
type pullRequestExtra func(p pullRequestPayload, data map[string]any)

// pullRequest reads the fields that every action of the pull_request event
// has, and then the extras of the action.
func pullRequest(extras ...pullRequestExtra) func(pullRequestPayload) (fields, bool) {
	return func(p pullRequestPayload) (fields, bool) {
		pr := p.PullRequest
		data := withSender(map[string]any{
			"html_url":     pr.HTMLURL,
			"title":        pr.Title,
			"state":        pr.State,
			"draft":        pr.Draft,
			"merged":       pr.Merged,
			"base_branch":  pr.Base.Ref,
			"head_branch":  pr.Head.Ref,
			"author_login": pr.User.Login,
		}, p.Sender)
		for _, extra := range extras {
			extra(p, data)
		}
		return fields{prs: []int32{p.Number}, headSHA: pr.Head.SHA, data: data}, true
	}
}

// withMergeCommit is for closed only. Before a merge GitHub sends its
// test-merge commit, which an agent could take for the merge.
func withMergeCommit(p pullRequestPayload, data map[string]any) {
	data["merge_commit_sha"] = p.PullRequest.MergeCommitSHA
}

func withPushedCommits(p pullRequestPayload, data map[string]any) {
	data["before"] = p.Before
	data["after"] = p.After
}

func withLabel(p pullRequestPayload, data map[string]any) {
	data["label_name"] = p.Label.Name
}

func withAssignee(p pullRequestPayload, data map[string]any) {
	data["assignee_login"] = p.Assignee.Login
}

// withRequestedReviewer reads a requested user or a requested team, as GitHub
// sends one or the other.
func withRequestedReviewer(p pullRequestPayload, data map[string]any) {
	if p.RequestedReviewer != nil {
		data["requested_reviewer_login"] = p.RequestedReviewer.Login
	}
	if p.RequestedTeam != nil {
		data["requested_team_slug"] = p.RequestedTeam.Slug
	}
}

func withMilestone(p pullRequestPayload, data map[string]any) {
	data["milestone_title"] = p.Milestone.Title
}

// withSender adds the account that made the change.
func withSender(data map[string]any, sender user) map[string]any {
	data["sender_login"] = sender.Login
	data["sender_id"] = sender.ID
	return data
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
	Sender user `json:"sender"`
}

func issueComment(p issueCommentPayload) (fields, bool) {
	if p.Issue.PullRequest == nil {
		return fields{}, false
	}
	return fields{
		prs: []int32{p.Issue.Number},
		data: withSender(map[string]any{
			"comment_id": p.Comment.ID,
			"user_login": p.Comment.User.Login,
			"html_url":   p.Comment.HTMLURL,
		}, p.Sender),
	}, true
}

type reviewCommentPayload struct {
	Comment struct {
		ID       int64  `json:"id"`
		ReviewID int64  `json:"pull_request_review_id"`
		User     user   `json:"user"`
		HTMLURL  string `json:"html_url"`
	} `json:"comment"`
	PullRequest pullRequestHead `json:"pull_request"`
	Sender      user            `json:"sender"`
}

func reviewComment(p reviewCommentPayload) (fields, bool) {
	return p.PullRequest.fields(withSender(map[string]any{
		"comment_id": p.Comment.ID,
		"review_id":  p.Comment.ReviewID,
		"user_login": p.Comment.User.Login,
		"html_url":   p.Comment.HTMLURL,
	}, p.Sender)), true
}

type reviewThreadPayload struct {
	Thread struct {
		NodeID   string `json:"node_id"`
		Comments []struct {
			ID          int64  `json:"id"`
			InReplyToID *int64 `json:"in_reply_to_id"`
			User        user   `json:"user"`
			HTMLURL     string `json:"html_url"`
		} `json:"comments"`
	} `json:"thread"`
	PullRequest pullRequestHead `json:"pull_request"`
	Sender      user            `json:"sender"`
}

// reviewThread names the thread by the comment that started it, the one that
// replies to no other.
func reviewThread(p reviewThreadPayload) (fields, bool) {
	comments := p.Thread.Comments
	ids := make([]int64, len(comments))
	for i, c := range comments {
		ids[i] = c.ID
	}
	data := withSender(map[string]any{
		"thread_node_id": p.Thread.NodeID,
		"comment_ids":    ids,
	}, p.Sender)
	for _, c := range comments {
		if c.InReplyToID == nil {
			data["user_login"] = c.User.Login
			data["html_url"] = c.HTMLURL
			break
		}
	}
	return p.PullRequest.fields(data), true
}
