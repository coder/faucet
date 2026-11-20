// Command faucet receives GitHub webhooks, stores them as events, and serves
// them to agents through the wait_for_github_events MCP tool.
package main

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"golang.org/x/xerrors"

	"cdr.dev/slog/v3"
	"cdr.dev/slog/v3/sloggers/sloghuman"
	"github.com/coder/faucet/cmd/faucet/internal/dbconn"
	"github.com/coder/faucet/faucet"
	"github.com/coder/faucet/faucet/oauth"
	"github.com/coder/faucet/faucet/pgstore"
	"github.com/coder/faucet/faucet/redelivery"
	"github.com/coder/quartz"
)

const (
	envDatabaseURL = "DATABASE_URL"
	// envDatabaseURLUnpooled is the direct connection that Neon's Vercel
	// integration sets next to DATABASE_URL, which is the pooled one. The
	// migrations run over it, because they take a session-level advisory lock
	// that a transaction-mode pooler does not keep.
	envDatabaseURLUnpooled = "DATABASE_URL_UNPOOLED"
	// envVercel is set on every Vercel deployment. envVercelEnv is "production",
	// "preview" or "development" there, and is unset when the project does not
	// expose its system environment variables.
	envVercel     = "VERCEL"
	envVercelEnv  = "VERCEL_ENV"
	envSecret     = "GITHUB_WEBHOOK_SECRET" //nolint:gosec // The name of an environment variable, not a credential.
	envPort       = "PORT"
	defaultPort   = "8080"
	envAPIURL     = "FAUCET_GITHUB_API_URL"
	defaultAPIURL = "https://api.github.com"

	envCronSecret    = "CRON_SECRET" //nolint:gosec // The name of an environment variable, not a credential.
	envRetentionDays = "FAUCET_RETENTION_DAYS"
	envHookToken     = "FAUCET_HOOK_TOKEN" //nolint:gosec // The name of an environment variable, not a credential.
	envHookOrg       = "FAUCET_HOOK_ORG"
	envHookRepo      = "FAUCET_HOOK_REPO"
	envHookID        = "FAUCET_HOOK_ID"
	// envPublicURL is faucet's public URL, where faucet's own login server
	// and the discovery documents that lead clients to it live. The server
	// needs it, because every caller logs in there.
	envPublicURL = "FAUCET_PUBLIC_URL"
	// envVercelProductionURL is the host of a project's production
	// deployment: its shortest custom domain, or its vercel.app domain.
	// Vercel sets it in every environment, previews too.
	envVercelProductionURL = "VERCEL_PROJECT_PRODUCTION_URL"
	// envRawEvents lists the GitHub events, comma-separated, that faucet
	// offers raw as github.<event>. Unset offers none.
	envRawEvents = "FAUCET_RAW_EVENTS"

	defaultRetentionDays = 30
	maxRetentionDays     = 3650
	minCronSecretBytes   = 16

	// maxOpenConns keeps a burst of requests under the connection limit of a
	// database that the server reaches directly. Idle connections close after
	// maxConnIdleTime and at most maxIdleConns stay open, because a Vercel
	// instance can freeze while it holds them, and a frozen connection holds a
	// server slot until the database times it out.
	maxOpenConns    = 10
	maxIdleConns    = 2
	maxConnIdleTime = 10 * time.Second

	dbSetupTimeout  = 30 * time.Second
	shutdownTimeout = 15 * time.Second
	githubTimeout   = 10 * time.Second
	// mcpWriteTimeout replaces the server's write deadline on the MCP
	// endpoint. It must outlast the longest wait the tool allows, which is 50
	// seconds.
	mcpWriteTimeout = 75 * time.Second
	// serverWriteTimeout is the server's write deadline, from the end of the
	// request's headers.
	serverWriteTimeout = 30 * time.Second
	// tokenTimeout keeps a request to the token endpoint inside the server's
	// write deadline, so that a slow GitHub or store fails a refresh before
	// its rotation, rather than rotating after the client got no answer.
	tokenTimeout = 25 * time.Second
)

type config struct {
	// databaseURL serves the requests.
	databaseURL string
	// migrateURL is the connection that applies the migrations, and migrateVar
	// names the variable it came from, for errors.
	migrateURL string
	migrateVar string
	deployment deployment
	secret     string
	port       string
	githubAPI  string
	// cronSecret is empty when the cron routes are off.
	cronSecret string
	retention  time.Duration
	// hookAdmin is nil when redelivery is off.
	hookAdmin redelivery.HookAdmin
	webhook   *faucet.WebhookDeps
	// publicURL is where faucet's login server lives.
	publicURL *url.URL
	// publicURLDerived is true when publicURL came from
	// VERCEL_PROJECT_PRODUCTION_URL, not FAUCET_PUBLIC_URL.
	publicURLDerived bool
	// browserLogin is nil when the browser login is off.
	browserLogin *oauth.BrowserLogin
	// rawEvents are the GitHub events offered raw. Empty offers none.
	rawEvents []string
}

func loadConfig(getenv func(string) string) (config, error) {
	var missing []string
	for _, name := range []string{envDatabaseURL, envSecret} {
		if getenv(name) == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return config{}, xerrors.Errorf("missing required environment variables: %s", strings.Join(missing, ", "))
	}

	port := getenv(envPort)
	if port == "" {
		port = defaultPort
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return config{}, xerrors.Errorf("%s must be a number from 1 to 65535", envPort)
	}

	githubAPI := getenv(envAPIURL)
	if githubAPI == "" {
		githubAPI = defaultAPIURL
	}
	if err := (faucet.TokenAuthorizer{BaseURL: githubAPI}).Validate(); err != nil {
		return config{}, xerrors.Errorf("%s: %w", envAPIURL, err)
	}
	cfg := config{databaseURL: getenv(envDatabaseURL), deployment: deploymentOf(getenv), secret: getenv(envSecret), port: port, githubAPI: githubAPI}
	cfg.migrateURL, cfg.migrateVar = cfg.databaseURL, envDatabaseURL
	if direct := getenv(envDatabaseURLUnpooled); direct != "" {
		cfg.migrateURL, cfg.migrateVar = direct, envDatabaseURLUnpooled
	}
	if err := loadCronConfig(getenv, &cfg); err != nil {
		return config{}, err
	}
	if err := loadWebhookConfig(getenv, &cfg); err != nil {
		return config{}, err
	}
	publicURL, derived, err := loadPublicURL(getenv, cfg.deployment)
	switch {
	case err != nil:
		return config{}, err
	case publicURL == nil:
		return config{}, xerrors.Errorf("%s is required: it is faucet's public URL, such as https://faucet.example.com or http://localhost:8080, where every caller logs in", envPublicURL)
	}
	cfg.publicURL, cfg.publicURLDerived = publicURL, derived
	if err := loadBrowserLoginConfig(getenv, &cfg); err != nil {
		return config{}, err
	}
	if raw := getenv(envRawEvents); raw != "" {
		events := strings.Split(raw, ",")
		for i := range events {
			events[i] = strings.TrimSpace(events[i])
		}
		if err := faucet.CheckRawEvents(events); err != nil {
			return config{}, xerrors.Errorf("%s: %w", envRawEvents, err)
		}
		cfg.rawEvents = events
	}
	return cfg, nil
}

// loadPublicURL reads faucet's public URL: FAUCET_PUBLIC_URL, or on Vercel
// production, where it is unset, https and the project's production host.
// Vercel sets that host in previews too, and a preview must not take the
// issuer of production, so only a process with VERCEL set and VERCEL_ENV
// production derives it.
// The server and the clients commands both read it here, so a client is
// made for the endpoint that the server serves. derived reports that the
// URL came from VERCEL_PROJECT_PRODUCTION_URL, and u is nil when there is
// none.
func loadPublicURL(getenv func(string) string, dep deployment) (u *url.URL, derived bool, err error) {
	name, raw := envPublicURL, getenv(envPublicURL)
	if host := getenv(envVercelProductionURL); raw == "" && host != "" && dep.onVercel && dep.env == "production" {
		name, raw, derived = envVercelProductionURL, "https://"+host, true
	}
	if raw == "" {
		return nil, false, nil
	}
	u, err = parseOrigin(raw)
	if err != nil {
		return nil, false, xerrors.Errorf("%s: %w", name, err)
	}
	return u, derived, nil
}

// loadCronConfig reads the optional settings of the cron routes. No error
// names a value, because some of the values are credentials.
func loadCronConfig(getenv func(string) string, cfg *config) error {
	cfg.cronSecret = getenv(envCronSecret)
	if cfg.cronSecret != "" && len(cfg.cronSecret) < minCronSecretBytes {
		return xerrors.Errorf("%s must be at least %d bytes", envCronSecret, minCronSecretBytes)
	}
	if cfg.cronSecret != "" && !validBearerToken(cfg.cronSecret) {
		return xerrors.Errorf("%s must be at most %d printable ASCII characters with no space, because the scheduler sends it as a bearer token", envCronSecret, maxTokenBytes)
	}

	days := defaultRetentionDays
	if raw := getenv(envRetentionDays); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > maxRetentionDays {
			return xerrors.Errorf("%s must be a whole number of days from 1 to %d", envRetentionDays, maxRetentionDays)
		}
		days = n
	}
	cfg.retention = time.Duration(days) * 24 * time.Hour

	token, org, repo, rawID := getenv(envHookToken), getenv(envHookOrg), getenv(envHookRepo), getenv(envHookID)
	if token == "" && org == "" && repo == "" && rawID == "" {
		return nil
	}
	admin, err := newHookAdmin(cfg.githubAPI, token, org, repo, rawID)
	if err != nil {
		return err
	}
	cfg.hookAdmin = admin
	return nil
}

// newHookAdmin builds the redelivery client. It runs only when some hook
// variable is set, and then every variable that is needed must be, so a typo
// does not turn redelivery off. No error names a value, because one of them is
// the hook token.
func newHookAdmin(githubAPI, token, org, repo, rawID string) (redelivery.HookAdmin, error) {
	var missing []string
	for _, v := range []struct{ name, value string }{{envHookToken, token}, {envHookID, rawID}} {
		if v.value == "" {
			missing = append(missing, v.name)
		}
	}
	switch {
	case org != "" && repo != "":
		return nil, xerrors.Errorf("set only one of %s and %s", envHookOrg, envHookRepo)
	case org == "" && repo == "":
		missing = append(missing, envHookOrg+" or "+envHookRepo)
	}
	if len(missing) > 0 {
		return nil, xerrors.Errorf("redelivery needs %s", strings.Join(missing, ", "))
	}

	id, err := strconv.ParseInt(rawID, 10, 64)
	if err != nil || id < 1 {
		return nil, xerrors.Errorf("%s must be a positive integer", envHookID)
	}
	admin, err := redelivery.NewGitHubHookAdmin(githubAPI, token, redelivery.HookTarget{Org: org, Repo: repo, HookID: id}, &http.Client{Timeout: githubTimeout})
	if err != nil {
		return nil, xerrors.Errorf("FAUCET_HOOK_*: %w", err)
	}
	return admin, nil
}

func main() {
	var err error
	if len(os.Args) > 1 && os.Args[1] == "clients" {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		err = runClients(ctx, os.Args[2:], os.Getenv, os.Stdout, os.Stderr)
		stop()
	} else {
		err = run()
	}
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "faucet:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := loadConfig(os.Getenv)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	logger := slog.Make(sloghuman.Sink(os.Stderr))
	if allowInsecureLoopback {
		logger.Warn(ctx, "built with faucet_e2e: the callback client reaches loopback over http, do not ship this binary")
	}
	if cfg.publicURLDerived {
		logger.Info(ctx, "public URL taken from "+envVercelProductionURL+", because "+envPublicURL+" is unset", slog.F("public_url", cfg.publicURL.String()))
	}

	db, err := dbconn.OpenPooled(cfg.databaseURL)
	if err != nil {
		return xerrors.Errorf("%s: %w", envDatabaseURL, err)
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(maxOpenConns)
	db.SetMaxIdleConns(maxIdleConns)
	db.SetConnMaxIdleTime(maxConnIdleTime)

	clock := quartz.NewReal()
	githubAPI := faucet.TokenAuthorizer{BaseURL: cfg.githubAPI, Client: &http.Client{Timeout: githubTimeout}}
	store := pgstore.New(db)
	tokens, login, checks, err := newLogin(cfg.publicURL, cfg.browserLogin, store, githubAPI, clock, logger)
	if err != nil {
		return err
	}
	svc, redeliverer, err := newServices(cfg, store, checks, clock, logger)
	if err != nil {
		return err
	}
	if err := migrateSchema(ctx, cfg, db); err != nil {
		return err
	}
	cron := cronRoutes{
		secret: []byte(cfg.cronSecret), svc: svc, retention: cfg.retention, delivers: cfg.webhook != nil, redeliverer: redeliverer, login: login,
		clock: clock, logger: logger,
	}
	return serve(ctx, logger, cfg.port, newRouter(svc, tokens, login, cron, logger))
}

// loginStore is what faucet's login server keeps its state in. pgstore.Store
// is one.
type loginStore interface {
	oauth.Store
	oauth.Repositories
}

// access is how the server checks what a caller may read.
type access struct {
	authorizer faucet.Authorizer
	// grants checks the owner and all targets of services.
	grants faucet.ServiceGrants
}

// newLogin returns the bearer check, the login server and the access checks
// of the server. A caller sends a token that faucet minted: a service is
// checked by its grants. With a browser login, a user who logged in through
// it is checked by GitHub with the GitHub token that faucet keeps for them:
// repos asks GitHub at every call, and the repositories that it let the user
// read stand until the user's session refreshes, which asks repos about them
// again.
func newLogin(publicURL *url.URL, browser *oauth.BrowserLogin, store loginStore, repos oauth.RepoChecker, clock quartz.Clock, logger slog.Logger) (bearers, *oauth.Server, access, error) {
	services, err := oauth.NewServiceAccess(store, store)
	if err != nil {
		return bearers{}, nil, access{}, err
	}
	subjects := map[oauth.SubjectKind]oauth.SubjectResolver{oauth.SubjectService: oauth.ServiceResolver(store)}
	var authorizer faucet.Authorizer = services
	if browser != nil {
		users, err := oauth.NewUserAccess(oauth.UserAccessDeps{Store: store, Clock: clock, Logger: logger, Login: *browser, Repos: repos})
		if err != nil {
			return bearers{}, nil, access{}, xerrors.Errorf("create the access check of the browser login: %w", err)
		}
		subjects[oauth.SubjectUser] = oauth.UserResolver(store)
		authorizer = kindAuthorizer{service: services, user: users}
	}
	verifier, err := oauth.NewVerifier(store, clock, subjects)
	if err != nil {
		return bearers{}, nil, access{}, xerrors.Errorf("create the token verifier: %w", err)
	}
	tokens, err := newBearers(verifier, publicURL.String())
	if err != nil {
		return bearers{}, nil, access{}, err
	}
	resources := make([]string, len(mcpPaths))
	for i, path := range mcpPaths {
		resources[i] = tokens.resource(path)
	}
	login, err := oauth.NewServer(oauth.ServerDeps{
		Store: store, Clock: clock, Logger: logger, Issuer: publicURL.String(), Resources: resources, Login: browser, Repos: repos,
		WorkloadKeys: faucet.HTTPClientOptions{AllowInsecureLoopback: allowInsecureLoopback},
	})
	if err != nil {
		return bearers{}, nil, access{}, xerrors.Errorf("create the login server: %w", err)
	}
	return tokens, login, access{authorizer: authorizer, grants: services}, nil
}

// kindAuthorizer sends the check of a service to the Authorizer of services,
// and every other principal's to that of users who logged in through a
// browser, which denies a principal that is no such user.
type kindAuthorizer struct {
	service faucet.Authorizer
	user    faucet.Authorizer
}

func (a kindAuthorizer) of(p faucet.Principal) faucet.Authorizer {
	if _, ok := p.ServiceClient(); ok {
		return a.service
	}
	return a.user
}

func (a kindAuthorizer) RepoID(ctx context.Context, p faucet.Principal, repo string) (int64, error) {
	return a.of(p).RepoID(ctx, p, repo)
}

func (a kindAuthorizer) RepoIDFresh(ctx context.Context, p faucet.Principal, repo string) (int64, error) {
	return a.of(p).RepoIDFresh(ctx, p, repo)
}

// newServices builds the event service and, when a webhook to redeliver is
// configured, the redelivery runner. Both use one store, which also holds
// the raw events, and the runner asks the service which failed deliveries
// a live raw interest wants.
func newServices(cfg config, store *pgstore.Store, checks access, clock quartz.Clock, logger slog.Logger) (*faucet.Service, *redelivery.Runner, error) {
	svc, err := faucet.New(faucet.Deps{
		Store:         store,
		RawStore:      store,
		RawEvents:     cfg.rawEvents,
		Secret:        []byte(cfg.secret),
		Logger:        logger,
		Authorizer:    checks.authorizer,
		ServiceGrants: checks.grants,
		PrincipalFrom: principalFrom,
		Clock:         clock,
		Webhook:       cfg.webhook,
		// The grants of services name repositories and owners, which the
		// login server finds by the deliveries that named them.
		RecordRepositories: true,
		Redelivered:        store.Redelivered,
	})
	if err != nil {
		return nil, nil, xerrors.Errorf("create service: %w", err)
	}
	if cfg.hookAdmin == nil {
		return svc, nil, nil
	}
	redeliverer, err := redelivery.New(redelivery.Deps{Store: store, Hooks: cfg.hookAdmin, Clock: clock, Logger: logger, Wants: svc.RedeliveryWants})
	if err != nil {
		return nil, nil, xerrors.Errorf("create service: redelivery: %w", err)
	}
	return svc, redeliverer, nil
}

// migrateSchema runs prepareSchema with a connection to cfg.migrateURL, which
// it closes at once: the server holds no connection of that kind while it
// serves.
func migrateSchema(ctx context.Context, cfg config, db *sql.DB) error {
	migrateDB, err := dbconn.Open(cfg.migrateURL)
	if err != nil {
		return xerrors.Errorf("%s: %w", cfg.migrateVar, err)
	}
	defer func() { _ = migrateDB.Close() }()
	return prepareSchema(ctx, db, migrateDB, cfg.deployment)
}

// deploymentOf reads where the process runs from the variables that Vercel
// sets.
func deploymentOf(getenv func(string) string) deployment {
	return deployment{onVercel: getenv(envVercel) != "", env: getenv(envVercelEnv)}
}

// deployment says where the process runs.
type deployment struct {
	onVercel bool
	// env is the value of VERCEL_ENV.
	env string
}

// migrates reports whether the process may change the schema. VERCEL_ENV
// decides when it is set. On Vercel without it, the process cannot tell that
// it is in production, so it does not migrate.
func (d deployment) migrates() bool {
	if d.env != "" {
		return d.env == "production"
	}
	return !d.onVercel
}

func (d deployment) String() string {
	if d.env == "" {
		return envVercel + " is set and " + envVercelEnv + " is not"
	}
	return envVercelEnv + "=" + d.env
}

// prepareSchema migrates the database over migrateDB when the process runs
// outside Vercel or in production. In any other Vercel environment it only
// checks over db that the schema is current and refuses to start otherwise. A
// preview runs the code of any branch, and can reach the production database,
// so a preview that migrated would leave production instances running against
// a schema they do not know.
func prepareSchema(ctx context.Context, db, migrateDB *sql.DB, dep deployment) error {
	ctx, cancel := context.WithTimeout(ctx, dbSetupTimeout)
	defer cancel()

	if err := db.PingContext(ctx); err != nil {
		return xerrors.Errorf("connect to database: %w", err)
	}
	if !dep.migrates() {
		if err := pgstore.CheckSchema(ctx, db); err != nil {
			return xerrors.Errorf("%s never migrates the database, so the schema must be current already: %w", dep, err)
		}
		return nil
	}
	if err := migrateDB.PingContext(ctx); err != nil {
		return xerrors.Errorf("connect to database to migrate: %w", err)
	}
	if err := pgstore.Migrate(ctx, migrateDB); err != nil {
		return xerrors.Errorf("migrate database: %w", err)
	}
	return nil
}

// mcpPaths are the paths of the GitHub events MCP endpoint: /github/mcp, and
// /mcp, its old path, kept for one release. Each is its own resource.
var mcpPaths = []string{"/github/mcp", "/mcp"}

// newRouter serves faucet. login is faucet's own authorization server, and
// tokens has faucet's origin. Each MCP path serves its protected resource
// metadata and names it in every 401.
func newRouter(svc *faucet.Service, tokens bearers, login *oauth.Server, cron cronRoutes, logger slog.Logger) http.Handler {
	r := chi.NewRouter()
	cron.mount(r)
	r.Method(http.MethodPost, "/webhook/github", svc.WebhookHandler())
	githubMCP := newMCPHandler(svc)
	// A faucet token is valid only on the resource it was issued for, so
	// each path checks tokens for its own.
	mcpEndpoint := func(path string) http.Handler {
		return longResponse(requireBearer(tokens, path, logger, githubMCP))
	}
	r.Handle("/github/mcp", mcpEndpoint("/github/mcp"))
	r.Handle("/mcp", mcpEndpoint("/mcp"))
	r.Handle("/.well-known/oauth-protected-resource/github/mcp", protectedResource(tokens, "/github/mcp"))
	r.Handle("/.well-known/oauth-protected-resource/mcp", protectedResource(tokens, "/mcp"))
	r.Handle("/.well-known/oauth-authorization-server", login.MetadataHandler())
	r.Handle("/oauth/authorize", login.AuthorizeHandler())
	r.Handle("/oauth/github/callback", login.GitHubCallbackHandler())
	r.Handle("/oauth/token", within(tokenTimeout, login.TokenHandler()))
	r.Handle("/oauth/revoke", login.RevokeHandler())
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok\n")
	})
	return r
}

// within runs next with a request context that ends after d.
func within(d time.Duration, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), d)
		defer cancel()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// protectedResource serves the metadata (RFC 9728) of the MCP endpoint at
// path, which names faucet's origin as its authorization server.
func protectedResource(tokens bearers, path string) http.Handler {
	return oauth.ProtectedResourceHandler(tokens.resource(path), tokens.origin.String())
}

func scopeNames(scopes []faucet.Scope) []string {
	names := make([]string, len(scopes))
	for i, s := range scopes {
		names[i] = string(s)
	}
	return names
}

func serve(ctx context.Context, logger slog.Logger, port string, handler http.Handler) error {
	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      serverWriteTimeout,
		IdleTimeout:       2 * time.Minute,
	}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	logger.Info(ctx, "listening", slog.F("addr", srv.Addr))

	select {
	case err := <-errCh:
		return xerrors.Errorf("serve: %w", err)
	case <-ctx.Done():
	}
	logger.Info(ctx, "shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return xerrors.Errorf("shutdown: %w", err)
	}
	return nil
}
