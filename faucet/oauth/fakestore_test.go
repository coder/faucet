package oauth_test

import (
	"context"
	"slices"
	"sync"
	"testing"

	"golang.org/x/xerrors"

	"github.com/coder/faucet/faucet/oauth"
	"github.com/coder/faucet/faucet/storetest"
)

type fakeStore struct {
	mu     sync.Mutex
	tokens map[oauth.TokenHash]oauth.AccessToken
	err    error
	reads  int
}

func newFakeStore() *fakeStore {
	return &fakeStore{tokens: map[oauth.TokenHash]oauth.AccessToken{}}
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

func (s *fakeStore) readCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reads
}

func TestFakeStore(t *testing.T) {
	t.Parallel()
	storetest.RunOAuth(t, func(*testing.T) oauth.Store { return newFakeStore() })
}
