package pgstore

import (
	"testing"

	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
	"golang.org/x/xerrors"
)

// Only the check that a refresh token is not both used and retired means
// that a rotation lost to another one. Any other violation is a failure.
func TestOnlyTheUsedOrRetiredCheckIsARotationThatLost(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		err  error
		lost bool
	}{
		"the check":                  {&pq.Error{Code: "23514", Constraint: usedOrRetiredConstraint}, true},
		"the check, wrapped":         {xerrors.Errorf("rotate session: %w", &pq.Error{Code: "23514", Constraint: usedOrRetiredConstraint}), true},
		"another check":              {&pq.Error{Code: "23514", Constraint: "faucet_refresh_tokens_token_hash_check"}, false},
		"another error of the check": {&pq.Error{Code: "23505", Constraint: usedOrRetiredConstraint}, false},
		"an error of no constraint":  {&pq.Error{Code: "23514"}, false},
		"an error of no driver":      {xerrors.New("violates check constraint \"" + usedOrRetiredConstraint + "\""), false},
		"no error":                   {nil, false},
	} {
		require.Equal(t, c.lost, violatesUsedOrRetired(c.err), name)
	}
}
