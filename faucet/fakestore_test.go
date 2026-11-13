package faucet_test

import (
	"context"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/xerrors"

	"github.com/coder/faucet/faucet"
	"github.com/coder/faucet/faucet/storetest"
)

func (s *fakeStore) nowLocked() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

func (s *fakeStore) PollEvents(_ context.Context, q faucet.PollQuery) (faucet.PollPage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return faucet.PollPage{}, s.err
	}
	if err := q.Validate(); err != nil {
		return faucet.PollPage{}, err
	}
	s.polls = append(s.polls, q)

	var head int64
	var page faucet.PollPage
	for _, e := range s.events {
		head = max(head, e.ID)
		if e.ID <= q.After || e.RepoID != q.RepoID || len(page.Events) == int(q.Limit) {
			continue
		}
		if len(q.Types) > 0 && !slices.Contains(q.Types, e.Type) {
			continue
		}
		if q.HeadSHA != "" && e.HeadSHA != q.HeadSHA {
			continue
		}
		if q.PR != 0 && !slices.Contains(e.PRs, q.PR) {
			continue
		}
		page.Events = append(page.Events, e)
	}
	head = max(head, s.purgeMark)
	page.Next = max(head, q.After)
	page.PurgedThrough = s.purgeMark
	if len(page.Events) == int(q.Limit) {
		page.Next = page.Events[len(page.Events)-1].ID
	}
	return page, nil
}

func (s *fakeStore) CursorAt(_ context.Context, t time.Time) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return 0, s.err
	}
	s.cursorLookups = append(s.cursorLookups, t)
	id := s.purgeMark
	for _, e := range s.events {
		if e.ReceivedAt.Before(t) {
			id = max(id, e.ID)
		}
	}
	return id, nil
}

func (s *fakeStore) Purge(_ context.Context, before time.Time) (faucet.PurgeResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return faucet.PurgeResult{}, s.err
	}
	var res faucet.PurgeResult
	kept := s.events[:0:0]
	for _, e := range s.events {
		if !e.ReceivedAt.Before(before) {
			kept = append(kept, e)
			continue
		}
		res.Deleted++
		s.purgeMark = max(s.purgeMark, e.ID)
	}
	s.events = kept
	res.PurgedThrough = s.purgeMark
	return res, nil
}

func (s *fakeStore) StoredDeliveries(_ context.Context, deliveryIDs []string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return nil, s.err
	}
	stored := []string{}
	for _, e := range s.events {
		if slices.Contains(deliveryIDs, e.DeliveryID) {
			stored = append(stored, e.DeliveryID)
		}
	}
	return stored, nil
}

func (s *fakeStore) UpsertSubscription(_ context.Context, sub faucet.Subscription, now time.Time, maxLive int) (faucet.Subscription, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return faucet.Subscription{}, s.err
	}
	if maxLive <= 0 {
		return faucet.Subscription{}, xerrors.Errorf("max live subscriptions must be positive, got %d", maxLive)
	}
	if err := sub.Validate(now); err != nil {
		return faucet.Subscription{}, err
	}
	s.initSubscriptionsLocked()
	others := 0
	for key, row := range s.subs {
		switch {
		case row.Principal != sub.Principal:
		case !row.ExpiresAt.After(now):
			s.dropSubscriptionLocked(key)
		case key != sub.KeyHash:
			others++
		}
	}
	if others >= maxLive {
		return faucet.Subscription{}, faucet.ErrSubscriptionLimit
	}
	sub.CreatedAt, sub.UpdatedAt = now, now
	sub.Status = faucet.DeliveryStatus{}
	if row, ok := s.subs[sub.KeyHash]; ok {
		sub.CreatedAt, sub.Cursor, sub.Status = row.CreatedAt, row.Cursor, row.Status
	} else {
		s.scanned[sub.KeyHash] = sub.Cursor
	}
	s.subs[sub.KeyHash] = sub
	return sub, nil
}

func (s *fakeStore) DeleteSubscription(_ context.Context, principal string, key faucet.SubscriptionKeyHash) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return false, s.err
	}
	row, ok := s.subs[key]
	if !ok || row.Principal != principal {
		return false, nil
	}
	s.dropSubscriptionLocked(key)
	return true, nil
}

func (s *fakeStore) DeleteExpiredSubscriptions(_ context.Context, now time.Time) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return 0, s.err
	}
	var deleted int64
	for key, row := range s.subs {
		if !row.ExpiresAt.After(now) {
			s.dropSubscriptionLocked(key)
			deleted++
		}
	}
	return deleted, nil
}

func (s *fakeStore) subscriptions() []faucet.Subscription {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows := slices.Collect(maps.Values(s.subs))
	slices.SortFunc(rows, func(a, b faucet.Subscription) int {
		return strings.Compare(a.CallbackURL+a.EventType+string(a.Arguments), b.CallbackURL+b.EventType+string(b.Arguments))
	})
	return rows
}

func (s *fakeStore) lookups() []time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.cursorLookups)
}

func (s *fakeStore) pollQueries() []faucet.PollQuery {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.polls)
}

func TestFakeStoreMatchesStoreContract(t *testing.T) {
	t.Parallel()

	storetest.Run(t, func(*testing.T) faucet.Store { return &fakeStore{} })
}
