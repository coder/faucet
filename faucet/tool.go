package faucet

import (
	"context"
	"regexp"
	"slices"
	"strings"
	"time"

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
	Repo     string   `json:"repo" jsonschema:"the repository, written owner/name"`
	Types    []string `json:"types" jsonschema:"the event types to wait for, at least one"`
	SHA      string   `json:"sha,omitempty" jsonschema:"only events for this 40 character head commit SHA"`
	PR       int32    `json:"pr,omitempty" jsonschema:"only events that mention this pull request number"`
	Cursor   *int64   `json:"cursor,omitempty" jsonschema:"return events after this cursor, which is the cursor of an earlier result"`
	TimeoutS *int     `json:"timeout_s,omitempty" jsonschema:"seconds to wait for an event, from 1 to 50, default 45"`
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
// soon as there are any, or none when timeout_s runs out. Either way it
// returns a cursor, and passing that cursor to the next call resumes exactly
// where this one stopped. Without a cursor it starts at the present, or ten
// minutes back when sha is set.
func (s *Service) Register(srv *mcp.Server) {
	types := eventTypes()
	mcp.AddTool(srv, &mcp.Tool{
		Name: toolName,
		Description: "Wait for GitHub events on a repository, such as a CI run finishing or a pull request review, " +
			"and return them as soon as they arrive. Returns no events and a cursor when timeout_s runs out; " +
			"call again with that cursor to keep waiting. When truncated is true, a retention purge deleted events after " +
			"the cursor you passed, so some may be missing. Event types: " + strings.Join(types, ", ") + ".",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, Title: "Wait for GitHub events"},
	}, s.waitForEvents)
	s.registerEvents(srv)
}

func eventTypes() []string {
	types := make([]string, 0, len(rules))
	for _, r := range rules {
		types = append(types, r.eventType)
	}
	slices.Sort(types)
	return slices.Compact(types)
}

type waitRequest struct {
	query   PollQuery
	cursor  *int64
	timeout time.Duration
}

func (in waitInput) parse() (waitRequest, error) {
	if _, _, err := parseRepo(in.Repo); err != nil {
		return waitRequest{}, err
	}
	known := eventTypes()
	if len(in.Types) == 0 {
		return waitRequest{}, xerrors.Errorf("types must name at least one of: %s", strings.Join(known, ", "))
	}
	for _, t := range in.Types {
		if !slices.Contains(known, t) {
			return waitRequest{}, xerrors.Errorf("unknown type %q, the types are: %s", t, strings.Join(known, ", "))
		}
	}
	if in.SHA != "" && !shaPattern.MatchString(in.SHA) {
		return waitRequest{}, xerrors.New("sha must be 40 hexadecimal characters")
	}
	if in.PR < 0 {
		return waitRequest{}, xerrors.New("pr must be a pull request number")
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

	types := slices.Clone(in.Types)
	slices.Sort(types)
	return waitRequest{
		query: PollQuery{
			Types:   slices.Compact(types),
			HeadSHA: strings.ToLower(in.SHA),
			PR:      in.PR,
			Limit:   pageLimit,
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
	logger := s.logger.With(slog.F("repo", in.Repo))

	p, err := s.principal(ctx, logger, "wait")
	if err != nil {
		return nil, waitOutput{}, err
	}
	repoID, err := s.authorizeRepo(ctx, logger, p, "wait", in.Repo)
	if err != nil {
		return nil, waitOutput{}, err
	}
	req.query.RepoID = repoID

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
		out.Events[i] = toWaitEvent(ev)
	}
	return nil, out, nil
}

func (s *Service) authorize(ctx context.Context, logger slog.Logger, op, repo string) (int64, error) {
	p, err := s.principal(ctx, logger, op)
	if err != nil {
		return 0, err
	}
	return s.authorizeRepo(ctx, logger, p, op, repo)
}

func (s *Service) principal(ctx context.Context, logger slog.Logger, op string) (Principal, error) {
	p, err := s.principalFrom(ctx)
	if err != nil {
		logger.Info(ctx, op+" without a principal", slog.Error(err))
		return Principal{}, errAuthRequired
	}
	return p, nil
}

func (s *Service) authorizeRepo(ctx context.Context, logger slog.Logger, p Principal, op, repo string) (int64, error) {
	logger = logger.With(slog.F("login", p.Login))
	repoID, err := s.authorizer.RepoID(ctx, p, repo)
	switch {
	case xerrors.Is(err, ErrNoAccess):
		logger.Info(ctx, op+" denied")
		return 0, ErrNoAccess
	case xerrors.Is(err, ErrInvalidToken):
		logger.Info(ctx, op+" denied, token rejected")
		return 0, ErrInvalidToken
	case err != nil:
		logger.Warn(ctx, "could not verify repository access", slog.Error(err))
		return 0, errVerify
	case repoID <= 0:
		logger.Error(ctx, "authorizer returned a repository without an ID")
		return 0, errVerify
	}
	return repoID, nil
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
		page, err := s.store.PollEvents(ctx, q)
		if err != nil {
			return PollPage{}, false, err
		}
		truncated = truncated || q.After < page.PurgedThrough
		if len(page.Events) > 0 {
			return page, truncated, nil
		}
		q.After = page.Next

		remaining := s.clock.Until(deadline)
		if remaining <= 0 {
			return page, truncated, nil
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

func storeError(ctx context.Context, logger slog.Logger, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	logger.Error(ctx, "read events", slog.Error(err))
	return errStore
}

func toWaitEvent(ev Event) waitEvent {
	prs := ev.PRs
	if prs == nil {
		prs = []int32{}
	}
	return waitEvent{
		ID:         ev.ID,
		Type:       ev.Type,
		RepoID:     ev.RepoID,
		PRs:        prs,
		HeadSHA:    ev.HeadSHA,
		Data:       ev.Data,
		ReceivedAt: ev.ReceivedAt,
	}
}
