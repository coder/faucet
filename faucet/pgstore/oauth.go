package pgstore

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"golang.org/x/xerrors"

	"github.com/coder/superblink-events/faucet"
	"github.com/coder/superblink-events/faucet/oauth"
	"github.com/coder/superblink-events/faucet/pgstore/database"
)

var _ oauth.Store = (*Store)(nil)

// InsertAccessToken implements oauth.Store. A second token with the same hash
// fails on the primary key.
func (s *Store) InsertAccessToken(ctx context.Context, t oauth.AccessToken) (err error) {
	defer func() { err = endedBy(ctx, err) }()
	if err := t.Validate(); err != nil {
		return xerrors.Errorf("validate access token: %w", err)
	}
	if err := s.q.InsertAccessToken(ctx, database.InsertAccessTokenParams{
		TokenHash:   t.Hash[:],
		ExpiresAt:   t.ExpiresAt,
		Scopes:      scopeNames(t.Scopes),
		Resource:    t.Resource,
		SubjectKind: string(t.SubjectKind),
		Subject:     t.Subject,
	}); err != nil {
		return xerrors.Errorf("insert access token: %w", err)
	}
	return nil
}

// AccessToken implements oauth.Store. It returns the row as it is stored: a
// scope or a subject kind that this build does not know is the verifier's to
// reject.
func (s *Store) AccessToken(ctx context.Context, hash oauth.TokenHash) (_ oauth.AccessToken, err error) {
	defer func() { err = endedBy(ctx, err) }()
	row, err := s.q.GetAccessToken(ctx, hash[:])
	switch {
	case xerrors.Is(err, sql.ErrNoRows):
		return oauth.AccessToken{}, oauth.ErrUnknownToken
	case err != nil:
		return oauth.AccessToken{}, xerrors.Errorf("read access token: %w", err)
	}
	scopes := make([]faucet.Scope, len(row.Scopes))
	for i, raw := range row.Scopes {
		scopes[i] = faucet.Scope(raw)
	}
	return oauth.AccessToken{
		Hash:        hash,
		ExpiresAt:   row.ExpiresAt,
		Scopes:      scopes,
		Resource:    row.Resource,
		SubjectKind: oauth.SubjectKind(row.SubjectKind),
		Subject:     row.Subject,
	}, nil
}

// DeleteAccessToken implements oauth.Store.
func (s *Store) DeleteAccessToken(ctx context.Context, hash oauth.TokenHash) (err error) {
	defer func() { err = endedBy(ctx, err) }()
	if err := s.q.DeleteAccessToken(ctx, hash[:]); err != nil {
		return xerrors.Errorf("delete access token: %w", err)
	}
	return nil
}

// DeleteExpiredAccessTokens implements oauth.Store.
func (s *Store) DeleteExpiredAccessTokens(ctx context.Context, now time.Time) (_ int64, err error) {
	defer func() { err = endedBy(ctx, err) }()
	deleted, err := s.q.DeleteExpiredAccessTokens(ctx, now)
	if err != nil {
		return 0, xerrors.Errorf("delete expired access tokens: %w", err)
	}
	return deleted, nil
}

// InsertClient implements oauth.Store. A client with the same ID, live or
// disabled, makes it oauth.ErrClientExists.
func (s *Store) InsertClient(ctx context.Context, c oauth.Client) (err error) {
	defer func() { err = endedBy(ctx, err) }()
	if err := c.Validate(); err != nil {
		return xerrors.Errorf("validate client: %w", err)
	}
	// The query keeps an empty hash as NULL, as lib/pq sends a nil []byte as
	// an empty bytea.
	var secretHash []byte
	if c.SecretHash != nil {
		secretHash = c.SecretHash[:]
	}
	n, err := s.q.InsertServiceClient(ctx, database.InsertServiceClientParams{
		ClientID:   c.ID,
		SecretHash: secretHash,
		Resource:   c.Resource,
		Grants:     append([]string{}, c.Grants...),
		Scopes:     scopeNames(c.Scopes),
		CreatedAt:  c.CreatedAt,
	})
	switch {
	case err != nil:
		return xerrors.Errorf("insert client: %w", err)
	case n == 0:
		return oauth.ErrClientExists
	}
	return nil
}

// Client implements oauth.Store.
func (s *Store) Client(ctx context.Context, id string) (_ oauth.Client, err error) {
	defer func() { err = endedBy(ctx, err) }()
	row, err := s.q.GetServiceClient(ctx, id)
	switch {
	case xerrors.Is(err, sql.ErrNoRows):
		return oauth.Client{}, oauth.ErrUnknownClient
	case err != nil:
		return oauth.Client{}, xerrors.Errorf("read client: %w", err)
	}
	return clientOf(row)
}

// Clients implements oauth.Store.
func (s *Store) Clients(ctx context.Context) (_ []oauth.Client, err error) {
	defer func() { err = endedBy(ctx, err) }()
	rows, err := s.q.ListServiceClients(ctx)
	if err != nil {
		return nil, xerrors.Errorf("list clients: %w", err)
	}
	clients := make([]oauth.Client, 0, len(rows))
	for _, row := range rows {
		c, err := clientOf(row)
		if err != nil {
			return nil, err
		}
		clients = append(clients, c)
	}
	return clients, nil
}

// DisableClient implements oauth.Store.
func (s *Store) DisableClient(ctx context.Context, id string, now time.Time) (_ oauth.Client, err error) {
	defer func() { err = endedBy(ctx, err) }()
	row, err := s.q.DisableServiceClient(ctx, database.DisableServiceClientParams{ClientID: id, Now: now})
	switch {
	case xerrors.Is(err, sql.ErrNoRows):
		return oauth.Client{}, oauth.ErrUnknownClient
	case err != nil:
		return oauth.Client{}, xerrors.Errorf("disable client: %w", err)
	}
	return clientOf(row)
}

// UpdateClient implements oauth.Store.
func (s *Store) UpdateClient(ctx context.Context, id string, u oauth.ClientUpdate) (_ oauth.Client, err error) {
	defer func() { err = endedBy(ctx, err) }()
	if err := u.Validate(); err != nil {
		return oauth.Client{}, xerrors.Errorf("validate the update: %w", err)
	}
	row, err := s.q.UpdateServiceClient(ctx, database.UpdateServiceClientParams{
		ClientID:  id,
		SetGrants: u.Grants != nil, Grants: append([]string{}, u.Grants...),
		SetScopes: u.Scopes != nil, Scopes: scopeNames(u.Scopes),
	})
	switch {
	case xerrors.Is(err, sql.ErrNoRows):
		return oauth.Client{}, oauth.ErrUnknownClient
	case err != nil:
		return oauth.Client{}, xerrors.Errorf("update client: %w", err)
	}
	return clientOf(row)
}

// AddTrustRule implements oauth.Store.
func (s *Store) AddTrustRule(ctx context.Context, clientID string, r oauth.TrustRule, now time.Time) (_ int64, err error) {
	defer func() { err = endedBy(ctx, err) }()
	claims, err := json.Marshal(r.Claims)
	if err != nil {
		return 0, xerrors.Errorf("encode the claims: %w", err)
	}
	id, err := s.q.AddTrustRule(ctx, database.AddTrustRuleParams{
		ClientID: clientID, Issuer: r.Issuer, Claims: string(claims), Jwks: string(r.JWKS), CreatedAt: now,
	})
	switch {
	case xerrors.Is(err, sql.ErrNoRows):
		return 0, oauth.ErrUnknownClient
	case err != nil:
		return 0, xerrors.Errorf("add trust rule: %w", err)
	}
	return id, nil
}

// TrustRules implements oauth.Store.
func (s *Store) TrustRules(ctx context.Context, clientID string) (_ []oauth.StoredTrustRule, err error) {
	defer func() { err = endedBy(ctx, err) }()
	rows, err := s.q.ListTrustRules(ctx, clientID)
	if err != nil {
		return nil, xerrors.Errorf("list trust rules: %w", err)
	}
	rules := make([]oauth.StoredTrustRule, 0, len(rows))
	for _, row := range rows {
		rule := oauth.StoredTrustRule{ID: row.ID, Rule: oauth.TrustRule{Issuer: row.Issuer}, CreatedAt: row.CreatedAt}
		if err := json.Unmarshal([]byte(row.Claims), &rule.Rule.Claims); err != nil {
			return nil, xerrors.Errorf("read the claims of trust rule %d: %w", row.ID, err)
		}
		if row.Jwks != "" {
			// jsonb's text puts a space after each colon and comma, which
			// can take a key set past oauth.MaxKeyDocumentBytes.
			var jwks bytes.Buffer
			if err := json.Compact(&jwks, []byte(row.Jwks)); err != nil {
				return nil, xerrors.Errorf("read the key set of trust rule %d: %w", row.ID, err)
			}
			rule.Rule.JWKS = jwks.Bytes()
		}
		rules = append(rules, rule)
	}
	return rules, nil
}

// RemoveTrustRule implements oauth.Store.
func (s *Store) RemoveTrustRule(ctx context.Context, clientID string, id int64) (err error) {
	defer func() { err = endedBy(ctx, err) }()
	n, err := s.q.RemoveTrustRule(ctx, database.RemoveTrustRuleParams{ClientID: clientID, ID: id})
	switch {
	case err != nil:
		return xerrors.Errorf("remove trust rule: %w", err)
	case n == 0:
		return oauth.ErrUnknownTrustRule
	}
	return nil
}

// CountTrustRules implements oauth.Store.
func (s *Store) CountTrustRules(ctx context.Context) (_ map[string]int, err error) {
	defer func() { err = endedBy(ctx, err) }()
	rows, err := s.q.CountTrustRules(ctx)
	if err != nil {
		return nil, xerrors.Errorf("count trust rules: %w", err)
	}
	counts := make(map[string]int, len(rows))
	for _, row := range rows {
		counts[row.ClientID] = int(row.Rules)
	}
	return counts, nil
}

func clientOf(row database.FaucetServiceClient) (oauth.Client, error) {
	c := oauth.Client{
		ID: row.ClientID, Resource: row.Resource,
		Scopes: make([]faucet.Scope, len(row.Scopes)), CreatedAt: row.CreatedAt,
	}
	if row.SecretHash != nil {
		var hash oauth.TokenHash
		if len(row.SecretHash) != len(hash) {
			return oauth.Client{}, xerrors.Errorf("client %q has a secret hash of %d bytes", row.ClientID, len(row.SecretHash))
		}
		copy(hash[:], row.SecretHash)
		c.SecretHash = &hash
	}
	if len(row.Grants) > 0 {
		c.Grants = row.Grants
	}
	for i, raw := range row.Scopes {
		c.Scopes[i] = faucet.Scope(raw)
	}
	if row.DisabledAt.Valid {
		c.DisabledAt = row.DisabledAt.Time
	}
	return c, nil
}

func scopeNames(scopes []faucet.Scope) []string {
	names := make([]string, len(scopes))
	for i, scope := range scopes {
		names[i] = string(scope)
	}
	return names
}
