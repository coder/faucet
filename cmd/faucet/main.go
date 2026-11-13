// Command faucet receives GitHub webhooks, stores them as events, and serves
// them to agents through the wait_for_github_events MCP tool.
package main

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"net/http"
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
	"github.com/coder/faucet/faucet/pgstore"
	"github.com/coder/quartz"
)

const (
	envDatabaseURL = "DATABASE_URL"
	envSecret      = "GITHUB_WEBHOOK_SECRET" //nolint:gosec // The name of an environment variable, not a credential.
	envPort        = "PORT"
	defaultPort    = "8080"
	envAPIURL      = "FAUCET_GITHUB_API_URL"
	defaultAPIURL  = "https://api.github.com"

	envCronSecret    = "CRON_SECRET" //nolint:gosec // The name of an environment variable, not a credential.
	envRetentionDays = "FAUCET_RETENTION_DAYS"

	defaultRetentionDays = 30
	maxRetentionDays     = 3650
	minCronSecretBytes   = 16

	dbSetupTimeout  = 30 * time.Second
	shutdownTimeout = 15 * time.Second
	githubTimeout   = 10 * time.Second
	// mcpWriteTimeout replaces the server's write deadline on /mcp. It must
	// outlast the longest wait the tool allows, which is 50 seconds.
	mcpWriteTimeout = 75 * time.Second
)

type config struct {
	databaseURL string
	secret      string
	port        string
	githubAPI   string
	// cronSecret is empty when the cron routes are off.
	cronSecret string
	retention  time.Duration
	webhook    *faucet.WebhookDeps
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
	cfg := config{databaseURL: getenv(envDatabaseURL), secret: getenv(envSecret), port: port, githubAPI: githubAPI}
	if err := loadCronConfig(getenv, &cfg); err != nil {
		return config{}, err
	}
	if err := loadWebhookConfig(getenv, &cfg); err != nil {
		return config{}, err
	}
	return cfg, nil
}

// loadCronConfig reads the optional settings of the cron routes. No error
// names a value, because some of the values are credentials.
func loadCronConfig(getenv func(string) string, cfg *config) error {
	cfg.cronSecret = getenv(envCronSecret)
	if cfg.cronSecret != "" && len(cfg.cronSecret) < minCronSecretBytes {
		return xerrors.Errorf("%s must be at least %d bytes", envCronSecret, minCronSecretBytes)
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
	return nil
}

func main() {
	if err := run(); err != nil {
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

	db, err := dbconn.Open(cfg.databaseURL)
	if err != nil {
		return xerrors.Errorf("%s: %w", envDatabaseURL, err)
	}
	defer func() { _ = db.Close() }()

	github := faucet.TokenAuthorizer{BaseURL: cfg.githubAPI, Client: &http.Client{Timeout: githubTimeout}}
	clock := quartz.NewReal()
	svc, err := faucet.New(faucet.Deps{
		Store:         pgstore.New(db),
		Secret:        []byte(cfg.secret),
		Logger:        logger,
		Authorizer:    github,
		PrincipalFrom: principalFrom,
		Clock:         clock,
		Webhook:       cfg.webhook,
	})
	if err != nil {
		return xerrors.Errorf("create service: %w", err)
	}
	if err := migrateDatabase(ctx, db); err != nil {
		return err
	}
	cron := cronRoutes{
		secret: []byte(cfg.cronSecret), svc: svc, retention: cfg.retention, clock: clock, logger: logger,
	}
	return serve(ctx, logger, cfg.port, newRouter(svc, github, cron, logger))
}

func migrateDatabase(ctx context.Context, db *sql.DB) error {
	ctx, cancel := context.WithTimeout(ctx, dbSetupTimeout)
	defer cancel()

	if err := db.PingContext(ctx); err != nil {
		return xerrors.Errorf("connect to database: %w", err)
	}
	if err := pgstore.Migrate(ctx, db); err != nil {
		return xerrors.Errorf("migrate database: %w", err)
	}
	return nil
}

func newRouter(svc *faucet.Service, authn authenticator, cron cronRoutes, logger slog.Logger) http.Handler {
	r := chi.NewRouter()
	cron.mount(r)
	r.Method(http.MethodPost, "/webhook/github", svc.WebhookHandler())
	r.Handle("/mcp", longResponse(requireBearer(authn, logger, newMCPHandler(svc))))
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok\n")
	})
	return r
}

func serve(ctx context.Context, logger slog.Logger, port string, handler http.Handler) error {
	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      30 * time.Second,
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
