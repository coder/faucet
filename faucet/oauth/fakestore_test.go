package oauth_test

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"golang.org/x/xerrors"

	"github.com/coder/faucet/faucet/oauth"
	"github.com/coder/faucet/faucet/storetest"
)

type fakeStore struct {
	mu      sync.Mutex
	tokens  map[oauth.TokenHash]oauth.AccessToken
	clients []oauth.Client
	err     error
	reads   int
}

func newFakeStore() *fakeStore {
	return &fakeStore{tokens: map[oauth.TokenHash]oauth.AccessToken{}}
}

func cloneClient(c oauth.Client) oauth.Client {
	c.Grants, c.Scopes = slices.Clone(c.Grants), slices.Clone(c.Scopes)
	return c
}

func (s *fakeStore) InsertClient(_ context.Context, c oauth.Client) error {
	if err := c.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if slices.ContainsFunc(s.clients, func(o oauth.Client) bool { return o.ID == c.ID }) {
		return oauth.ErrClientExists
	}
	s.clients = append(s.clients, cloneClient(c))
	return nil
}

func (s *fakeStore) Client(_ context.Context, id string) (oauth.Client, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return oauth.Client{}, s.err
	}
	i := slices.IndexFunc(s.clients, func(c oauth.Client) bool { return c.ID == id })
	if i < 0 {
		return oauth.Client{}, oauth.ErrUnknownClient
	}
	return cloneClient(s.clients[i]), nil
}

func (s *fakeStore) Clients(context.Context) ([]oauth.Client, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	all := make([]oauth.Client, len(s.clients))
	for i, c := range s.clients {
		all[i] = cloneClient(c)
	}
	slices.SortStableFunc(all, func(a, b oauth.Client) int { return a.CreatedAt.Compare(b.CreatedAt) })
	return all, nil
}

func (s *fakeStore) DisableClient(_ context.Context, id string, now time.Time) (oauth.Client, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := slices.IndexFunc(s.clients, func(c oauth.Client) bool { return c.ID == id })
	if i < 0 {
		return oauth.Client{}, oauth.ErrUnknownClient
	}
	if s.clients[i].DisabledAt.IsZero() {
		s.clients[i].DisabledAt = now
	}
	return cloneClient(s.clients[i]), nil
}

func (s *fakeStore) InsertAccessToken(_ context.Context, t oauth.AccessToken) error {
	if err := t.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.tokens[t.Hash]; ok {
		return xerrors.New("duplicate token hash")
	}
	t.Scopes = slices.Clone(t.Scopes)
	s.tokens[t.Hash] = t
	return nil
}

func (s *fakeStore) AccessToken(_ context.Context, hash oauth.TokenHash) (oauth.AccessToken, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reads++
	if s.err != nil {
		return oauth.AccessToken{}, s.err
	}
	t, ok := s.tokens[hash]
	if !ok {
		return oauth.AccessToken{}, oauth.ErrUnknownToken
	}
	t.Scopes = slices.Clone(t.Scopes)
	return t, nil
}

func (s *fakeStore) DeleteExpiredAccessTokens(_ context.Context, now time.Time) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var n int64
	for hash, t := range s.tokens {
		if !t.ExpiresAt.After(now) {
			delete(s.tokens, hash)
			n++
		}
	}
	return n, nil
}

func (s *fakeStore) tokenCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.tokens)
}

func (s *fakeStore) readCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reads
}

func TestFakeStore(t *testing.T) {
	t.Parallel()
	storetest.RunOAuth(t, func(*testing.T) oauth.Store { return newFakeStore() })
}
