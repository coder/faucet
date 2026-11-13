package faucet

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

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
	cursorPrefix   = "v1."

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

var listedEvents = describeEvents()

func describeEvents() []eventDescriptor {
	descriptors := make([]eventDescriptor, len(eventCatalog))
	for i, t := range eventCatalog {
		descriptors[i] = eventDescriptor{
			Name:          t.name,
			Description:   t.description,
			Delivery:      []string{deliveryPoll},
			InputSchema:   json.RawMessage(pollArgumentsSchema),
			PayloadSchema: t.payloadSchema,
		}
	}
	return descriptors
}

func (s *Service) registerEvents(srv *mcp.Server) {
	if err := mcp.AddReceivingCustomMethod(srv, methodEventsList, listEvents); err != nil {
		panic(fmt.Sprintf("register %s: %v", methodEventsList, err))
	}
	if err := mcp.AddReceivingCustomMethod(srv, methodEventsPoll, s.pollEventsMethod); err != nil {
		panic(fmt.Sprintf("register %s: %v", methodEventsPoll, err))
	}
}

func listEvents(_ context.Context, _ *mcp.ServerSession, p *eventsListParams) (*eventsListResult, error) {
	if p != nil && p.Cursor != "" {
		return nil, invalidParams("cursor is not valid, the event types fit one page")
	}
	return &eventsListResult{ResultType: resultComplete, Events: listedEvents}, nil
}

type pollRequest struct {
	eventType eventType
	repo      string
	query     PollQuery
	cursor    *int64
	maxAge    time.Duration
	limit     int32
}

func parsePoll(p *eventsPollParams) (pollRequest, error) {
	if p == nil || p.Name == "" {
		return pollRequest{}, invalidParams("name is required")
	}
	t, ok := lookupEventType(p.Name)
	if !ok {
		return pollRequest{}, rpcError(codeNotFound,
			fmt.Sprintf("unknown event type %q, the types are: %s", p.Name, strings.Join(eventTypes(), ", ")),
			map[string]string{"kind": "event"})
	}
	args, err := parsePollArguments(p.Arguments)
	if err != nil {
		return pollRequest{}, err
	}
	req := pollRequest{
		eventType: t,
		repo:      args.Repo,
		query:     PollQuery{Types: []string{t.name}, HeadSHA: strings.ToLower(args.SHA), PR: args.PR},
		limit:     pageLimit,
	}
	if p.Cursor != nil {
		id, ok := decodeCursor(*p.Cursor)
		if !ok {
			return pollRequest{}, invalidParams("cursor is not one this server returned")
		}
		req.cursor = &id
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

func parsePollArguments(raw json.RawMessage) (pollArguments, error) {
	var generic any = map[string]any{}
	if len(raw) > 0 && !bytes.Equal(raw, []byte("null")) {
		if err := json.Unmarshal(raw, &generic); err != nil {
			return pollArguments{}, invalidParams("arguments are not valid JSON")
		}
	}
	if err := pollArgumentsResolved.Validate(generic); err != nil {
		return pollArguments{}, invalidParams("arguments do not match the inputSchema: %v", err)
	}
	var args pollArguments
	if err := json.Unmarshal(raw, &args); err != nil {
		return pollArguments{}, invalidParams("arguments do not match the inputSchema: %v", err)
	}
	if _, _, err := parseRepo(args.Repo); err != nil {
		return pollArguments{}, invalidParams("%v", err)
	}
	return args, nil
}

func (s *Service) pollEventsMethod(ctx context.Context, _ *mcp.ServerSession, p *eventsPollParams) (*eventsPollResult, error) {
	req, err := parsePoll(p)
	if err != nil {
		return nil, err
	}
	logger := s.logger.With(slog.F("repo", req.repo), slog.F("event", req.eventType.name))

	repoID, err := s.authorize(ctx, logger, "poll", req.repo)
	if err != nil {
		return nil, accessError(err)
	}
	req.query.RepoID = repoID

	res, err := s.readEvents(ctx, req)
	if err != nil {
		return nil, readError(storeError(ctx, logger, err))
	}
	return res, nil
}

func (s *Service) readEvents(ctx context.Context, req pollRequest) (*eventsPollResult, error) {
	if req.cursor == nil {
		head, err := s.store.CursorAt(ctx, s.clock.Now())
		if err != nil {
			return nil, err
		}
		return newPollResult(nil, head, false, false), nil
	}
	after, truncated, err := s.replayStart(ctx, *req.cursor, req.maxAge)
	if err != nil {
		return nil, err
	}
	q := req.query
	q.After = after
	q.Limit = req.limit + 1
	page, err := s.store.PollEvents(ctx, q)
	if err != nil {
		return nil, err
	}
	events, cursor, hasMore := page.Events, page.Next, false
	if len(events) > int(req.limit) {
		events = events[:req.limit]
		cursor = events[len(events)-1].ID
		hasMore = true
	}
	occurrences := make([]eventOccurrence, len(events))
	for i, ev := range events {
		if occurrences[i], err = occurrenceOf(ev); err != nil {
			return nil, err
		}
	}
	return newPollResult(occurrences, cursor, truncated, hasMore), nil
}

func (s *Service) replayStart(ctx context.Context, cursor int64, maxAge time.Duration) (after int64, truncated bool, err error) {
	floor, err := s.retentionFloor(ctx)
	if err != nil {
		return 0, false, err
	}
	if maxAge > 0 {
		aged, err := s.store.CursorAt(ctx, s.clock.Now().Add(-maxAge))
		if err != nil {
			return 0, false, err
		}
		floor = max(floor, aged)
	}
	if cursor < floor {
		return floor, true, nil
	}
	return cursor, false, nil
}

func (*Service) retentionFloor(context.Context) (int64, error) {
	return 0, nil
}

func newPollResult(events []eventOccurrence, cursor int64, truncated, hasMore bool) *eventsPollResult {
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

func occurrenceOf(ev Event) (eventOccurrence, error) {
	var data map[string]json.RawMessage
	if err := json.Unmarshal(ev.Data, &data); err != nil || data == nil {
		return eventOccurrence{}, xerrors.Errorf("data of event %d is not a JSON object", ev.ID)
	}
	prs := ev.PRs
	if prs == nil {
		prs = []int32{}
	}
	data["prs"] = marshalRaw(prs)
	if ev.HeadSHA != "" {
		data["head_sha"] = marshalRaw(ev.HeadSHA)
	}
	return eventOccurrence{
		EventID:   ev.DeliveryID,
		Name:      ev.Type,
		Timestamp: ev.ReceivedAt.UTC(),
		Data:      marshalRaw(data),
	}, nil
}

func marshalRaw(v any) json.RawMessage {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("marshal %T: %v", v, err))
	}
	return raw
}

func encodeCursor(id int64) string {
	return cursorPrefix + strconv.FormatInt(id, 10)
}

func decodeCursor(cursor string) (int64, bool) {
	digits, ok := strings.CutPrefix(cursor, cursorPrefix)
	if !ok {
		return 0, false
	}
	id, err := strconv.ParseInt(digits, 10, 64)
	if err != nil || id < 0 || encodeCursor(id) != cursor {
		return 0, false
	}
	return id, true
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
