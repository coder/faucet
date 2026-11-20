package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"golang.org/x/xerrors"

	"github.com/coder/superblink-events/cmd/faucet/internal/dbconn"
	"github.com/coder/superblink-events/faucet"
	"github.com/coder/superblink-events/faucet/oauth"
	"github.com/coder/superblink-events/faucet/pgstore"
)

const (
	updateUsage  = "faucet clients update <client-id> [-scope <scopes>] [-grant <grant> [-grant ...]]"
	trustUsage   = "faucet clients trust <client-id> -issuer <url> -claim <name>=<value> [-claim ...] [-jwks-file <path>]"
	trustsUsage  = "faucet clients trusts <client-id>"
	untrustUsage = "faucet clients untrust <client-id> <rule-id>"
)

const clientsUsage = `usage:
  faucet clients create [-resource <url>] [-scope <scopes>] [-no-secret] -grant <grant> [-grant ...] <client-id>
  faucet clients list
  faucet clients disable <client-id>
  ` + updateUsage + `
  ` + trustUsage + `
  ` + trustsUsage + `
  ` + untrustUsage + `

The commands read DATABASE_URL and faucet's public URL as the server does:
FAUCET_PUBLIC_URL, or on Vercel production https://$VERCEL_PROJECT_PRODUCTION_URL.
-resource defaults to the public URL and /github/mcp, and must be that
endpoint when FAUCET_PUBLIC_URL is set.
A grant is repo:<owner>/<name> for one repository, org:<login> for every
repository of the user or organization that holds the login now, under any
later login too, or all for every repository. org:<login>#<account ID> is
taken only when that account holds the login now. The client keeps each org
grant with the ID of its account, which create and list show.
update sets the scopes, the grants or both of a live client, and keeps the
rest. On its next request, a token of the client takes the new grants and
holds only the scopes that it was issued with and the client holds, so a new
scope needs a new token. A -scope without events:subscribe deletes the
client's subscriptions. An org grant that the client holds stays bound to its
account.
-no-secret makes a client with no secret, which the client credentials grant
refuses, for workloads that log in with their own OIDC tokens. A trust rule
lets a token that -issuer signs act as the client when it holds each -claim
with that value. faucet takes the issuer's keys from its OpenID discovery
document, or from -jwks-file, the issuer's JWK Set, for an issuer that faucet
cannot reach. trust refuses a rule that pins too little for its issuer, and
says what to pin. trusts lists the rules of a client with their IDs.`

// clientEndpoint is the path of the endpoint that new clients are for. /mcp,
// its old path, takes no new client, because a later release removes it.
const clientEndpoint = "/github/mcp"

// runClients runs "faucet clients", the operator's commands for the clients
// of services.
func runClients(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) error {
	if len(args) == 0 || !isClientsCommand(args[0]) {
		return xerrors.New(clientsUsage)
	}
	databaseURL := getenv(envDatabaseURL)
	if databaseURL == "" {
		return xerrors.Errorf("missing required environment variable %s", envDatabaseURL)
	}
	publicURL, derived, err := loadPublicURL(getenv, deploymentOf(getenv))
	if err != nil {
		return err
	}
	if derived {
		if _, err := fmt.Fprintf(stderr, "public URL taken from %s: %s\n", envVercelProductionURL, publicURL); err != nil {
			return err
		}
	}
	// DATABASE_URL can be a transaction-mode pooler, as on Vercel with Neon.
	db, err := dbconn.OpenPooled(databaseURL)
	if err != nil {
		return xerrors.Errorf("%s: %w", envDatabaseURL, err)
	}
	defer func() { _ = db.Close() }()
	if err := pgstore.CheckSchema(ctx, db); err != nil {
		return xerrors.Errorf("check the schema: %w", err)
	}
	pinned := publicURL
	if derived {
		// Production may set FAUCET_PUBLIC_URL to another domain, and an
		// environment pulled from Vercel lacks it when it is sensitive, so
		// the derived URL is only the default.
		pinned = nil
	}
	return clientsCommand(ctx, args, pgstore.New(db), publicURL, pinned, time.Now(), stdout, stderr)
}

// clientStore holds the clients and the subscriptions that services make,
// and the accounts that org grants name. pgstore.Store is one.
type clientStore interface {
	oauth.Store
	oauth.Repositories
	DeleteSubscriptionsOf(ctx context.Context, principal string) (int64, error)
}

func isClientsCommand(name string) bool {
	switch name {
	case "create", "list", "disable", "update", "trust", "trusts", "untrust":
		return true
	}
	return false
}

// clientsCommand runs one command. publicURL is nil when the server has no
// public URL, and pinned is publicURL when FAUCET_PUBLIC_URL set it.
func clientsCommand(ctx context.Context, args []string, store clientStore, publicURL, pinned *url.URL, now time.Time, stdout, stderr io.Writer) error {
	switch args[0] {
	case "create":
		return createClient(ctx, args[1:], store, publicURL, pinned, now, stdout, stderr)
	case "list":
		if len(args) > 1 {
			return xerrors.New(clientsUsage)
		}
		return listClients(ctx, store, stdout)
	case "disable":
		if len(args) != 2 {
			return xerrors.New(clientsUsage)
		}
		return disableClient(ctx, args[1], store, now, stdout)
	case "update":
		return updateClient(ctx, args[1:], store, stdout, stderr)
	case "trust":
		return trustClient(ctx, args[1:], store, now, stdout, stderr)
	case "trusts":
		return listTrustRules(ctx, args[1:], store, stdout, stderr)
	case "untrust":
		return untrustClient(ctx, args[1:], store, stdout, stderr)
	}
	return xerrors.New(clientsUsage)
}

// disableClient disables the client, so its tokens fail on their next
// request, then deletes the subscriptions of its service and the deliveries
// they queued: faucet owns that state. Disabling a disabled client deletes
// what is left.
func disableClient(ctx context.Context, id string, store clientStore, now time.Time, stdout io.Writer) error {
	if err := oauth.ValidateClientID(id); err != nil {
		return err
	}
	c, err := store.DisableClient(ctx, id, now)
	if xerrors.Is(err, oauth.ErrUnknownClient) {
		return xerrors.Errorf("there is no client %q", id)
	}
	if err != nil {
		return err
	}
	deleted, err := store.DeleteSubscriptionsOf(ctx, faucet.ServiceLogin(c.ID))
	if err != nil {
		return xerrors.Errorf("%s is disabled, but its subscriptions could not be deleted, so run the command again: %w", c.ID, err)
	}
	_, err = fmt.Fprintf(stdout, "%s is disabled since %s, and faucet deleted its %d subscriptions. Its tokens fail on their next request.\n",
		c.ID, c.DisabledAt.UTC().Format(time.RFC3339), deleted)
	return err
}

type grantFlags []string

func (g *grantFlags) String() string { return strings.Join(*g, ",") }

func (g *grantFlags) Set(v string) error {
	*g = append(*g, v)
	return nil
}

func createClient(ctx context.Context, args []string, store clientStore, publicURL, pinned *url.URL, now time.Time, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("faucet clients create", flag.ContinueOnError)
	flags.SetOutput(stderr)
	defaultResource := ""
	if publicURL != nil {
		defaultResource = publicURL.String() + clientEndpoint
	}
	resource := flags.String("resource", defaultResource, "the MCP endpoint that the client's tokens are for, such as https://faucet.example.com/github/mcp")
	scopeList := flags.String("scope", strings.Join(scopeNames(faucet.AllScopes()), ","), "the scopes that the client's tokens can hold, comma separated")
	var grants grantFlags
	flags.Var(&grants, "grant", "what the client may read: repo:<owner>/<name>, org:<login>, org:<login>#<account ID> or all; repeat it for more")
	noSecret := flags.Bool("no-secret", false, "make a client with no secret, for workloads that its trust rules let act as it")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 1 {
		return xerrors.New("name one client ID, after the flags\n" + clientsUsage)
	}
	if err := checkResource(*resource, pinned); err != nil {
		return err
	}
	scopes, err := parseScopes(*scopeList)
	if err != nil {
		return err
	}
	bound, err := oauth.BindGrants(ctx, store, grants)
	if err != nil {
		return err
	}
	var c oauth.Client
	var secret string
	if *noSecret {
		c, err = oauth.NewSecretlessClient(flags.Arg(0), *resource, scopes, bound, now)
	} else {
		c, secret, err = oauth.NewClient(flags.Arg(0), *resource, scopes, bound, now)
	}
	if err != nil {
		return err
	}
	if err := store.InsertClient(ctx, c); err != nil {
		if xerrors.Is(err, oauth.ErrClientExists) {
			return xerrors.Errorf("client %q exists, or existed and was disabled: an ID is never given to a second client", c.ID)
		}
		return err
	}
	if _, err := fmt.Fprintf(stdout, "client_id: %s\n", c.ID); err != nil {
		return err
	}
	next := "The client has no secret: give it a trust rule with faucet clients trust " + c.ID + "."
	if c.SecretHash != nil {
		if _, err := fmt.Fprintf(stdout, "client_secret: %s\n", secret); err != nil {
			return err
		}
		next = "Keep the secret now: faucet stores only its hash and cannot show it again."
	}
	_, err = fmt.Fprintf(stderr, "The client holds %s. An org grant names the account after its #.\n%s\n", strings.Join(c.Grants, ", "), next)
	return err
}

func parseScopes(list string) ([]faucet.Scope, error) {
	var scopes []faucet.Scope
	for _, name := range strings.Split(list, ",") {
		scope, err := faucet.ParseScope(strings.TrimSpace(name))
		if err != nil {
			return nil, xerrors.Errorf("-scope: %w", err)
		}
		scopes = append(scopes, scope)
	}
	return scopes, nil
}

// updateClient sets the scopes, the grants or both of a live client in place,
// so the service keeps its ID, its secret and its trust rules, and its
// subscriptions unless -scope leaves out events:subscribe. RebindGrants binds
// the grants, so an org grant that the client holds keeps its account.
func updateClient(ctx context.Context, args []string, store clientStore, stdout, stderr io.Writer) error {
	flags := commandFlags("update", updateUsage, stderr)
	scopeList := flags.String("scope", "", "the scopes that the client's tokens can hold, comma separated, in place of the ones it holds")
	var grants grantFlags
	flags.Var(&grants, "grant", "what the client may read, in place of its grants: repo:<owner>/<name>, org:<login>, org:<login>#<account ID> or all; repeat it for more")
	var id string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		id, args = args[0], args[1:]
	}
	if err := flags.Parse(args); err != nil {
		return err
	}
	if id == "" || flags.NArg() != 0 {
		return xerrors.New("name the client ID before the flags, and nothing after them\n" + clientsUsage)
	}
	set := map[string]bool{}
	flags.Visit(func(f *flag.Flag) { set[f.Name] = true })
	if !set["scope"] && !set["grant"] {
		return xerrors.New("name -scope, -grant or both: update changes nothing else\n" + clientsUsage)
	}
	if err := oauth.ValidateClientID(id); err != nil {
		return err
	}
	c, err := knownClient(ctx, store, id)
	switch {
	case err != nil:
		return err
	case !c.Live():
		return xerrors.Errorf("client %q is disabled, and an update cannot bring it back", id)
	}
	var u oauth.ClientUpdate
	if set["scope"] {
		if u.Scopes, err = parseScopes(*scopeList); err != nil {
			return err
		}
	}
	if set["grant"] {
		if u.Grants, err = oauth.RebindGrants(ctx, store, c.Grants, grants); err != nil {
			return err
		}
	}
	updated, err := store.UpdateClient(ctx, id, u)
	if xerrors.Is(err, oauth.ErrUnknownClient) {
		return xerrors.Errorf("client %q was disabled before the update, which changed nothing", id)
	}
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(stdout, "%s holds %s, with the scopes %s.\n", updated.ID, strings.Join(updated.Grants, ", "), strings.Join(scopeNames(updated.Scopes), ",")); err != nil {
		return err
	}
	if set["scope"] && !slices.Contains(updated.Scopes, faucet.ScopeEventsSubscribe) {
		deleted, err := store.DeleteSubscriptionsOf(ctx, faucet.ServiceLogin(id))
		if err != nil {
			return xerrors.Errorf("%s holds no %s now, but its subscriptions could not be deleted, so run faucet clients update again: %w", id, faucet.ScopeEventsSubscribe, err)
		}
		if _, err := fmt.Fprintf(stdout, "It holds no %s, so faucet deleted its %d subscriptions.\n", faucet.ScopeEventsSubscribe, deleted); err != nil {
			return err
		}
	}
	_, err = fmt.Fprintln(stderr, "On its next request, a token of the client takes the new grants and holds only the scopes that it was issued with and the client holds: for a new scope, the service needs a new token. A subscription to a target that the new grants do not cover gets no renewal, and ends when it expires, within 10 minutes.")
	return err
}

// checkResource accepts the URL of faucet's GitHub MCP endpoint: faucet's
// public URL, in the form FAUCET_PUBLIC_URL takes, and clientEndpoint. With
// pinned, the URL that FAUCET_PUBLIC_URL sets, it must be that server's,
// because the server issues no token for another's.
func checkResource(resource string, pinned *url.URL) error {
	if pinned != nil {
		if want := pinned.String() + clientEndpoint; resource != want {
			return xerrors.Errorf("-resource must be %s, the endpoint of %s", want, envPublicURL)
		}
		return nil
	}
	origin, ok := strings.CutSuffix(resource, clientEndpoint)
	if !ok {
		return xerrors.Errorf("-resource must be faucet's public URL and %s; /mcp, the endpoint's old path, takes no new client", clientEndpoint)
	}
	if _, err := parseOrigin(origin); err != nil {
		return xerrors.Errorf("-resource: %w", err)
	}
	return nil
}

func listClients(ctx context.Context, store oauth.Store, stdout io.Writer) error {
	clients, err := store.Clients(ctx)
	if err != nil {
		return err
	}
	rules, err := store.CountTrustRules(ctx)
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "CLIENT ID\tRESOURCE\tSCOPES\tGRANTS\tCREATED\tDISABLED\tSECRET\tTRUST RULES")
	for _, c := range clients {
		disabled := "-"
		if !c.Live() {
			disabled = c.DisabledAt.UTC().Format(time.RFC3339)
		}
		secret := "yes"
		if c.SecretHash == nil {
			secret = "no"
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%d\n", c.ID, c.Resource, strings.Join(scopeNames(c.Scopes), ","),
			strings.Join(c.Grants, ","), c.CreatedAt.UTC().Format(time.RFC3339), disabled, secret, rules[c.ID])
	}
	return w.Flush()
}

type claimFlags map[string]string

func (c claimFlags) String() string { return strings.Join(sortedClaims(c), ",") }

func (c claimFlags) Set(v string) error {
	name, value, ok := strings.Cut(v, "=")
	if !ok {
		return xerrors.New("a claim is <name>=<value>")
	}
	if _, pinned := c[name]; pinned {
		return xerrors.Errorf("claim %s is pinned twice", name)
	}
	c[name] = value
	return nil
}

func sortedClaims(claims map[string]string) []string {
	pinned := make([]string, 0, len(claims))
	for name, value := range claims {
		pinned = append(pinned, name+"="+value)
	}
	slices.Sort(pinned)
	return pinned
}

func commandFlags(name, usage string, stderr io.Writer) *flag.FlagSet {
	flags := flag.NewFlagSet("faucet clients "+name, flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() {
		_, _ = fmt.Fprintln(stderr, "usage: "+usage)
		flags.PrintDefaults()
	}
	return flags
}

func knownClient(ctx context.Context, store oauth.Store, id string) (oauth.Client, error) {
	c, err := store.Client(ctx, id)
	if xerrors.Is(err, oauth.ErrUnknownClient) {
		return oauth.Client{}, xerrors.Errorf("there is no client %q", id)
	}
	return c, err
}

// trustClient adds a trust rule to a live client. Adding a rule that the
// client holds prints that rule's ID again.
func trustClient(ctx context.Context, args []string, store oauth.Store, now time.Time, stdout, stderr io.Writer) error {
	flags := commandFlags("trust", trustUsage, stderr)
	issuer := flags.String("issuer", "", "the issuer of the workload's tokens, as their iss names it, such as https://oidc.vercel.com/<team>")
	claims := claimFlags{}
	flags.Var(claims, "claim", "a claim that a token must hold, as <name>=<value>; repeat it for more")
	var jwksFile string
	flags.Func("jwks-file", "a file that holds the issuer's JWK Set, for an issuer that faucet cannot reach; without it, faucet fetches the keys that the issuer's discovery document names", func(path string) error {
		// A shell passes an empty path for an unset variable.
		if path == "" {
			return xerrors.New("it names no file: leave -jwks-file out for the keys that the issuer's discovery document names")
		}
		jwksFile = path
		return nil
	})
	var id string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		id, args = args[0], args[1:]
	}
	if err := flags.Parse(args); err != nil {
		return err
	}
	if id == "" || flags.NArg() != 0 {
		return xerrors.New("name the client ID before the flags, and nothing after them\n" + clientsUsage)
	}
	if err := oauth.ValidateClientID(id); err != nil {
		return err
	}
	rule := oauth.TrustRule{Issuer: *issuer, Claims: claims}
	if jwksFile != "" {
		jwks, err := readKeySetFile(jwksFile)
		if err != nil {
			return err
		}
		rule.JWKS = jwks
	}
	rule, err := oauth.CanonicalTrustRule(rule)
	if err != nil {
		return err
	}
	c, err := knownClient(ctx, store, id)
	switch {
	case err != nil:
		return err
	case !c.Live():
		return xerrors.Errorf("client %q is disabled, so no workload can act as it", id)
	}
	ruleID, err := store.AddTrustRule(ctx, id, rule, now)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "rule_id: %d\n", ruleID)
	return err
}

func readKeySetFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, xerrors.Errorf("-jwks-file: %w", err)
	}
	defer func() { _ = f.Close() }()
	jwks, err := io.ReadAll(io.LimitReader(f, oauth.MaxKeyDocumentBytes+1))
	switch {
	case err != nil:
		return nil, xerrors.Errorf("-jwks-file: %w", err)
	case len(jwks) == 0:
		return nil, xerrors.Errorf("-jwks-file %s is empty", path)
	case len(jwks) > oauth.MaxKeyDocumentBytes:
		return nil, xerrors.Errorf("-jwks-file %s is over %d bytes, the most that a key set can hold", path, oauth.MaxKeyDocumentBytes)
	case bytes.HasPrefix(jwks, []byte("\xef\xbb\xbf")):
		return nil, xerrors.Errorf("-jwks-file %s starts with a UTF-8 byte order mark, which JSON does not allow: save it without one", path)
	}
	return jwks, nil
}

func listTrustRules(ctx context.Context, args []string, store oauth.Store, stdout, stderr io.Writer) error {
	flags := commandFlags("trusts", trustsUsage, stderr)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 1 {
		return xerrors.New(clientsUsage)
	}
	id := flags.Arg(0)
	if err := oauth.ValidateClientID(id); err != nil {
		return err
	}
	if _, err := knownClient(ctx, store, id); err != nil {
		return err
	}
	rules, err := store.TrustRules(ctx, id)
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "RULE ID\tISSUER\tCLAIMS\tKEYS\tCREATED")
	for _, r := range rules {
		keys := "discovery"
		if len(r.Rule.JWKS) > 0 {
			keys = "pasted"
		}
		// json.Marshal sorts the keys, and no value can fake a column or
		// another pin.
		claims, err := json.Marshal(r.Rule.Claims)
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\n", r.ID, r.Rule.Issuer, claims, keys, r.CreatedAt.UTC().Format(time.RFC3339))
	}
	return w.Flush()
}

func untrustClient(ctx context.Context, args []string, store oauth.Store, stdout, stderr io.Writer) error {
	flags := commandFlags("untrust", untrustUsage, stderr)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 2 {
		return xerrors.New(clientsUsage)
	}
	id := flags.Arg(0)
	if err := oauth.ValidateClientID(id); err != nil {
		return err
	}
	ruleID, err := strconv.ParseInt(flags.Arg(1), 10, 64)
	if err != nil || ruleID <= 0 {
		return xerrors.Errorf("a rule ID is a whole number above 0, as faucet clients trusts %s lists", id)
	}
	if _, err := knownClient(ctx, store, id); err != nil {
		return err
	}
	err = store.RemoveTrustRule(ctx, id, ruleID)
	switch {
	case xerrors.Is(err, oauth.ErrUnknownTrustRule):
		return xerrors.Errorf("client %q has no trust rule %d", id, ruleID)
	case err != nil:
		return err
	}
	_, err = fmt.Fprintf(stdout, "Trust rule %d of %s is removed. A token that a workload got with it works until it expires.\n", ruleID, id)
	return err
}
