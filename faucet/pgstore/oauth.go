package pgstore

import (
	"context"
	"database/sql"
	"time"

	"golang.org/x/xerrors"

	"github.com/coder/faucet/faucet"
	"github.com/coder/faucet/faucet/oauth"
	"github.com/coder/faucet/faucet/pgstore/database"
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

// InsertClient implements oauth.Store. A client with the same ID, live or
// disabled, makes it oauth.ErrClientExists.
func (s *Store) InsertClient(ctx context.Context, c oauth.Client) (err error) {
	defer func() { err = endedBy(ctx, err) }()
	if err := c.Validate(); err != nil {
		return xerrors.Errorf("validate client: %w", err)
	}
	n, err := s.q.InsertServiceClient(ctx, database.InsertServiceClientParams{
		ClientID:   c.ID,
		SecretHash: c.SecretHash[:],
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

func clientOf(row database.FaucetServiceClient) (oauth.Client, error) {
	var hash oauth.TokenHash
	if len(row.SecretHash) != len(hash) {
		return oauth.Client{}, xerrors.Errorf("client %q has a secret hash of %d bytes", row.ClientID, len(row.SecretHash))
	}
	copy(hash[:], row.SecretHash)
	c := oauth.Client{
		ID: row.ClientID, SecretHash: hash, Resource: row.Resource,
		Scopes: make([]faucet.Scope, len(row.Scopes)), CreatedAt: row.CreatedAt,
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
