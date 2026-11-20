package redelivery_test

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"golang.org/x/xerrors"

	"github.com/coder/superblink-events/faucet"
	"github.com/coder/superblink-events/faucet/redelivery"
	"github.com/coder/superblink-events/faucet/storetest"
)

type fakeRedeliveryState int

const (
	fakePending fakeRedeliveryState = iota
	fakeRecovered
	fakeGaveUp
)

type fakeRedeliveryRow struct {
	state     fakeRedeliveryState
	hook      string
	lastID    int64
	lastAt    time.Time
	firstAt   time.Time
	asks      int
	nextAskAt time.Time
	settledAt time.Time
}

type fakeRange struct {
	redelivery.Range
	gap      bool
	closedAt time.Time
}

// fakeRedeliveryStore is an in-memory redelivery.Store that also stores
// events, as pgstore does. TestFakeRedeliveryStoreMatchesContract keeps it
// honest.
type fakeRedeliveryStore struct {
	// Store stays nil. The redelivery tests call only InsertEvent, which the
	// fake implements. The rest of faucet.Store is here to satisfy
	// storetest.RedeliveryStore.
	faucet.Store
	rmu sync.Mutex

	stored      map[string]bool
	rows        map[string]*fakeRedeliveryRow
	hook        string
	epoch       int64
	leaseUntil  time.Time
	top         time.Time
	clearAt     time.Time
	ranges      []*fakeRange
	nextRangeID int64
}

var _ redelivery.Store = (*fakeRedeliveryStore)(nil)

func newFakeRedeliveryStore() *fakeRedeliveryStore {
	return &fakeRedeliveryStore{
		stored: map[string]bool{},
		rows:   map[string]*fakeRedeliveryRow{},
	}
}

// InsertEvent only remembers the delivery ID, which is all that redelivery
// reads, so that a test with tens of thousands of deliveries stays fast.
func (s *fakeRedeliveryStore) InsertEvent(_ context.Context, ev faucet.Event) (faucet.Event, bool, error) {
	if err := ev.Validate(); err != nil {
		return faucet.Event{}, false, err
	}
	s.rmu.Lock()
	defer s.rmu.Unlock()
	if s.stored[ev.DeliveryID] {
		return ev, false, nil
	}
	s.stored[ev.DeliveryID] = true
	ev.ID = int64(len(s.stored))
	return ev, true, nil
}

func (s *fakeRedeliveryStore) AcquireRedeliveryLease(_ context.Context, hook string, now time.Time, ttl time.Duration) (redelivery.Scan, bool, error) {
	if hook == "" || ttl <= 0 || now.IsZero() {
		return redelivery.Scan{}, false, xerrors.New("a lease needs a hook, a time and a ttl")
	}
	s.rmu.Lock()
	defer s.rmu.Unlock()
	if s.epoch > 0 && s.leaseUntil.After(now) {
		return redelivery.Scan{}, false, nil
	}
	scan := redelivery.Scan{}
	if s.epoch > 0 && s.hook != hook {
		scan.HookChanged = true
		open := s.openRangesLocked()
		s.ranges = slices.DeleteFunc(s.ranges, func(r *fakeRange) bool { return !r.gap })
		for _, r := range open {
			gap := r.UnreadGap()
			s.recordGapLocked(gap, now)
			scan.ClosedGaps = append(scan.ClosedGaps, gap)
		}
		s.top, s.clearAt = time.Time{}, time.Time{}
	}
	s.hook = hook
	s.epoch++
	s.leaseUntil = now.Add(ttl)
	scan.Epoch, scan.Top, scan.Clear = s.epoch, s.top, s.clearAt
	scan.Ranges = s.openRangesLocked()
	return scan, true, nil
}

func laterOf(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func (s *fakeRedeliveryStore) openRangesLocked() []redelivery.Range {
	var open []redelivery.Range
	for _, r := range s.ranges {
		if !r.gap {
			open = append(open, r.Range)
		}
	}
	slices.SortStableFunc(open, func(a, b redelivery.Range) int {
		if c := b.NewerAt.Compare(a.NewerAt); c != 0 {
			return c
		}
		return int(b.ID - a.ID)
	})
	return open
}

func (s *fakeRedeliveryStore) ReleaseRedeliveryLease(_ context.Context, epoch int64) error {
	s.rmu.Lock()
	defer s.rmu.Unlock()
	if s.epoch == epoch {
		s.leaseUntil = time.Time{}
	}
	return nil
}

func (s *fakeRedeliveryStore) ApplyRedeliveryScan(_ context.Context, step redelivery.ScanStep) (redelivery.ScanResult, error) {
	if err := step.Validate(); err != nil {
		return redelivery.ScanResult{}, err
	}
	s.rmu.Lock()
	defer s.rmu.Unlock()
	if step.Epoch != s.epoch {
		return redelivery.ScanResult{}, redelivery.ErrLeaseLost
	}
	switch {
	case step.Advance != nil:
		if r := s.openRangeLocked(step.Advance.ID); r == nil {
			return redelivery.ScanResult{}, xerrors.Errorf("range %d is not open", step.Advance.ID)
		}
	case step.CloseID != 0:
		if r := s.openRangeLocked(step.CloseID); r == nil {
			return redelivery.ScanResult{}, xerrors.Errorf("range %d is not open", step.CloseID)
		}
	}

	var res redelivery.ScanResult
	for _, f := range step.Failed {
		row, ok := s.rows[f.GUID]
		switch {
		case !ok:
			s.rows[f.GUID] = &fakeRedeliveryRow{
				state: fakePending, hook: s.hook, lastID: f.AttemptID, lastAt: f.DeliveredAt, firstAt: f.DeliveredAt, nextAskAt: step.Now,
			}
			res.Tracked++
		case row.state == fakePending:
			if f.DeliveredAt.After(row.lastAt) || (f.DeliveredAt.Equal(row.lastAt) && f.AttemptID > row.lastID) {
				row.lastID, row.lastAt = f.AttemptID, f.DeliveredAt
			}
			if f.DeliveredAt.Before(row.firstAt) {
				row.firstAt = f.DeliveredAt
			}
		}
	}
	for _, guid := range step.Succeeded {
		if row, ok := s.rows[guid]; ok && row.state == fakePending {
			row.state, row.settledAt = fakeRecovered, step.Now
			res.Recovered = append(res.Recovered, guid)
		}
	}
	if step.Top.After(s.top) {
		s.top = step.Top
	}
	if !step.Clear.IsZero() {
		s.clearAt = step.Clear
	}
	switch {
	case step.Open != nil:
		s.nextRangeID++
		r := *step.Open
		r.ID = s.nextRangeID
		s.ranges = append(s.ranges, &fakeRange{Range: r})
		res.OpenedID = r.ID
	case step.Advance != nil:
		r := s.openRangeLocked(step.Advance.ID)
		r.NewerAt, r.Cursor = step.Advance.NewerAt, step.Advance.Cursor
	case step.CloseID != 0:
		r := s.openRangeLocked(step.CloseID)
		s.ranges = slices.DeleteFunc(s.ranges, func(x *fakeRange) bool { return x == r })
	}
	if step.Gap != nil {
		s.recordGapLocked(*step.Gap, step.Now)
	}
	return res, nil
}

func (s *fakeRedeliveryStore) recordGapLocked(gap redelivery.Gap, now time.Time) {
	s.nextRangeID++
	s.ranges = append(s.ranges, &fakeRange{
		Range: redelivery.Range{ID: s.nextRangeID, NewerAt: gap.To, OlderAt: gap.From},
		gap:   true, closedAt: now,
	})
}

func (s *fakeRedeliveryStore) openRangeLocked(id int64) *fakeRange {
	for _, r := range s.ranges {
		if r.ID == id && !r.gap {
			return r
		}
	}
	return nil
}

func (s *fakeRedeliveryStore) SettleRedeliveries(_ context.Context, req redelivery.SettleRequest) (redelivery.SettleResult, error) {
	if err := req.Validate(); err != nil {
		return redelivery.SettleResult{}, err
	}
	s.rmu.Lock()
	defer s.rmu.Unlock()
	var res redelivery.SettleResult
	for _, guid := range s.sortedGUIDsLocked() {
		row := s.rows[guid]
		if row.state != fakePending {
			continue
		}
		switch {
		case s.stored[guid]:
			row.state, row.settledAt = fakeRecovered, req.Now
			res.Recovered = append(res.Recovered, guid)
		case !row.firstAt.Add(req.Retention).After(req.Now):
			giveUp(&res, guid, row, redelivery.GiveUpExpired, req.Now)
		case row.asks >= req.MaxAsks && !row.nextAskAt.After(req.Now):
			giveUp(&res, guid, row, redelivery.GiveUpLimit, req.Now)
		default:
			res.Pending++
		}
	}
	return res, nil
}

func giveUp(res *redelivery.SettleResult, guid string, row *fakeRedeliveryRow, reason redelivery.GiveUpReason, now time.Time) {
	row.state, row.settledAt = fakeGaveUp, now
	res.GaveUp = append(res.GaveUp, redelivery.GaveUp{GUID: guid, Reason: reason, Asks: row.asks, FirstDeliveredAt: row.firstAt})
}

func (s *fakeRedeliveryStore) sortedGUIDsLocked() []string {
	guids := make([]string, 0, len(s.rows))
	for guid := range s.rows {
		guids = append(guids, guid)
	}
	slices.Sort(guids)
	return guids
}

func (s *fakeRedeliveryStore) ClaimRedelivery(_ context.Context, req redelivery.ClaimRequest) (redelivery.Claim, bool, error) {
	if err := req.Validate(); err != nil {
		return redelivery.Claim{}, false, err
	}
	s.rmu.Lock()
	defer s.rmu.Unlock()
	var (
		pick     *fakeRedeliveryRow
		pickGUID string
	)
	for _, guid := range s.sortedGUIDsLocked() {
		row := s.rows[guid]
		due := row.state == fakePending && row.hook == s.hook && row.asks < req.MaxAsks && !row.nextAskAt.After(req.Now) &&
			row.firstAt.Add(req.Retention).After(req.Now) && !s.stored[guid]
		if !due {
			continue
		}
		if pick == nil || row.nextAskAt.Before(pick.nextAskAt) || (row.nextAskAt.Equal(pick.nextAskAt) && row.firstAt.Before(pick.firstAt)) {
			pick, pickGUID = row, guid
		}
	}
	if pick == nil {
		return redelivery.Claim{}, false, nil
	}
	pick.nextAskAt = req.Now.Add(req.Schedule[pick.asks])
	pick.asks++
	return redelivery.Claim{GUID: pickGUID, AttemptID: pick.lastID, Ask: pick.asks}, true, nil
}

func (s *fakeRedeliveryStore) PurgeRedeliveries(_ context.Context, before time.Time) (int64, error) {
	s.rmu.Lock()
	defer s.rmu.Unlock()
	var deleted int64
	for guid, row := range s.rows {
		if row.state != fakePending && laterOf(row.lastAt, row.settledAt).Before(before) {
			delete(s.rows, guid)
			deleted++
		}
	}
	s.ranges = slices.DeleteFunc(s.ranges, func(r *fakeRange) bool {
		if r.gap && r.closedAt.Before(before) {
			deleted++
			return true
		}
		return false
	})
	return deleted, nil
}

func TestFakeRedeliveryStoreMatchesContract(t *testing.T) {
	t.Parallel()

	storetest.RunRedelivery(t, func(*testing.T) storetest.RedeliveryStore { return newFakeRedeliveryStore() })
}

var _ storetest.GapRecords = (*fakeRedeliveryStore)(nil)

func (s *fakeRedeliveryStore) RecordedRedeliveryGaps(context.Context) ([]redelivery.Gap, error) {
	s.rmu.Lock()
	defer s.rmu.Unlock()
	var gaps []redelivery.Gap
	for _, r := range s.ranges {
		if r.gap {
			gaps = append(gaps, redelivery.Gap{From: r.OlderAt, To: r.NewerAt})
		}
	}
	return gaps, nil
}

func (s *fakeRedeliveryStore) applyMigration000008() {
	s.rmu.Lock()
	defer s.rmu.Unlock()
	s.clearAt, s.top = s.top, time.Time{}
}

func (s *fakeRedeliveryStore) seedTopWithoutClear(top time.Time) {
	s.rmu.Lock()
	defer s.rmu.Unlock()
	s.top = top
}

func (s *fakeRedeliveryStore) seedMigratedTop(oldTop time.Time) {
	s.seedTopWithoutClear(oldTop)
	s.applyMigration000008()
}

func (s *fakeRedeliveryStore) rowCount() int {
	s.rmu.Lock()
	defer s.rmu.Unlock()
	return len(s.rows)
}

// deliveries copies the state of every delivery that the store keeps.
func (s *fakeRedeliveryStore) deliveries() map[string]fakeRedeliveryRow {
	s.rmu.Lock()
	defer s.rmu.Unlock()
	rows := make(map[string]fakeRedeliveryRow, len(s.rows))
	for guid, row := range s.rows {
		rows[guid] = *row
	}
	return rows
}
