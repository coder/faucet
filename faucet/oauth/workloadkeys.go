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
// An error that wraps it holds the cause, which is for the log only.
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

// fetchedKeySet is the outcome of the last fetch of an issuer's keys.
type fetchedKeySet struct {
	keys jose.JSONWebKeySet
	err  error
	at   time.Time
}

func (f fetchedKeySet) keptUntil() time.Time {
	if f.err != nil {
		return f.at.Add(keyFetchInterval)
	}
	return f.at.Add(keySetLifetime)
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
// fetches them when it holds none, or when the ones it holds lack kid, unless
// a fetch of issuer ended less than keyFetchInterval ago: then it returns what
// that fetch got, failure included. Every error but the end of ctx wraps
// errIssuerKeys.
func (s *keySource) keySet(ctx context.Context, issuer, kid string) (jose.JSONWebKeySet, error) {
	s.mu.Lock()
	now := s.clock.Now()
	if f, ok := s.sets[issuer]; ok && now.Before(f.keptUntil()) &&
		(now.Before(f.at.Add(keyFetchInterval)) || len(f.keys.Key(kid)) > 0) {
		s.mu.Unlock()
		return f.keys, f.err
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
		err = fmt.Errorf("%w: %w", errIssuerKeys, err)
	}
	s.mu.Lock()
	delete(s.flights, issuer)
	f := fetchedKeySet{keys: keys, err: err, at: s.clock.Now()}
	keep(s.sets, issuer, f, maxCachedKeySets, f.at, fetchedKeySet.keptUntil)
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
	keys, err := parseKeySet(body)
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
		if xerrors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, xerrors.Errorf("it must arrive within %s", keyFetchTimeout)
		}
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, xerrors.Errorf("it must be served with status 200 and no redirect, not %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxKeyDocumentBytes+1))
	switch {
	case err != nil:
		return nil, err
	case len(body) > maxKeyDocumentBytes:
		return nil, xerrors.Errorf("it must be at most %d bytes", maxKeyDocumentBytes)
	}
	return body, nil
}
