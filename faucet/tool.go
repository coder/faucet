package faucet

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/xerrors"

	"cdr.dev/slog/v3"
)

const (
	toolName = "wait_for_github_events"

	defaultWaitSeconds = 45
	maxWaitSeconds     = 50
	pollInterval       = 2 * time.Second
	// shaLookback is how far back a wait with a sha and no cursor reads, so an
	// event that landed just before the agent asked is not missed.
	shaLookback = 10 * time.Minute
	pageLimit   = 100
)

var shaPattern = regexp.MustCompile(`^[0-9A-Fa-f]{40}$`)

var (
	errAuthRequired = xerrors.New("authentication required")
	errStore        = xerrors.New("could not read events, try again")
	errVerify       = xerrors.New("could not verify repository access, try again")
)

type waitInput struct {
	targetArguments
	Types    []string `json:"types" jsonschema:"the event names to wait for, at least one: a family such as pr.activity or an exact type such as pr.closed"`
	Actions  []string `json:"actions,omitempty" jsonschema:"only events of these GitHub actions, such as closed; each narrows the families in types that have it, and a family that has none of them stays whole; each must be an action of one of the families"`
	SHA      string   `json:"sha,omitempty" jsonschema:"only events for this 40 character head commit SHA"`
	PR       int32    `json:"pr,omitempty" jsonschema:"only events that mention this pull request number"`
	Cursor   *int64   `json:"cursor,omitempty" jsonschema:"return events after this cursor, which is the cursor of an earlier result"`
	TimeoutS *int     `json:"timeout_s,omitempty" jsonschema:"seconds to wait for an event, from 1 to 50, default 45"`
	fieldArguments
}

type waitEvent struct {
	ID      int64   `json:"id"`
	Type    string  `json:"type"`
	RepoID  int64   `json:"repo_id"`
	PRs     []int32 `json:"prs"`
	HeadSHA string  `json:"head_sha"`
	// Data holds a json.RawMessage. The field is typed any because the SDK
	// infers an array schema for json.RawMessage and rejects the object.
	Data       any       `json:"data"`
	ReceivedAt time.Time `json:"received_at"`
}

type waitOutput struct {
	Events []waitEvent `json:"events"`
	Cursor int64       `json:"cursor"`
	// Truncated is true when a retention purge deleted events after the
	// cursor that the caller passed, so events may be missing.
	Truncated bool `json:"truncated"`
}

// Register adds the wait_for_github_events tool and the events/list and
// events/poll methods of the MCP Events extension to srv. Build srv with
// ServerCapabilities so that clients learn the server speaks Events.
//
// The tool returns the events of one repository that match its filters, as
// soon as there are any, or none when timeout_s runs out. A service client
// may name every repository of an owner, or every repository, instead. Either
// way it returns a cursor, and passing that cursor to the next call resumes
// exactly where this one stopped, as long as the target and the filters stay
// the same: the cursor is the newest event of any repository, so an event at
// or below it that matches only a new filter is never returned. Without a
// cursor it starts at the present, or ten minutes back when sha is set.
func (s *Service) Register(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        toolName,
		InputSchema: waitInputSchema(),
		Description: "Wait for GitHub events on a repository, such as a CI run finishing or a pull request review, " +
			"and return them as soon as they arrive. Name the repository with repo; a service client may name owner or all instead, and narrow them with repos. " +
			"Returns no events and a cursor when timeout_s runs out; " +
			"call again with that cursor and the same target and filters to keep waiting. When truncated is true, a retention purge deleted events after " +
			"the cursor you passed, so some may be missing. Event names: " + strings.Join(catalogNames(), ", ") + ". " +
			"A family such as pr.activity covers every action of one GitHub event. Each event's type is its exact type, such as pr.closed. " +
			"actions and the field filters narrow only the families in types that offer them, and leave the other names whole; " +
			"a filter that no name in types offers is an error. The field filters are named after the data field they match: " + fieldFiltersOffered() + ".",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, Title: "Wait for GitHub events"},
	}, s.waitForEvents)
	s.registerEvents(srv)
}

type waitRequest struct {
	query   PollQuery
	cursor  *int64
	timeout time.Duration
}

// waitInputSchema is the schema that the SDK infers from waitInput, with all
// held to true, repos to one repository at least and every list to
// maxListItems, as the inputSchema of every event name holds them.
func waitInputSchema() *jsonschema.Schema {
	schema, err := jsonschema.For[waitInput](nil)
	if err != nil {
		panic(fmt.Sprintf("infer the input schema of %s: %v", toolName, err))
	}
	schema.Properties["all"].Const = jsonschema.Ptr[any](true)
	schema.Properties["repos"].MinItems = jsonschema.Ptr(1)
	for _, property := range schema.Properties {
		if property.Type == "array" || slices.Contains(property.Types, "array") {
			property.MaxItems = jsonschema.Ptr(maxListItems)
		}
	}
	return schema
}

func (in waitInput) parse() (waitRequest, error) {
	if err := in.check(); err != nil {
		return waitRequest{}, err
	}
	names, actions := sortedSet(in.Types), sortedSet(in.Actions)
	if len(names) == 0 {
		return waitRequest{}, xerrors.Errorf("types must name at least one of: %s", strings.Join(catalogNames(), ", "))
	}
	entries := make([]*catalogEntry, len(names))
	for i, name := range names {
		entry, ok := lookupEntry(name)
		if !ok {
			return waitRequest{}, xerrors.Errorf("unknown type %q, the types are: %s", name, strings.Join(catalogNames(), ", "))
		}
		entries[i] = entry
	}
	for _, action := range actions {
		if !slices.ContainsFunc(entries, func(e *catalogEntry) bool { return e.hasAction(action) }) {
			return waitRequest{}, xerrors.Errorf("action %q is not an action of any family in types", action)
		}
	}
	entryTypes := make([][]string, len(entries))
	var types []string
	for i, entry := range entries {
		entryTypes[i] = entry.types(actions)
		types = append(types, entryTypes[i]...)
	}
	values := in.byName()
	for _, f := range fieldFilters {
		if len(values[f.name]) > 0 && !slices.ContainsFunc(entries, func(e *catalogEntry) bool { return e.offers(f.name) }) {
			return waitRequest{}, xerrors.Errorf("%s is a filter of none of the names in types, only of %s", f.name, strings.Join(familiesOffering(f.name), ", "))
		}
	}
	if in.SHA != "" && !shaPattern.MatchString(in.SHA) {
		return waitRequest{}, xerrors.New("sha must be 40 hexadecimal characters")
	}
	if in.SHA != "" && !slices.ContainsFunc(types, func(t string) bool { return !storedTypes[t].family.noHeadSHA }) {
		return waitRequest{}, xerrors.New("sha matches none of these types, because they carry no head commit; filter by pr")
	}
	if in.PR < 0 {
		return waitRequest{}, xerrors.New("pr must be a pull request number")
	}
	if in.PR != 0 && !slices.ContainsFunc(types, func(t string) bool { return !storedTypes[t].family.noPRs }) {
		return waitRequest{}, xerrors.New("pr matches none of these types, because GitHub names no pull request in them; filter by sha")
	}
	if in.Cursor != nil && *in.Cursor < 0 {
		return waitRequest{}, xerrors.New("cursor must not be negative")
	}
	seconds := defaultWaitSeconds
	if in.TimeoutS != nil {
		seconds = *in.TimeoutS
	}
	if seconds < 1 || seconds > maxWaitSeconds {
		return waitRequest{}, xerrors.Errorf("timeout_s must be from 1 to %d", maxWaitSeconds)
	}

	slices.Sort(types)
	return waitRequest{
		query: PollQuery{
			Types:     slices.Compact(types),
			HeadSHA:   strings.ToLower(in.SHA),
			PR:        in.PR,
			Fields:    in.fieldFilters(func(name string) []string { return narrowedTypes(name, entries, entryTypes) }),
			RepoNames: in.repoNames(),
			Limit:     pageLimit,
		},
		cursor:  in.Cursor,
		timeout: time.Duration(seconds) * time.Second,
	}, nil
}

func (s *Service) waitForEvents(ctx context.Context, _ *mcp.CallToolRequest, in waitInput) (*mcp.CallToolResult, waitOutput, error) {
	req, err := in.parse()
	if err != nil {
		return nil, waitOutput{}, err
	}
	logger := s.logger.With(in.logField())

	p, err := s.principal(ctx, logger, "wait", ScopeEventsRead)
	if err != nil {
		return nil, waitOutput{}, err
	}
	req.query.Target, err = s.authorizeTarget(ctx, logger, p, "wait", in.targetArguments)
	if err != nil {
		return nil, waitOutput{}, err
	}

	release, ok := s.waits.acquire(p.Login)
	if !ok {
		logger.Info(ctx, "wait refused, too many open", slog.F("login", p.Login))
		return nil, waitOutput{}, errTooManyWaits
	}
	defer release()

	req.query.After, err = s.startCursor(ctx, req)
	if err != nil {
		return nil, waitOutput{}, storeError(ctx, logger, err)
	}
	page, truncated, err := s.poll(ctx, req)
	if err != nil {
		return nil, waitOutput{}, storeError(ctx, logger, err)
	}

	out := waitOutput{Events: make([]waitEvent, len(page.Events)), Cursor: page.Next, Truncated: truncated}
	for i, ev := range page.Events {
		if out.Events[i], err = toWaitEvent(ev, req.query.Target); err != nil {
			return nil, waitOutput{}, storeError(ctx, logger, err)
		}
	}
	return nil, out, nil
}

// principal returns the caller of this request when it holds scope, the
// scope of op, before any repository is looked up.
func (s *Service) principal(ctx context.Context, logger slog.Logger, op string, scope Scope) (Principal, error) {
	p, err := s.principalFrom(ctx)
	switch {
	case err != nil:
		logger.Info(ctx, op+" without a principal", slog.Error(err))
		return Principal{}, errAuthRequired
	case !p.Authenticated():
		logger.Info(ctx, op+" by a principal of no kind")
		return Principal{}, errAuthRequired
	case !p.HasScope(scope):
		logger.Info(ctx, op+" denied, scope missing", slog.F("login", p.Login), slog.F("scope", scope))
		return Principal{}, xerrors.Errorf("the token does not grant the %s scope", scope)
	}
	return p, nil
}

var (
	errServiceTargets = xerrors.Errorf("owner and all targets are for service clients; name one repository with repo: %w", ErrNoAccess)
	errNoGrants       = xerrors.Errorf("this server reads no owner or all target: %w", ErrNoAccess)
)

// authorizeTarget returns the target that a names when p may read it. A
// repository is the Authorizer's to answer, and an owner or every repository
// is the ServiceGrants', for a service only. It may answer from what they
// remember.
func (s *Service) authorizeTarget(ctx context.Context, logger slog.Logger, p Principal, op string, a targetArguments) (Target, error) {
	return s.checkTarget(ctx, logger, p, op, a, s.authorizer.RepoID, ServiceGrants.OwnerID, ServiceGrants.CoversAll)
}

// authorizeFreshTarget is authorizeTarget that asks the source of the answer
// again, as every subscribe does, so a renewal checks access again.
func (s *Service) authorizeFreshTarget(ctx context.Context, logger slog.Logger, p Principal, op string, a targetArguments) (Target, error) {
	return s.checkTarget(ctx, logger, p, op, a, s.authorizer.RepoIDFresh, ServiceGrants.OwnerIDFresh, ServiceGrants.CoversAllFresh)
}

func (s *Service) checkTarget(ctx context.Context, logger slog.Logger, p Principal, op string, a targetArguments,
	repoID func(context.Context, Principal, string) (int64, error),
	ownerID func(ServiceGrants, context.Context, Principal, string) (int64, error),
	coversAll func(ServiceGrants, context.Context, Principal) error,
) (Target, error) {
	logger = logger.With(slog.F("login", p.Login))
	if a.Repo != "" {
		id, err := repoID(ctx, p, a.Repo)
		if err != nil {
			return Target{}, denied(ctx, logger, op, err)
		}
		if id <= 0 {
			logger.Error(ctx, "authorizer returned a repository without an ID")
			return Target{}, errVerify
		}
		return RepoTarget(id), nil
	}
	if _, ok := p.ServiceClient(); !ok {
		return Target{}, denied(ctx, logger, op, errServiceTargets)
	}
	if s.grants == nil {
		return Target{}, denied(ctx, logger, op, errNoGrants)
	}
	if a.Owner != "" {
		id, err := ownerID(s.grants, ctx, p, a.Owner)
		if err != nil {
			return Target{}, denied(ctx, logger, op, err)
		}
		if id <= 0 {
			logger.Error(ctx, "service grants returned an owner without an ID")
			return Target{}, errVerify
		}
		return OwnerTarget(id), nil
	}
	if err := coversAll(s.grants, ctx, p); err != nil {
		return Target{}, denied(ctx, logger, op, err)
	}
	return AllTarget(), nil
}

// denied logs why an access check failed and returns the error that the
// caller gets.
func denied(ctx context.Context, logger slog.Logger, op string, err error) error {
	switch {
	// A denial logs its reason as one line of text: slog.Error would add the
	// error's stack frames to every routine denial.
	case xerrors.Is(err, ErrNoAccess):
		logger.Info(ctx, op+" denied", slog.F("reason", err.Error()))
		return err
	case xerrors.Is(err, ErrInvalidToken):
		logger.Info(ctx, op+" denied, token rejected")
		return ErrInvalidToken
	default:
		logger.Warn(ctx, "could not verify repository access", slog.Error(err))
		return errVerify
	}
}

// startCursor picks where a wait begins. A given cursor wins. With a sha the
// wait begins shaLookback ago. Otherwise it begins at the present, which is
// the highest ID received before now. If the store's clock is ahead of ours,
// an event from just before the call can still carry a time after now, and the
// wait returns it. It cannot skip one that arrives later, because later events
// get higher IDs than any that exist now.
func (s *Service) startCursor(ctx context.Context, req waitRequest) (int64, error) {
	switch {
	case req.cursor != nil:
		return *req.cursor, nil
	case req.query.HeadSHA != "":
		return s.store.CursorAt(ctx, s.clock.Now().Add(-shaLookback))
	default:
		return s.store.CursorAt(ctx, s.clock.Now())
	}
}

// poll reads until a page has events or the deadline passes. truncated is true
// when any read found that a purge had deleted events above its start.
func (s *Service) poll(ctx context.Context, req waitRequest) (PollPage, bool, error) {
	deadline := s.clock.Now().Add(req.timeout)
	q := req.query
	truncated := false
	for {
		q.Through = windowThrough(q.After)
		page, err := s.store.PollEvents(ctx, q)
		if err != nil {
			return PollPage{}, false, err
		}
		truncated = truncated || crossedPurge(q.After, page)
		if len(page.Events) > 0 {
			return page, truncated, nil
		}
		windowEnded := q.Through > 0 && page.Next == q.Through
		q.After = page.Next

		remaining := s.clock.Until(deadline)
		if remaining <= 0 {
			return page, truncated, nil
		}
		if windowEnded {
			if err := ctx.Err(); err != nil {
				return PollPage{}, false, err
			}
			continue
		}
		timer := s.clock.NewTimer(min(pollInterval, remaining))
		select {
		case <-ctx.Done():
			timer.Stop()
			return PollPage{}, false, ctx.Err()
		case <-timer.C:
		}
	}
}

// narrowedTypes are the stored types that a field filter narrows: those of
// the names in types that offer it, less those that another name matches
// whole, such as pr.closed beside pr.activity.
func narrowedTypes(filter string, entries []*catalogEntry, entryTypes [][]string) []string {
	var narrowed, whole []string
	for i, entry := range entries {
		if entry.offers(filter) {
			narrowed = append(narrowed, entryTypes[i]...)
		} else {
			whole = append(whole, entryTypes[i]...)
		}
	}
	return slices.DeleteFunc(narrowed, func(t string) bool { return slices.Contains(whole, t) })
}

func fieldFiltersOffered() string {
	described := make([]string, len(fieldFilters))
	for i, f := range fieldFilters {
		described[i] = f.name + " (" + strings.Join(familiesOffering(f.name), ", ") + ")"
	}
	return strings.Join(described, ", ")
}

func storeError(ctx context.Context, logger slog.Logger, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	logger.Error(ctx, "read events", slog.Error(err))
	return errStore
}

// toWaitEvent returns ev as the wait tool returns it. Read through target, an
// owner or all target, its data names the repository, as withRepository
// does.
func toWaitEvent(ev Event, target Target) (waitEvent, error) {
	prs := ev.PRs
	if prs == nil {
		prs = []int32{}
	}
	data := ev.Data
	if target.Kind() != TargetRepo {
		var members map[string]json.RawMessage
		if err := json.Unmarshal(ev.Data, &members); err != nil || members == nil {
			return waitEvent{}, xerrors.Errorf("data of event %d is not a JSON object", ev.ID)
		}
		withRepository(members, ev, target)
		data = marshalRaw(members)
	}
	return waitEvent{
		ID:         ev.ID,
		Type:       ev.Type,
		RepoID:     ev.RepoID,
		PRs:        prs,
		HeadSHA:    ev.HeadSHA,
		Data:       data,
		ReceivedAt: ev.ReceivedAt,
	}, nil
}
