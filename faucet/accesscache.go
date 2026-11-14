package faucet

import (
	"context"
	"crypto/sha256"
	"strings"
	"sync"
	"time"

	"golang.org/x/xerrors"

	"github.com/coder/quartz"
)

const (
	accessCacheTTL        = time.Minute
	maxAccessCacheEntries = 10000
)

// tokenChecker is what AccessCache wraps. TokenAuthorizer is one.
type tokenChecker interface {
	Authorizer
	Authenticate(ctx context.Context, token string) (Principal, error)
}

// AccessCache remembers the answers of a TokenAuthorizer for one minute, so
// that each events/poll and wait_for_github_events call does not spend two
// GitHub API calls of the caller's quota (GET /user and GET /repos/{owner}/{name}).
// Use it as the Authenticate step of your server and as Deps.Authorizer.
//
// Only a success is remembered. A denial, a rejected token, a rate limit and any
// other error ask GitHub again on the next call, so a new grant takes effect at
// once. A grant that GitHub withdraws keeps working for up to a minute, and an
// entry is never extended by use. The cache holds a SHA-256 of the token as its
// key and never the token. It keeps at most 10,000 entries and holds them per
// process. It ignores the case of a repository name, like GitHub does.
//
// events/subscribe does not use the remembered answer: a renewal calls
// RepoIDUncached, because every renewal must check access again. A denial that
// a renewal finds leaves a marker for a minute, and a lookup of any path that
// finds the token rejected leaves one in place of the remembered login. Answers
// are ordered by the start of their lookup, not by when GitHub looked: a marker
// keeps a grant from an older lookup, still in flight, from being stored, and it
// replaces a grant only when that grant's lookup started before the denial's.
type AccessCache struct {
	inner tokenChecker
	clock quartz.Clock

	mu        sync.Mutex
	entries   map[accessKey]accessEntry
	nextSweep time.Time
}

type accessKey struct {
	token [sha256.Size]byte
	// repo is empty for the answer of Authenticate. It is in lower case,
	// because GitHub ignores the case of a repository name.
	repo string
}

// accessEntry is a remembered answer. started is when its lookup began, and
// denied marks a denial that only keeps an older lookup from being stored.
type accessEntry struct {
	login   string
	repoID  int64
	started time.Time
	expires time.Time
	denied  bool
}

var _ Authorizer = (*AccessCache)(nil)

func NewAccessCache(inner tokenChecker, clock quartz.Clock) *AccessCache {
	return &AccessCache{inner: inner, clock: clock, entries: map[accessKey]accessEntry{}}
}

// Authenticate implements the authenticator of the server. A cached login is
// returned with the token of this call.
func (c *AccessCache) Authenticate(ctx context.Context, token string) (Principal, error) {
	key := keyOf(token, "")
	if e, ok := c.lookup(key); ok {
		return NewPrincipal(e.login, token)
	}
	started := c.clock.Now()
	p, err := c.inner.Authenticate(ctx, token)
	if err != nil {
		return Principal{}, err
	}
	c.store(key, accessEntry{login: p.Login}, started)
	return p, nil
}

func (c *AccessCache) RepoID(ctx context.Context, p Principal, repo string) (int64, error) {
	if p.token != "" && repo != "" {
		if e, ok := c.lookup(keyOf(p.token, repo)); ok {
			return e.repoID, nil
		}
	}
	id, _, err := c.ask(ctx, p, repo)
	return id, err
}

// RepoIDUncached asks the wrapped authorizer and remembers a success. A denial
// leaves a marker in place of what was remembered, so a poll right after a
// renewal that lost access is refused.
func (c *AccessCache) RepoIDUncached(ctx context.Context, p Principal, repo string) (int64, error) {
	id, started, err := c.ask(ctx, p, repo)
	if p.token != "" && (xerrors.Is(err, ErrNoAccess) || xerrors.Is(err, ErrInvalidToken)) {
		c.deny(started, keyOf(p.token, repo))
	}
	return id, err
}

// ask puts the question to the wrapped authorizer and remembers a success. A
// rejected token leaves a marker in place of its remembered login, on every
// path. It never asks about an empty repo, whose key is the key of the
// remembered login.
func (c *AccessCache) ask(ctx context.Context, p Principal, repo string) (int64, time.Time, error) {
	if repo == "" {
		return 0, time.Time{}, xerrors.New("repo must be written owner/name")
	}
	started := c.clock.Now()
	id, err := c.inner.RepoID(ctx, p, repo)
	switch {
	case p.token == "":
	case err == nil:
		c.store(keyOf(p.token, repo), accessEntry{repoID: id}, started)
	case xerrors.Is(err, ErrInvalidToken):
		c.deny(started, keyOf(p.token, ""))
	}
	return id, started, err
}

func keyOf(token, repo string) accessKey {
	return accessKey{token: sha256.Sum256([]byte(token)), repo: strings.ToLower(repo)}
}

func (c *AccessCache) lookup(key accessKey) (accessEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	return e, ok && !e.denied && e.expires.After(c.clock.Now())
}

// store keeps the answer of a lookup that began at started, unless the key holds
// a denial from a lookup that began at the same time or later.
func (c *AccessCache) store(key accessKey, e accessEntry, started time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if old, ok := c.entries[key]; ok && old.denied && !started.After(old.started) {
		return
	}
	c.put(key, e, started)
}

// deny marks each key as denied, except a key that holds the answer of a lookup
// that began after this one.
func (c *AccessCache) deny(started time.Time, keys ...accessKey) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, key := range keys {
		if old, ok := c.entries[key]; !ok || !old.started.After(started) {
			c.put(key, accessEntry{denied: true}, started)
		}
	}
}

// put counts the minute from started, the start of the lookup, so a slow GitHub
// answer does not stretch what it says. It drops the expired entries once a
// minute and whenever the cache is full, and when it is still full it keeps the
// answer to itself. The caller holds the lock.
func (c *AccessCache) put(key accessKey, e accessEntry, started time.Time) {
	now := c.clock.Now()
	if !now.Before(c.nextSweep) || len(c.entries) >= maxAccessCacheEntries {
		for k, old := range c.entries {
			if !old.expires.After(now) {
				delete(c.entries, k)
			}
		}
		c.nextSweep = now.Add(accessCacheTTL)
	}
	if _, known := c.entries[key]; !known && len(c.entries) >= maxAccessCacheEntries {
		return
	}
	e.started, e.expires = started, started.Add(accessCacheTTL)
	c.entries[key] = e
}

type uncachedAuthorizer interface {
	RepoIDUncached(ctx context.Context, p Principal, repo string) (int64, error)
}
