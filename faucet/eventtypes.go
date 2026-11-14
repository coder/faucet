package faucet

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
)

// eventType describes one event type. noHeadSHA is set for a type whose GitHub
// payload names no head commit, so no event of it has a head_sha and a sha
// filter could never match one.
type eventType struct {
	name          string
	description   string
	payloadSchema json.RawMessage
	noHeadSHA     bool
	resolvedInput *jsonschema.Resolved
}

const pollArgumentsTemplate = `{
  "type": "object",
  "properties": {
    "repo": {"type": "string", "description": "The repository, written owner/name."},%s
    "pr": {"type": "integer", "minimum": 1, "maximum": 2147483647, "description": "Only events that mention this pull request number."}
  },
  "required": ["repo"],
  "additionalProperties": false
}`

const shaArgumentProperty = `
    "sha": {"type": "string", "pattern": "^[0-9A-Fa-f]{40}$", "description": "Only events for this 40 character head commit SHA."},`

const payloadTemplate = `{
  "type": "object",
  "properties": {
    "prs": {"type": "array", "items": {"type": "integer"}, "description": "Pull request numbers the event mentions."},%s
    %s
  },
  "required": ["prs", %s]
}`

const headSHAProperty = `
    "head_sha": {"type": "string", "description": "The head commit SHA. Absent when the event has none."},`

func payloadSchema(properties string, required ...string) json.RawMessage {
	return payloadSchemaOf(headSHAProperty, properties, required...)
}

func payloadSchemaOf(headSHA, properties string, required ...string) json.RawMessage {
	quoted := make([]string, len(required))
	for i, name := range required {
		quoted[i] = fmt.Sprintf("%q", name)
	}
	return json.RawMessage(fmt.Sprintf(payloadTemplate, headSHA, properties, strings.Join(quoted, ", ")))
}

// shaInputResolved is the arguments schema of an event type that has a sha. A
// version before pr.comment_created lost its sha stored subscriptions with one,
// and events/unsubscribe still reads that shape so they can be ended.
var shaInputResolved = mustResolve(fmt.Sprintf(pollArgumentsTemplate, shaArgumentProperty))

func (t eventType) inputSchema() json.RawMessage {
	if t.noHeadSHA {
		return json.RawMessage(fmt.Sprintf(pollArgumentsTemplate, ""))
	}
	return json.RawMessage(fmt.Sprintf(pollArgumentsTemplate, shaArgumentProperty))
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
    "merge_commit_sha": {"type": ["string", "null"], "description": "The merge commit. Before a merge it is the test-merge commit of GitHub or null, so it does not mean the pull request merged. Read merged."},
    "html_url": {"type": "string"}`,
			"merged", "merge_commit_sha", "html_url"),
	},
	{
		name:        "pr.comment_created",
		description: "A comment was posted in the conversation of a pull request. GitHub names no head commit in this event, so filter by pr and not by sha.",
		payloadSchema: payloadSchemaOf("", `
    "comment_id": {"type": "integer"},
    "user_login": {"type": "string"},
    "html_url": {"type": "string"}`,
			"comment_id", "user_login", "html_url"),
		noHeadSHA: true,
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

type pollArguments struct {
	Repo string `json:"repo"`
	SHA  string `json:"sha"`
	PR   int32  `json:"pr"`
}

func init() {
	names := make([]string, len(eventCatalog))
	for i, t := range eventCatalog {
		names[i] = t.name
		mustResolve(string(t.payloadSchema))
		eventCatalog[i].resolvedInput = mustResolve(string(t.inputSchema()))
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
