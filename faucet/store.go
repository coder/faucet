// Package faucet turns GitHub webhooks into events that agents can wait for.
package faucet

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"time"

	"golang.org/x/xerrors"
)

// Event is one stored GitHub webhook, already normalized.
type Event struct {
	// ID is assigned by the store. It is the poll cursor.
	ID     int64
	Source string
	// DeliveryID is the X-GitHub-Delivery header. It is unique per event.
	DeliveryID string
	// Type is the normalized event type, for example "ci.run_completed".
	Type   string
	RepoID int64
	PRs    []int32
	// HeadSHA is empty when the event carries none.
	HeadSHA string
	Data    json.RawMessage
	// ReceivedAt is assigned by the store.
	ReceivedAt time.Time
}

// Validate checks the fields a caller supplies to Store.InsertEvent.
// ID and ReceivedAt are ignored because the store assigns them.
//
// Validate accepts any valid JSON without NUL characters. Postgres jsonb
// rejects a few other values, such as numbers beyond the float64 range or lone
// UTF-16 surrogates. Data built with encoding/json never contains them, and
// InsertEvent returns the database error if a caller sends one anyway.
func (e Event) Validate() error {
	switch {
	case e.Source == "":
		return xerrors.New("source is required")
	case e.DeliveryID == "":
		return xerrors.New("delivery ID is required")
	case e.Type == "":
		return xerrors.New("type is required")
	case e.RepoID == 0:
		return xerrors.New("repo ID is required")
	case len(e.Data) == 0:
		return xerrors.New("data is required")
	case !json.Valid(e.Data):
		return xerrors.New("data is not valid JSON")
	}
	var v any
	if err := json.Unmarshal(e.Data, &v); err != nil {
		return xerrors.Errorf("decode data: %w", err)
	}
	if hasNUL(v) {
		// Postgres jsonb rejects U+0000 in any string.
		return xerrors.New("data contains a NUL character")
	}
	return nil
}

// PollQuery selects the events one repository's waiter wants to see.
type PollQuery struct {
	RepoID int64
	// Types limits the result to these event types. Empty means every type.
	Types []string
	// HeadSHA limits the result to events with this head SHA. Empty means no limit.
	HeadSHA string
	// PR limits the result to events that mention this pull request number.
	// Zero means no limit.
	PR int32
	// After is the cursor: only events with ID > After are returned.
	After int64
	// Limit is the page size. It must be positive.
	Limit int32
}

// Validate checks the fields a caller supplies to Store.PollEvents.
func (q PollQuery) Validate() error {
	switch {
	case q.RepoID <= 0:
		return xerrors.New("repo ID must be positive")
	case q.Limit <= 0:
		return xerrors.New("limit must be positive")
	case q.After < 0:
		return xerrors.New("after must not be negative")
	case q.PR < 0:
		return xerrors.New("pr must not be negative")
	}
	return nil
}

// PollPage is the result of Store.PollEvents.
type PollPage struct {
	// Events are the matching events in ID order.
	Events []Event
	// Next is the cursor to poll with next. It is Events[len(Events)-1].ID
	// when the page is full. Otherwise it is the highest event ID the store
	// had when it answered, so a waiter whose repository is quiet does not
	// rescan other repositories' events. It is never lower than the query's
	// After or than PurgedThrough.
	Next int64
	// PurgedThrough is the purge mark: the highest event ID any purge has
	// deleted, or 0. The store reads it in the same snapshot as Events, so a
	// caller whose cursor is below it knows that events above the cursor may be
	// gone, whatever a purge did while the page was read.
	PurgedThrough int64
}

// PurgeResult is what Store.Purge reports.
type PurgeResult struct {
	// Deleted is the number of events this call deleted.
	Deleted int64
	// PurgedThrough is the purge mark after the call.
	PurgedThrough int64
}

func hasNUL(v any) bool {
	switch v := v.(type) {
	case string:
		return strings.ContainsRune(v, 0)
	case []any:
		return slices.ContainsFunc(v, hasNUL)
	case map[string]any:
		for k, e := range v {
			if strings.ContainsRune(k, 0) || hasNUL(e) {
				return true
			}
		}
	}
	return false
}

// Store persists events and serves them to pollers.
//
// Implementations must guarantee that event IDs are visible in commit order:
// a reader that has seen ID n has already seen every ID below n that will ever
// be visible. Without this, a poller that advances its cursor to n can skip an
// event that commits late.
type Store interface {
	// InsertEvent stores ev once per DeliveryID. It returns the stored event and
	// inserted=false when the DeliveryID already exists, in which case stored is
	// the existing row and ev is discarded.
	InsertEvent(ctx context.Context, ev Event) (stored Event, inserted bool, err error)
	// EventsAfter returns events with ID > afterID in ID order, at most limit rows.
	// limit must be positive.
	EventsAfter(ctx context.Context, afterID int64, limit int32) ([]Event, error)
	// PollEvents returns one page for q. Every filter is applied before the
	// limit, and the events and PollPage.Next come from one snapshot. Because
	// IDs are visible in commit order, polling again with After set to Next
	// never skips a matching event.
	PollEvents(ctx context.Context, q PollQuery) (PollPage, error)
	// CursorAt returns the highest event ID received before t, or 0 when there
	// is none. It is never lower than the purge mark, so polling with it as
	// the cursor returns the events from t on and is not reported as a gap.
	CursorAt(ctx context.Context, t time.Time) (int64, error)
	// Purge deletes the events received before the given time and raises the
	// purge mark to the highest ID it deleted, in one transaction. The mark
	// never moves down. It needs no insert lock: events commit in ID order, so
	// no event at or below the mark can appear after the purge.
	Purge(ctx context.Context, before time.Time) (PurgeResult, error)
}
