package oauth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-jose/go-jose/v4"
	"golang.org/x/xerrors"

	"github.com/coder/quartz"
)

const (
	// keySetLifetime is how long faucet keeps the keys of an issuer, so that
	// a key that the issuer drops stops working within this time. It does
	// not follow the cache headers: Vercel serves its key set with an Age
	// past its max-age.
	keySetLifetime = 5 * time.Minute
	// keyFetchInterval is the least time between two fetches of the keys of
	// one issuer, also when a token names a kid that the keys lack, so that
	// tokens with made-up kids cost one fetch a minute.
	keyFetchInterval = time.Minute
	// keyFetchTimeout bounds the fetch of a discovery document and of the
	// key set that it names, together.
	keyFetchTimeout = 5 * time.Second
	// maxCachedKeySets bounds how many issuers' keys faucet keeps. Faucet
	// fetches only the issuers of the rules that a client holds.
	maxCachedKeySets = 64
	discoveryPath    = "/.well-known/openid-configuration"
)

// errIssuerKeys is the error of an issuer whose keys faucet could not fetch.
// An error that wraps it names the issuer and holds the cause, which are for
// the log only.
var errIssuerKeys = xerrors.New("faucet could not fetch the keys of the subject token's issuer")

// keySource fetches the keys of issuers through their OpenID discovery
// documents, and keeps them. Callers that ask at once for an issuer whose
// keys it does not hold share one fetch.
type keySource struct {
	// client is the guarded client of faucet.NewHTTPClient.
	client  *http.Client
	clock   quartz.Clock
	mu      sync.Mutex
	sets    map[string]fetchedKeySet
	flights map[string]*keySetFlight
}

// fetchedKeySet is what faucet holds of an issuer's keys.
type fetchedKeySet struct {
	// keys are the keys of the last fetch that worked, which ended at
	// fetchedAt. A fetch that fails keeps them.
	keys      jose.JSONWebKeySet
	fetchedAt time.Time
	// err is the error of the last fetch, which ended at triedAt, or nil.
	err     error
	triedAt time.Time
}

// keysAt returns the keys while they last.
func (f fetchedKeySet) keysAt(now time.Time) jose.JSONWebKeySet {
	if now.Before(f.fetchedAt.Add(keySetLifetime)) {
		return f.keys
	}
	return jose.JSONWebKeySet{}
}

func (f fetchedKeySet) keptUntil() time.Time {
	keys, cooldown := f.fetchedAt.Add(keySetLifetime), f.triedAt.Add(keyFetchInterval)
	if cooldown.After(keys) {
		return cooldown
	}
	return keys
}

// keySetFlight is a fetch that callers share. keys and err are set before
// done closes.
type keySetFlight struct {
	done chan struct{}
	keys jose.JSONWebKeySet
	err  error
}

func newKeySource(client *http.Client, clock quartz.Clock) *keySource {
	return &keySource{client: client, clock: clock, sets: map[string]fetchedKeySet{}, flights: map[string]*keySetFlight{}}
}

// keySet returns the keys of issuer, for a token whose header names kid. It
// fetches them when the keys that it holds, which last keySetLifetime from
// the fetch that got them, lack kid, unless a fetch of issuer ended less than
// keyFetchInterval ago: then it returns the keys that it holds, or the error
// of that fetch. A fetch that fails keeps the keys. It starts no fetch once
// ctx has ended, and every error but the end of ctx wraps errIssuerKeys.
func (s *keySource) keySet(ctx context.Context, issuer, kid string) (jose.JSONWebKeySet, error) {
	s.mu.Lock()
	now := s.clock.Now()
	f := s.sets[issuer]
	keys := f.keysAt(now)
	switch {
	case len(keys.Key(kid)) > 0:
		s.mu.Unlock()
		return keys, nil
	case now.Before(f.triedAt.Add(keyFetchInterval)):
		s.mu.Unlock()
		return keys, f.err
	case ctx.Err() != nil:
		s.mu.Unlock()
		return jose.JSONWebKeySet{}, ctx.Err()
	}
	if fl, ok := s.flights[issuer]; ok {
		s.mu.Unlock()
		select {
		case <-fl.done:
			return fl.keys, fl.err
		case <-ctx.Done():
			return jose.JSONWebKeySet{}, ctx.Err()
		}
	}
	fl := &keySetFlight{done: make(chan struct{})}
	s.flights[issuer] = fl
	s.mu.Unlock()

	// The fetch has a timeout of its own, and every caller at once waits
	// for it, so a caller that leaves does not end it.
	keys, err := s.fetch(context.WithoutCancel(ctx), issuer)
	if err != nil {
		err = fmt.Errorf("%w %s: %w", errIssuerKeys, issuer, err)
	}
	s.mu.Lock()
	delete(s.flights, issuer)
	f = s.sets[issuer]
	f.err, f.triedAt = err, s.clock.Now()
	if err == nil {
		f.keys, f.fetchedAt = keys, f.triedAt
	}
	keep(s.sets, issuer, f, maxCachedKeySets, f.triedAt, fetchedKeySet.keptUntil)
	s.mu.Unlock()
	fl.keys, fl.err = keys, err
	close(fl.done)
	return keys, err
}

// fetch reads the discovery document of issuer (OpenID Connect Discovery
// 1.0 section 4), whose issuer must equal it byte for byte, and then the key
// set that the document names.
func (s *keySource) fetch(ctx context.Context, issuer string) (jose.JSONWebKeySet, error) {
	ctx, cancel := context.WithTimeout(ctx, keyFetchTimeout)
	defer cancel()
	body, err := s.get(ctx, strings.TrimSuffix(issuer, "/")+discoveryPath)
	if err != nil {
		return jose.JSONWebKeySet{}, xerrors.Errorf("the discovery document: %w", err)
	}
	var doc struct {
		Issuer  string `json:"issuer"`
		JWKSURI string `json:"jwks_uri"`
	}
	switch {
	case json.Unmarshal(body, &doc) != nil:
		return jose.JSONWebKeySet{}, xerrors.New("the discovery document must be a JSON object with a string issuer and jwks_uri")
	case doc.Issuer != issuer:
		return jose.JSONWebKeySet{}, xerrors.New("the discovery document names another issuer")
	case doc.JWKSURI == "":
		return jose.JSONWebKeySet{}, xerrors.New("the discovery document names no jwks_uri")
	}
	if body, err = s.get(ctx, doc.JWKSURI); err != nil {
		return jose.JSONWebKeySet{}, xerrors.Errorf("the key set: %w", err)
	}
	keys, _, err := parseKeySet(body)
	if err != nil {
		return jose.JSONWebKeySet{}, xerrors.Errorf("the key set %w", err)
	}
	return keys, nil
}

func (s *keySource) get(ctx context.Context, rawURL string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, xerrors.New("its URL is not valid")
	}
	req.Header.Set("Accept", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, lateOr(ctx, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, xerrors.Errorf("it must be served with status 200 and no redirect, not %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxKeyDocumentBytes+1))
	switch {
	case err != nil:
		return nil, lateOr(ctx, err)
	case len(body) > MaxKeyDocumentBytes:
		return nil, xerrors.Errorf("it must be at most %d bytes", MaxKeyDocumentBytes)
	}
	return body, nil
}

// lateOr is the error of a document that did not arrive before the deadline
// of ctx, or else err.
func lateOr(ctx context.Context, err error) error {
	if xerrors.Is(ctx.Err(), context.DeadlineExceeded) {
		return xerrors.Errorf("it must arrive within %s", keyFetchTimeout)
	}
	return err
}
