package faucet

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/xerrors"

	"cdr.dev/slog/v3"
)

const (
	// EventsExtension identifies the MCP Events extension (SEP-3415) in the
	// capabilities of server/discover.
	EventsExtension = "io.modelcontextprotocol/events"

	methodEventsList = "events/list"
	methodEventsPoll = "events/poll"

	deliveryPoll   = "poll"
	resultComplete = "complete"

	recommendedPollMs = 5000
	maxAgeLimitMs     = math.MaxInt64 / int64(time.Millisecond)

	codeNotFound  = -32023
	codeForbidden = -32024
)

// ServerCapabilities returns the capabilities of an MCP server that serves
// the Events extension. Pass it as ServerOptions.Capabilities to mcp.NewServer
// before Service.Register, so that server/discover advertises events. Setting
// Capabilities replaces the SDK default, which is the deprecated logging
// capability, so add anything else the server offers to the result.
func ServerCapabilities() *mcp.ServerCapabilities {
	caps := &mcp.ServerCapabilities{}
	caps.AddExtension(EventsExtension, map[string]any{"listChanged": false})
	return caps
}

type eventsListParams struct {
	mcp.ParamsBase
	Cursor string `json:"cursor,omitempty"`
}

type eventDescriptor struct {
	Name          string          `json:"name"`
	Description   string          `json:"description"`
	Delivery      []string        `json:"delivery"`
	InputSchema   json.RawMessage `json:"inputSchema"`
	PayloadSchema json.RawMessage `json:"payloadSchema"`
}

type eventsListResult struct {
	mcp.ResultBase
	ResultType string            `json:"resultType"`
	Events     []eventDescriptor `json:"events"`
}

type eventsPollParams struct {
	mcp.ParamsBase
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
	Cursor    *string         `json:"cursor,omitempty"`
	MaxAgeMs  *int64          `json:"maxAgeMs,omitempty"`
	MaxEvents *int32          `json:"maxEvents,omitempty"`
}

type eventOccurrence struct {
	EventID   string          `json:"eventId"`
	Name      string          `json:"name"`
	Timestamp time.Time       `json:"timestamp"`
	Data      json.RawMessage `json:"data"`
}

type eventsPollResult struct {
	mcp.ResultBase
	ResultType string            `json:"resultType"`
	Events     []eventOccurrence `json:"events"`
	Cursor     string            `json:"cursor"`
	Truncated  bool              `json:"truncated"`
	HasMore    bool              `json:"hasMore"`
	NextPollMs int               `json:"nextPollMs"`
}

func describeEvents(delivery []string) []eventDescriptor {
	descriptors := make([]eventDescriptor, len(catalog))
	for i, e := range catalog {
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

func (s *Service) registerEvents(srv *mcp.Server) {
	registered := func(method string, err error) {
		if err != nil {
			panic(fmt.Sprintf("register %s: %v", method, err))
		}
	}
	registered(methodEventsList, mcp.AddReceivingCustomMethod(srv, methodEventsList, s.listEvents))
	registered(methodEventsPoll, mcp.AddReceivingCustomMethod(srv, methodEventsPoll, s.pollEventsMethod))
	registered(methodEventsSubscribe, mcp.AddReceivingCustomMethod(srv, methodEventsSubscribe, s.subscribeMethod))
	registered(methodEventsUnsubscribe, mcp.AddReceivingCustomMethod(srv, methodEventsUnsubscribe, s.unsubscribeMethod))
}

func (s *Service) listEvents(_ context.Context, _ *mcp.ServerSession, p *eventsListParams) (*eventsListResult, error) {
	if p != nil && p.Cursor != "" {
		return nil, invalidParams("cursor is not valid, the event types fit one page")
	}
	return &eventsListResult{ResultType: resultComplete, Events: s.listed}, nil
}

type pollRequest struct {
	entry  *catalogEntry
	target targetArguments
	query  PollQuery
	cursor *cursor
	maxAge time.Duration
	limit  int32
}

func parsePoll(p *eventsPollParams) (pollRequest, error) {
	if p == nil || p.Name == "" {
		return pollRequest{}, invalidParams("name is required")
	}
	entry, err := eventByName(p.Name)
	if err != nil {
		return pollRequest{}, err
	}
	args, err := parsePollArguments(entry.input, p.Arguments)
	if err != nil {
		return pollRequest{}, err
	}
	types := entry.types(args.Actions)
	req := pollRequest{
		entry:  entry,
		target: args.targetArguments,
		query: PollQuery{EventFilter: EventFilter{
			Types:     types,
			HeadSHA:   strings.ToLower(args.SHA),
			PR:        args.PR,
			Fields:    args.fieldFilters(func(string) []string { return types }),
			RepoNames: args.repoNames(),
		}},
		limit: pageLimit,
	}
	if p.Cursor != nil {
		c, ok := decodeCursor(*p.Cursor)
		if !ok {
			return pollRequest{}, invalidParams("cursor is not one this server returned")
		}
		req.cursor = &c
	}
	if p.MaxEvents != nil {
		if *p.MaxEvents < 1 {
			return pollRequest{}, invalidParams("maxEvents must be positive")
		}
		req.limit = min(*p.MaxEvents, pageLimit)
	}
	if p.MaxAgeMs != nil {
		if *p.MaxAgeMs < 1 {
			return pollRequest{}, invalidParams("maxAgeMs must be positive")
		}
		req.maxAge = time.Duration(min(*p.MaxAgeMs, maxAgeLimitMs)) * time.Millisecond
	}
	return req, nil
}

func eventByName(name string) (*catalogEntry, error) {
	entry, ok := lookupEntry(name)
	if !ok {
		return nil, rpcError(codeNotFound,
			fmt.Sprintf("unknown event type %q, the types are: %s", name, strings.Join(catalogNames(), ", ")),
			map[string]string{"kind": "event"})
	}
	return entry, nil
}

func parsePollArguments(input *jsonschema.Resolved, raw json.RawMessage) (pollArguments, error) {
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		raw = json.RawMessage("{}")
	}
	var generic any
	if err := json.Unmarshal(raw, &generic); err != nil {
		return pollArguments{}, invalidParams("arguments are not valid JSON")
	}
	if err := input.Validate(generic); err != nil {
		return pollArguments{}, invalidParams("arguments do not match the inputSchema: %v", err)
	}
	var args pollArguments
	if err := json.Unmarshal(raw, &args); err != nil {
		return pollArguments{}, invalidParams("arguments do not match the inputSchema: %v", err)
	}
	if err := args.check(); err != nil {
		return pollArguments{}, invalidParams("%v", err)
	}
	return args, nil
}

// readWindow is how many positions of the target's line, or event IDs for the
// delivery scan, one read of events looks at, so a statement costs about the
// same however far behind its cursor is and however few events match. A read
// that stops short of the head says so, and the caller goes on from where it
// stopped.
const readWindow = 50_000

// windowThrough is the Through of a read of one window from after.
func windowThrough[N ~int64](after N) N {
	if after > math.MaxInt64-readWindow {
		return 0
	}
	return after + readWindow
}

// crossedPurge reports whether a read from after reached the purge mark of
// the line it read, which deleted events that the read did not get. Only the
// read that reaches the mark says so, so a purge that spans several windows is
// reported once.
func crossedPurge(after Position, page PollPage) bool {
	return after < page.PurgedThrough && page.Next >= page.PurgedThrough
}

// startPoll takes one of the login's places for a poll, so that one login
// cannot hold every connection of the store.
func (s *Service) startPoll(ctx context.Context, logger slog.Logger, p Principal) (release func(), err error) {
	release, ok := s.polls.acquire(p.Login)
	if !ok {
		logger.Info(ctx, "poll refused, too many running", slog.F("login", p.Login))
		return nil, rpcError(codeResourceExhausted, errTooManyPolls.Error(), map[string]any{"limit": "polls", "max": s.polls.limit})
	}
	return release, nil
}

func (s *Service) pollEventsMethod(ctx context.Context, _ *mcp.ServerSession, p *eventsPollParams) (*eventsPollResult, error) {
	if p != nil && p.Name != "" && len(s.rawOffered) > 0 {
		if entry, ok := s.rawEntry(p.Name); ok {
			return s.pollRaw(ctx, entry, p)
		}
		if _, typed := lookupEntry(p.Name); !typed {
			return nil, s.unknownEvent(p.Name)
		}
	}
	req, err := parsePoll(p)
	if err != nil {
		return nil, err
	}
	logger := s.logger.With(req.target.logField(), slog.F("event", req.entry.name))

	principal, err := s.principal(ctx, logger, "poll", ScopeEventsRead)
	if err != nil {
		return nil, accessError(err)
	}
	target, err := s.authorizeTarget(ctx, logger, principal, "poll", req.target)
	if err != nil {
		return nil, accessError(err)
	}
	req.query.Target = target
	release, err := s.startPoll(ctx, logger, principal)
	if err != nil {
		return nil, err
	}
	defer release()

	res, err := s.readEvents(ctx, req)
	if err != nil {
		return nil, readError(storeError(ctx, logger, err))
	}
	return res, nil
}

func (s *Service) readEvents(ctx context.Context, req pollRequest) (*eventsPollResult, error) {
	target := req.query.Target
	if req.cursor == nil || req.cursor.legacy {
		head, err := s.store.PositionAt(ctx, target, s.clock.Now())
		if err != nil {
			return nil, err
		}
		return newPollResult(nil, head, req.cursor != nil, false), nil
	}
	after, truncated, err := s.replayStart(ctx, target, req.cursor.position, req.maxAge)
	if err != nil {
		return nil, err
	}
	q := req.query
	q.After = after
	q.Through = windowThrough(after)
	q.Limit = req.limit + 1
	page, err := s.store.PollEvents(ctx, q)
	if err != nil {
		return nil, err
	}
	truncated = truncated || crossedPurge(after, page)
	events, cursor, hasMore := page.Events, page.Next, q.Through > 0 && page.Next == q.Through
	if len(events) > int(req.limit) {
		events = events[:req.limit]
		cursor = target.PositionOf(events[len(events)-1])
		hasMore = true
	}
	occurrences := make([]eventOccurrence, len(events))
	for i, ev := range events {
		if occurrences[i], err = occurrenceOf(req.entry.name, ev, target); err != nil {
			return nil, err
		}
	}
	return newPollResult(occurrences, cursor, truncated, hasMore), nil
}

func (s *Service) replayStart(ctx context.Context, target Target, cursor Position, maxAge time.Duration) (after Position, truncated bool, err error) {
	if maxAge == 0 {
		return cursor, false, nil
	}
	floor, err := s.store.PositionAt(ctx, target, s.clock.Now().Add(-maxAge))
	if err != nil {
		return 0, false, err
	}
	if cursor < floor {
		return floor, true, nil
	}
	return cursor, false, nil
}

func newPollResult(events []eventOccurrence, cursor Position, truncated, hasMore bool) *eventsPollResult {
	if events == nil {
		events = []eventOccurrence{}
	}
	return &eventsPollResult{
		ResultType: resultComplete,
		Events:     events,
		Cursor:     encodeCursor(cursor),
		Truncated:  truncated,
		HasMore:    hasMore,
		NextPollMs: recommendedPollMs,
	}
}

// occurrenceOf names the event by the name it was asked for. It adds the pull
// request numbers, the head SHA, the stored type and its action to the data,
// because the store keeps them in columns and the payload schemas list them.
// Read through target, an owner or all target, the data also names the
// repository, as withRepository does.
func occurrenceOf(name string, ev Event, target Target) (eventOccurrence, error) {
	var data map[string]json.RawMessage
	if err := json.Unmarshal(ev.Data, &data); err != nil || data == nil {
		return eventOccurrence{}, xerrors.Errorf("data of event %d is not a JSON object", ev.ID)
	}
	withRepository(data, ev, target)
	prs := ev.PRs
	if prs == nil {
		prs = []int32{}
	}
	data["prs"] = marshalRaw(prs)
	if ev.HeadSHA != "" {
		data["head_sha"] = marshalRaw(ev.HeadSHA)
	}
	data["event"] = marshalRaw(ev.Type)
	// A type that this build does not know comes from a newer build during a
	// rolling deploy, through the stored types of a subscription it renewed.
	if t, ok := storedTypes[ev.Type]; ok {
		data["action"] = marshalRaw(t.action)
	}
	return eventOccurrence{
		EventID:   ev.DeliveryID,
		Name:      name,
		Timestamp: ev.ReceivedAt.UTC(),
		Data:      marshalRaw(data),
	}, nil
}

// withRepository adds repo_id and repo, the full name that the delivery
// carried, to the data of an event read through an owner or all target,
// which can be of any of several repositories while the data of some types,
// such as a check suite's, names none. A repo target's data is as before.
func withRepository(data map[string]json.RawMessage, ev Event, target Target) {
	if target.Kind() == TargetRepo {
		return
	}
	data["repo_id"] = marshalRaw(ev.RepoID)
	if ev.RepoName != "" {
		data["repo"] = marshalRaw(ev.RepoName)
	}
}

func marshalRaw(v any) json.RawMessage {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("marshal %T: %v", v, err))
	}
	return raw
}

func rpcError(code int64, message string, data any) error {
	e := &jsonrpc.Error{Code: code, Message: message}
	if data != nil {
		e.Data = marshalRaw(data)
	}
	return e
}

func invalidParams(format string, args ...any) error {
	return rpcError(jsonrpc.CodeInvalidParams, fmt.Sprintf(format, args...), nil)
}

// accessError gives a missing repository and an unreadable one the same
// Forbidden answer, so a caller cannot tell which repositories exist.
func accessError(err error) error {
	if xerrors.Is(err, errVerify) {
		return rpcError(jsonrpc.CodeInternalError, err.Error(), nil)
	}
	return rpcError(codeForbidden, err.Error(), nil)
}

func readError(err error) error {
	if xerrors.Is(err, errStore) {
		return rpcError(jsonrpc.CodeInternalError, err.Error(), nil)
	}
	return err
}
