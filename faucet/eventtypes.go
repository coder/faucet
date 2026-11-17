package faucet

import (
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
    "conclusion": {"type": "string", "description": "For example success, failure or timed_out."},
    "head_branch": {"type": "string"},
    "html_url": {"type": "string"},
    "run_attempt": {"type": "integer"}
}`,
	},
	{
		name:        "ci.checks",
		githubEvent: "check_run",
		description: "Check runs on a commit, from GitHub Actions or another app.",
		properties: `{
    "check_run_id": {"type": "integer"},
    "name": {"type": "string", "description": "The check name."},
    "conclusion": {"type": "string", "description": "For example success, failure or timed_out."},
    "html_url": {"type": "string"},
    "app_slug": {"type": "string", "description": "The app that ran the check."}
}`,
	},
	{
		name:        "pr.activity",
		githubEvent: "pull_request",
		description: "Pull requests, for example one that was closed.",
		properties: `{
    "merged": {"type": "boolean"},
    "merge_commit_sha": {"type": ["string", "null"], "description": "The merge commit. Before a merge it is the test-merge commit of GitHub or null, so it does not mean the pull request merged. Read merged."},
    "html_url": {"type": "string"}
}`,
	},
	{
		name:        "pr.reviews",
		githubEvent: "pull_request_review",
		description: "Reviews on a pull request.",
		properties: `{
    "review_id": {"type": "integer"},
    "state": {"type": "string", "description": "The review state, for example approved, changes_requested or commented."},
    "user_login": {"type": "string"},
    "html_url": {"type": "string"}
}`,
	},
	{
		name:        "pr.comments",
		githubEvent: "issue_comment",
		description: "Comments in the conversation of a pull request. GitHub names no head commit in these events, so filter by pr and not by sha.",
		noHeadSHA:   true,
		properties: `{
    "comment_id": {"type": "integer"},
    "user_login": {"type": "string"},
    "html_url": {"type": "string"}
}`,
	},
}

var legacyNames = []legacyName{
	{
		name:        "ci.check_completed",
		description: "A check run finished on a commit, from GitHub Actions or another app.",
		required:    []string{"check_run_id", "name", "conclusion", "html_url", "app_slug"},
	},
	{
		name:        "ci.run_completed",
		description: "A GitHub Actions workflow run finished. It fires once per run attempt.",
		required:    []string{"run_id", "name", "conclusion", "head_branch", "html_url", "run_attempt"},
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

// argumentsSchema is the inputSchema of an event name: repo and pr, and the
// filters of the name. It offers sha unless the family's events carry no head
// commit (no family means sha), and actions when it is given them.
func argumentsSchema(f *family, actions []string) json.RawMessage {
	properties := map[string]any{"repo": repoArgument, "pr": prArgument}
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
		payloadSchema: payloadSchemaOf(f, f.types, f.actions, []string{"prs", "event", "action"}),
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
		payloadSchema: payloadSchemaOf(t.family, []string{l.name}, []string{t.action}, append([]string{"prs"}, l.required...)),
		input:         mustResolve(string(input)),
		endInput:      shaInputResolved,
	}
}

func payloadSchemaOf(f *family, types, actions, required []string) json.RawMessage {
	var own map[string]json.RawMessage
	if err := json.Unmarshal([]byte(f.properties), &own); err != nil {
		panic(fmt.Sprintf("payload properties of %s: %v", f.name, err))
	}
	properties := map[string]any{
		"prs":    prsProperty,
		"event":  map[string]any{"type": "string", "enum": types, "description": "The exact event type, for example pr.closed."},
		"action": map[string]any{"type": "string", "enum": actions, "description": "The action of the GitHub webhook, for example closed."},
	}
	if !f.noHeadSHA {
		properties["head_sha"] = headSHAProperty
	}
	for name, property := range own {
		properties[name] = property
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
