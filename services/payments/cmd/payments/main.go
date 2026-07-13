// Service skeleton (US-001). Owns: PSP port (fake provider first), wallets/cashless, NF525 journal, settlement ledger
// (ADR-002). No domain routes yet — those arrive with their stories.
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/nats-io/nats.go"

	"ticketing/services/payments/internal/api"
	paymentstore "ticketing/services/payments/internal/store"
	"ticketing/shared/httpx"
	"ticketing/shared/obs"
)

const serviceName = "payments"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "verify-journal" {
		if err := verifyJournal(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(healthcheck())
	}
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", serviceName, err)
		os.Exit(1)
	}
}

func signingConfig() (string, []byte, error) {
	id, secret := os.Getenv("JOURNAL_KEY_ID"), os.Getenv("JOURNAL_SIGNING_KEY")
	if id == "" || len(secret) < 16 {
		return "", nil, errors.New("JOURNAL_KEY_ID and JOURNAL_SIGNING_KEY (>=16 bytes) required")
	}
	return id, []byte(secret), nil
}
func verifyJournal() error {
	id, key, err := signingConfig()
	if err != nil {
		return err
	}
	db, err := sql.Open("pgx", os.Getenv("DATABASE_URL"))
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	return paymentstore.New(db, id, key).Verify(context.Background())
}

// healthcheck is the container health probe: distroless images have no
// shell/curl, so the binary probes itself (compose exec's this subcommand).
func healthcheck() int {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get("http://localhost:" + port() + "/healthz")
	if err != nil || resp.StatusCode != http.StatusOK {
		return 1
	}
	_ = resp.Body.Close()
	return 0
}

func port() string {
	if p := os.Getenv("PORT"); p != "" {
		return p
	}
	return "8080"
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log, shutdown, err := obs.Setup(ctx, serviceName)
	if err != nil {
		return fmt.Errorf("otel setup: %w", err)
	}
	defer func() {
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = shutdown(sctx)
	}()

	db, err := sql.Open("pgx", os.Getenv("DATABASE_URL"))
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer func() { _ = db.Close() }()
	mctx, mcancel := context.WithTimeout(ctx, 30*time.Second)
	defer mcancel()
	if err := paymentstore.Migrate(mctx, db); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	keyID, key, err := signingConfig()
	if err != nil {
		return err
	}
	internalToken := os.Getenv("INTERNAL_SERVICE_TOKEN")
	if internalToken == "" {
		return errors.New("INTERNAL_SERVICE_TOKEN required")
	}

	nc, err := nats.Connect(os.Getenv("NATS_URL"),
		nats.RetryOnFailedConnect(true), nats.MaxReconnects(-1))
	if err != nil {
		return fmt.Errorf("nats connect: %w", err)
	}
	defer nc.Close()

	r := chi.NewRouter()
	health := httpx.Healthz(serviceName,
		httpx.Check("db", func() error {
			pctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			return db.PingContext(pctx)
		}),
		httpx.Check("nats", func() error {
			if !nc.IsConnected() {
				return errors.New("not connected")
			}
			return nil
		}),
	)
	r.Method(http.MethodGet, "/healthz", health)
	r.Method(http.MethodGet, "/readyz", health)
	r.Mount("/", api.New(paymentstore.New(db, keyID, key), internalToken).Router())

	srv := &http.Server{
		Addr:              ":" + port(),
		Handler:           obs.Middleware(serviceName, obs.RequestLogger(log, r)),
		ReadHeaderTimeout: 5 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	log.InfoContext(ctx, "listening", "addr", srv.Addr)

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(sctx)
	}
}
