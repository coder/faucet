package faucet_test

import (
	"cmp"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/coder/superblink-events/faucet"
)

const (
	listLimit       = 100
	fieldPathsLimit = 32
)

func TestEveryListInAnInputSchemaDeclaresMaxItems(t *testing.T) {
	t.Parallel()

	schemas := map[string]json.RawMessage{}
	w := newWaitHarness(t)
	tools, err := w.session.ListTools(w.ctx, nil)
	require.NoError(t, err)
	for _, tool := range tools.Tools {
		schemas["tool "+tool.Name] = raw(tool.InputSchema)
	}
	for _, e := range listEvents(t, newEventsHarness(t, withRawEvents)) {
		schemas["event "+e.Name] = e.InputSchema
	}
	require.Len(t, schemas, 1+len(allNames)+len(rawNames))

	var unbounded []string
	for name, schema := range schemas {
		var root any
		require.NoError(t, json.Unmarshal(schema, &root), name)
		unbounded = append(unbounded, listsWithoutMaxItems(name, root)...)
	}
	slices.Sort(unbounded)
	require.Empty(t, unbounded, "a list without maxItems lets one request make a read compare each event with every item")
}

func listsWithoutMaxItems(path string, node any) []string {
	var found []string
	switch n := node.(type) {
	case map[string]any:
		if _, bounded := n["maxItems"]; isArraySchema(n) && !bounded {
			found = append(found, path)
		}
		for key, child := range n {
			found = append(found, listsWithoutMaxItems(path+"/"+key, child)...)
		}
	case []any:
		for i, child := range n {
			found = append(found, listsWithoutMaxItems(path+"/"+strconv.Itoa(i), child)...)
		}
	}
	return found
}

func isArraySchema(schema map[string]any) bool {
	switch typ := schema["type"].(type) {
	case string:
		return typ == "array"
	case []any:
		return slices.Contains(typ, any("array"))
	}
	return false
}

func TestEveryListArgumentHoldsItsLimitAndRefusesOneMore(t *testing.T) {
	t.Parallel()

	numbered := func(prefix string) func(int) string {
		return func(i int) string { return prefix + strconv.Itoa(i) }
	}
	cycling := func(values ...string) func(int) string {
		return func(i int) string { return values[i%len(values)] }
	}
	tests := []struct {
		event, list string
		limit       int
		item        func(int) string
	}{
		{"", "types", listLimit, cycling("pr.activity", "ci.runs")},
		{"", "actions", listLimit, cycling("opened", "completed")},
		{"", "repos", listLimit, numbered("coder/repo-")},
		{"", "base_branch", listLimit, numbered("value-")},
		{"", "head_branch", listLimit, numbered("value-")},
		{"", "conclusion", listLimit, numbered("value-")},
		{"pr.activity", "actions", listLimit, cycling("opened", "closed")},
		{"pr.activity", "repos", listLimit, numbered("coder/repo-")},
		{"pr.activity", "base_branch", listLimit, numbered("value-")},
		{"pr.activity", "head_branch", listLimit, numbered("value-")},
		{"ci.runs", "conclusion", listLimit, numbered("value-")},
		{"ci.run_completed", "repos", listLimit, numbered("coder/repo-")},
		{"github.push", "actions", listLimit, numbered("action-")},
		{"github.push", "repos", listLimit, numbered("coder/repo-")},
		{"github.push", "fields", fieldPathsLimit, numbered("field")},
	}
	for _, tt := range tests {
		t.Run(cmp.Or(tt.event, "wait_for_github_events")+" "+tt.list, func(t *testing.T) {
			t.Parallel()

			list := func(n int) []string {
				items := make([]string, n)
				for i := range items {
					items[i] = tt.item(i)
				}
				return items
			}
			refusal := fmt.Sprintf("/properties/%s: maxItems: array length %d is greater than %d", tt.list, tt.limit+1, tt.limit)
			if tt.event == "" {
				w := newWaitHarness(t, asCaller(newServicePrincipal(t), orgGrants()))
				matching, _, err := w.store.InsertEvent(w.ctx, faucet.Event{
					Source: "github", DeliveryID: "run", Type: "ci.run_completed", RepoID: 77, OwnerID: ownerID, RepoName: "coder/repo-0",
					Data: json.RawMessage(`{"conclusion":"value-0","head_branch":"value-0","from_fork":false}`),
				})
				require.NoError(t, err)
				wait := func(items []string) callOutcome {
					return w.call(args("owner", "coder", "types", []string{"pr.activity", "ci.runs"}, "cursor", "v2.0", tt.list, items))
				}
				require.Equal(t, []int64{matching.ID}, wait(list(tt.limit)).success(t).ids())
				require.Contains(t, wait(list(tt.limit+1)).failure(t), refusal)
				require.Len(t, w.store.pollQueries(), 1, "the refused wait read nothing")
				return
			}
			h := newRawPollHarness(t, asCaller(newServicePrincipal(t), orgGrants()))
			params := map[string]func(items []string) *rawParams{
				methodPoll: func(items []string) *rawParams {
					return pollParams(tt.event, args("owner", "coder", tt.list, items), nil)
				},
				methodSubscribe: func(items []string) *rawParams {
					return &rawParams{Name: tt.event, Arguments: raw(args("owner", "coder", tt.list, items)), Delivery: deliveryTo(callbackURL, newSecret(t, 32))}
				},
			}
			h.poll(params[methodPoll](list(tt.limit)))
			h.subscribe(params[methodSubscribe](list(tt.limit)))
			for method, of := range params {
				rpcErr := h.callError(method, of(list(tt.limit+1)))
				require.EqualValues(t, codeInvalidParams, rpcErr.Code, method)
				require.Contains(t, rpcErr.Message, refusal, method)
			}
		})
	}
}

func TestAFieldFilterReachesTheStoreWithEachValueOnce(t *testing.T) {
	t.Parallel()

	branches := []string{"main", "release"}
	repeated := slices.Repeat(branches, listLimit/len(branches))
	baseBranches := func(store *fakeStore) []string {
		t.Helper()
		polls := store.pollQueries()
		require.NotEmpty(t, polls)
		filters := polls[len(polls)-1].Fields
		i := slices.IndexFunc(filters, func(f faucet.FieldFilter) bool { return f.Field == "base_branch" })
		require.GreaterOrEqual(t, i, 0, "the read filters base_branch")
		return filters[i].Values
	}

	w := newWaitHarness(t)
	storePullRequests(w.ctx, t, w.store)
	want := waitIDs(t, w, []string{"pr.activity"}, "base_branch", branches)
	require.NotEmpty(t, want)
	require.Equal(t, want, waitIDs(t, w, slices.Repeat([]string{"pr.activity"}, listLimit),
		"actions", slices.Repeat([]string{"opened", "labeled", "synchronize", "closed"}, listLimit/4), "base_branch", repeated),
		"repeated items read what each item once reads")
	require.ElementsMatch(t, branches, baseBranches(w.store), "the wait")

	h := newEventsHarness(t)
	start := h.headCursor(repoArgs())
	storePullRequests(h.ctx, t, h.store)
	poll := func(values []string) []string {
		t.Helper()
		return h.poll(pollParams("pr.activity", repoArgs("base_branch", values), cursorOf(start))).eventIDs()
	}
	wantIDs := poll(branches)
	require.NotEmpty(t, wantIDs)
	require.Equal(t, wantIDs, poll(repeated), "repeated values read what each value once reads")
	require.ElementsMatch(t, branches, baseBranches(h.store), "the poll")
}
