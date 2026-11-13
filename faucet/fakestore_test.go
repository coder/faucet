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
	page.Next = max(head, q.After)
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
	var id int64
	for _, e := range s.events {
		if e.ReceivedAt.Before(t) {
			id = max(id, e.ID)
		}
	}
	return id, nil
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
