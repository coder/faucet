package faucet

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
	"golang.org/x/xerrors"
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

// rawEvents are the GitHub webhook events that no typed family stores in
// full and that a host may offer raw, sorted by event. issue_comment is here
// because pr.comments stores only the comments on pull requests. Each payload
// names a repository, or for some events of an organization, such as
// project, project_card, project_column, package and registry_package, only
// the organization: such a delivery reaches an owner or all interest and no
// repository's.
var rawEvents = []rawEvent{
	{"commit_comment", "Comments that were posted on a commit."},
	{"create", "Git branches and tags that were created."},
	{"custom_property_values", "Changes to the values of the custom properties of a repository."},
	{"delete", "Git branches and tags that were deleted."},
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
	{"release", "Releases that were created, published, edited, deleted or changed in another way. A draft release is left out, because GitHub shows it only to people with push access."},
	{"repository", "Changes to a repository itself, for example one that was renamed, archived, transferred or made public."},
	{"repository_import", "Imports of a repository into GitHub that succeeded, were canceled or failed."},
	{"star", "Stars that were added to or removed from a repository."},
	{"sub_issues", "Sub-issues that were added to or removed from a parent issue."},
	{"watch", "Accounts that started to watch a repository."},
	{"workflow_dispatch", "GitHub Actions workflows that someone started by hand."},
}

// refusedRawEvents are the GitHub events whose payload GitHub shows only to
// people with more than read access to a repository: security alerts and
// advisories, deploy keys, collaborators, the teams with access, rulesets
// with the actors that may bypass them, protection and security settings,
// and the client payload of a repository dispatch, which its sender sets and
// no reader of the repository sees. Faucet checks only that a caller can
// read the repository, so no service offers them, whatever its host lists.
// Sorted.
var refusedRawEvents = []string{
	"branch_protection_configuration", "branch_protection_rule", "code_scanning_alert", "dependabot_alert",
	"deploy_key", "member", "repository_advisory", "repository_dispatch", "repository_ruleset",
	"repository_vulnerability_alert", "secret_scanning_alert", "secret_scanning_alert_location",
	"secret_scanning_scan", "security_and_analysis", "team_add",
}

// RawEventNames returns the GitHub events, such as "push", that a service can
// offer raw when its host lists them in Deps.RawEvents, sorted.
func RawEventNames() []string {
	names := make([]string, len(rawEvents))
	for i, e := range rawEvents {
		names[i] = e.event
	}
	return names
}

// RefusedRawEvents returns the GitHub events that no service offers raw,
// because GitHub shows their payload only to people with more than read
// access to a repository, sorted.
func RefusedRawEvents() []string {
	return slices.Clone(refusedRawEvents)
}

// CheckRawEvents returns an error for the first of events that a service
// cannot offer raw: a refused event, or one that RawEventNames lacks.
func CheckRawEvents(events []string) error {
	for _, event := range events {
		switch {
		case slices.Contains(refusedRawEvents, event):
			return xerrors.Errorf("%s is refused: GitHub shows its payload only to people with more than read access to a repository, and faucet checks only read access", event)
		case !slices.ContainsFunc(rawEvents, func(e rawEvent) bool { return e.event == event }):
			return xerrors.Errorf("%q is not a GitHub event that faucet can offer raw", event)
		}
	}
	return nil
}

// offeredRawEntries returns the entries of events in catalog order.
func offeredRawEntries(events []string) ([]*rawEntry, error) {
	if err := CheckRawEvents(events); err != nil {
		return nil, err
	}
	return slices.DeleteFunc(slices.Clone(rawCatalog), func(e *rawEntry) bool { return !slices.Contains(events, e.event) }), nil
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
	if !slices.IsSorted(refusedRawEvents) {
		panic("the refused raw events are not sorted")
	}
	for i, e := range rawEvents {
		if i > 0 && rawEvents[i-1].event >= e.event {
			panic(fmt.Sprintf("the raw events are not sorted or repeat one at %s", e.event))
		}
		if slices.Contains(refusedRawEvents, e.event) {
			panic(fmt.Sprintf("the raw event %s is refused", e.event))
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
	properties := targetProperties()
	properties["actions"] = map[string]any{
		"type":        "array",
		"minItems":    1,
		"maxItems":    maxListItems,
		"items":       map[string]any{"type": "string"},
		"description": "Only deliveries whose action is one of these. Without it, every action, and events that have none.",
	}
	properties["fields"] = map[string]any{
		"type":     "array",
		"minItems": 1,
		"maxItems": maxFieldPaths,
		"items":    map[string]any{"type": "string"},
		"description": fmt.Sprintf("Only these members of the payload, each a path of 1 to %d names of letters, digits, _ or - joined by dots and at most %d bytes long, such as commits.id, which keeps the id of each commit.",
			maxFieldPathSegments, maxFieldPathBytes),
	}
	return marshalRaw(map[string]any{"type": "object", "properties": properties, "additionalProperties": false})
}

// rawEntry returns the entry of a raw event name that the service offers.
func (s *Service) rawEntry(name string) (*rawEntry, bool) {
	i := slices.IndexFunc(s.rawOffered, func(e *rawEntry) bool { return e.name == name })
	if i < 0 {
		return nil, false
	}
	return s.rawOffered[i], true
}

// unknownEvent is the error for a name that is no event name of the service.
// It lists the raw names that the service offers after the typed ones.
func (s *Service) unknownEvent(name string) error {
	names := catalogNames()
	for _, e := range s.rawOffered {
		names = append(names, e.name)
	}
	return rpcError(codeNotFound,
		fmt.Sprintf("unknown event type %q, the types are: %s", name, strings.Join(names, ", ")),
		map[string]string{"kind": "event"})
}

func describeRawEvents(entries []*rawEntry, delivery []string) []eventDescriptor {
	descriptors := make([]eventDescriptor, len(entries))
	for i, e := range entries {
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
	targetArguments
	Actions []string `json:"actions,omitempty"`
	Fields  []string `json:"fields,omitempty"`
}

func parseRawArguments(entry *rawEntry, raw json.RawMessage) (rawArguments, error) {
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		raw = json.RawMessage("{}")
	}
	var generic any
	if err := json.Unmarshal(raw, &generic); err != nil {
		return rawArguments{}, invalidParams("arguments are not valid JSON")
	}
	if err := entry.input.Validate(generic); err != nil {
		return rawArguments{}, invalidParams("arguments do not match the inputSchema: %v", err)
	}
	var args rawArguments
	if err := json.Unmarshal(raw, &args); err != nil {
		return rawArguments{}, invalidParams("arguments do not match the inputSchema: %v", err)
	}
	if err := args.check(); err != nil {
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
	return rawArguments{targetArguments: a.targetArguments.normalized(), Actions: sortedSetOrNil(a.Actions), Fields: sortedSetOrNil(a.Fields)}
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

// rawDocument is a webhook payload that is a JSON object.
type rawDocument struct {
	body []byte
	root *rawValue
}

// rawValue is one JSON value of a payload. An object or an array is split into
// its members or elements the first time a projection reads into it, and kept
// split, so that the projections of one payload parse each value once.
type rawValue struct {
	raw      json.RawMessage
	split    bool
	members  map[string]*rawValue
	elements []*rawValue
}

func parseRawDocument(body []byte) (rawDocument, bool) {
	root := &rawValue{raw: bytes.TrimLeft(body, " \t\r\n")}
	if _, ok := root.object(); !ok {
		return rawDocument{}, false
	}
	return rawDocument{body: body, root: root}, true
}

// object returns the members of an object, and false for any other value.
func (v *rawValue) object() (map[string]*rawValue, bool) {
	if !bytes.HasPrefix(v.raw, []byte("{")) {
		return nil, false
	}
	if !v.split {
		v.split = true
		var members map[string]json.RawMessage
		if json.Unmarshal(v.raw, &members) == nil {
			v.members = make(map[string]*rawValue, len(members))
			for name, raw := range members {
				v.members[name] = &rawValue{raw: raw}
			}
		}
	}
	return v.members, v.members != nil
}

// array returns the elements of an array, and false for any other value.
func (v *rawValue) array() ([]*rawValue, bool) {
	if !bytes.HasPrefix(v.raw, []byte("[")) {
		return nil, false
	}
	if !v.split {
		v.split = true
		var elements []json.RawMessage
		if json.Unmarshal(v.raw, &elements) == nil {
			v.elements = make([]*rawValue, len(elements))
			for i, raw := range elements {
				v.elements[i] = &rawValue{raw: raw}
			}
		}
	}
	return v.elements, v.elements != nil
}

// envelope reads where a delivery comes from and its action into a delivery
// of event. A delivery that names a repository comes from it and its owner.
// One that names none, such as a project of an organization, comes from the
// organization it names. ok is false for a delivery that names neither, and
// for one whose repository has no ID. A full name that a store cannot hold
// is no name, which no filter of names matches.
func (d rawDocument) envelope(deliveryID, event string) (delivery RawDelivery, ok bool) {
	members, _ := d.root.object()
	member := func(name string) json.RawMessage {
		if v := members[name]; v != nil {
			return v.raw
		}
		return nil
	}
	delivery = RawDelivery{DeliveryID: deliveryID, Event: event}
	if json.Unmarshal(member("action"), &delivery.Action) != nil {
		delivery.Action = ""
	}
	if isAbsent(member("repository")) {
		var organization map[string]json.RawMessage
		if json.Unmarshal(member("organization"), &organization) != nil ||
			json.Unmarshal(organization["id"], &delivery.OwnerID) != nil || delivery.OwnerID <= 0 {
			return RawDelivery{}, false
		}
		return delivery, true
	}
	var repository map[string]json.RawMessage
	if json.Unmarshal(member("repository"), &repository) != nil ||
		json.Unmarshal(repository["id"], &delivery.RepoID) != nil || delivery.RepoID <= 0 {
		return RawDelivery{}, false
	}
	var owner map[string]json.RawMessage
	if json.Unmarshal(repository["owner"], &owner) != nil || json.Unmarshal(owner["id"], &delivery.OwnerID) != nil || delivery.OwnerID < 0 {
		delivery.OwnerID = 0
	}
	if json.Unmarshal(repository["full_name"], &delivery.RepoName) != nil || strings.ContainsRune(delivery.RepoName, 0) {
		delivery.RepoName = ""
	}
	return delivery, true
}

// readersSee reports whether everyone who can read the repository may see
// what a delivery of event carries. GitHub shows a draft release only to
// people with push access, and faucet checks only read access, so a release
// passes only when its payload says that it is no draft.
func (d rawDocument) readersSee(event string) bool {
	if event != "release" {
		return true
	}
	members, _ := d.root.object()
	release, ok := members["release"]
	if !ok {
		return false
	}
	fields, _ := release.object()
	draft, ok := fields["draft"]
	return ok && string(draft.raw) == "false"
}

// project copies each selected value as the payload's own bytes, because a
// decoded and encoded value could differ in escapes and number spellings.
func (d rawDocument) project(fields []string) []byte {
	out, _ := d.projectWithin(fields, math.MaxInt)
	return out
}

// projectWithin is project when the projection is at most limit bytes. A
// longer one is cut short, and fits is false.
func (d rawDocument) projectWithin(fields []string, limit int) (out []byte, fits bool) {
	members, _ := d.root.object()
	out, selected := appendProjectedObject(nil, members, newFieldTrie(fields), limit)
	if !selected {
		out = []byte("{}")
	}
	return out, len(out) <= limit
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

// appendProjectedObject and appendProjectedValue stop once out is longer than
// limit, because the caller then only needs to know that it is too long.
func appendProjectedObject(out []byte, members map[string]*rawValue, trie fieldTrie, limit int) ([]byte, bool) {
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
			out = append(out, value.raw...)
		} else if out, ok = appendProjectedValue(out, value, sub, limit); !ok {
			out = out[:mark]
			continue
		}
		selected = true
		if len(out) > limit {
			return out, true
		}
	}
	if !selected {
		return out[:start], false
	}
	return append(out, '}'), true
}

// appendProjectedValue keeps an element of an array that is an object as {}
// when the paths select nothing in it, so the elements keep their positions.
func appendProjectedValue(out []byte, value *rawValue, trie fieldTrie, limit int) ([]byte, bool) {
	if members, ok := value.object(); ok {
		return appendProjectedObject(out, members, trie, limit)
	}
	elements, ok := value.array()
	if !ok {
		return out, false
	}
	start := len(out)
	out = append(out, '[')
	kept := 0
	for _, element := range elements {
		members, ok := element.object()
		if !ok {
			continue
		}
		if kept > 0 {
			out = append(out, ',')
		}
		var selected bool
		if out, selected = appendProjectedObject(out, members, trie, limit); !selected {
			out = append(out, "{}"...)
		}
		kept++
		if len(out) > limit {
			return out, true
		}
	}
	if kept == 0 {
		return out[:start], false
	}
	return append(out, ']'), true
}
