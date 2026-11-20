package oauth_test

import (
	"bytes"
	"context"
	"encoding/json"
	"maps"
	"reflect"
	"slices"
	"strings"
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
	refresh     map[oauth.TokenHash]oauth.RefreshToken
	connections map[string]oauth.Connection
	// checks are the access checks. Their methods fail once their context
	// is done, as Postgres does, and with the error that failing holds for
	// their name.
	checks  map[accessCheckKey]oauth.AccessCheck
	failing map[string]error
	// stalls holds, for a method's name, how many more calls of it go
	// through before each call waits for its context to end, as a stalled
	// Postgres does.
	stalls map[string]int
	// connectionLocks holds a lock per connection ID, which RefreshConnection
	// holds while its callback runs and ApproveAuthRequest takes, as the row
	// lock does in Postgres. A caller takes it before mu, never while it
	// holds mu.
	connectionLocks map[string]*sync.Mutex
	trustRules      []fakeTrustRule
	lastTrustRuleID int64
	err             error
	reads           int
}

type fakeTrustRule struct {
	clientID string
	oauth.StoredTrustRule
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		tokens:          map[oauth.TokenHash]oauth.AccessToken{},
		requests:        map[string]oauth.AuthRequest{},
		sessions:        map[string]oauth.Session{},
		refresh:         map[oauth.TokenHash]oauth.RefreshToken{},
		connections:     map[string]oauth.Connection{},
		checks:          map[accessCheckKey]oauth.AccessCheck{},
		failing:         map[string]error{},
		stalls:          map[string]int{},
		connectionLocks: map[string]*sync.Mutex{},
	}
}

// accessCheckKey is what names an access check: a connection has one check
// per repository.
type accessCheckKey struct{ connectionID, repo string }

// fail makes each call of the method with this name fail with err, and nil
// makes the calls work again.
func (s *fakeStore) fail(method string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failing[method] = err
}

// stallAfter makes every call of the method with this name after the next
// n wait until its context ends, and fail with the context's error.
func (s *fakeStore) stallAfter(method string, n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stalls[method] = n
}

// callErr is the error that a call of the method with this name fails with:
// the error of ctx once it is done, or the one that fail set. A call that
// stallAfter stalls waits for ctx first.
func (s *fakeStore) callErr(ctx context.Context, method string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	left, stalls := s.stalls[method]
	if stalls && left > 0 {
		s.stalls[method] = left - 1
	}
	s.mu.Unlock()
	if stalls && left == 0 {
		<-ctx.Done()
		return ctx.Err()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.failing[method]
}

func cloneClient(c oauth.Client) oauth.Client {
	c.Grants, c.Scopes = slices.Clone(c.Grants), slices.Clone(c.Scopes)
	if c.SecretHash != nil {
		hash := *c.SecretHash
		c.SecretHash = &hash
	}
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

func (s *fakeStore) UpdateClient(_ context.Context, id string, u oauth.ClientUpdate) (oauth.Client, error) {
	if err := u.Validate(); err != nil {
		return oauth.Client{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	i := slices.IndexFunc(s.clients, func(c oauth.Client) bool { return c.ID == id && c.Live() })
	if i < 0 {
		return oauth.Client{}, oauth.ErrUnknownClient
	}
	if u.Grants != nil {
		s.clients[i].Grants = slices.Clone(u.Grants)
	}
	if u.Scopes != nil {
		s.clients[i].Scopes = slices.Clone(u.Scopes)
	}
	return cloneClient(s.clients[i]), nil
}

func (s *fakeStore) AddTrustRule(_ context.Context, clientID string, r oauth.TrustRule, now time.Time) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !slices.ContainsFunc(s.clients, func(c oauth.Client) bool { return c.ID == clientID }) {
		return 0, oauth.ErrUnknownClient
	}
	for _, held := range s.trustRules {
		if held.clientID == clientID && sameTrustRule(held.Rule, r) {
			return held.ID, nil
		}
	}
	s.lastTrustRuleID++
	s.trustRules = append(s.trustRules, fakeTrustRule{clientID, oauth.StoredTrustRule{ID: s.lastTrustRuleID, Rule: cloneTrustRule(r), CreatedAt: now}})
	return s.lastTrustRuleID, nil
}

func (s *fakeStore) TrustRules(_ context.Context, clientID string) ([]oauth.StoredTrustRule, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rules := []oauth.StoredTrustRule{}
	for _, held := range s.trustRules {
		if held.clientID == clientID {
			stored := held.StoredTrustRule
			stored.Rule = cloneTrustRule(stored.Rule)
			rules = append(rules, stored)
		}
	}
	return rules, nil
}

func (s *fakeStore) RemoveTrustRule(_ context.Context, clientID string, id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := slices.IndexFunc(s.trustRules, func(r fakeTrustRule) bool { return r.clientID == clientID && r.ID == id })
	if i < 0 {
		return oauth.ErrUnknownTrustRule
	}
	s.trustRules = slices.Delete(s.trustRules, i, i+1)
	return nil
}

func (s *fakeStore) CountTrustRules(context.Context) (map[string]int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	counts := map[string]int{}
	for _, held := range s.trustRules {
		counts[held.clientID]++
	}
	return counts, nil
}

// It compares numbers by their text, so 1 and 1.0 differ as in jsonb, but so
// do 1e2 and 100, which jsonb holds as one.
func sameTrustRule(a, b oauth.TrustRule) bool {
	return a.Issuer == b.Issuer && maps.Equal(a.Claims, b.Claims) && sameJSON(a.JWKS, b.JWKS)
}

func sameJSON(a, b json.RawMessage) bool {
	if len(a) == 0 || len(b) == 0 {
		return len(a) == len(b)
	}
	x, xOK := decodeJSON(a)
	y, yOK := decodeJSON(b)
	return xOK && yOK && reflect.DeepEqual(x, y)
}

func decodeJSON(text json.RawMessage) (any, bool) {
	d := json.NewDecoder(bytes.NewReader(text))
	d.UseNumber()
	var v any
	return v, d.Decode(&v) == nil
}

func cloneTrustRule(r oauth.TrustRule) oauth.TrustRule {
	r.Claims, r.JWKS = maps.Clone(r.Claims), bytes.Clone(r.JWKS)
	return r
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

func (s *fakeStore) DeleteAccessToken(_ context.Context, hash oauth.TokenHash) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.tokens, hash)
	return nil
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

func (s *fakeStore) ConsentAuthRequest(_ context.Context, r oauth.AuthRequest, now time.Time) error {
	if err := r.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	stored, ok := s.requests[r.ID]
	switch {
	case !r.ExpiresAt.After(now):
		return oauth.ErrUnknownAuthRequest
	case ok && (stored.State != oauth.AuthRequestPending || stored.BrowserHash != r.BrowserHash || !stored.ExpiresAt.After(now)):
		return oauth.ErrUnknownAuthRequest
	case s.requestHolds(r.ID, func(o oauth.AuthRequest) bool { return o.GitHubStateHash == r.GitHubStateHash }):
		return xerrors.New("duplicate GitHub state hash")
	case ok:
		stored.GitHubStateHash = r.GitHubStateHash
		s.requests[r.ID] = stored
	default:
		s.requests[r.ID] = cloneAuthRequest(r)
	}
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

func (s *fakeStore) ApproveAuthRequest(ctx context.Context, a oauth.Approval, alive func(current oauth.Connection) bool) error {
	if err := a.Validate(); err != nil {
		return err
	}
	id := a.Connection.ID
	l := s.connectionLock(id)
	l.Lock()
	defer l.Unlock()
	current, err := s.Connection(ctx, id)
	keep := err == nil && alive(current)
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.requests[a.RequestID]
	if !ok || r.State != oauth.AuthRequestPending || r.GitHubStateHash != a.GitHubState || !r.ExpiresAt.After(a.Now) {
		return oauth.ErrUnknownAuthRequest
	}
	if s.requestHolds(r.ID, func(o oauth.AuthRequest) bool { return o.CodeHash == a.Code }) {
		return xerrors.New("duplicate code hash")
	}
	s.connections[id] = cloneConnection(a.Connection)
	if !keep {
		for otherID, other := range s.requests {
			if other.ConnectionID == id && other.State == oauth.AuthRequestApproved {
				other.State, other.CodeHash, other.ConnectionID = oauth.AuthRequestExpired, oauth.TokenHash{}, ""
				s.requests[otherID] = other
			}
		}
		s.endConnectionSessions(id, a.Now)
	}
	r.State, r.CodeHash, r.ConnectionID, r.ExpiresAt = oauth.AuthRequestApproved, a.Code, id, a.ExpiresAt
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
	if err := s.checkNewTokens(red.Refresh, red.Access); err != nil {
		return oauth.Session{}, err
	}
	if _, ok := s.sessions[r.ID]; ok {
		return oauth.Session{}, xerrors.New("duplicate session ID")
	}
	session := oauth.Session{
		ID: r.ID, ClientID: r.ClientID, ClientName: r.ClientName, ConnectionID: r.ConnectionID,
		Resource: r.Resource, Scopes: slices.Clone(r.Scopes), RefreshedAt: red.Now,
	}
	r.State = oauth.AuthRequestRedeemed
	s.requests[r.ID] = r
	s.sessions[r.ID] = session
	s.refresh[red.Refresh] = oauth.RefreshToken{Hash: red.Refresh, SessionID: r.ID, IssuedAt: red.Now}
	s.tokens[red.Access] = oauth.AccessToken{
		Hash: red.Access, ExpiresAt: red.AccessExpiresAt, Scopes: slices.Clone(r.Scopes),
		Resource: r.Resource, SubjectKind: oauth.SubjectUser, Subject: r.ID,
	}
	return cloneSession(session), nil
}

// checkNewTokens refuses a refresh hash that a refresh token holds, and an
// access hash that an access token holds, as the primary keys in Postgres
// do.
func (s *fakeStore) checkNewTokens(refresh, access oauth.TokenHash) error {
	if _, ok := s.refresh[refresh]; ok {
		return xerrors.New("duplicate refresh token hash")
	}
	if _, ok := s.tokens[access]; ok {
		return xerrors.New("duplicate token hash")
	}
	return nil
}

func (s *fakeStore) SessionByRefreshToken(_ context.Context, refresh oauth.TokenHash) (oauth.Session, oauth.RefreshToken, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	token, ok := s.refresh[refresh]
	if !ok {
		return oauth.Session{}, oauth.RefreshToken{}, oauth.ErrUnknownSession
	}
	return cloneSession(s.sessions[token.SessionID]), token, nil
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
	presented, ok := s.refresh[r.Presented]
	session := s.sessions[presented.SessionID]
	if !ok || !session.Live() || !holdsAll(session, r) || presented.Retired() || presented.Used() && !presented.UsedAt.After(r.UsedAfter) {
		return oauth.Session{}, oauth.ErrUnknownSession
	}
	if err := s.checkNewTokens(r.Next, r.Access); err != nil {
		return oauth.Session{}, err
	}
	for hash, o := range s.refresh {
		if o.SessionID == session.ID && hash != r.Presented && !o.Used() && !o.Retired() && !o.IssuedAt.After(r.UsedAfter) {
			o.RetiredAt = r.Now
			s.refresh[hash] = o
		}
	}
	if !presented.Used() {
		presented.UsedAt = r.Now
		s.refresh[r.Presented] = presented
	}
	s.refresh[r.Next] = oauth.RefreshToken{Hash: r.Next, SessionID: session.ID, IssuedAt: r.Now}
	if r.Now.After(session.RefreshedAt) {
		session.RefreshedAt = r.Now
	}
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
		s.endConnectionSessions(id, now)
	}
	return cloneConnection(s.connections[id]), nil
}

// endConnectionSessions ends at now the live sessions of the connection
// with this ID. The caller holds s.mu.
func (s *fakeStore) endConnectionSessions(id string, now time.Time) {
	for sessionID, session := range s.sessions {
		if session.ConnectionID == id && session.Live() {
			session.EndedAt = now
			s.sessions[sessionID] = session
		}
	}
}

func (s *fakeStore) DeleteEndedSessions(_ context.Context, now, idleSince time.Time) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var n int64
	for id, session := range s.sessions {
		if session.Live() && session.RefreshedAt.After(idleSince) || !session.Live() && session.EndedAt.After(now) {
			continue
		}
		delete(s.sessions, id)
		for hash, token := range s.refresh {
			if token.SessionID == id {
				delete(s.refresh, hash)
			}
		}
		n++
	}
	return n, nil
}

func (s *fakeStore) DeleteUnusedConnections(_ context.Context, updatedBefore time.Time) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var n int64
	for id, c := range s.connections {
		named := slices.ContainsFunc(slices.Collect(maps.Values(s.sessions)), func(o oauth.Session) bool { return o.ConnectionID == id }) ||
			slices.ContainsFunc(slices.Collect(maps.Values(s.requests)), func(r oauth.AuthRequest) bool { return r.ConnectionID == id })
		if !named && c.UpdatedAt.Before(updatedBefore) {
			delete(s.connections, id)
			maps.DeleteFunc(s.checks, func(k accessCheckKey, _ oauth.AccessCheck) bool { return k.connectionID == id })
			n++
		}
	}
	return n, nil
}

func (s *fakeStore) DeleteExpiredAuthRequests(_ context.Context, now, codesBefore time.Time) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var n int64
	for id, r := range s.requests {
		coded := r.CodeHash != oauth.TokenHash{}
		if !coded && !r.ExpiresAt.After(now) || coded && r.ExpiresAt.Before(codesBefore) {
			delete(s.requests, id)
			n++
		}
	}
	return n, nil
}

func (s *fakeStore) FreshAccessCheck(ctx context.Context, sessionID, repo string, now, usedBefore time.Time) (int64, bool, error) {
	if err := s.callErr(ctx, "FreshAccessCheck"); err != nil {
		return 0, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	session, ok := s.sessions[sessionID]
	if !ok || !session.Live() {
		return 0, false, nil
	}
	key := accessCheckKey{session.ConnectionID, repo}
	c, ok := s.checks[key]
	if !ok || c.Denied() || c.CheckedAt.Before(session.RefreshedAt) {
		return 0, false, nil
	}
	if c.LastUsedAt.Before(usedBefore) && now.After(c.LastUsedAt) {
		c.LastUsedAt = now
	}
	s.checks[key] = c
	return c.RepoID, true, nil
}

func (s *fakeStore) StoreAccessCheck(ctx context.Context, c oauth.AccessCheck) error {
	if err := s.callErr(ctx, "StoreAccessCheck"); err != nil {
		return err
	}
	if err := c.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.connections[c.ConnectionID]; !ok {
		return oauth.ErrUnknownConnection
	}
	key := accessCheckKey{c.ConnectionID, c.Repo}
	if stored, ok := s.checks[key]; ok && c.Denied() && !stored.Denied() {
		for k, alias := range s.checks {
			if k.connectionID == c.ConnectionID && k.repo != c.Repo && alias.RepoID == stored.RepoID && !alias.CheckedAt.After(c.CheckedAt) {
				alias.RepoID, alias.CheckedAt = 0, c.CheckedAt
				s.checks[k] = alias
			}
		}
	}
	if stored, ok := s.checks[key]; ok {
		if stored.CheckedAt.After(c.CheckedAt) || stored.CheckedAt.Equal(c.CheckedAt) && !c.Denied() {
			c.RepoID, c.CheckedAt = stored.RepoID, stored.CheckedAt
		}
		if stored.LastUsedAt.After(c.LastUsedAt) {
			c.LastUsedAt = stored.LastUsedAt
		}
	}
	s.checks[key] = c
	return nil
}

func (s *fakeStore) RecentAccessChecks(ctx context.Context, connectionID string, usedSince time.Time, limit int) ([]oauth.AccessCheck, error) {
	if err := s.callErr(ctx, "RecentAccessChecks"); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var checks []oauth.AccessCheck
	for key, c := range s.checks {
		if key.connectionID == connectionID && !c.Denied() && !c.LastUsedAt.Before(usedSince) {
			checks = append(checks, c)
		}
	}
	slices.SortFunc(checks, func(a, b oauth.AccessCheck) int {
		if c := b.LastUsedAt.Compare(a.LastUsedAt); c != 0 {
			return c
		}
		return strings.Compare(a.Repo, b.Repo)
	})
	return checks[:min(len(checks), max(limit, 0))], nil
}

func (s *fakeStore) DeleteUnusedAccessChecks(ctx context.Context, usedBefore, deniedBefore time.Time) (int64, error) {
	if err := s.callErr(ctx, "DeleteUnusedAccessChecks"); err != nil {
		return 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	n := len(s.checks)
	maps.DeleteFunc(s.checks, func(_ accessCheckKey, c oauth.AccessCheck) bool {
		return c.LastUsedAt.Before(usedBefore) || c.Denied() && c.CheckedAt.Before(deniedBefore)
	})
	return int64(n - len(s.checks)), nil
}

func TestFakeStore(t *testing.T) {
	t.Parallel()
	storetest.RunOAuth(t, func(*testing.T) oauth.Store { return newFakeStore() })
}
