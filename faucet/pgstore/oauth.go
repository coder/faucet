package pgstore

import (
	"context"
	"database/sql"

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
	scopes := make([]string, len(t.Scopes))
	for i, scope := range t.Scopes {
		scopes[i] = string(scope)
	}
	if err := s.q.InsertAccessToken(ctx, database.InsertAccessTokenParams{
		TokenHash:   t.Hash[:],
		ExpiresAt:   t.ExpiresAt,
		Scopes:      scopes,
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
