package faucet_test

import (
	"bytes"
	"context"
	"encoding/json"
	"maps"
	"math"
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
	// quarantined is set by a rename from a name that repo did not hold. A
	// quarantined row resolves no name.
	quarantined bool
}

type fakeRepoOwner struct {
	login string
}

func (s *fakeStore) RecordRepository(_ context.Context, repo faucet.Repository, seenAt time.Time) error {
	return s.writeRepository(repo, func() {
		if !slices.ContainsFunc(s.repos, func(r fakeRepo) bool { return r.repo.ID == repo.ID }) {
			s.repos = append(s.repos, fakeRepo{repo: repo, seenAt: seenAt})
		}
	})
}

func (s *fakeStore) RenameRepository(_ context.Context, rename faucet.RepositoryRename, seenAt time.Time) (faucet.RenameOutcome, error) {
	var outcome faucet.RenameOutcome
	err := s.writeRepository(rename.Repo, func() {
		if s.renameBodies[rename.Body] {
			outcome = faucet.RenameSeenBefore
			return
		}
		if s.renameBodies == nil {
			s.renameBodies = map[[32]byte]bool{}
		}
		s.renameBodies[rename.Body] = true
		outcome = faucet.Renamed
		i := slices.IndexFunc(s.repos, func(r fakeRepo) bool { return r.repo.ID == rename.Repo.ID })
		held := i >= 0 && s.holdsLocked(s.repos[i].repo, rename.From)
		switch {
		case i < 0:
			s.repos = append(s.repos, fakeRepo{repo: rename.Repo, seenAt: seenAt})
			return
		case rename.Late && (s.repos[i].quarantined || !held):
			outcome = faucet.RenameLate
			return
		case held:
		default:
			outcome = faucet.RenameQuarantined
			s.repos[i].quarantined = true
			return
		}
		s.repos[i] = fakeRepo{repo: rename.Repo, seenAt: seenAt}
	})
	return outcome, err
}

func (s *fakeStore) AddOwner(_ context.Context, owner faucet.Owner) error {
	if err := owner.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.repoWrites++
	if s.repoErr != nil {
		return s.repoErr
	}
	if _, held := s.owners[owner.ID]; held {
		return nil
	}
	if _, _, taken := s.ownerLocked(owner.Login); taken {
		return nil
	}
	s.recordOwnerLocked(owner)
	return nil
}

func (s *fakeStore) RecordOwner(_ context.Context, owner faucet.Owner) error {
	if err := owner.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.repoWrites++
	if s.repoErr != nil {
		return s.repoErr
	}
	s.recordOwnerLocked(owner)
	return nil
}

func (s *fakeStore) ownerWriteCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ownerWrites
}

// recordOwnerLocked gives owner its login, which every other account gives
// up.
func (s *fakeStore) recordOwnerLocked(owner faucet.Owner) {
	s.ownerWrites++
	if s.owners == nil {
		s.owners = map[int64]fakeRepoOwner{}
	}
	maps.DeleteFunc(s.owners, func(id int64, o fakeRepoOwner) bool {
		return id != owner.ID && strings.EqualFold(o.login, owner.Login)
	})
	s.owners[owner.ID] = fakeRepoOwner{login: owner.Login}
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
	login, name, _ := strings.Cut(fullName, "/")
	id, owner, ok := s.ownerLocked(login)
	if !ok {
		return faucet.Repository{}, faucet.ErrUnknownRepository
	}
	var found *fakeRepo
	for i, r := range s.repos {
		_, held, _ := strings.Cut(r.repo.FullName, "/")
		if r.repo.OwnerID == id && strings.EqualFold(held, name) &&
			(found == nil || r.seenAt.After(found.seenAt) || r.seenAt.Equal(found.seenAt) && r.repo.ID > found.repo.ID) {
			found = &s.repos[i]
		}
	}
	if found == nil || found.quarantined {
		return faucet.Repository{}, faucet.ErrUnknownRepository
	}
	_, held, _ := strings.Cut(found.repo.FullName, "/")
	return faucet.Repository{ID: found.repo.ID, OwnerID: id, OwnerLogin: owner.login, FullName: owner.login + "/" + held}, nil
}

func (s *fakeStore) OwnerByLogin(_ context.Context, login string) (faucet.Owner, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.repoErr != nil {
		return faucet.Owner{}, s.repoErr
	}
	id, owner, ok := s.ownerLocked(login)
	if !ok {
		return faucet.Owner{}, faucet.ErrUnknownOwner
	}
	return faucet.Owner{ID: id, Login: owner.login}, nil
}

func (s *fakeStore) OwnerByID(_ context.Context, id int64) (faucet.Owner, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.repoErr != nil {
		return faucet.Owner{}, s.repoErr
	}
	o, ok := s.owners[id]
	if !ok {
		return faucet.Owner{}, faucet.ErrUnknownOwner
	}
	return faucet.Owner{ID: id, Login: o.login}, nil
}

// holdsLocked reports whether from names repo, as it was seen or under the
// login that its owner holds now.
func (s *fakeStore) holdsLocked(repo faucet.Repository, from string) bool {
	return strings.EqualFold(repo.FullName, from) || strings.EqualFold(s.currentNameLocked(repo), from)
}

// currentNameLocked is the full name of repo under the login that its owner
// holds now, and "" when the owner holds none.
func (s *fakeStore) currentNameLocked(repo faucet.Repository) string {
	o, ok := s.owners[repo.OwnerID]
	if !ok {
		return ""
	}
	_, name, _ := strings.Cut(repo.FullName, "/")
	return o.login + "/" + name
}

// ownerLocked returns the account that holds login, which is at most one.
func (s *fakeStore) ownerLocked(login string) (int64, fakeRepoOwner, bool) {
	for id, o := range s.owners {
		if strings.EqualFold(o.login, login) {
			return id, o, true
		}
	}
	return 0, fakeRepoOwner{}, false
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

	through := q.Through
	if through == 0 {
		through = math.MaxInt64
	}
	page := faucet.PollPage{Next: min(s.headLocked(q.Target), through), PurgedThrough: s.purgedThroughLocked(q.Target)}
	for _, e := range s.events {
		at := q.Target.PositionOf(e)
		if at > q.After && at <= through && matches(e, q.EventFilter) && len(page.Events) < int(q.Limit) {
			page.Events = append(page.Events, e)
		}
	}
	if len(page.Events) == int(q.Limit) {
		page.Next = q.Target.PositionOf(page.Events[len(page.Events)-1])
	}
	return page, nil
}

func (s *fakeStore) ScanEvents(_ context.Context, q faucet.ScanQuery) (faucet.ScanPage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return faucet.ScanPage{}, s.err
	}
	if err := q.Validate(); err != nil {
		return faucet.ScanPage{}, err
	}
	s.scans = append(s.scans, q)

	through := q.Through
	if through == 0 {
		through = math.MaxInt64
	}
	var page faucet.ScanPage
	for _, e := range s.events {
		if e.ID > q.After && e.ID <= through && matches(e, q.EventFilter) && len(page.Events) < int(q.Limit) {
			page.Events = append(page.Events, e)
		}
	}
	page.Next = max(min(int64(s.headLocked(faucet.AllTarget())), through), q.After)
	page.PurgedThrough = s.purgeMark
	if len(page.Events) == int(q.Limit) {
		page.Next = page.Events[len(page.Events)-1].ID
	}
	return page, nil
}

func matches(e faucet.Event, f faucet.EventFilter) bool {
	switch {
	case !f.Target.Covers(e.RepoID, e.OwnerID):
		return false
	case len(f.RepoNames) > 0 && !slices.Contains(f.RepoNames, strings.ToLower(e.RepoName)):
		return false
	case len(f.Types) > 0 && !slices.Contains(f.Types, e.Type):
		return false
	case f.HeadSHA != "" && e.HeadSHA != f.HeadSHA:
		return false
	case f.PR != 0 && !slices.Contains(e.PRs, f.PR):
		return false
	}
	return !slices.ContainsFunc(f.Fields, func(field faucet.FieldFilter) bool { return !passes(e, field) })
}

// fakeLine names the line of a repository or an owner.
type fakeLine struct {
	kind faucet.TargetKind
	id   int64
}

func lineOf(t faucet.Target) fakeLine {
	return fakeLine{kind: t.Kind(), id: t.RepoID() + t.OwnerID()}
}

// purgedThroughLocked is the purge mark of t's line.
func (s *fakeStore) purgedThroughLocked(t faucet.Target) faucet.Position {
	if t.Kind() == faucet.TargetAll {
		return faucet.Position(s.purgeMark)
	}
	return s.linePurges[lineOf(t)]
}

// positionThroughLocked is the position of t's line at event ID through, as
// faucet_line_position reads it: the newest position still stored at or below
// through, raised to the line's purge mark unless through is below the purge
// mark of the all target.
func (s *fakeStore) positionThroughLocked(t faucet.Target, through int64) faucet.Position {
	if t.Kind() == faucet.TargetAll {
		return faucet.Position(through)
	}
	var p faucet.Position
	if through >= s.purgeMark {
		p = s.purgedThroughLocked(t)
	}
	for _, e := range s.events {
		if e.ID <= through && t.Covers(e.RepoID, e.OwnerID) {
			p = max(p, t.PositionOf(e))
		}
	}
	return p
}

// headLocked is the newest position of t's line, which a purge may have
// deleted.
func (s *fakeStore) headLocked(t faucet.Target) faucet.Position {
	if t.Kind() != faucet.TargetAll {
		return s.lineHeads[lineOf(t)]
	}
	head := faucet.Position(s.purgeMark)
	for _, e := range s.events {
		head = max(head, faucet.Position(e.ID))
	}
	return head
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

func (s *fakeStore) PositionAt(_ context.Context, target faucet.Target, t time.Time) (faucet.Position, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return 0, s.err
	}
	s.cursorLookups = append(s.cursorLookups, t)
	p := s.purgedThroughLocked(target)
	for _, e := range s.events {
		if target.Covers(e.RepoID, e.OwnerID) && e.ReceivedAt.Before(t) {
			p = max(p, target.PositionOf(e))
		}
	}
	return p, nil
}

func (s *fakeStore) StartSubscription(_ context.Context, q faucet.StartQuery) (faucet.Start, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return faucet.Start{}, s.err
	}
	if err := q.Validate(); err != nil {
		return faucet.Start{}, err
	}
	target := q.Target
	floor := s.purgedThroughLocked(target)
	if !q.Since.IsZero() {
		for _, e := range s.events {
			if target.Covers(e.RepoID, e.OwnerID) && e.ReceivedAt.Before(q.Since) {
				floor = max(floor, target.PositionOf(e))
			}
		}
	}
	var start faucet.Start
	start.Position = floor
	if q.Cursor != nil {
		start.Position = max(*q.Cursor, floor)
		start.Truncated = *q.Cursor < floor
	}
	head := s.headLocked(target)
	start.Position = min(start.Position, head)
	start.After = int64(s.headLocked(faucet.AllTarget()))
	if start.Position < head {
		for _, e := range s.events {
			if target.Covers(e.RepoID, e.OwnerID) && target.PositionOf(e) > start.Position {
				start.After = max(e.ID-1, s.purgeMark)
				break
			}
		}
	}
	return start, nil
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
		s.raiseLinePurgeLocked(faucet.RepoTarget(e.RepoID), e)
		if e.OwnerID > 0 {
			s.raiseLinePurgeLocked(faucet.OwnerTarget(e.OwnerID), e)
		}
	}
	s.events = kept
	res.PurgedThrough = s.purgeMark
	return res, nil
}

func (s *fakeStore) raiseLinePurgeLocked(t faucet.Target, e faucet.Event) {
	if s.linePurges == nil {
		s.linePurges = map[fakeLine]faucet.Position{}
	}
	s.linePurges[lineOf(t)] = max(s.linePurges[lineOf(t)], t.PositionOf(e))
}

// advanceSubscriptionsThatLoseNothingLocked moves a subscription that is not
// below the purge mark yet to the mark that the purge will set (doomed), unless
// an event that its target covers, of one of its types, above its position and
// still in the log, lies at or below that mark.
func (s *fakeStore) advanceSubscriptionsThatLoseNothingLocked(doomed int64) {
	for key, sub := range s.subs {
		pos := s.scanned[key]
		if pos < s.purgeMark || pos >= doomed {
			continue
		}
		if !slices.ContainsFunc(s.events, func(e faucet.Event) bool {
			return sub.Target.Covers(e.RepoID, e.OwnerID) && slices.Contains(sub.EventTypes, e.Type) && e.ID > pos && e.ID <= doomed
		}) {
			s.scanned[key] = doomed
		}
	}
}

func (s *fakeStore) UpsertSubscription(_ context.Context, sub faucet.Subscription, now time.Time, maxLive int) (faucet.Subscription, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.upsertSubscriptionLocked(sub, now, maxLive, nil)
}

// upsertSubscriptionLocked runs check, when it is not nil, after the
// principal's limit passed and before the write.
func (s *fakeStore) upsertSubscriptionLocked(sub faucet.Subscription, now time.Time, maxLive int, check func() error) (faucet.Subscription, error) {
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
	if check != nil {
		if err := check(); err != nil {
			return faucet.Subscription{}, err
		}
	}
	sub.CreatedAt, sub.UpdatedAt = now, now
	sub.Status = faucet.DeliveryStatus{}
	sub.PreviousSecretEnc, sub.PreviousSecretUntil = nil, time.Time{}
	grace := sub.RotationGrace
	sub.RotationGrace = 0
	if row, ok := s.subs[sub.KeyHash]; ok {
		sub.CreatedAt, sub.Cursor, sub.Position, sub.Status = row.CreatedAt, row.Cursor, row.Position, row.Status
		if sub.Target != row.Target {
			sub.Position = s.positionThroughLocked(sub.Target, row.Cursor)
		}
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

func (s *fakeStore) scanQueries() []faucet.ScanQuery {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.scans)
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
