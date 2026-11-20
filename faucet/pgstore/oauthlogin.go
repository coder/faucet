package pgstore

import (
	"bytes"
	"context"
	"database/sql"
	"time"

	"github.com/lib/pq"
	"golang.org/x/xerrors"

	"github.com/coder/faucet/faucet"
	"github.com/coder/faucet/faucet/oauth"
	"github.com/coder/faucet/faucet/pgstore/database"
)

var _ oauth.LoginStore = (*Store)(nil)

// ConsentAuthRequest implements oauth.LoginStore in one statement. A
// GitHub state that another request holds fails on the unique index, so a
// state finds one request.
func (s *Store) ConsentAuthRequest(ctx context.Context, r oauth.AuthRequest, now time.Time) (err error) {
	defer func() { err = endedBy(ctx, err) }()
	if err := r.Validate(); err != nil {
		return xerrors.Errorf("validate authorization request: %w", err)
	}
	n, err := s.q.ConsentAuthRequest(ctx, database.ConsentAuthRequestParams{
		ID:              r.ID,
		ClientID:        r.ClientID,
		ClientName:      r.ClientName,
		RedirectUri:     r.RedirectURI,
		ClientState:     r.ClientState,
		Resource:        r.Resource,
		Scopes:          scopeNames(r.Scopes),
		CodeChallenge:   r.CodeChallenge,
		BrowserHash:     r.BrowserHash[:],
		GithubStateHash: r.GitHubStateHash[:],
		CreatedAt:       r.CreatedAt,
		ExpiresAt:       r.ExpiresAt,
		Now:             now,
	})
	switch {
	case err != nil:
		return xerrors.Errorf("store the consented authorization request: %w", err)
	case n == 0:
		return oauth.ErrUnknownAuthRequest
	}
	return nil
}

// ExpireAuthRequest implements oauth.LoginStore.
func (s *Store) ExpireAuthRequest(ctx context.Context, id string) (err error) {
	defer func() { err = endedBy(ctx, err) }()
	if err := s.q.ExpireAuthRequest(ctx, id); err != nil {
		return xerrors.Errorf("expire authorization request: %w", err)
	}
	return nil
}

// AuthRequestByGitHubState implements oauth.LoginStore.
func (s *Store) AuthRequestByGitHubState(ctx context.Context, githubState oauth.TokenHash) (_ oauth.AuthRequest, err error) {
	defer func() { err = endedBy(ctx, err) }()
	row, err := s.q.GetAuthRequestByGitHubState(ctx, githubState[:])
	switch {
	case xerrors.Is(err, sql.ErrNoRows):
		return oauth.AuthRequest{}, oauth.ErrUnknownAuthRequest
	case err != nil:
		return oauth.AuthRequest{}, xerrors.Errorf("read authorization request by GitHub state: %w", err)
	}
	return authRequestOf(row)
}

// ApproveAuthRequest implements oauth.LoginStore in one transaction. The
// lock of the connection goes first, waits for a RefreshConnection that
// holds the row, and holds the row while alive runs. When the lock finds no
// row, alive does not run and the sessions of the connection end: it has
// none, unless another login stored it since. When the request is not there
// to approve, the rollback undoes every change.
func (s *Store) ApproveAuthRequest(ctx context.Context, a oauth.Approval, alive func(current oauth.Connection) bool) (err error) {
	defer func() { err = endedBy(ctx, err) }()
	if err := a.Validate(); err != nil {
		return xerrors.Errorf("validate approval: %w", err)
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return xerrors.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	q := s.q.WithTx(tx)

	c := a.Connection
	keep := false
	row, err := q.LockConnection(ctx, c.ID)
	switch {
	case err == nil:
		keep = alive(connectionOf(row))
	case !xerrors.Is(err, sql.ErrNoRows):
		return xerrors.Errorf("lock connection: %w", err)
	}
	if err := q.UpsertConnection(ctx, database.UpsertConnectionParams{
		ID:               c.ID,
		Provider:         c.Provider,
		UpstreamUserID:   c.UpstreamUserID,
		Login:            c.Login,
		AccessTokenEnc:   c.AccessToken,
		RefreshTokenEnc:  c.RefreshToken,
		KeyID:            c.KeyID,
		AccessExpiresAt:  c.AccessExpiresAt,
		RefreshExpiresAt: c.RefreshExpiresAt,
		UpdatedAt:        c.UpdatedAt,
	}); err != nil {
		return xerrors.Errorf("upsert connection: %w", err)
	}
	n, err := q.ApproveAuthRequest(ctx, database.ApproveAuthRequestParams{
		CodeHash:        a.Code[:],
		ConnectionID:    c.ID,
		CodeExpiresAt:   a.ExpiresAt,
		ID:              a.RequestID,
		GithubStateHash: a.GitHubState[:],
		Now:             a.Now,
	})
	switch {
	case err != nil:
		return xerrors.Errorf("approve authorization request: %w", err)
	case n == 0:
		return oauth.ErrUnknownAuthRequest
	}
	if !keep {
		// The codes go first: a redemption that holds one commits its
		// session before the sessions end.
		if err := q.ExpireConnectionCodes(ctx, database.ExpireConnectionCodesParams{ConnectionID: c.ID, ID: a.RequestID}); err != nil {
			return xerrors.Errorf("expire the codes of the connection: %w", err)
		}
		if err := q.EndConnectionSessions(ctx, database.EndConnectionSessionsParams{Now: a.Now, ConnectionID: c.ID}); err != nil {
			return xerrors.Errorf("end the sessions of the connection: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return xerrors.Errorf("commit: %w", err)
	}
	return nil
}

// AuthRequestByCode implements oauth.LoginStore.
func (s *Store) AuthRequestByCode(ctx context.Context, code oauth.TokenHash) (_ oauth.AuthRequest, err error) {
	defer func() { err = endedBy(ctx, err) }()
	row, err := s.q.GetAuthRequestByCode(ctx, code[:])
	switch {
	case xerrors.Is(err, sql.ErrNoRows):
		return oauth.AuthRequest{}, oauth.ErrUnknownAuthRequest
	case err != nil:
		return oauth.AuthRequest{}, xerrors.Errorf("read authorization request by code: %w", err)
	}
	return authRequestOf(row)
}

// RedeemAuthRequest implements oauth.LoginStore in one statement, which
// copies the session's fields from the request's row.
func (s *Store) RedeemAuthRequest(ctx context.Context, r oauth.Redemption) (_ oauth.Session, err error) {
	defer func() { err = endedBy(ctx, err) }()
	if err := r.Validate(); err != nil {
		return oauth.Session{}, xerrors.Errorf("validate redemption: %w", err)
	}
	row, err := s.q.ExchangeCode(ctx, database.ExchangeCodeParams{
		CodeHash:        r.Code[:],
		Now:             r.Now,
		RefreshHash:     r.Refresh[:],
		AccessHash:      r.Access[:],
		AccessExpiresAt: r.AccessExpiresAt,
	})
	switch {
	case xerrors.Is(err, sql.ErrNoRows):
		return oauth.Session{}, oauth.ErrUnknownAuthRequest
	case err != nil:
		return oauth.Session{}, xerrors.Errorf("redeem authorization request: %w", err)
	}
	return sessionOf(database.FaucetUserSession(row)), nil
}

// SessionByRefreshToken implements oauth.LoginStore.
func (s *Store) SessionByRefreshToken(ctx context.Context, refresh oauth.TokenHash) (_ oauth.Session, _ oauth.RefreshToken, err error) {
	defer func() { err = endedBy(ctx, err) }()
	row, err := s.q.GetSessionByRefreshHash(ctx, refresh[:])
	switch {
	case xerrors.Is(err, sql.ErrNoRows):
		return oauth.Session{}, oauth.RefreshToken{}, oauth.ErrUnknownSession
	case err != nil:
		return oauth.Session{}, oauth.RefreshToken{}, xerrors.Errorf("read session by refresh token: %w", err)
	}
	hash, err := tokenHashOf(row.FaucetRefreshToken.TokenHash)
	if err != nil {
		return oauth.Session{}, oauth.RefreshToken{}, xerrors.Errorf("refresh token of session %q: %w", row.FaucetUserSession.ID, err)
	}
	token := oauth.RefreshToken{
		Hash:      hash,
		SessionID: row.FaucetRefreshToken.SessionID,
		IssuedAt:  row.FaucetRefreshToken.IssuedAt,
		UsedAt:    row.FaucetRefreshToken.UsedAt.Time,
		RetiredAt: row.FaucetRefreshToken.RetiredAt.Time,
	}
	return sessionOf(row.FaucetUserSession), token, nil
}

// Session implements oauth.LoginStore.
func (s *Store) Session(ctx context.Context, id string) (_ oauth.Session, err error) {
	defer func() { err = endedBy(ctx, err) }()
	row, err := s.q.GetSession(ctx, id)
	switch {
	case xerrors.Is(err, sql.ErrNoRows):
		return oauth.Session{}, oauth.ErrUnknownSession
	case err != nil:
		return oauth.Session{}, xerrors.Errorf("read session: %w", err)
	}
	return sessionOf(row), nil
}

// RotateSession implements oauth.LoginStore in one statement: a
// compare-and-swap on the current refresh hash, so of two rotations with the
// same token one wins.
func (s *Store) RotateSession(ctx context.Context, r oauth.Rotation) (_ oauth.Session, err error) {
	defer func() { err = endedBy(ctx, err) }()
	if err := r.Validate(); err != nil {
		return oauth.Session{}, xerrors.Errorf("validate rotation: %w", err)
	}
	row, err := s.q.RotateSession(ctx, database.RotateSessionParams{
		NextHash:        r.Next[:],
		Now:             r.Now,
		PresentedHash:   r.Presented[:],
		AccessHash:      r.Access[:],
		AccessExpiresAt: r.AccessExpiresAt,
		Scopes:          scopeNames(r.Scopes),
		UsedAfter:       r.UsedAfter,
	})
	switch {
	case xerrors.Is(err, sql.ErrNoRows), violatesUsedOrRetired(err):
		// A check fails when a rotation of another token of the session
		// retired this one while this rotation waited for it: the token
		// is retired, and the statement changed nothing.
		return oauth.Session{}, oauth.ErrUnknownSession
	case err != nil:
		return oauth.Session{}, xerrors.Errorf("rotate session: %w", err)
	}
	return sessionOf(database.FaucetUserSession(row)), nil
}

// usedOrRetiredConstraint is the check that no refresh token is both used
// and retired.
const usedOrRetiredConstraint = "faucet_refresh_tokens_used_or_retired"

// violatesUsedOrRetired reports whether err is lib/pq's error for a row that
// breaks usedOrRetiredConstraint (SQLSTATE 23514). Any other violated check
// is a failure of the store.
func violatesUsedOrRetired(err error) bool {
	var pqErr *pq.Error
	return xerrors.As(err, &pqErr) && pqErr.Code == "23514" && pqErr.Constraint == usedOrRetiredConstraint
}

// EndSession implements oauth.LoginStore.
func (s *Store) EndSession(ctx context.Context, id string, now time.Time) (err error) {
	defer func() { err = endedBy(ctx, err) }()
	if err := s.q.EndSession(ctx, database.EndSessionParams{Now: now, ID: id}); err != nil {
		return xerrors.Errorf("end session: %w", err)
	}
	return nil
}

// Connection implements oauth.LoginStore.
func (s *Store) Connection(ctx context.Context, id string) (_ oauth.Connection, err error) {
	defer func() { err = endedBy(ctx, err) }()
	row, err := s.q.GetConnection(ctx, id)
	switch {
	case xerrors.Is(err, sql.ErrNoRows):
		return oauth.Connection{}, oauth.ErrUnknownConnection
	case err != nil:
		return oauth.Connection{}, xerrors.Errorf("read connection: %w", err)
	}
	return connectionOf(row), nil
}

// RefreshConnection implements oauth.LoginStore in one transaction, which
// holds the connection's row lock while refresh runs. The lock is FOR NO KEY
// UPDATE, the one that the upsert of ApproveAuthRequest takes, so a second
// refresh and an approval wait, and a redemption that only checks its
// foreign key to the row does not wait for GitHub.
func (s *Store) RefreshConnection(ctx context.Context, id string, now time.Time, refresh func(current oauth.Connection) (oauth.ConnectionChange, error)) (_ oauth.Connection, err error) {
	defer func() { err = endedBy(ctx, err) }()
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return oauth.Connection{}, xerrors.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	q := s.q.WithTx(tx)

	row, err := q.LockConnection(ctx, id)
	switch {
	case xerrors.Is(err, sql.ErrNoRows):
		return oauth.Connection{}, oauth.ErrUnknownConnection
	case err != nil:
		return oauth.Connection{}, xerrors.Errorf("lock connection: %w", err)
	}
	change, err := refresh(connectionOf(row))
	if err != nil {
		return oauth.Connection{}, err
	}
	if err := change.Validate(id); err != nil {
		return oauth.Connection{}, xerrors.Errorf("validate connection change: %w", err)
	}
	switch change.Kind {
	case oauth.ReplaceConnection:
		c := change.Connection
		row, err = q.UpdateConnection(ctx, database.UpdateConnectionParams{
			Login:            c.Login,
			AccessTokenEnc:   c.AccessToken,
			RefreshTokenEnc:  c.RefreshToken,
			KeyID:            c.KeyID,
			AccessExpiresAt:  c.AccessExpiresAt,
			RefreshExpiresAt: c.RefreshExpiresAt,
			UpdatedAt:        c.UpdatedAt,
			ID:               id,
		})
		if err != nil {
			return oauth.Connection{}, xerrors.Errorf("replace connection: %w", err)
		}
	case oauth.EndConnectionSessions:
		if err := q.EndConnectionSessions(ctx, database.EndConnectionSessionsParams{Now: now, ConnectionID: id}); err != nil {
			return oauth.Connection{}, xerrors.Errorf("end the sessions of the connection: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return oauth.Connection{}, xerrors.Errorf("commit: %w", err)
	}
	return connectionOf(row), nil
}

// DeleteExpiredAuthRequests implements oauth.LoginStore. No session refers to its
// request, so the sessions stay.
func (s *Store) DeleteExpiredAuthRequests(ctx context.Context, now, codesBefore time.Time) (_ int64, err error) {
	defer func() { err = endedBy(ctx, err) }()
	n, err := s.q.DeleteExpiredAuthRequests(ctx, database.DeleteExpiredAuthRequestsParams{Now: now, CodesBefore: codesBefore})
	if err != nil {
		return 0, xerrors.Errorf("delete expired authorization requests: %w", err)
	}
	return n, nil
}

// DeleteEndedSessions implements oauth.LoginStore. Their refresh tokens go
// with them by the foreign key's cascade.
func (s *Store) DeleteEndedSessions(ctx context.Context, now, idleSince time.Time) (_ int64, err error) {
	defer func() { err = endedBy(ctx, err) }()
	n, err := s.q.DeleteEndedSessions(ctx, database.DeleteEndedSessionsParams{Now: now, IdleSince: idleSince})
	if err != nil {
		return 0, xerrors.Errorf("delete ended sessions: %w", err)
	}
	return n, nil
}

// DeleteUnusedConnections implements oauth.LoginStore in one statement.
func (s *Store) DeleteUnusedConnections(ctx context.Context, updatedBefore time.Time) (_ int64, err error) {
	defer func() { err = endedBy(ctx, err) }()
	n, err := s.q.DeleteUnusedConnections(ctx, updatedBefore)
	if err != nil {
		return 0, xerrors.Errorf("delete unused connections: %w", err)
	}
	return n, nil
}

func authRequestOf(row database.FaucetAuthRequest) (oauth.AuthRequest, error) {
	var hashes [3]oauth.TokenHash
	for i, raw := range [][]byte{row.BrowserHash, row.GithubStateHash, row.CodeHash} {
		h, err := tokenHashOf(raw)
		if err != nil {
			return oauth.AuthRequest{}, xerrors.Errorf("authorization request %q: %w", row.ID, err)
		}
		hashes[i] = h
	}
	return oauth.AuthRequest{
		ID:              row.ID,
		State:           oauth.AuthRequestState(row.State),
		ClientID:        row.ClientID,
		ClientName:      row.ClientName,
		RedirectURI:     row.RedirectUri,
		ClientState:     row.ClientState,
		Resource:        row.Resource,
		Scopes:          scopesOf(row.Scopes),
		CodeChallenge:   row.CodeChallenge,
		BrowserHash:     hashes[0],
		GitHubStateHash: hashes[1],
		CodeHash:        hashes[2],
		ConnectionID:    row.ConnectionID.String,
		CreatedAt:       row.CreatedAt,
		ExpiresAt:       row.ExpiresAt,
	}, nil
}

func sessionOf(row database.FaucetUserSession) oauth.Session {
	return oauth.Session{
		ID:           row.ID,
		ClientID:     row.ClientID,
		ClientName:   row.ClientName,
		ConnectionID: row.ConnectionID,
		Resource:     row.Resource,
		Scopes:       scopesOf(row.Scopes),
		RefreshedAt:  row.RefreshedAt,
		EndedAt:      row.EndedAt.Time,
	}
}

// connectionOf copies the row's tokens, so two connections made from one row
// share no memory.
func connectionOf(row database.FaucetConnection) oauth.Connection {
	return oauth.Connection{
		ID:               row.ID,
		Provider:         row.Provider,
		UpstreamUserID:   row.UpstreamUserID,
		Login:            row.Login,
		AccessToken:      bytes.Clone(row.AccessTokenEnc),
		RefreshToken:     bytes.Clone(row.RefreshTokenEnc),
		KeyID:            row.KeyID,
		AccessExpiresAt:  row.AccessExpiresAt,
		RefreshExpiresAt: row.RefreshExpiresAt,
		UpdatedAt:        row.UpdatedAt,
	}
}

// tokenHashOf reads a hash column. NULL is the zero hash.
func tokenHashOf(raw []byte) (oauth.TokenHash, error) {
	var h oauth.TokenHash
	switch len(raw) {
	case 0:
	case len(h):
		copy(h[:], raw)
	default:
		return oauth.TokenHash{}, xerrors.Errorf("a stored hash has %d bytes, want %d", len(raw), len(h))
	}
	return h, nil
}

func scopesOf(names []string) []faucet.Scope {
	scopes := make([]faucet.Scope, len(names))
	for i, name := range names {
		scopes[i] = faucet.Scope(name)
	}
	return scopes
}
