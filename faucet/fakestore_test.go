package faucet_test

import (
	"context"
	"slices"
	"testing"
	"time"

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
