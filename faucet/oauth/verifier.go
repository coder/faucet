package oauth

import (
	"context"
	"maps"
	"slices"

	"golang.org/x/xerrors"

	"github.com/coder/quartz"
	"github.com/coder/superblink-events/faucet"
)

// SubjectResolver returns the principal of a live subject, holding scopes or
// some of them, such as those that a client still holds. It returns
// faucet.ErrInvalidToken when the subject has ended, such as a disabled
// client or a session that logged out, or holds none of scopes.
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

// rejected is faucet.ErrInvalidToken with the reason, which a debug log may
// show. No reason holds the token.
func rejected(reason string) error {
	return xerrors.Errorf("%s: %w", reason, faucet.ErrInvalidToken)
}

// Verify returns the principal of token when faucet minted it as an access
// token for resource, it has not expired, and its subject is live. A token
// that fails any of these gives an error that wraps faucet.ErrInvalidToken
// and names the reason. Any other error means the check could not run.
func (v *Verifier) Verify(ctx context.Context, token, resource string) (faucet.Principal, error) {
	if !wellFormedAccessToken(token) {
		return faucet.Principal{}, rejected("not an access token")
	}
	t, err := v.store.AccessToken(ctx, HashToken(token))
	switch {
	case xerrors.Is(err, ErrUnknownToken):
		return faucet.Principal{}, rejected("unknown access token")
	case err != nil:
		return faucet.Principal{}, xerrors.Errorf("look up the access token: %w", err)
	}
	// A row that a later build wrote can hold a scope or a subject kind that
	// this build does not know.
	if err := t.Validate(); err != nil {
		return faucet.Principal{}, rejected("unreadable access token: " + err.Error())
	}
	switch {
	case !t.ExpiresAt.After(v.clock.Now()):
		return faucet.Principal{}, rejected("access token expired")
	case t.Resource != resource:
		return faucet.Principal{}, rejected("access token is for another resource")
	}
	resolve, ok := v.subjects[t.SubjectKind]
	if !ok {
		return faucet.Principal{}, rejected("no resolver for the subject kind " + string(t.SubjectKind))
	}
	// The resolver gets its own copy, so it cannot widen the scopes that
	// issuedTo checks it against.
	p, err := resolve(ctx, t.Subject, slices.Clone(t.Scopes))
	switch {
	case xerrors.Is(err, faucet.ErrInvalidToken):
		return faucet.Principal{}, xerrors.Errorf("the %s subject is not live or holds none of the token's scopes: %w", t.SubjectKind, err)
	case err != nil:
		return faucet.Principal{}, xerrors.Errorf("resolve the %s subject: %w", t.SubjectKind, err)
	}
	if err := issuedTo(p, t); err != nil {
		return faucet.Principal{}, xerrors.Errorf("the %s resolver answered for another principal: %w", t.SubjectKind, err)
	}
	return p, nil
}

// issuedTo checks that p is the subject of t, under the login of its kind,
// and holds no scope that t does not grant.
func issuedTo(p faucet.Principal, t AccessToken) error {
	if !p.Authenticated() {
		return xerrors.New("it is no kind of caller, or its login is not the one its kind gives")
	}
	if subject, ok := subjectOf[t.SubjectKind](p); !ok || subject != t.Subject {
		return xerrors.New("it is not the subject of the token")
	}
	for _, scope := range p.Scopes() {
		if !slices.Contains(t.Scopes, scope) {
			return xerrors.Errorf("it holds %s, which the token does not grant", scope)
		}
	}
	return nil
}
