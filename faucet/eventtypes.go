package faucet

import (
	"cmp"
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
)

// family is the events of one GitHub webhook event. Its actions, and the
// stored type of each, come from the keys of rules that start with its GitHub
// event, so rules stays the one list of what faucet stores.
type family struct {
	name        string
	githubEvent string
	description string
	// noHeadSHA is set when GitHub's payload names no head commit, so no event
	// of the family has a head_sha and a sha filter could never match one.
	noHeadSHA bool
	// noPRs is set when GitHub's payload names no pull request, so every
	// event of the family has empty prs and a pr filter could never match.
	noPRs bool
	// properties are the payload properties of the family's data, written as
	// a JSON object.
	properties string
	// actions are GitHub's action words in sorted order, and types holds the
	// stored type of each. Both are filled from rules.
	actions []string
	types   []string
}

// legacyName is a stored type that events/list offers under its own name, as
// it did before families. SEP-3415 asks that the schemas of a name change only
// by addition, so it keeps the fields it always required.
type legacyName struct {
	name        string
	description string
	required    []string
	// pinned are payload properties, written as a JSON object, that the name
	// keeps as they were while its family's property grew wider for other
	// actions.
	pinned string
}

// catalogEntry is one name that events/list offers.
type catalogEntry struct {
	name        string
	description string
	family      *family
	// legacy is set for a name that is one stored type.
	legacy        bool
	inputSchema   json.RawMessage
	payloadSchema json.RawMessage
	input         *jsonschema.Resolved
	// endInput is the arguments schema that events/unsubscribe reads.
	endInput *jsonschema.Resolved
}

// storedType is what the registry knows of one value of Event.Type.
type storedType struct {
	family *family
	action string
}

var families = []*family{
	{
		name:        "ci.runs",
		githubEvent: "workflow_run",
		description: "GitHub Actions workflow runs. Each run attempt has its own events.",
		properties: `{
    "run_id": {"type": "integer"},
    "name": {"type": "string", "description": "The workflow name."},
    "status": {"type": "string", "description": "For example queued, in_progress or completed."},
    "conclusion": {"type": ["string", "null"], "description": "For example success, failure or timed_out. Null until the run completes."},
    "head_branch": {"type": "string"},
    "trigger_event": {"type": "string", "description": "The GitHub event that started the run, for example push or pull_request."},
    "html_url": {"type": "string"},
    "run_attempt": {"type": "integer"},
    "workflow_id": {"type": "integer"},
    "sender_login": {"type": "string"},
    "sender_id": {"type": "integer"}
}`,
	},
	{
		name:        "ci.jobs",
		githubEvent: "workflow_job",
		description: "Jobs of GitHub Actions workflow runs. GitHub names no pull request in these events, so filter by sha and not by pr.",
		noPRs:       true,
		properties: `{
    "job_id": {"type": "integer"},
    "run_id": {"type": "integer"},
    "run_attempt": {"type": "integer"},
    "name": {"type": "string", "description": "The job name."},
    "workflow_name": {"type": ["string", "null"]},
    "status": {"type": "string", "description": "For example queued, waiting, in_progress or completed."},
    "conclusion": {"type": ["string", "null"], "description": "For example success, failure or skipped. Null until the job completes."},
    "head_branch": {"type": ["string", "null"]},
    "runner_name": {"type": ["string", "null"]},
    "labels": {"type": "array", "items": {"type": "string"}, "description": "The runner labels that the job asks for."},
    "html_url": {"type": "string"},
    "sender_login": {"type": "string"},
    "sender_id": {"type": "integer"}
}`,
	},
	{
		name:        "ci.checks",
		githubEvent: "check_run",
		description: "Check runs on a commit, from GitHub Actions or another app.",
		properties: `{
    "check_run_id": {"type": "integer"},
    "name": {"type": "string", "description": "The check name."},
    "status": {"type": "string", "description": "For example queued, in_progress or completed."},
    "conclusion": {"type": ["string", "null"], "description": "For example success, failure or timed_out. Null until the check completes."},
    "html_url": {"type": "string"},
    "app_slug": {"type": "string", "description": "The app that ran the check."},
    "check_suite_id": {"type": "integer"},
    "head_branch": {"type": ["string", "null"], "description": "The branch of the check suite. Null for a push to a fork."},
    "sender_login": {"type": "string"},
    "sender_id": {"type": "integer"}
}`,
	},
	{
		name:        "ci.suites",
		githubEvent: "check_suite",
		description: "Check suites on a commit. A suite holds the check runs of one app, and it completes when they all have.",
		properties: `{
    "check_suite_id": {"type": "integer"},
    "status": {"type": ["string", "null"]},
    "conclusion": {"type": ["string", "null"], "description": "For example success, failure or timed_out."},
    "head_branch": {"type": ["string", "null"], "description": "Null for a push to a fork."},
    "app_slug": {"type": "string", "description": "The app whose check runs the suite holds."},
    "sender_login": {"type": "string"},
    "sender_id": {"type": "integer"}
}`,
	},
	{
		name:        "ci.statuses",
		githubEvent: "status",
		description: "Commit statuses, which CI services and apps set on a commit. The action is the state: pending, success, failure or error. GitHub names no pull request in these events, so filter by sha and not by pr.",
		noPRs:       true,
		properties: `{
    "state": {"type": "string", "description": "pending, success, failure or error, the same as action."},
    "context": {"type": "string", "description": "The label that tells this status from those of other systems, for example default."},
    "target_url": {"type": ["string", "null"], "description": "The link that GitHub shows with the status."},
    "branches": {"type": "array", "items": {"type": "string"}, "description": "Up to 10 branches that hold the commit. It need not be their head."},
    "html_url": {"type": "string", "description": "The commit page."},
    "sender_login": {"type": "string"},
    "sender_id": {"type": "integer"}
}`,
	},
	{
		name:        "pr.activity",
		githubEvent: "pull_request",
		description: "Pull requests: every action of GitHub's pull_request event, for example opened, synchronize, labeled or closed.",
		properties: `{
    "merged": {"type": "boolean"},
    "merge_commit_sha": {"type": ["string", "null"], "description": "The merge commit, in closed events only. When the pull request closed unmerged it is the test-merge commit of GitHub or null, so it does not mean the pull request merged. Read merged."},
    "html_url": {"type": "string"},
    "title": {"type": "string"},
    "state": {"type": "string", "description": "open or closed."},
    "draft": {"type": "boolean"},
    "base_branch": {"type": "string", "description": "The branch that the pull request merges into."},
    "head_branch": {"type": "string", "description": "The branch that holds the changes."},
    "author_login": {"type": "string", "description": "The account that opened the pull request."},
    "sender_login": {"type": "string", "description": "The account that made the change."},
    "sender_id": {"type": "integer", "description": "The GitHub user ID of sender_login."},
    "before": {"type": "string", "description": "In synchronize events, the head commit before the push."},
    "after": {"type": "string", "description": "In synchronize events, the head commit after the push."},
    "label_name": {"type": "string", "description": "In labeled and unlabeled events, the label."},
    "assignee_login": {"type": "string", "description": "In assigned and unassigned events, the account."},
    "requested_reviewer_login": {"type": "string", "description": "In review_requested and review_request_removed events for a user, the user."},
    "requested_team_slug": {"type": "string", "description": "In review_requested and review_request_removed events for a team, the team."},
    "milestone_title": {"type": "string", "description": "In milestoned and demilestoned events, the milestone."}
}`,
	},
	{
		name:        "pr.reviews",
		githubEvent: "pull_request_review",
		description: "Reviews on a pull request that were submitted, edited or dismissed.",
		properties: `{
    "review_id": {"type": "integer"},
    "state": {"type": "string", "description": "The review state, for example approved, changes_requested, commented or dismissed."},
    "user_login": {"type": "string", "description": "The author of the review."},
    "html_url": {"type": "string"},
    "sender_login": {"type": "string", "description": "The account that made the change."},
    "sender_id": {"type": "integer", "description": "The GitHub user ID of sender_login."}
}`,
	},
	{
		name:        "pr.comments",
		githubEvent: "issue_comment",
		description: "Comments in the conversation of a pull request that were created, edited or deleted. GitHub names no head commit in these events, so filter by pr and not by sha.",
		noHeadSHA:   true,
		properties: `{
    "comment_id": {"type": "integer"},
    "user_login": {"type": "string", "description": "The author of the comment."},
    "html_url": {"type": "string"},
    "sender_login": {"type": "string", "description": "The account that made the change."},
    "sender_id": {"type": "integer", "description": "The GitHub user ID of sender_login."}
}`,
	},
	{
		name:        "pr.review_comments",
		githubEvent: "pull_request_review_comment",
		description: "Comments on the diff of a pull request that were created, edited or deleted. Each belongs to a review.",
		properties: `{
    "comment_id": {"type": "integer"},
    "review_id": {"type": "integer", "description": "The review that the comment belongs to."},
    "user_login": {"type": "string", "description": "The author of the comment."},
    "html_url": {"type": "string"},
    "sender_login": {"type": "string", "description": "The account that made the change."},
    "sender_id": {"type": "integer", "description": "The GitHub user ID of sender_login."}
}`,
	},
	{
		name:        "pr.review_threads",
		githubEvent: "pull_request_review_thread",
		description: "Threads of review comments on a pull request that were resolved or unresolved.",
		properties: `{
    "thread_node_id": {"type": "string", "description": "The GraphQL node ID of the thread, its only ID in the payload."},
    "comment_ids": {"type": "array", "items": {"type": "integer"}, "description": "The review comments in the thread."},
    "user_login": {"type": "string", "description": "The author of the comment that started the thread."},
    "html_url": {"type": "string", "description": "The comment that started the thread."},
    "sender_login": {"type": "string", "description": "The account that made the change."},
    "sender_id": {"type": "integer", "description": "The GitHub user ID of sender_login."}
}`,
	},
}

var legacyNames = []legacyName{
	{
		name:        "ci.check_completed",
		description: "A check run finished on a commit, from GitHub Actions or another app.",
		required:    []string{"check_run_id", "name", "conclusion", "html_url", "app_slug"},
		pinned:      `{"conclusion": {"type": "string", "description": "For example success, failure or timed_out."}}`,
	},
	{
		name:        "ci.run_completed",
		description: "A GitHub Actions workflow run finished. It fires once per run attempt.",
		required:    []string{"run_id", "name", "conclusion", "head_branch", "html_url", "run_attempt"},
		pinned:      `{"conclusion": {"type": "string", "description": "For example success, failure or timed_out."}}`,
	},
	{
		name:        "pr.closed",
		description: "A pull request was closed, merged or not.",
		required:    []string{"merged", "merge_commit_sha", "html_url"},
	},
	{
		name:        "pr.comment_created",
		description: "A comment was posted in the conversation of a pull request. GitHub names no head commit in this event, so filter by pr and not by sha.",
		required:    []string{"comment_id", "user_login", "html_url"},
	},
	{
		name:        "pr.review_submitted",
		description: "A review was submitted on a pull request.",
		required:    []string{"review_id", "state", "user_login", "html_url"},
	},
}

var (
	storedTypes = map[string]storedType{}
	catalog     []*catalogEntry
)

var (
	repoArgument = map[string]any{"type": "string", "description": "The repository, written owner/name."}
	prArgument   = map[string]any{"type": "integer", "minimum": 1, "maximum": math.MaxInt32, "description": "Only events that mention this pull request number."}
	shaArgument  = map[string]any{"type": "string", "pattern": "^[0-9A-Fa-f]{40}$", "description": "Only events for this 40 character head commit SHA."}

	prsProperty     = map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "Pull request numbers the event mentions."}
	headSHAProperty = map[string]any{"type": "string", "description": "The head commit SHA. Absent when the event has none."}
)

// argumentsSchema is the inputSchema of an event name: repo and the filters of
// the name. It offers pr and sha unless the family's events name no pull
// request or no head commit (no family means both), and actions when it is
// given them.
func argumentsSchema(f *family, actions []string) json.RawMessage {
	properties := map[string]any{"repo": repoArgument}
	if f == nil || !f.noPRs {
		properties["pr"] = prArgument
	}
	if f == nil || !f.noHeadSHA {
		properties["sha"] = shaArgument
	}
	if len(actions) > 0 {
		properties["actions"] = map[string]any{
			"type":        "array",
			"minItems":    1,
			"items":       map[string]any{"type": "string", "enum": actions},
			"description": "Only events of these GitHub actions. Without it, events of every action.",
		}
	}
	return marshalRaw(map[string]any{"type": "object", "properties": properties, "required": []string{"repo"}, "additionalProperties": false})
}

// shaInputResolved is the arguments schema of an event type that has a sha. A
// version before pr.comment_created lost its sha stored subscriptions with one,
// and events/unsubscribe still reads that shape so they can be ended.
var shaInputResolved = mustResolve(string(argumentsSchema(nil, nil)))

func init() {
	byEvent := map[string]*family{}
	for _, f := range families {
		if _, dup := byEvent[f.githubEvent]; dup {
			panic(fmt.Sprintf("two event families read the GitHub event %s", f.githubEvent))
		}
		byEvent[f.githubEvent] = f
	}
	for _, key := range slices.Sorted(maps.Keys(rules)) {
		event, action, _ := strings.Cut(key, ".")
		f, ok := byEvent[event]
		if !ok {
			panic(fmt.Sprintf("rule %s belongs to no event family", key))
		}
		t := rules[key].eventType
		if _, dup := storedTypes[t]; dup {
			panic(fmt.Sprintf("two rules store the type %s", t))
		}
		storedTypes[t] = storedType{family: f, action: action}
		f.actions = append(f.actions, action)
		f.types = append(f.types, t)
	}
	for _, f := range families {
		if len(f.actions) == 0 {
			panic(fmt.Sprintf("event family %s has no action in rules", f.name))
		}
		catalog = append(catalog, newFamilyEntry(f))
	}
	for _, l := range legacyNames {
		t, ok := storedTypes[l.name]
		if !ok {
			panic(fmt.Sprintf("legacy event name %s is not a stored type", l.name))
		}
		catalog = append(catalog, newLegacyEntry(l, t))
	}
	names := catalogNames()
	if len(slices.Compact(slices.Sorted(slices.Values(names)))) != len(names) {
		panic(fmt.Sprintf("the event catalog repeats a name: %v", names))
	}
}

func newFamilyEntry(f *family) *catalogEntry {
	input := argumentsSchema(f, f.actions)
	resolved := mustResolve(string(input))
	return &catalogEntry{
		name:          f.name,
		description:   f.description,
		family:        f,
		inputSchema:   input,
		payloadSchema: payloadSchemaOf(f, f.types, f.actions, []string{"prs", "event", "action"}, ""),
		input:         resolved,
		endInput:      resolved,
	}
}

func newLegacyEntry(l legacyName, t storedType) *catalogEntry {
	input := argumentsSchema(t.family, nil)
	return &catalogEntry{
		name:          l.name,
		description:   l.description,
		family:        t.family,
		legacy:        true,
		inputSchema:   input,
		payloadSchema: payloadSchemaOf(t.family, []string{l.name}, []string{t.action}, append([]string{"prs"}, l.required...), l.pinned),
		input:         mustResolve(string(input)),
		endInput:      shaInputResolved,
	}
}

// payloadSchemaOf is the payloadSchema of the given types of family f. The
// properties in pinned, a JSON object or empty, replace the family's own.
func payloadSchemaOf(f *family, types, actions, required []string, pinned string) json.RawMessage {
	properties := map[string]any{
		"prs":    prsProperty,
		"event":  map[string]any{"type": "string", "enum": types, "description": "The exact event type, for example pr.closed."},
		"action": map[string]any{"type": "string", "enum": actions, "description": "The action of the GitHub webhook, for example closed."},
	}
	if !f.noHeadSHA {
		properties["head_sha"] = headSHAProperty
	}
	for _, own := range []string{f.properties, cmp.Or(pinned, "{}")} {
		var object map[string]json.RawMessage
		if err := json.Unmarshal([]byte(own), &object); err != nil {
			panic(fmt.Sprintf("payload properties of %s: %v", f.name, err))
		}
		for name, property := range object {
			properties[name] = property
		}
	}
	schema := marshalRaw(map[string]any{"type": "object", "properties": properties, "required": required})
	mustResolve(string(schema))
	return schema
}

// types returns the stored types that the entry matches. A legacy name matches
// its own type. A family matches the types of the given actions that are its
// own, and every type of it when actions is empty.
func (e *catalogEntry) types(actions []string) []string {
	if e.legacy {
		return []string{e.name}
	}
	if len(actions) == 0 {
		return slices.Clone(e.family.types)
	}
	var types []string
	for i, action := range e.family.actions {
		if slices.Contains(actions, action) {
			types = append(types, e.family.types[i])
		}
	}
	return types
}

func (e *catalogEntry) hasAction(action string) bool {
	return !e.legacy && slices.Contains(e.family.actions, action)
}

func catalogNames() []string {
	names := make([]string, len(catalog))
	for i, e := range catalog {
		names[i] = e.name
	}
	return names
}

type pollArguments struct {
	Repo    string   `json:"repo"`
	SHA     string   `json:"sha"`
	PR      int32    `json:"pr"`
	Actions []string `json:"actions"`
}

func mustResolve(schema string) *jsonschema.Resolved {
	var s jsonschema.Schema
	if err := json.Unmarshal([]byte(schema), &s); err != nil {
		panic(fmt.Sprintf("invalid JSON Schema: %v\n%s", err, schema))
	}
	resolved, err := s.Resolve(nil)
	if err != nil {
		panic(fmt.Sprintf("unresolvable JSON Schema: %v\n%s", err, schema))
	}
	return resolved
}

func lookupEntry(name string) (*catalogEntry, bool) {
	i := slices.IndexFunc(catalog, func(e *catalogEntry) bool { return e.name == name })
	if i < 0 {
		return nil, false
	}
	return catalog[i], true
}
