package faucet

import (
	"context"
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
				planRawIngest(context.Background(), doc, interests)
			}
		})
	}
}

// BenchmarkPlanRawIngestOfTheLargestPush parses and plans a push of about 23
// MiB, 2000 commits of 12 KiB, for leases that all name commits.id.
func BenchmarkPlanRawIngestOfTheLargestPush(b *testing.B) {
	commits := make([]map[string]any, 2000)
	for i := range commits {
		commits[i] = map[string]any{"id": fmt.Sprintf("%040d", i), "message": strings.Repeat("m", 12<<10)}
	}
	body, err := json.Marshal(map[string]any{"ref": "refs/heads/main", "commits": commits, "repository": map[string]any{"id": 42}})
	require.NoError(b, err)
	b.Logf("payload %d bytes", len(body))
	for _, n := range []int{50, 200} {
		interests := fieldLeases(n, func(int) []string { return []string{"commits.id"} })
		b.Run(fmt.Sprintf("%dLeases", n), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				doc, _ := parseRawDocument(body)
				planRawIngest(context.Background(), doc, interests)
			}
		})
	}
}

func TestAPlanWhoseContextEndedServesNobody(t *testing.T) {
	t.Parallel()
	doc := largePush(t)
	interests := []RawInterest{
		{IsLease: true, Lease: RawLeaseID{1}, Fields: []string{"commits.id"}},
		{IsLease: true, Lease: RawLeaseID{2}, Fields: []string{"commits.message"}},
		{Subscription: SubscriptionKeyHash{3}, Fields: []string{"ref"}},
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	plan, _ := planRawIngest(ctx, doc, interests)
	require.Equal(t, RawPlan{}, plan, "a plan that its request outlived does no more work, and the store writes none")

	plan, _ = planRawIngest(context.Background(), doc, interests)
	require.Equal(t, []RawLeaseID{{1}}, plan.Unread)
	require.Equal(t, []RawLeaseID{{2}}, plan.Lost)
	require.Len(t, plan.Deliveries, 1)
}
