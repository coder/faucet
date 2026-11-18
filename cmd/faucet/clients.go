package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/url"
	"strings"
	"text/tabwriter"
	"time"

	"golang.org/x/xerrors"

	"github.com/coder/faucet/cmd/faucet/internal/dbconn"
	"github.com/coder/faucet/faucet"
	"github.com/coder/faucet/faucet/oauth"
	"github.com/coder/faucet/faucet/pgstore"
)

const clientsUsage = `usage:
  faucet clients create [-resource <url>] [-scope <scopes>] -grant <grant> [-grant ...] <client-id>
  faucet clients list
  faucet clients disable <client-id>

The commands read DATABASE_URL and faucet's public URL as the server does:
FAUCET_PUBLIC_URL, or on Vercel production https://$VERCEL_PROJECT_PRODUCTION_URL.
-resource defaults to the public URL and /github/mcp.
A grant is repo:<owner>/<name> for one repository, org:<login> for every
repository of the user or organization that holds the login now, under any
later login too, or all for every repository. org:<login>#<account ID> is
taken only when that account holds the login now. The client keeps each org
grant with the ID of its account, which create and list show.`

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
	return clientsCommand(ctx, args, pgstore.New(db), publicURL, time.Now(), stdout, stderr)
}

// clientStore holds the clients and the subscriptions that services make,
// and the accounts that org grants name. pgstore.Store is one.
type clientStore interface {
	oauth.Store
	oauth.Repositories
	DeleteSubscriptionsOf(ctx context.Context, principal string) (int64, error)
}

func isClientsCommand(name string) bool {
	return name == "create" || name == "list" || name == "disable"
}

// clientsCommand runs one command. publicURL is nil when the server has no
// public URL.
func clientsCommand(ctx context.Context, args []string, store clientStore, publicURL *url.URL, now time.Time, stdout, stderr io.Writer) error {
	switch args[0] {
	case "create":
		return createClient(ctx, args[1:], store, publicURL, now, stdout, stderr)
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

func createClient(ctx context.Context, args []string, store clientStore, publicURL *url.URL, now time.Time, stdout, stderr io.Writer) error {
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
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 1 {
		return xerrors.New("name one client ID, after the flags\n" + clientsUsage)
	}
	if err := checkResource(*resource, publicURL); err != nil {
		return err
	}
	var scopes []faucet.Scope
	for _, name := range strings.Split(*scopeList, ",") {
		scope, err := faucet.ParseScope(strings.TrimSpace(name))
		if err != nil {
			return xerrors.Errorf("-scope: %w", err)
		}
		scopes = append(scopes, scope)
	}
	bound, err := oauth.BindGrants(ctx, store, grants)
	if err != nil {
		return err
	}
	c, secret, err := oauth.NewClient(flags.Arg(0), *resource, scopes, bound, now)
	if err != nil {
		return err
	}
	if err := store.InsertClient(ctx, c); err != nil {
		if xerrors.Is(err, oauth.ErrClientExists) {
			return xerrors.Errorf("client %q exists, or existed and was disabled: an ID is never given to a second client", c.ID)
		}
		return err
	}
	if _, err := fmt.Fprintf(stdout, "client_id: %s\nclient_secret: %s\n", c.ID, secret); err != nil {
		return err
	}
	_, err = fmt.Fprintf(stderr, "The client holds %s. An org grant names the account after its #.\nKeep the secret now: faucet stores only its hash and cannot show it again.\n", strings.Join(c.Grants, ", "))
	return err
}

// checkResource accepts the URL of faucet's GitHub MCP endpoint: faucet's
// public URL, in the form FAUCET_PUBLIC_URL takes, and clientEndpoint. With
// publicURL, it must be that server's, because the server issues no token
// for another's.
func checkResource(resource string, publicURL *url.URL) error {
	if publicURL != nil {
		if want := publicURL.String() + clientEndpoint; resource != want {
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
	w := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "CLIENT ID\tRESOURCE\tSCOPES\tGRANTS\tCREATED\tDISABLED")
	for _, c := range clients {
		disabled := "-"
		if !c.Live() {
			disabled = c.DisabledAt.UTC().Format(time.RFC3339)
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", c.ID, c.Resource, strings.Join(scopeNames(c.Scopes), ","),
			strings.Join(c.Grants, ","), c.CreatedAt.UTC().Format(time.RFC3339), disabled)
	}
	return w.Flush()
}
