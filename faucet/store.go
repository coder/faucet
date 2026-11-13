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
}
