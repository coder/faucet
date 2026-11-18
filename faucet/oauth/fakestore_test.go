package oauth_test

import (
	"bytes"
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
	mu          sync.Mutex
	tokens      map[oauth.TokenHash]oauth.AccessToken
	clients     []oauth.Client
	requests    map[string]oauth.AuthRequest
	sessions    map[string]oauth.Session
	connections map[string]oauth.Connection
	// connectionLocks holds a lock per connection ID, which RefreshConnection
	// holds while its callback runs and ApproveAuthRequest takes, as the row
	// lock does in Postgres. A caller takes it before mu, never while it
	// holds mu.
	connectionLocks map[string]*sync.Mutex
	err             error
	reads           int
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		tokens:          map[oauth.TokenHash]oauth.AccessToken{},
		requests:        map[string]oauth.AuthRequest{},
		sessions:        map[string]oauth.Session{},
		connections:     map[string]oauth.Connection{},
		connectionLocks: map[string]*sync.Mutex{},
	}
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

func cloneAuthRequest(r oauth.AuthRequest) oauth.AuthRequest {
	r.Scopes = slices.Clone(r.Scopes)
	return r
}

func cloneSession(s oauth.Session) oauth.Session {
	s.Scopes = slices.Clone(s.Scopes)
	return s
}

func cloneConnection(c oauth.Connection) oauth.Connection {
	c.AccessToken, c.RefreshToken = bytes.Clone(c.AccessToken), bytes.Clone(c.RefreshToken)
	return c
}

func (s *fakeStore) InsertAuthRequest(_ context.Context, r oauth.AuthRequest) error {
	if err := r.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.requests[r.ID]; ok {
		return xerrors.New("duplicate authorization request ID")
	}
	s.requests[r.ID] = cloneAuthRequest(r)
	return nil
}

func (s *fakeStore) ConsentAuthRequest(_ context.Context, id string, browser, githubState oauth.TokenHash, now time.Time) error {
	if githubState == (oauth.TokenHash{}) {
		return xerrors.New("a consent needs the hash of the GitHub state")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.requests[id]
	if !ok || r.State != oauth.AuthRequestPending || r.BrowserHash != browser || !r.ExpiresAt.After(now) {
		return oauth.ErrUnknownAuthRequest
	}
	if s.requestHolds(id, func(o oauth.AuthRequest) bool { return o.GitHubStateHash == githubState }) {
		return xerrors.New("duplicate GitHub state hash")
	}
	r.GitHubStateHash = githubState
	s.requests[id] = r
	return nil
}

// requestHolds reports whether a request other than the one with this ID
// matches, as a unique index would.
func (s *fakeStore) requestHolds(id string, match func(oauth.AuthRequest) bool) bool {
	for otherID, o := range s.requests {
		if otherID != id && match(o) {
			return true
		}
	}
	return false
}

func (s *fakeStore) ExpireAuthRequest(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.requests[id]; ok && r.State == oauth.AuthRequestPending {
		r.State = oauth.AuthRequestExpired
		s.requests[id] = r
	}
	return nil
}

func (s *fakeStore) AuthRequestByGitHubState(_ context.Context, githubState oauth.TokenHash) (oauth.AuthRequest, error) {
	return s.requestWith(githubState, func(r oauth.AuthRequest) oauth.TokenHash { return r.GitHubStateHash })
}

func (s *fakeStore) AuthRequestByCode(_ context.Context, code oauth.TokenHash) (oauth.AuthRequest, error) {
	return s.requestWith(code, func(r oauth.AuthRequest) oauth.TokenHash { return r.CodeHash })
}

// requestWith finds the request whose hash is h. A zero hash is one that a
// request does not hold yet, so it finds none.
func (s *fakeStore) requestWith(h oauth.TokenHash, hash func(oauth.AuthRequest) oauth.TokenHash) (oauth.AuthRequest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if h == (oauth.TokenHash{}) {
		return oauth.AuthRequest{}, oauth.ErrUnknownAuthRequest
	}
	for _, r := range s.requests {
		if hash(r) == h {
			return cloneAuthRequest(r), nil
		}
	}
	return oauth.AuthRequest{}, oauth.ErrUnknownAuthRequest
}

func (s *fakeStore) connectionLock(id string) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, ok := s.connectionLocks[id]
	if !ok {
		l = &sync.Mutex{}
		s.connectionLocks[id] = l
	}
	return l
}

func (s *fakeStore) ApproveAuthRequest(_ context.Context, a oauth.Approval) error {
	if err := a.Validate(); err != nil {
		return err
	}
	l := s.connectionLock(a.Connection.ID)
	l.Lock()
	defer l.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.requests[a.RequestID]
	if !ok || r.State != oauth.AuthRequestPending || r.GitHubStateHash != a.GitHubState || !r.ExpiresAt.After(a.Now) {
		return oauth.ErrUnknownAuthRequest
	}
	if s.requestHolds(r.ID, func(o oauth.AuthRequest) bool { return o.CodeHash == a.Code }) {
		return xerrors.New("duplicate code hash")
	}
	s.connections[a.Connection.ID] = cloneConnection(a.Connection)
	r.State, r.CodeHash, r.ConnectionID, r.ExpiresAt = oauth.AuthRequestApproved, a.Code, a.Connection.ID, a.ExpiresAt
	s.requests[r.ID] = r
	return nil
}

func (s *fakeStore) RedeemAuthRequest(_ context.Context, red oauth.Redemption) (oauth.Session, error) {
	if err := red.Validate(); err != nil {
		return oauth.Session{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var r oauth.AuthRequest
	found := false
	for _, o := range s.requests {
		if o.CodeHash == red.Code && o.State == oauth.AuthRequestApproved && o.ExpiresAt.After(red.Now) {
			r, found = o, true
		}
	}
	if !found {
		return oauth.Session{}, oauth.ErrUnknownAuthRequest
	}
	if err := s.checkNewTokens(r.ID, red.Refresh, red.Access); err != nil {
		return oauth.Session{}, err
	}
	if _, ok := s.sessions[r.ID]; ok {
		return oauth.Session{}, xerrors.New("duplicate session ID")
	}
	session := oauth.Session{
		ID: r.ID, ClientID: r.ClientID, ClientName: r.ClientName, ConnectionID: r.ConnectionID,
		Resource: r.Resource, Scopes: slices.Clone(r.Scopes), RefreshHash: red.Refresh, RefreshedAt: red.Now,
	}
	r.State = oauth.AuthRequestRedeemed
	s.requests[r.ID] = r
	s.sessions[r.ID] = session
	s.tokens[red.Access] = oauth.AccessToken{
		Hash: red.Access, ExpiresAt: red.AccessExpiresAt, Scopes: slices.Clone(r.Scopes),
		Resource: r.Resource, SubjectKind: oauth.SubjectUser, Subject: r.ID,
	}
	return cloneSession(session), nil
}

// checkNewTokens refuses a refresh hash that a session other than id holds
// as its current one, and an access hash that a token holds, as the unique
// indexes in Postgres do.
func (s *fakeStore) checkNewTokens(id string, refresh, access oauth.TokenHash) error {
	for otherID, o := range s.sessions {
		if otherID != id && o.RefreshHash == refresh {
			return xerrors.New("duplicate refresh token hash")
		}
	}
	if _, ok := s.tokens[access]; ok {
		return xerrors.New("duplicate token hash")
	}
	return nil
}

func (s *fakeStore) SessionByRefreshToken(_ context.Context, refresh oauth.TokenHash) (oauth.Session, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if refresh == (oauth.TokenHash{}) {
		return oauth.Session{}, false, oauth.ErrUnknownSession
	}
	var previous []oauth.Session
	for _, session := range s.sessions {
		switch refresh {
		case session.RefreshHash:
			return cloneSession(session), true, nil
		case session.PreviousRefreshHash:
			previous = append(previous, session)
		}
	}
	if len(previous) == 0 {
		return oauth.Session{}, false, oauth.ErrUnknownSession
	}
	return cloneSession(previous[0]), false, nil
}

func (s *fakeStore) Session(_ context.Context, id string) (oauth.Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	session, ok := s.sessions[id]
	if !ok {
		return oauth.Session{}, oauth.ErrUnknownSession
	}
	return cloneSession(session), nil
}

// holdsAll reports whether the session holds every scope of the rotation's
// access token.
func holdsAll(session oauth.Session, r oauth.Rotation) bool {
	for _, scope := range r.Scopes {
		if !slices.Contains(session.Scopes, scope) {
			return false
		}
	}
	return true
}

func (s *fakeStore) RotateSession(_ context.Context, r oauth.Rotation) (oauth.Session, error) {
	if err := r.Validate(); err != nil {
		return oauth.Session{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var session oauth.Session
	found := false
	for _, o := range s.sessions {
		if o.RefreshHash == r.Presented && o.Live() && holdsAll(o, r) {
			session, found = o, true
		}
	}
	if !found {
		return oauth.Session{}, oauth.ErrUnknownSession
	}
	if err := s.checkNewTokens(session.ID, r.Next, r.Access); err != nil {
		return oauth.Session{}, err
	}
	session.PreviousRefreshHash, session.RefreshHash, session.RefreshedAt = r.Presented, r.Next, r.Now
	s.sessions[session.ID] = session
	s.tokens[r.Access] = oauth.AccessToken{
		Hash: r.Access, ExpiresAt: r.AccessExpiresAt, Scopes: slices.Clone(r.Scopes),
		Resource: session.Resource, SubjectKind: oauth.SubjectUser, Subject: session.ID,
	}
	return cloneSession(session), nil
}

func (s *fakeStore) EndSession(_ context.Context, id string, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if session, ok := s.sessions[id]; ok && session.Live() {
		session.EndedAt = now
		s.sessions[id] = session
	}
	return nil
}

func (s *fakeStore) Connection(_ context.Context, id string) (oauth.Connection, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.connections[id]
	if !ok {
		return oauth.Connection{}, oauth.ErrUnknownConnection
	}
	return cloneConnection(c), nil
}

func (s *fakeStore) RefreshConnection(ctx context.Context, id string, now time.Time, refresh func(current oauth.Connection) (oauth.ConnectionChange, error)) (oauth.Connection, error) {
	l := s.connectionLock(id)
	l.Lock()
	defer l.Unlock()
	current, err := s.Connection(ctx, id)
	if err != nil {
		return oauth.Connection{}, err
	}
	change, err := refresh(current)
	if err != nil {
		return oauth.Connection{}, err
	}
	if err := change.Validate(id); err != nil {
		return oauth.Connection{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch change.Kind {
	case oauth.ReplaceConnection:
		s.connections[id] = cloneConnection(change.Connection)
	case oauth.EndConnectionSessions:
		for sessionID, session := range s.sessions {
			if session.ConnectionID == id && session.Live() {
				session.EndedAt = now
				s.sessions[sessionID] = session
			}
		}
	}
	return cloneConnection(s.connections[id]), nil
}

func (s *fakeStore) DeleteExpiredAuthRequests(_ context.Context, before time.Time) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var n int64
	for id, r := range s.requests {
		if r.ExpiresAt.Before(before) {
			delete(s.requests, id)
			n++
		}
	}
	return n, nil
}

func TestFakeStore(t *testing.T) {
	t.Parallel()
	storetest.RunOAuth(t, func(*testing.T) oauth.Store { return newFakeStore() })
}
