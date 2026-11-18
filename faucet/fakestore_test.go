package faucet_test

import (
	"bytes"
	"context"
	"encoding/json"
	"maps"
	"slices"
	"strconv"
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

type fakeRepo struct {
	repo   faucet.Repository
	seenAt time.Time
}

func (s *fakeStore) RecordRepository(_ context.Context, repo faucet.Repository, seenAt time.Time) error {
	return s.writeRepository(repo, func() {
		if !slices.ContainsFunc(s.repos, func(r fakeRepo) bool { return r.repo.ID == repo.ID }) {
			s.repos = append(s.repos, fakeRepo{repo: repo, seenAt: seenAt})
		}
	})
}

func (s *fakeStore) RenameRepository(_ context.Context, repo faucet.Repository, from string, seenAt time.Time) error {
	return s.writeRepository(repo, func() {
		i := slices.IndexFunc(s.repos, func(r fakeRepo) bool { return r.repo.ID == repo.ID })
		switch {
		case i < 0:
			s.repos = append(s.repos, fakeRepo{repo: repo, seenAt: seenAt})
		case strings.EqualFold(s.repos[i].repo.FullName, from):
			s.repos[i] = fakeRepo{repo: repo, seenAt: seenAt}
		}
	})
}

// writeRepository counts a write of repo and, unless it fails, runs write
// under the lock.
func (s *fakeStore) writeRepository(repo faucet.Repository, write func()) error {
	if err := repo.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.repoWrites++
	if s.repoErr != nil {
		return s.repoErr
	}
	write()
	return nil
}

func (s *fakeStore) RepositoryByName(_ context.Context, fullName string) (faucet.Repository, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.repoErr != nil {
		return faucet.Repository{}, s.repoErr
	}
	var found *fakeRepo
	for i, r := range s.repos {
		if strings.EqualFold(r.repo.FullName, fullName) && (found == nil || !r.seenAt.Before(found.seenAt)) {
			found = &s.repos[i]
		}
	}
	if found == nil {
		return faucet.Repository{}, faucet.ErrUnknownRepository
	}
	return found.repo, nil
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
		if !slices.ContainsFunc(q.Fields, func(f faucet.FieldFilter) bool { return !passes(e, f) }) {
			page.Events = append(page.Events, e)
		}
	}
	head = max(head, s.purgeMark)
	page.Next = max(head, q.After)
	page.PurgedThrough = s.purgeMark
	if len(page.Events) == int(q.Limit) {
		page.Next = page.Events[len(page.Events)-1].ID
	}
	return page, nil
}

func passes(e faucet.Event, f faucet.FieldFilter) bool {
	if !slices.Contains(f.Types, e.Type) {
		return true
	}
	var data map[string]any
	if json.Unmarshal(e.Data, &data) != nil {
		return false
	}
	switch v := data[f.Field].(type) {
	case string:
		return slices.Contains(f.Values, v)
	case bool:
		return slices.Contains(f.Values, strconv.FormatBool(v))
	}
	return false
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
	var doomed int64
	for _, e := range s.events {
		if e.ReceivedAt.Before(before) {
			doomed = max(doomed, e.ID)
		}
	}
	s.advanceSubscriptionsThatLoseNothingLocked(doomed)
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

// advanceSubscriptionsThatLoseNothingLocked moves a subscription that is not
// below the purge mark yet to the mark that the purge will set (doomed), unless
// an event of its repository and of one of its types above its position, still
// in the log, lies at or below that mark.
func (s *fakeStore) advanceSubscriptionsThatLoseNothingLocked(doomed int64) {
	for key, sub := range s.subs {
		pos := s.scanned[key]
		if pos < s.purgeMark || pos >= doomed {
			continue
		}
		if !slices.ContainsFunc(s.events, func(e faucet.Event) bool {
			return e.RepoID == sub.RepoID && slices.Contains(sub.EventTypes, e.Type) && e.ID > pos && e.ID <= doomed
		}) {
			s.scanned[key] = doomed
		}
	}
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
	sub.PreviousSecretEnc, sub.PreviousSecretUntil = nil, time.Time{}
	grace := sub.RotationGrace
	sub.RotationGrace = 0
	if row, ok := s.subs[sub.KeyHash]; ok {
		sub.CreatedAt, sub.Cursor, sub.Status = row.CreatedAt, row.Cursor, row.Status
		switch {
		case bytes.Equal(row.SecretEnc, sub.SecretEnc):
			sub.PreviousSecretEnc, sub.PreviousSecretUntil = row.PreviousSecretEnc, row.PreviousSecretUntil
		case grace > 0:
			sub.PreviousSecretEnc, sub.PreviousSecretUntil = row.SecretEnc, now.Add(grace)
		}
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

func (s *fakeStore) DeleteSubscriptionsOf(_ context.Context, principal string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return 0, s.err
	}
	var n int64
	for key, row := range s.subs {
		if row.Principal == principal {
			s.dropSubscriptionLocked(key)
			n++
		}
	}
	return n, nil
}

func (s *fakeStore) DeleteLiveSubscription(_ context.Context, principal string, key faucet.SubscriptionKeyHash, now time.Time) (faucet.Subscription, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return faucet.Subscription{}, false, s.err
	}
	row, ok := s.subs[key]
	if !ok || row.Principal != principal || !row.ExpiresAt.After(now) {
		return faucet.Subscription{}, false, nil
	}
	s.dropSubscriptionLocked(key)
	return row, true, nil
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

func (s *fakeStore) ClearExpiredPreviousSecrets(_ context.Context, now time.Time) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return 0, s.err
	}
	var cleared int64
	for key, row := range s.subs {
		if row.PreviousSecretEnc != nil && !row.PreviousSecretUntil.After(now) {
			row.PreviousSecretEnc, row.PreviousSecretUntil = nil, time.Time{}
			s.subs[key] = row
			cleared++
		}
	}
	return cleared, nil
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
