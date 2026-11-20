package storetest

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/coder/superblink-events/faucet/oauth"
)

// trustRuleSubtests is the contract of the trust rules of oauth.Store, one
// behavior per name.
var trustRuleSubtests = []struct {
	name string
	run  func(t *testing.T, s oauth.Store)
}{
	{"TrustRulesReadBackAsAddedInTheirOrder", trustRulesReadBackAsAddedInTheirOrder},
	{"AddingARuleThatTheClientHoldsReturnsItsID", addingARuleThatTheClientHoldsReturnsItsID},
	{"ARuleWithAKeySetOf30KBIsAddedOnce", aRuleWithAKeySetOf30KBIsAddedOnce},
	{"ACompactKeySetOf32KiBReadsBackWithinTheLimit", aCompactKeySetOf32KiBReadsBackWithinTheLimit},
	{"AddTrustRuleOfAnUnknownClientIsUnknown", addTrustRuleOfAnUnknownClientIsUnknown},
	{"RemoveTrustRuleRemovesThatRuleOfThatClientOnly", removeTrustRuleRemovesThatRuleOfThatClientOnly},
	{"CountTrustRulesCountsTheRulesOfEachClient", countTrustRulesCountsTheRulesOfEachClient},
	{"TrustRuleSharesNoMapWithItsCaller", trustRuleSharesNoMapWithItsCaller},
}

func runTrustRules(t *testing.T, newStore func(t *testing.T) oauth.Store) {
	for _, st := range trustRuleSubtests {
		t.Run(st.name, func(t *testing.T) {
			t.Parallel()
			st.run(t, newStore(t))
		})
	}
}

func vercelRule() oauth.TrustRule {
	return oauth.TrustRule{
		Issuer: "https://oidc.vercel.com/codercom",
		Claims: map[string]string{"owner_id": "team_tGkWfhEGGelkkqUUm9nXq17r", "project_id": "prj_7Gw5ZMBpQA8h9GF832KGp7nwbuh3", "environment": "production"},
	}
}

func clusterRule() oauth.TrustRule {
	return oauth.TrustRule{
		Issuer: "https://k8s.wi-test.invalid",
		Claims: map[string]string{"sub": "system:serviceaccount:apps:digest-bot"},
		JWKS:   json.RawMessage(`{"keys":[{"use":"sig","kty":"RSA","kid":"Tmbipp6k","alg":"RS256","n":"3EuwQQ0Y67Iq","e":"AQAB"}]}`),
	}
}

func requireSameTrustRule(t *testing.T, want oauth.TrustRule, got oauth.TrustRule) {
	t.Helper()
	require.Equal(t, want.Issuer, got.Issuer)
	require.Equal(t, want.Claims, got.Claims)
	if len(want.JWKS) == 0 {
		require.Empty(t, got.JWKS, "a rule whose keys come from discovery reads back with none")
		return
	}
	require.JSONEq(t, string(want.JWKS), string(got.JWKS))
}

func addClients(t *testing.T, s oauth.Store, ids ...string) {
	t.Helper()
	for _, id := range ids {
		require.NoError(t, s.InsertClient(context.Background(), clientAt(id, time.Now())))
	}
}

func trustRulesReadBackAsAddedInTheirOrder(t *testing.T, s oauth.Store) {
	ctx := context.Background()
	addClients(t, s, "digest-bot", "ci-watcher")
	at := time.Now().Truncate(time.Microsecond)
	cluster, err := s.AddTrustRule(ctx, "digest-bot", clusterRule(), at)
	require.NoError(t, err)
	vercel, err := s.AddTrustRule(ctx, "digest-bot", vercelRule(), at.Add(time.Second))
	require.NoError(t, err)
	other, err := s.AddTrustRule(ctx, "ci-watcher", vercelRule(), at)
	require.NoError(t, err)
	require.NotEqual(t, vercel, other, "the same rule of another client is a rule of its own")

	rules, err := s.TrustRules(ctx, "digest-bot")
	require.NoError(t, err)
	require.Len(t, rules, 2)
	require.Equal(t, []int64{cluster, vercel}, []int64{rules[0].ID, rules[1].ID}, "in the order they were added")
	requireSameTrustRule(t, clusterRule(), rules[0].Rule)
	requireSameTrustRule(t, vercelRule(), rules[1].Rule)
	require.True(t, at.Equal(rules[0].CreatedAt), "created_at: want %v, got %v", at, rules[0].CreatedAt)
	require.True(t, at.Add(time.Second).Equal(rules[1].CreatedAt))

	rules, err = s.TrustRules(ctx, "ci-watcher")
	require.NoError(t, err)
	require.Len(t, rules, 1)
	require.Equal(t, other, rules[0].ID)
	rules, err = s.TrustRules(ctx, "nobody")
	require.NoError(t, err)
	require.Empty(t, rules)
}

func addingARuleThatTheClientHoldsReturnsItsID(t *testing.T, s oauth.Store) {
	ctx := context.Background()
	addClients(t, s, "digest-bot")
	at := time.Now().Truncate(time.Microsecond)
	first, err := s.AddTrustRule(ctx, "digest-bot", clusterRule(), at)
	require.NoError(t, err)

	respaced := clusterRule()
	respaced.JWKS = json.RawMessage("{\n  \"keys\": [ {\"e\": \"AQAB\", \"n\": \"3EuwQQ0Y67Iq\", \"alg\": \"RS256\", \"kid\": \"Tmbipp6k\", \"kty\": \"RSA\", \"use\": \"sig\"} ]\n}\n")
	for name, r := range map[string]oauth.TrustRule{"the same rule": clusterRule(), "its key set respaced and reordered": respaced} {
		again, err := s.AddTrustRule(ctx, "digest-bot", r, at.Add(time.Hour))
		require.NoError(t, err, name)
		require.Equal(t, first, again, name)
	}
	rules, err := s.TrustRules(ctx, "digest-bot")
	require.NoError(t, err)
	require.Len(t, rules, 1, "nothing changes")
	require.True(t, at.Equal(rules[0].CreatedAt), "the rule keeps the time it was first added")

	otherSub := clusterRule()
	otherSub.Claims["sub"] = "system:serviceaccount:apps:other-bot"
	moreClaims := clusterRule()
	moreClaims.Claims["kubernetes.io/namespace"] = "apps"
	discovery := clusterRule()
	discovery.JWKS = nil
	otherKey := clusterRule()
	otherKey.JWKS = json.RawMessage(`{"keys":[{"use":"sig","kty":"RSA","kid":"rotated","alg":"RS256","n":"3EuwQQ0Y67Iq","e":"AQAB"}]}`)
	otherIssuer := clusterRule()
	otherIssuer.Issuer = "https://k8s.wi-test.invalid/"
	withNumber := func(n string) oauth.TrustRule {
		r := clusterRule()
		r.JWKS = json.RawMessage(`{"keys":[{"use":"sig","kty":"RSA","kid":"Tmbipp6k","alg":"RS256","n":"3EuwQQ0Y67Iq","e":"AQAB","v":` + n + `}]}`)
		return r
	}
	ids := map[int64]bool{first: true}
	for name, r := range map[string]oauth.TrustRule{
		"another sub": otherSub, "one more claim": moreClaims, "keys from discovery": discovery, "another key": otherKey, "another issuer": otherIssuer,
		// jsonb keeps the scale of a number, and every digit.
		"a number": withNumber("1"), "the number at another scale": withNumber("1.0"),
		"a large number": withNumber("12345678901234567890"), "a large number one above": withNumber("12345678901234567891"),
	} {
		id, err := s.AddTrustRule(ctx, "digest-bot", r, at)
		require.NoError(t, err, name)
		require.False(t, ids[id], "%s is a rule of its own", name)
		ids[id] = true
	}
}

// keySetOfSize is a compact key set of RSA keys of 4096 bits and size bytes.
func keySetOfSize(t *testing.T, size int) json.RawMessage {
	t.Helper()
	var keys []map[string]string
	seed := sha256.Sum256([]byte("keys"))
	for {
		var n []byte
		for range 16 {
			seed = sha256.Sum256(seed[:])
			n = append(n, seed[:]...)
		}
		key := map[string]string{"kty": "RSA", "kid": strconv.Itoa(len(keys)), "alg": "RS256", "use": "sig", "n": base64.RawURLEncoding.EncodeToString(n), "e": "AQAB"}
		more, err := json.Marshal(map[string]any{"keys": append(keys, key)})
		require.NoError(t, err)
		if len(more) > size {
			break
		}
		keys = append(keys, key)
	}
	jwks, err := json.Marshal(map[string]any{"keys": keys})
	require.NoError(t, err)
	keys[len(keys)-1]["kid"] += strings.Repeat("k", size-len(jwks))
	jwks, err = json.Marshal(map[string]any{"keys": keys})
	require.NoError(t, err)
	require.Len(t, jwks, size)
	return jwks
}

// A btree entry holds at most 2704 bytes, and random key bytes do not
// compress, so a unique index on the values themselves would refuse this
// rule.
func aRuleWithAKeySetOf30KBIsAddedOnce(t *testing.T, s oauth.Store) {
	ctx := context.Background()
	addClients(t, s, "digest-bot")
	r := clusterRule()
	r.JWKS = keySetOfSize(t, 30<<10)
	require.NoError(t, oauth.ValidateTrustRule(r))

	first, err := s.AddTrustRule(ctx, "digest-bot", r, time.Now())
	require.NoError(t, err)
	again, err := s.AddTrustRule(ctx, "digest-bot", r, time.Now())
	require.NoError(t, err)
	require.Equal(t, first, again)
	rules, err := s.TrustRules(ctx, "digest-bot")
	require.NoError(t, err)
	require.Len(t, rules, 1)
	requireSameTrustRule(t, r, rules[0].Rule)
	require.NoError(t, oauth.ValidateTrustRule(rules[0].Rule), "the checker takes the rule that the store keeps")
}

// 32 KiB is the most that a pasted key set holds. A store that reads the set
// back with white space that the caller did not add takes it past that.
func aCompactKeySetOf32KiBReadsBackWithinTheLimit(t *testing.T, s oauth.Store) {
	ctx := context.Background()
	addClients(t, s, "digest-bot")
	r := clusterRule()
	r.JWKS = keySetOfSize(t, 32<<10)
	require.NoError(t, oauth.ValidateTrustRule(r))

	_, err := s.AddTrustRule(ctx, "digest-bot", r, time.Now())
	require.NoError(t, err)
	rules, err := s.TrustRules(ctx, "digest-bot")
	require.NoError(t, err)
	require.Len(t, rules, 1)
	requireSameTrustRule(t, r, rules[0].Rule)
	require.NoError(t, oauth.ValidateTrustRule(rules[0].Rule), "the checker takes the rule that the store keeps")
}

func addTrustRuleOfAnUnknownClientIsUnknown(t *testing.T, s oauth.Store) {
	ctx := context.Background()
	addClients(t, s, "digest-bot")
	_, err := s.AddTrustRule(ctx, "digest-bot2", clusterRule(), time.Now())
	require.ErrorIs(t, err, oauth.ErrUnknownClient)
	rules, err := s.TrustRules(ctx, "digest-bot2")
	require.NoError(t, err)
	require.Empty(t, rules, "nothing is stored")
}

func removeTrustRuleRemovesThatRuleOfThatClientOnly(t *testing.T, s oauth.Store) {
	ctx := context.Background()
	addClients(t, s, "digest-bot", "ci-watcher")
	now := time.Now()
	cluster, err := s.AddTrustRule(ctx, "digest-bot", clusterRule(), now)
	require.NoError(t, err)
	vercel, err := s.AddTrustRule(ctx, "digest-bot", vercelRule(), now)
	require.NoError(t, err)
	others, err := s.AddTrustRule(ctx, "ci-watcher", clusterRule(), now)
	require.NoError(t, err)

	require.ErrorIs(t, s.RemoveTrustRule(ctx, "ci-watcher", cluster), oauth.ErrUnknownTrustRule, "the rule of another client")
	require.ErrorIs(t, s.RemoveTrustRule(ctx, "nobody", cluster), oauth.ErrUnknownTrustRule)
	require.NoError(t, s.RemoveTrustRule(ctx, "digest-bot", cluster))
	require.ErrorIs(t, s.RemoveTrustRule(ctx, "digest-bot", cluster), oauth.ErrUnknownTrustRule, "a rule is removed once")
	rules, err := s.TrustRules(ctx, "digest-bot")
	require.NoError(t, err)
	require.Len(t, rules, 1)
	require.Equal(t, vercel, rules[0].ID)
	rules, err = s.TrustRules(ctx, "ci-watcher")
	require.NoError(t, err)
	require.Len(t, rules, 1)
	require.Equal(t, others, rules[0].ID, "the other client keeps its rule")

	again, err := s.AddTrustRule(ctx, "digest-bot", clusterRule(), now)
	require.NoError(t, err)
	require.NotContains(t, []int64{cluster, vercel, others}, again, "a rule added again gets an ID that no rule had")
}

func countTrustRulesCountsTheRulesOfEachClient(t *testing.T, s oauth.Store) {
	ctx := context.Background()
	counts, err := s.CountTrustRules(ctx)
	require.NoError(t, err)
	require.Empty(t, counts)
	addClients(t, s, "digest-bot", "ci-watcher", "triage")
	now := time.Now()
	cluster, err := s.AddTrustRule(ctx, "digest-bot", clusterRule(), now)
	require.NoError(t, err)
	for _, add := range []struct {
		clientID string
		rule     oauth.TrustRule
	}{{"digest-bot", vercelRule()}, {"digest-bot", clusterRule()}, {"ci-watcher", clusterRule()}} {
		_, err := s.AddTrustRule(ctx, add.clientID, add.rule, now)
		require.NoError(t, err)
	}
	counts, err = s.CountTrustRules(ctx)
	require.NoError(t, err)
	require.Equal(t, map[string]int{"digest-bot": 2, "ci-watcher": 1}, counts, "a rule added again is one rule, and a client with none has no entry")

	require.NoError(t, s.RemoveTrustRule(ctx, "digest-bot", cluster))
	counts, err = s.CountTrustRules(ctx)
	require.NoError(t, err)
	require.Equal(t, map[string]int{"digest-bot": 1, "ci-watcher": 1}, counts)
}

func trustRuleSharesNoMapWithItsCaller(t *testing.T, s oauth.Store) {
	ctx := context.Background()
	addClients(t, s, "digest-bot")
	added := clusterRule()
	_, err := s.AddTrustRule(ctx, "digest-bot", added, time.Now())
	require.NoError(t, err)
	added.Claims["sub"], added.JWKS[2] = "system:serviceaccount:apps:evil", 'X'

	rules, err := s.TrustRules(ctx, "digest-bot")
	require.NoError(t, err)
	require.Len(t, rules, 1)
	requireSameTrustRule(t, clusterRule(), rules[0].Rule)
	rules[0].Rule.Claims["sub"], rules[0].Rule.JWKS[2] = "system:serviceaccount:apps:evil", 'X'
	again, err := s.TrustRules(ctx, "digest-bot")
	require.NoError(t, err)
	requireSameTrustRule(t, clusterRule(), again[0].Rule)
}
