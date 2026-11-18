package faucet

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// largePush is a push of about 1.9 MB: 2000 commits of about 950 bytes.
func largePush(t testing.TB) rawDocument {
	t.Helper()
	commits := make([]map[string]any, 2000)
	for i := range commits {
		id := make([]byte, 20)
		_, _ = rand.Read(id)
		commits[i] = map[string]any{
			"id":       hex.EncodeToString(id),
			"message":  strings.Repeat("m", 700),
			"author":   map[string]any{"name": "octocat", "email": "octocat@example.com"},
			"modified": []string{fmt.Sprintf("path/to/file-%d.go", i)},
		}
	}
	body, err := json.Marshal(map[string]any{
		"ref": "refs/heads/main", "commits": commits, "repository": map[string]any{"id": 42},
	})
	require.NoError(t, err)
	doc, ok := parseRawDocument(body)
	require.True(t, ok)
	return doc
}

func fieldLeases(n int, fields func(i int) []string) []RawInterest {
	interests := make([]RawInterest, n)
	for i := range interests {
		interests[i] = RawInterest{IsLease: true, Lease: RawLeaseID{byte(i), byte(i >> 8)}, Fields: fields(i)}
	}
	return interests
}

// BenchmarkPlanRawIngest parses and plans a push of about 1.9 MB for
// interests that all name fields: 1000 of one field set, 1000 of distinct sets whose union fits,
// and as many distinct sets as an event of a repository may have, whose
// projections are each too large.
func BenchmarkPlanRawIngest(b *testing.B) {
	body := largePush(b).body
	b.Logf("payload %d bytes", len(body))
	for name, interests := range map[string][]RawInterest{
		"1000SameFields": fieldLeases(1000, func(int) []string { return []string{"commits.id"} }),
		"1000DistinctFieldsUnionFits": fieldLeases(1000, func(i int) []string {
			return []string{"commits.id", fmt.Sprintf("absent%d", i)}
		}),
		"CapDistinctFieldsEachTooLarge": fieldLeases(maxRawInterestsPerTarget, func(i int) []string {
			return []string{"commits.message", fmt.Sprintf("absent%d", i)}
		}),
	} {
		b.Run(name, func(b *testing.B) {
			for b.Loop() {
				doc, _ := parseRawDocument(body)
				planRawIngest(doc, interests)
			}
		})
	}
}
