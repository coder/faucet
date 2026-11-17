package faucet

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
	"golang.org/x/xerrors"

	"github.com/coder/faucet/faucet/internal/githubapi"
)

const (
	rawNamePrefix   = "github."
	rawCursorPrefix = "r1."

	maxFieldPaths        = 32
	maxFieldPathSegments = 8
	maxFieldPathBytes    = 128
)

type rawEvent struct {
	event       string
	description string
}

// rawEvents are the GitHub webhook events whose payload names a repository
// and that no typed family stores in full, sorted by event. issue_comment is
// here because pr.comments stores only the comments on pull requests.
var rawEvents = []rawEvent{
	{"branch_protection_configuration", "Branch protection that was turned on or off for a whole repository."},
	{"branch_protection_rule", "Branch protection rules that were created, edited or deleted."},
	{"code_scanning_alert", "Code scanning alerts that appeared, were fixed, were closed or reopened, or changed in another way."},
	{"commit_comment", "Comments that were posted on a commit."},
	{"create", "Git branches and tags that were created."},
	{"custom_property_values", "Changes to the values of the custom properties of a repository."},
	{"delete", "Git branches and tags that were deleted."},
	{"dependabot_alert", "Dependabot alerts that were created, dismissed, fixed, reopened or changed in another way."},
	{"deploy_key", "Deploy keys that were added to or removed from a repository."},
	{"deployment", "Deployments that were created."},
	{"deployment_protection_rule", "Requests that a custom deployment protection rule approve or reject a deployment."},
	{"deployment_review", "Reviews of a deployment that waits for approval: requested, approved or rejected."},
	{"deployment_status", "New statuses of a deployment, for example in progress, success or failure."},
	{"discussion", "Discussions that were created, edited, answered, labeled, closed or changed in another way."},
	{"discussion_comment", "Comments on a discussion that were created, edited or deleted."},
	{"fork", "Forks of a repository."},
	{"gollum", "Wiki pages that were created or updated."},
	{"issue_comment", "Comments on plain issues that were created, edited or deleted, while comments on pull requests are pr.comments."},
	{"issue_dependencies", "Blocking relationships between issues that were added or removed."},
	{"issue_relates_to", "Links that mark two issues as related that were added or removed."},
	{"issues", "Issues that were opened, edited, labeled, assigned, closed or changed in another way."},
	{"label", "Labels of a repository that were created, edited or deleted."},
	{"member", "Collaborators that were added to a repository, removed from it, or given another permission."},
	{"merge_group", "Merge groups of a merge queue that need their checks run or that were destroyed."},
	{"milestone", "Milestones that were created, edited, opened, closed or deleted."},
	{"package", "Packages in GitHub Packages that were published or updated."},
	{"page_build", "Builds of the GitHub Pages site of a repository, whether they succeeded or failed."},
	{"project", "Classic projects that were created, edited, closed, reopened or deleted."},
	{"project_card", "Cards of a classic project that were created, edited, moved, converted or deleted."},
	{"project_column", "Columns of a classic project that were created, edited, moved or deleted."},
	{"public", "A private repository that was made public."},
	{"push", "Pushes of commits or tags to a repository, and branches and tags that a push deleted."},
	{"registry_package", "Registry packages in GitHub Packages that were published or updated."},
	{"release", "Releases that were created, published, edited, deleted or changed in another way."},
	{"repository", "Changes to a repository itself, for example one that was renamed, archived, transferred or made public."},
	{"repository_advisory", "Security advisories of a repository that were reported or published."},
	{"repository_dispatch", "Custom events that a client sent to the repository dispatch endpoint of the REST API, with the event type it chose as the action."},
	{"repository_import", "Imports of a repository into GitHub that succeeded, were canceled or failed."},
	{"repository_ruleset", "Rulesets of a repository that were created, edited or deleted."},
	{"repository_vulnerability_alert", "Vulnerability alerts of a repository that were created, dismissed, reopened or resolved."},
	{"secret_scanning_alert", "Secret scanning alerts that were created, resolved, reopened or changed in another way."},
	{"secret_scanning_alert_location", "New places where the secret of a secret scanning alert was found."},
	{"secret_scanning_scan", "Secret scanning scans of a repository that completed."},
	{"security_and_analysis", "Code security and analysis features that were turned on or off for a repository."},
	{"star", "Stars that were added to or removed from a repository."},
	{"sub_issues", "Sub-issues that were added to or removed from a parent issue."},
	{"team_add", "Teams that were given access to a repository."},
	{"watch", "Accounts that started to watch a repository."},
	{"workflow_dispatch", "GitHub Actions workflows that someone started by hand."},
}

type rawEntry struct {
	name          string
	event         string
	description   string
	inputSchema   json.RawMessage
	payloadSchema json.RawMessage
	input         *jsonschema.Resolved
}

var (
	rawCatalog []*rawEntry

	fieldSegmentPattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
)

func init() {
	// The typed catalog is built by the init function of eventtypes.go, which
	// runs first because the go command orders a package's files by name.
	if len(catalog) == 0 {
		panic("the raw event names are checked before the typed catalog is built")
	}
	for t := range storedTypes {
		if strings.HasPrefix(t, rawNamePrefix) {
			panic(fmt.Sprintf("the stored type %s starts with the raw name prefix %s", t, rawNamePrefix))
		}
	}
	input := rawArgumentsSchema()
	resolved := mustResolve(string(input))
	payload := marshalRaw(map[string]any{
		"type":        "object",
		"description": "The webhook payload that GitHub sent, or only the members that fields selects, nested as in the payload.",
	})
	mustResolve(string(payload))
	for i, e := range rawEvents {
		if i > 0 && rawEvents[i-1].event >= e.event {
			panic(fmt.Sprintf("the raw events are not sorted or repeat one at %s", e.event))
		}
		name := rawNamePrefix + e.event
		if _, typed := lookupEntry(name); typed {
			panic(fmt.Sprintf("the raw name %s is also a typed name", name))
		}
		rawCatalog = append(rawCatalog, &rawEntry{
			name:          name,
			event:         e.event,
			description:   e.description,
			inputSchema:   input,
			payloadSchema: payload,
			input:         resolved,
		})
	}
}

func rawArgumentsSchema() json.RawMessage {
	return marshalRaw(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"repo": repoArgument,
			"actions": map[string]any{
				"type":        "array",
				"minItems":    1,
				"items":       map[string]any{"type": "string"},
				"description": "Only deliveries whose action is one of these. Without it, every action, and events that have none.",
			},
			"fields": map[string]any{
				"type":     "array",
				"minItems": 1,
				"maxItems": maxFieldPaths,
				"items":    map[string]any{"type": "string"},
				"description": fmt.Sprintf("Only these members of the payload, each a path of 1 to %d names of letters, digits, _ or - joined by dots and at most %d bytes long, such as commits.id, which keeps the id of each commit.",
					maxFieldPathSegments, maxFieldPathBytes),
			},
		},
		"required":             []string{"repo"},
		"additionalProperties": false,
	})
}

func lookupRawEntry(name string) (*rawEntry, bool) {
	i := slices.IndexFunc(rawCatalog, func(e *rawEntry) bool { return e.name == name })
	if i < 0 {
		return nil, false
	}
	return rawCatalog[i], true
}

func rawNames() []string {
	names := make([]string, len(rawCatalog))
	for i, e := range rawCatalog {
		names[i] = e.name
	}
	return names
}

func describeRawEvents(delivery []string) []eventDescriptor {
	descriptors := make([]eventDescriptor, len(rawCatalog))
	for i, e := range rawCatalog {
		descriptors[i] = eventDescriptor{
			Name:          e.name,
			Description:   e.description,
			Delivery:      delivery,
			InputSchema:   e.inputSchema,
			PayloadSchema: e.payloadSchema,
		}
	}
	return descriptors
}

type rawArguments struct {
	Repo    string   `json:"repo"`
	Actions []string `json:"actions,omitempty"`
	Fields  []string `json:"fields,omitempty"`
}

func parseRawArguments(entry *rawEntry, raw json.RawMessage) (rawArguments, error) {
	var generic any = map[string]any{}
	if len(raw) > 0 && !bytes.Equal(raw, []byte("null")) {
		if err := json.Unmarshal(raw, &generic); err != nil {
			return rawArguments{}, invalidParams("arguments are not valid JSON")
		}
	}
	if err := entry.input.Validate(generic); err != nil {
		return rawArguments{}, invalidParams("arguments do not match the inputSchema: %v", err)
	}
	var args rawArguments
	if err := json.Unmarshal(raw, &args); err != nil {
		return rawArguments{}, invalidParams("arguments do not match the inputSchema: %v", err)
	}
	if _, _, err := githubapi.ParseRepo(args.Repo); err != nil {
		return rawArguments{}, invalidParams("%v", err)
	}
	if i := slices.Index(args.Actions, ""); i >= 0 {
		return rawArguments{}, invalidParams("actions[%d] is empty", i)
	}
	if i := slices.IndexFunc(args.Fields, func(path string) bool { return !validFieldPath(path) }); i >= 0 {
		return rawArguments{}, invalidParams("fields[%d] is not a path of 1 to %d names of letters, digits, _ or - joined by dots and at most %d bytes long",
			i, maxFieldPathSegments, maxFieldPathBytes)
	}
	return args, nil
}

func validFieldPath(path string) bool {
	if len(path) > maxFieldPathBytes {
		return false
	}
	segments := strings.Split(path, ".")
	return len(segments) <= maxFieldPathSegments && !slices.ContainsFunc(segments, func(s string) bool { return !fieldSegmentPattern.MatchString(s) })
}

func (a rawArguments) normalized() rawArguments {
	return rawArguments{Repo: a.Repo, Actions: sortedSetOrNil(a.Actions), Fields: sortedSetOrNil(a.Fields)}
}

// canonical is the one JSON spelling of a, so that two requests that differ
// only in key order, whitespace or the order and repeats of a list address the
// same subscription or lease.
func (a rawArguments) canonical() json.RawMessage {
	return marshalRaw(a.normalized())
}

func sortedSetOrNil(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	return sortedSet(values)
}

func encodeRawCursor(pos int64) string {
	return rawCursorPrefix + strconv.FormatInt(pos, 10)
}

func encodeRawPollCursor(pos int64, lease RawLeaseID) string {
	return encodeRawCursor(pos) + "." + hex.EncodeToString(lease[:])
}

func decodeRawCursor(cursor string) (pos int64, lease RawLeaseID, hasLease bool, ok bool) {
	rest, found := strings.CutPrefix(cursor, rawCursorPrefix)
	if !found {
		return 0, RawLeaseID{}, false, false
	}
	digits, leaseHex, hasLease := strings.Cut(rest, ".")
	pos, err := strconv.ParseInt(digits, 10, 64)
	if err != nil || pos < 0 || strconv.FormatInt(pos, 10) != digits {
		return 0, RawLeaseID{}, false, false
	}
	if !hasLease {
		return pos, RawLeaseID{}, false, true
	}
	id, err := hex.DecodeString(leaseHex)
	if err != nil || len(id) != len(lease) || hex.EncodeToString(id) != leaseHex {
		return 0, RawLeaseID{}, false, false
	}
	copy(lease[:], id)
	return pos, lease, true, true
}

// rawDocument is a webhook payload that is a JSON object. Its members are
// read once, for the envelope and for every projection.
type rawDocument struct {
	body    []byte
	members map[string]json.RawMessage
}

func parseRawDocument(body []byte) (rawDocument, bool) {
	var members map[string]json.RawMessage
	if err := json.Unmarshal(body, &members); err != nil || members == nil {
		return rawDocument{}, false
	}
	return rawDocument{body: body, members: members}, true
}

func (d rawDocument) envelope() (repoID int64, action string, ok bool) {
	var repository map[string]json.RawMessage
	if json.Unmarshal(d.members["repository"], &repository) != nil ||
		json.Unmarshal(repository["id"], &repoID) != nil || repoID <= 0 {
		return 0, "", false
	}
	if json.Unmarshal(d.members["action"], &action) != nil {
		action = ""
	}
	return repoID, action, true
}

// project copies each selected value as the payload's own bytes, because a
// decoded and encoded value could differ in escapes and number spellings.
func (d rawDocument) project(fields []string) []byte {
	out, selected := appendProjectedObject(nil, d.members, newFieldTrie(fields))
	if !selected {
		return []byte("{}")
	}
	return out
}

func projectFields(body []byte, fields []string) ([]byte, error) {
	if len(fields) == 0 {
		return nil, xerrors.New("a projection needs at least one field path")
	}
	doc, ok := parseRawDocument(body)
	if !ok {
		return nil, xerrors.New("the body is not a JSON object")
	}
	return doc.project(fields), nil
}

// fieldTrie holds field paths by member name. A name that maps to nil ends a
// path, which selects the whole value.
type fieldTrie map[string]fieldTrie

// newFieldTrie keeps the shorter of two paths when one is a prefix of the
// other, because the shorter one already selects the whole value.
func newFieldTrie(fields []string) fieldTrie {
	root := fieldTrie{}
	for _, path := range fields {
		node := root
		segments := strings.Split(path, ".")
		for i, name := range segments {
			if i == len(segments)-1 {
				node[name] = nil
				break
			}
			child, seen := node[name]
			if seen && child == nil {
				break
			}
			if !seen {
				child = fieldTrie{}
				node[name] = child
			}
			node = child
		}
	}
	return root
}

func appendProjectedObject(out []byte, members map[string]json.RawMessage, trie fieldTrie) ([]byte, bool) {
	start := len(out)
	out = append(out, '{')
	selected := false
	for _, name := range slices.Sorted(maps.Keys(trie)) {
		value, ok := members[name]
		if !ok {
			continue
		}
		mark := len(out)
		if selected {
			out = append(out, ',')
		}
		out = append(append(out, marshalRaw(name)...), ':')
		if sub := trie[name]; sub == nil {
			out = append(out, value...)
		} else if out, ok = appendProjectedValue(out, value, sub); !ok {
			out = out[:mark]
			continue
		}
		selected = true
	}
	if !selected {
		return out[:start], false
	}
	return append(out, '}'), true
}

// appendProjectedValue keeps an element of an array that is an object as {}
// when the paths select nothing in it, so the elements keep their positions.
func appendProjectedValue(out []byte, value json.RawMessage, trie fieldTrie) ([]byte, bool) {
	switch {
	case bytes.HasPrefix(value, []byte("{")):
		var members map[string]json.RawMessage
		if json.Unmarshal(value, &members) != nil {
			return out, false
		}
		return appendProjectedObject(out, members, trie)
	case bytes.HasPrefix(value, []byte("[")):
		var elements []json.RawMessage
		if json.Unmarshal(value, &elements) != nil {
			return out, false
		}
		start := len(out)
		out = append(out, '[')
		kept := 0
		for _, element := range elements {
			var members map[string]json.RawMessage
			if !bytes.HasPrefix(element, []byte("{")) || json.Unmarshal(element, &members) != nil {
				continue
			}
			if kept > 0 {
				out = append(out, ',')
			}
			var selected bool
			if out, selected = appendProjectedObject(out, members, trie); !selected {
				out = append(out, "{}"...)
			}
			kept++
		}
		if kept == 0 {
			return out[:start], false
		}
		return append(out, ']'), true
	}
	return out, false
}
