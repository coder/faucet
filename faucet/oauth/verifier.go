package oauth

import (
	"context"
	"maps"

	"golang.org/x/xerrors"

	"github.com/coder/faucet/faucet"
	"github.com/coder/quartz"
)

// SubjectResolver returns the principal of a live subject, holding scopes.
// It returns faucet.ErrInvalidToken when the subject has ended, such as a
// disabled client or a session that logged out.
type SubjectResolver func(ctx context.Context, subject string, scopes []faucet.Scope) (faucet.Principal, error)

// Verifier checks the access tokens that faucet mints.
type Verifier struct {
	store    Store
	clock    quartz.Clock
	subjects map[SubjectKind]SubjectResolver
}

// NewVerifier returns a Verifier over store. subjects holds the resolver of
// each subject kind; a token of a kind with no resolver never verifies.
func NewVerifier(store Store, clock quartz.Clock, subjects map[SubjectKind]SubjectResolver) (*Verifier, error) {
	switch {
	case store == nil:
		return nil, xerrors.New("store is required")
	case clock == nil:
		return nil, xerrors.New("clock is required")
	}
	for kind, resolve := range subjects {
		if resolve == nil {
			return nil, xerrors.Errorf("the resolver of %q is nil", kind)
		}
	}
	return &Verifier{store: store, clock: clock, subjects: maps.Clone(subjects)}, nil
}

// Verify returns the principal of token when faucet minted it as an access
// token for resource, it has not expired, and its subject is live. A token
// that fails any of these gives faucet.ErrInvalidToken, whatever the reason.
// Any other error means the check could not run.
func (v *Verifier) Verify(ctx context.Context, token, resource string) (faucet.Principal, error) {
	if !wellFormedAccessToken(token) {
		return faucet.Principal{}, faucet.ErrInvalidToken
	}
	t, err := v.store.AccessToken(ctx, HashToken(token))
	switch {
	case xerrors.Is(err, ErrUnknownToken):
		return faucet.Principal{}, faucet.ErrInvalidToken
	case err != nil:
		return faucet.Principal{}, xerrors.Errorf("look up the access token: %w", err)
	case !t.ExpiresAt.After(v.clock.Now()), t.Resource != resource:
		return faucet.Principal{}, faucet.ErrInvalidToken
	}
	resolve, ok := v.subjects[t.SubjectKind]
	if !ok {
		return faucet.Principal{}, faucet.ErrInvalidToken
	}
	return resolve(ctx, t.Subject, t.Scopes)
}
