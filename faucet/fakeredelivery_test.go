package faucet_test

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"golang.org/x/xerrors"

	"github.com/coder/faucet/faucet"
	"github.com/coder/faucet/faucet/storetest"
)

type fakeRedeliveryState int

const (
	fakePending fakeRedeliveryState = iota
	fakeRecovered
	fakeGaveUp
)

type fakeRedeliveryRow struct {
	state     fakeRedeliveryState
	lastID    int64
	lastAt    time.Time
	firstAt   time.Time
	asks      int
	nextAskAt time.Time
	settledAt time.Time
}

type fakeRange struct {
	faucet.RedeliveryRange
	gap      bool
	closedAt time.Time
}

// fakeRedeliveryStore is an in-memory RedeliveryStore that also stores
// events, as pgstore does. TestFakeRedeliveryStoreMatchesContract keeps it
// honest.
type fakeRedeliveryStore struct {
	*fakeStore
	rmu sync.Mutex

	stored      map[string]bool
	rows        map[string]*fakeRedeliveryRow
	hook        string
	epoch       int64
	leaseUntil  time.Time
	top         time.Time
	ranges      []*fakeRange
	nextRangeID int64
}

var _ faucet.RedeliveryStore = (*fakeRedeliveryStore)(nil)

func newFakeRedeliveryStore(clock func() time.Time) *fakeRedeliveryStore {
	return &fakeRedeliveryStore{
		fakeStore: &fakeStore{now: clock},
		stored:    map[string]bool{},
		rows:      map[string]*fakeRedeliveryRow{},
	}
}

func (s *fakeRedeliveryStore) InsertEvent(ctx context.Context, ev faucet.Event) (faucet.Event, bool, error) {
	stored, inserted, err := s.fakeStore.InsertEvent(ctx, ev)
	if err == nil {
		s.rmu.Lock()
		s.stored[stored.DeliveryID] = true
		s.rmu.Unlock()
	}
	return stored, inserted, err
}

func (s *fakeRedeliveryStore) AcquireRedeliveryLease(_ context.Context, hook string, now time.Time, ttl time.Duration) (faucet.RedeliveryScan, bool, error) {
	if hook == "" || ttl <= 0 || now.IsZero() {
		return faucet.RedeliveryScan{}, false, xerrors.New("a lease needs a hook, a time and a ttl")
	}
	s.rmu.Lock()
	defer s.rmu.Unlock()
	if s.epoch > 0 && s.leaseUntil.After(now) {
		return faucet.RedeliveryScan{}, false, nil
	}
	scan := faucet.RedeliveryScan{}
	if s.epoch > 0 && s.hook != hook {
		scan.HookChanged = true
		for _, r := range s.ranges {
			if r.gap {
				continue
			}
			r.gap, r.closedAt = true, now
			scan.ClosedGaps = append(scan.ClosedGaps, faucet.RedeliveryGap{From: r.OlderAt, To: laterOf(r.NewerAt, r.OlderAt)})
		}
		s.top = time.Time{}
	}
	s.hook = hook
	s.epoch++
	s.leaseUntil = now.Add(ttl)
	scan.Epoch, scan.Top = s.epoch, s.top
	scan.Ranges = s.openRangesLocked()
	return scan, true, nil
}

func laterOf(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func (s *fakeRedeliveryStore) openRangesLocked() []faucet.RedeliveryRange {
	var open []faucet.RedeliveryRange
	for _, r := range s.ranges {
		if !r.gap {
			open = append(open, r.RedeliveryRange)
		}
	}
	slices.SortStableFunc(open, func(a, b faucet.RedeliveryRange) int {
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

func (s *fakeRedeliveryStore) ApplyRedeliveryScan(_ context.Context, step faucet.ScanStep) (faucet.ScanResult, error) {
	if err := step.Validate(); err != nil {
		return faucet.ScanResult{}, err
	}
	s.rmu.Lock()
	defer s.rmu.Unlock()
	if step.Epoch != s.epoch {
		return faucet.ScanResult{}, faucet.ErrRedeliveryLeaseLost
	}
	switch {
	case step.Advance != nil:
		if r := s.openRangeLocked(step.Advance.ID); r == nil {
			return faucet.ScanResult{}, xerrors.Errorf("range %d is not open", step.Advance.ID)
		}
	case step.CloseID != 0:
		if r := s.openRangeLocked(step.CloseID); r == nil {
			return faucet.ScanResult{}, xerrors.Errorf("range %d is not open", step.CloseID)
		}
	}

	var res faucet.ScanResult
	for _, f := range step.Failed {
		row, ok := s.rows[f.GUID]
		switch {
		case !ok:
			s.rows[f.GUID] = &fakeRedeliveryRow{
				state: fakePending, lastID: f.AttemptID, lastAt: f.DeliveredAt, firstAt: f.DeliveredAt, nextAskAt: step.Now,
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
	switch {
	case step.Open != nil:
		s.nextRangeID++
		r := *step.Open
		r.ID = s.nextRangeID
		s.ranges = append(s.ranges, &fakeRange{RedeliveryRange: r})
		res.OpenedID = r.ID
	case step.Advance != nil:
		r := s.openRangeLocked(step.Advance.ID)
		r.NewerAt, r.Cursor = step.Advance.NewerAt, step.Advance.Cursor
	case step.CloseID != 0:
		r := s.openRangeLocked(step.CloseID)
		s.ranges = slices.DeleteFunc(s.ranges, func(x *fakeRange) bool { return x == r })
	}
	if step.Gap != nil {
		s.nextRangeID++
		s.ranges = append(s.ranges, &fakeRange{
			RedeliveryRange: faucet.RedeliveryRange{ID: s.nextRangeID, NewerAt: step.Gap.To, OlderAt: step.Gap.From},
			gap:             true, closedAt: step.Now,
		})
	}
	return res, nil
}

func (s *fakeRedeliveryStore) openRangeLocked(id int64) *fakeRange {
	for _, r := range s.ranges {
		if r.ID == id && !r.gap {
			return r
		}
	}
	return nil
}

func (s *fakeRedeliveryStore) SettleRedeliveries(_ context.Context, req faucet.SettleRequest) (faucet.SettleResult, error) {
	if err := req.Validate(); err != nil {
		return faucet.SettleResult{}, err
	}
	s.rmu.Lock()
	defer s.rmu.Unlock()
	var res faucet.SettleResult
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
			s.giveUpLocked(&res, guid, row, faucet.GiveUpExpired, req.Now)
		case row.asks >= req.MaxAsks && !row.nextAskAt.After(req.Now):
			s.giveUpLocked(&res, guid, row, faucet.GiveUpLimit, req.Now)
		default:
			res.Pending++
		}
	}
	return res, nil
}

func (s *fakeRedeliveryStore) giveUpLocked(res *faucet.SettleResult, guid string, row *fakeRedeliveryRow, reason faucet.GiveUpReason, now time.Time) {
	row.state, row.settledAt = fakeGaveUp, now
	res.GaveUp = append(res.GaveUp, faucet.GaveUp{GUID: guid, Reason: reason, Asks: row.asks, FirstDeliveredAt: row.firstAt})
}

func (s *fakeRedeliveryStore) sortedGUIDsLocked() []string {
	guids := make([]string, 0, len(s.rows))
	for guid := range s.rows {
		guids = append(guids, guid)
	}
	slices.Sort(guids)
	return guids
}

func (s *fakeRedeliveryStore) ClaimRedelivery(_ context.Context, req faucet.ClaimRequest) (faucet.RedeliveryClaim, bool, error) {
	if err := req.Validate(); err != nil {
		return faucet.RedeliveryClaim{}, false, err
	}
	s.rmu.Lock()
	defer s.rmu.Unlock()
	var (
		pick     *fakeRedeliveryRow
		pickGUID string
	)
	for _, guid := range s.sortedGUIDsLocked() {
		row := s.rows[guid]
		due := row.state == fakePending && row.asks < req.MaxAsks && !row.nextAskAt.After(req.Now) &&
			row.firstAt.Add(req.Retention).After(req.Now) && !s.stored[guid]
		if !due {
			continue
		}
		if pick == nil || row.nextAskAt.Before(pick.nextAskAt) || (row.nextAskAt.Equal(pick.nextAskAt) && row.firstAt.Before(pick.firstAt)) {
			pick, pickGUID = row, guid
		}
	}
	if pick == nil {
		return faucet.RedeliveryClaim{}, false, nil
	}
	pick.nextAskAt = req.Now.Add(req.Schedule[pick.asks])
	pick.asks++
	return faucet.RedeliveryClaim{GUID: pickGUID, AttemptID: pick.lastID, Ask: pick.asks}, true, nil
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

	storetest.RunRedelivery(t, func(*testing.T) storetest.RedeliveryStore { return newFakeRedeliveryStore(nil) })
}
