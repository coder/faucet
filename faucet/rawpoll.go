package faucet

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"time"

	"golang.org/x/xerrors"

	"cdr.dev/slog/v3"
)

const (
	// rawLeaseTTL is how long a poll lease lives after its last poll.
	rawLeaseTTL = time.Hour
	// maxLiveRawLeases is the most live poll leases of one login. Opening
	// one more ends the one polled longest ago.
	maxLiveRawLeases = 100
	// rawPageBytes ends a page of a raw poll after the event that takes its
	// bodies to this size.
	rawPageBytes = 1 << 20
)

type rawCursor struct {
	pos      int64
	lease    RawLeaseID
	hasLease bool
}

type rawPollRequest struct {
	entry  *rawEntry
	args   rawArguments
	cursor *rawCursor
	maxAge time.Duration
	limit  int32
}

func parseRawPoll(entry *rawEntry, p *eventsPollParams) (rawPollRequest, error) {
	args, err := parseRawArguments(entry, p.Arguments)
	if err != nil {
		return rawPollRequest{}, err
	}
	req := rawPollRequest{entry: entry, args: args.normalized(), limit: pageLimit}
	if p.Cursor != nil {
		pos, lease, hasLease, ok := decodeRawCursor(*p.Cursor)
		if !ok {
			return rawPollRequest{}, invalidParams("cursor is not one this server returned")
		}
		req.cursor = &rawCursor{pos: pos, lease: lease, hasLease: hasLease}
	}
	if p.MaxEvents != nil {
		if *p.MaxEvents < 1 {
			return rawPollRequest{}, invalidParams("maxEvents must be positive")
		}
		req.limit = min(*p.MaxEvents, pageLimit)
	}
	if p.MaxAgeMs != nil {
		if *p.MaxAgeMs < 1 {
			return rawPollRequest{}, invalidParams("maxAgeMs must be positive")
		}
		req.maxAge = time.Duration(min(*p.MaxAgeMs, maxAgeLimitMs)) * time.Millisecond
	}
	return req, nil
}

// pollRaw serves events/poll for a raw name from a poll lease. A poll
// without a cursor that names a live lease of the same login, arguments and
// repository opens a new lease at the head, and says truncated when it was
// given a cursor, because the events since that cursor are not kept for it.
func (s *Service) pollRaw(ctx context.Context, entry *rawEntry, p *eventsPollParams) (*eventsPollResult, error) {
	req, err := parseRawPoll(entry, p)
	if err != nil {
		return nil, err
	}
	logger := s.logger.With(req.args.logField(), slog.F("event", entry.name))
	principal, err := s.principal(ctx, logger, "poll", ScopeEventsRead)
	if err != nil {
		return nil, accessError(err)
	}
	target, err := s.authorizeTarget(ctx, logger, principal, "poll", req.args.targetArguments)
	if err != nil {
		return nil, accessError(err)
	}
	release, err := s.startPoll(ctx, logger, principal)
	if err != nil {
		return nil, err
	}
	defer release()
	key := NewSubscriptionKeyHash(principal.Login, "", entry.name, req.args.canonical())
	now := s.clock.Now()
	if req.cursor != nil && req.cursor.hasLease {
		res, found, err := s.readRawLease(ctx, logger, req, key, target, now)
		if err != nil {
			return nil, readError(storeError(ctx, logger, err))
		}
		if found {
			return res, nil
		}
	}
	lease := RawLease{
		Key:       key,
		Principal: principal.Login,
		Event:     entry.event,
		Actions:   req.args.Actions,
		Fields:    req.args.Fields,
		Target:    target,
		RepoNames: req.args.Repos,
		ExpiresAt: now.Add(rawLeaseTTL),
	}
	if _, err := rand.Read(lease.ID[:]); err != nil {
		return nil, readError(storeError(ctx, logger, xerrors.Errorf("make a lease ID: %w", err)))
	}
	head, err := s.raw.OpenRawLease(ctx, lease, now, RawLimits{Live: maxLiveRawLeases, PerTarget: maxRawInterestsPerTarget, PerPrincipalTarget: maxRawInterestsPerLogin})
	switch {
	case xerrors.Is(err, ErrRawInterestLimit):
		logger.Info(ctx, "poll refused, raw interest limit reached", slog.F("login", principal.Login))
		return nil, rawInterestLimitError(entry, req.args.targetArguments)
	case err != nil:
		return nil, readError(storeError(ctx, logger, err))
	}
	logger.Debug(ctx, "poll lease opened", slog.F("login", principal.Login))
	return newRawPollResult(nil, head, lease.ID, req.cursor != nil, false), nil
}

func (s *Service) readRawLease(ctx context.Context, logger slog.Logger, req rawPollRequest, key SubscriptionKeyHash, target Target, now time.Time) (*eventsPollResult, bool, error) {
	q := RawRead{
		Lease:     req.cursor.lease,
		Key:       key,
		Target:    target,
		After:     req.cursor.pos,
		Limit:     req.limit,
		MaxBytes:  rawPageBytes,
		Now:       now,
		ExpiresAt: now.Add(rawLeaseTTL),
	}
	if req.maxAge > 0 {
		q.NotBefore = now.Add(-req.maxAge)
	}
	page, found, err := s.raw.ReadRawLease(ctx, q)
	if err != nil || !found {
		return nil, false, err
	}
	truncated := q.After < page.AckedThrough || page.Skipped > 0
	occurrences := make([]eventOccurrence, 0, len(page.Events))
	for _, ev := range page.Events {
		data, err := rawEventData(ev, req.args.Fields)
		if err != nil {
			logger.Error(ctx, "skip raw event", slog.F("raw_id", ev.ID), slog.Error(err))
			truncated = true
			continue
		}
		occurrences = append(occurrences, eventOccurrence{
			EventID:   ev.DeliveryID,
			Name:      req.entry.name,
			Timestamp: ev.ReceivedAt.UTC(),
			Data:      data,
		})
	}
	return newRawPollResult(occurrences, page.Next, q.Lease, truncated, page.More), true, nil
}

// rawEventData fails closed: an event that a lease without fields reads must
// hold the whole payload, and no event is returned over the size that a
// delivery may carry.
func rawEventData(ev RawEvent, fields []string) (json.RawMessage, error) {
	data := []byte(ev.Body)
	if len(fields) > 0 {
		projected, err := projectFields(ev.Body, fields)
		if err != nil {
			return nil, err
		}
		data = projected
	} else if ev.Fields != nil {
		return nil, xerrors.New("the stored body holds only some fields, and the lease wants the whole payload")
	}
	if len(data) > maxEventBodyBytes {
		return nil, xerrors.Errorf("the event data is %d bytes, more than %d", len(data), maxEventBodyBytes)
	}
	return data, nil
}

func newRawPollResult(events []eventOccurrence, pos int64, lease RawLeaseID, truncated, hasMore bool) *eventsPollResult {
	if events == nil {
		events = []eventOccurrence{}
	}
	return &eventsPollResult{
		ResultType: resultComplete,
		Events:     events,
		Cursor:     encodeRawPollCursor(pos, lease),
		Truncated:  truncated,
		HasMore:    hasMore,
		NextPollMs: recommendedPollMs,
	}
}
