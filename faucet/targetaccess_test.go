package faucet_test

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/xerrors"

	"github.com/coder/superblink-events/faucet"
)

const ownerID = 5

// fakeGrants covers the owners it lists, by lower-case login, and every
// repository when all is set. err fails every fresh check.
type fakeGrants struct {
	mu     sync.Mutex
	owners map[string]int64
	all    bool
	err    error
	calls  []string
}

func (g *fakeGrants) record(call string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls = append(g.calls, call)
}

func (g *fakeGrants) freshErr() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.err
}

func (g *fakeGrants) ownerOf(owner string) (int64, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	id, ok := g.owners[strings.ToLower(owner)]
	if !ok {
		return 0, xerrors.Errorf("no grant names the owner %s: %w", owner, faucet.ErrNoAccess)
	}
	return id, nil
}

func (g *fakeGrants) grantsAll() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.all {
		return xerrors.Errorf("no grant names every repository: %w", faucet.ErrNoAccess)
	}
	return nil
}

func (g *fakeGrants) OwnerID(_ context.Context, _ faucet.Principal, owner string) (int64, error) {
	g.record("owner " + owner)
	return g.ownerOf(owner)
}

func (g *fakeGrants) OwnerIDFresh(_ context.Context, _ faucet.Principal, owner string) (int64, error) {
	g.record("owner " + owner + " fresh")
	if err := g.freshErr(); err != nil {
		return 0, err
	}
	return g.ownerOf(owner)
}

func (g *fakeGrants) CoversAll(context.Context, faucet.Principal) error {
	g.record("all")
	return g.grantsAll()
}

func (g *fakeGrants) CoversAllFresh(context.Context, faucet.Principal) error {
	g.record("all fresh")
	if err := g.freshErr(); err != nil {
		return err
	}
	return g.grantsAll()
}

func (g *fakeGrants) seen() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.calls...)
}

func (g *fakeGrants) fail(err error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.err = err
}

func newServicePrincipal(t *testing.T) faucet.Principal {
	t.Helper()
	p, err := faucet.NewService(faucet.ServiceClient{ClientID: "org-watcher", Grants: []string{"org:coder"}}, faucet.AllScopes())
	require.NoError(t, err)
	return p
}

// asCaller makes every request come from p, and gives the service grants.
func asCaller(p faucet.Principal, grants faucet.ServiceGrants) func(*faucet.Deps) {
	return func(d *faucet.Deps) {
		d.PrincipalFrom = func(context.Context) (faucet.Principal, error) { return p, nil }
		d.ServiceGrants = grants
	}
}

// targetRefusals calls every path that names a target, the typed and raw
// names of events/poll and events/subscribe and the wait tool, and returns
// the error text of each.
func targetRefusals(t *testing.T, mutate func(*faucet.Deps), arguments map[string]any) []string {
	t.Helper()
	h := newRawPollHarness(t, mutate)
	var texts []string
	for _, name := range []string{"ci.run_completed", "github.push"} {
		rpcErr := h.pollError(pollParams(name, arguments, nil))
		require.EqualValues(t, codeForbidden, rpcErr.Code, name)
		texts = append(texts, rpcErr.Message)
		rpcErr = h.subscribeError(&rawParams{Name: name, Arguments: raw(arguments), Delivery: deliveryTo(callbackURL, newSecret(t, 32))})
		require.EqualValues(t, codeForbidden, rpcErr.Code, name)
		texts = append(texts, rpcErr.Message)
	}
	require.Empty(t, h.rows())
	require.Zero(t, h.authz.callCount(), "no repository is looked up")

	w := newWaitHarness(t, mutate)
	waitArgs := args("types", []string{"ci.run_completed"}, "timeout_s", 1)
	for k, v := range arguments {
		waitArgs[k] = v
	}
	texts = append(texts, w.call(waitArgs).failure(t))
	require.Zero(t, w.authz.callCount())
	return texts
}

func TestOwnerAndAllTargetsAreForServiceClients(t *testing.T) {
	t.Parallel()

	token, err := faucet.NewPrincipal("octocat", testToken)
	require.NoError(t, err)
	user, err := faucet.NewOAuthUser("octocat", faucet.OAuthSession{SessionID: "s", ConnectionID: "c"}, faucet.AllScopes())
	require.NoError(t, err)
	const want = "owner and all targets are for service clients; name one repository with repo: repository not found or not accessible"
	for _, p := range []faucet.Principal{token, user} {
		grants := &fakeGrants{owners: map[string]int64{"coder": ownerID}, all: true}
		for _, arguments := range []map[string]any{args("owner", "coder"), args("all", true)} {
			for _, text := range targetRefusals(t, asCaller(p, grants), arguments) {
				require.Equal(t, want, text)
			}
		}
		require.Empty(t, grants.seen(), "a person's grants are GitHub's to answer, and GitHub has none")
	}
}

func TestAServerWithoutServiceGrantsRefusesEveryOwnerAndAllTarget(t *testing.T) {
	t.Parallel()

	p := newServicePrincipal(t)
	for _, arguments := range []map[string]any{args("owner", "coder"), args("all", true)} {
		for _, text := range targetRefusals(t, asCaller(p, nil), arguments) {
			require.Equal(t, "this server reads no owner or all target: repository not found or not accessible", text)
		}
	}
}

func TestAServiceReadsTheOwnerAndAllTargetsThatItsGrantsCover(t *testing.T) {
	t.Parallel()

	grants := &fakeGrants{owners: map[string]int64{"coder": ownerID}, all: true}
	h := newRawPollHarness(t, asCaller(newServicePrincipal(t), grants))

	owner := args("owner", "coder", "repos", []string{"Coder/B", "coder/a", "coder/b"})
	h.subscribe(&rawParams{Name: "ci.run_completed", Arguments: raw(owner), Delivery: deliveryTo(callbackURL, newSecret(t, 32))})
	h.subscribe(&rawParams{Name: "ci.run_completed", Arguments: raw(args("all", true)), Delivery: deliveryTo(callbackURL, newSecret(t, 32))})
	h.subscribe(&rawParams{Name: "github.push", Arguments: raw(args("owner", "coder")), Delivery: deliveryTo(callbackURL, newSecret(t, 32))})
	rows := h.rows()
	require.Len(t, rows, 3)
	byArguments := map[string]faucet.Target{}
	for _, row := range rows {
		byArguments[row.EventType+" "+string(row.Arguments)] = row.Target
	}
	require.Equal(t, map[string]faucet.Target{
		`ci.run_completed {"owner":"coder","repos":["coder/a","coder/b"]}`: faucet.OwnerTarget(ownerID),
		`ci.run_completed {"all":true}`:                                    faucet.AllTarget(),
		`github.push {"owner":"coder"}`:                                    faucet.OwnerTarget(ownerID),
	}, byArguments, "the owner's ID, not its login, and repos in the one spelling that the store compares")
	require.Equal(t, []string{"owner coder fresh", "all fresh", "owner coder fresh"}, grants.seen(), "every subscribe asks the source again")

	before := len(grants.seen())
	h.poll(pollParams("ci.run_completed", args("owner", "coder"), nil))
	h.pollRaw("github.push", args("all", true), "")
	require.Equal(t, []string{"owner coder", "all"}, grants.seen()[before:], "a poll may use what the grants held when the token was checked")

	rpcErr := h.pollError(pollParams("ci.run_completed", args("owner", "octo"), nil))
	require.EqualValues(t, codeForbidden, rpcErr.Code)
	require.Equal(t, "no grant names the owner octo: repository not found or not accessible", rpcErr.Message, "the grants say why")

	grants.fail(xerrors.Errorf("the grant was removed: %w", faucet.ErrNoAccess))
	rpcErr = h.subscribeError(&rawParams{Name: "ci.run_completed", Arguments: raw(owner), Delivery: deliveryTo(callbackURL, newSecret(t, 32))})
	require.EqualValues(t, codeForbidden, rpcErr.Code)
	require.Len(t, h.rows(), 2, "the renewal that access refused ended its subscription")
}
