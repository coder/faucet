package faucet

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
)

// eventType describes one event type for events/list. Its name is the
// normalized event type that Normalize stores.
type eventType struct {
	name          string
	description   string
	payloadSchema json.RawMessage
}

const pollArgumentsSchema = `{
  "type": "object",
  "properties": {
    "repo": {"type": "string", "description": "The repository, written owner/name."},
    "sha": {"type": "string", "pattern": "^[0-9A-Fa-f]{40}$", "description": "Only events for this 40 character head commit SHA."},
    "pr": {"type": "integer", "minimum": 1, "maximum": 2147483647, "description": "Only events that mention this pull request number."}
  },
  "required": ["repo"],
  "additionalProperties": false
}`

const payloadTemplate = `{
  "type": "object",
  "properties": {
    "prs": {"type": "array", "items": {"type": "integer"}, "description": "Pull request numbers the event mentions."},
    "head_sha": {"type": "string", "description": "The head commit SHA. Absent when the event has none."},
    %s
  },
  "required": ["prs", %s]
}`

func payloadSchema(properties string, required ...string) json.RawMessage {
	quoted := make([]string, len(required))
	for i, name := range required {
		quoted[i] = fmt.Sprintf("%q", name)
	}
	return json.RawMessage(fmt.Sprintf(payloadTemplate, properties, strings.Join(quoted, ", ")))
}

var eventCatalog = []eventType{
	{
		name:        "ci.check_completed",
		description: "A check run finished on a commit, from GitHub Actions or another app.",
		payloadSchema: payloadSchema(`
    "check_run_id": {"type": "integer"},
    "name": {"type": "string", "description": "The check name."},
    "conclusion": {"type": "string", "description": "For example success, failure or timed_out."},
    "html_url": {"type": "string"},
    "app_slug": {"type": "string", "description": "The app that ran the check."}`,
			"check_run_id", "name", "conclusion", "html_url", "app_slug"),
	},
	{
		name:        "ci.run_completed",
		description: "A GitHub Actions workflow run finished. It fires once per run attempt.",
		payloadSchema: payloadSchema(`
    "run_id": {"type": "integer"},
    "name": {"type": "string", "description": "The workflow name."},
    "conclusion": {"type": "string", "description": "For example success, failure or timed_out."},
    "head_branch": {"type": "string"},
    "html_url": {"type": "string"},
    "run_attempt": {"type": "integer"}`,
			"run_id", "name", "conclusion", "head_branch", "html_url", "run_attempt"),
	},
	{
		name:        "pr.closed",
		description: "A pull request was closed, merged or not.",
		payloadSchema: payloadSchema(`
    "merged": {"type": "boolean"},
    "merge_commit_sha": {"type": ["string", "null"], "description": "Null when the pull request was closed without a merge."},
    "html_url": {"type": "string"}`,
			"merged", "merge_commit_sha", "html_url"),
	},
	{
		name:        "pr.comment_created",
		description: "A comment was posted in the conversation of a pull request.",
		payloadSchema: payloadSchema(`
    "comment_id": {"type": "integer"},
    "user_login": {"type": "string"},
    "html_url": {"type": "string"}`,
			"comment_id", "user_login", "html_url"),
	},
	{
		name:        "pr.review_submitted",
		description: "A review was submitted on a pull request.",
		payloadSchema: payloadSchema(`
    "review_id": {"type": "integer"},
    "state": {"type": "string", "description": "The review state, for example approved, changes_requested or commented."},
    "user_login": {"type": "string"},
    "html_url": {"type": "string"}`,
			"review_id", "state", "user_login", "html_url"),
	},
}

// pollArguments is the arguments object of events/poll, after validation
// against pollArgumentsSchema.
type pollArguments struct {
	Repo string `json:"repo"`
	SHA  string `json:"sha"`
	PR   int32  `json:"pr"`
}

var pollArgumentsResolved = mustResolve(pollArgumentsSchema)

func init() {
	names := make([]string, len(eventCatalog))
	for i, t := range eventCatalog {
		names[i] = t.name
		mustResolve(string(t.payloadSchema))
	}
	if !slices.Equal(names, eventTypes()) {
		panic(fmt.Sprintf("event catalog %v does not match the normalized event types %v", names, eventTypes()))
	}
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

func lookupEventType(name string) (eventType, bool) {
	i := slices.IndexFunc(eventCatalog, func(t eventType) bool { return t.name == name })
	if i < 0 {
		return eventType{}, false
	}
	return eventCatalog[i], true
}
