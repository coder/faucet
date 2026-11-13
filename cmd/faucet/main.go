// Command faucet receives GitHub webhooks and stores them as events.
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
)

const (
	envDatabaseURL = "DATABASE_URL"
	envSecret      = "GITHUB_WEBHOOK_SECRET" //nolint:gosec // The name of an environment variable, not a credential.
	envPort        = "PORT"
	defaultPort    = "8080"

	dbSetupTimeout  = 30 * time.Second
	shutdownTimeout = 15 * time.Second
)

type config struct {
	databaseURL string
	secret      string
	port        string
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
	return config{databaseURL: getenv(envDatabaseURL), secret: getenv(envSecret), port: port}, nil
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

	svc, err := faucet.New(faucet.Deps{
		Store:  pgstore.New(db),
		Secret: []byte(cfg.secret),
		Logger: logger,
	})
	if err != nil {
		return xerrors.Errorf("create service: %w", err)
	}
	if err := migrateDatabase(ctx, db); err != nil {
		return err
	}
	return serve(ctx, logger, cfg.port, newRouter(svc))
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

func newRouter(svc *faucet.Service) http.Handler {
	r := chi.NewRouter()
	r.Method(http.MethodPost, "/webhook/github", svc.WebhookHandler())
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
