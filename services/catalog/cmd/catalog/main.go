// Catalog service. Owns: organizers/tenants, venues, seat maps, events,
// performances, series/seasons, festival structure, rule definitions
// (ADR-002). US-002 (TKT-26): venues/events/performances/ticket types,
// publish + domain event, aggregated public reads (contract in api/openapi.yaml).
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
	_ "time/tzdata" // IANA zones inside distroless images (timezone validation)

	"github.com/go-chi/chi/v5"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/nats-io/nats.go"

	"ticketing/services/catalog/internal/api"
	"ticketing/services/catalog/internal/events"
	"ticketing/services/catalog/internal/store"
	"ticketing/shared/httpx"
	"ticketing/shared/obs"
	"ticketing/shared/runtimecfg"
)

const serviceName = "catalog"

func main() {
	if len(os.Args) > 1 {
		if sub, ok := subcommands()[os.Args[1]]; ok {
			if err := sub(os.Args[2:]); err != nil {
				fmt.Fprintf(os.Stderr, "%s %s: %v\n", serviceName, os.Args[1], err)
				os.Exit(1)
			}
			return
		}
	}
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", serviceName, err)
		os.Exit(1)
	}
}

// subcommands are the one-shot modes (the registry shape services/access and
// services/inventory use). migrate is the out-of-band migration job (ADR-022);
// reemit-policies re-emits published slots' re_entry policy so access re-projects
// slots published before the field existed (TKT-96) — a one-shot data repair,
// idempotent and safe to re-run.
func subcommands() map[string]func([]string) error {
	return map[string]func([]string) error{
		"migrate":                  func([]string) error { return migrate() },
		"healthcheck":              func([]string) error { os.Exit(healthcheck()); return nil },
		"reemit-policies":          reemitPolicies,
		"reemit-orphan-prevention": reemitOrphanPrevention,
		"provision-staff":          provisionStaffCommand,
	}
}

// migrate applies this service's embedded migrations and exits (ADR-022).
// It runs as a one-shot job that must complete before the service starts;
// the server path never migrates. Fail-fast and the 30s deadline are kept
// from ADR-008 — only the placement changed.
func migrate() error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	db, err := sql.Open("pgx", os.Getenv("DATABASE_URL"))
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer func() { _ = db.Close() }()

	if err := store.Migrate(ctx, db); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	return nil
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

// staffWriteTokenEnv names the catalog-only staff-write credential (TKT-191).
const staffWriteTokenEnv = "CATALOG_STAFF_WRITE_TOKEN"

func run() error {
	internalToken, err := runtimecfg.InternalTokenFromEnv()
	if err != nil {
		return err
	}
	// The back office's credential for catalog writes (TKT-191). Read before any
	// dependency so a misconfigured deployment fails fast rather than starting up
	// and refusing every write at runtime. Separate from INTERNAL_SERVICE_TOKEN
	// on purpose: that one opens every service, this one opens catalog writes.
	staffWriteToken, err := runtimecfg.RequiredCredential(staffWriteTokenEnv, "")
	if err != nil {
		return err
	}
	// The whole point of a second credential is that it opens LESS than the first
	// (ADR-042: this one opens catalog writes, INTERNAL_SERVICE_TOKEN opens every
	// service's internal surface). Set them to the same value and that separation
	// silently evaporates — the back office would be holding, under a different
	// name, the credential that unlocks commerce's refunds and inventory's
	// operational holds. Nothing else in the system would notice, so refuse here.
	// Neither value is echoed.
	//
	// Comparing the RAW strings is sound only because RequiredCredential has
	// already refused any value HTTP would alter in transit (ai-review pass 2):
	// header parsing strips leading/trailing whitespace, so " secret " and
	// "secret" are one credential on the wire while differing here. Without that
	// refusal upstream, this comparison would pass while the boundary it protects
	// was already gone.
	if staffWriteToken == internalToken {
		return fmt.Errorf("%s must not equal INTERNAL_SERVICE_TOKEN: the separate credential exists "+
			"so the back office cannot reach other services' internal surfaces, and identical values "+
			"remove that boundary while looking configured", staffWriteTokenEnv)
	}
	httpConfig, err := runtimecfg.HTTPFromEnv()
	if err != nil {
		return fmt.Errorf("http configuration: %w", err)
	}
	validateResponses, err := runtimecfg.ResponseValidationFromEnv()
	if err != nil {
		return fmt.Errorf("response validation configuration: %w", err)
	}
	dbConfig, err := runtimecfg.DatabaseFromEnv()
	if err != nil {
		return fmt.Errorf("database configuration: %w", err)
	}
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
	dbConfig.Apply(db)

	// Migrations ran out-of-band before this process started (ADR-022): the
	// `migrate` subcommand, as a one-shot job the service depends on.

	nc, err := nats.Connect(os.Getenv("NATS_URL"),
		nats.RetryOnFailedConnect(true), nats.MaxReconnects(-1))
	if err != nil {
		return fmt.Errorf("nats connect: %w", err)
	}
	defer nc.Close()

	publisher, err := events.NewJetStream(nc)
	if err != nil {
		return fmt.Errorf("jetstream: %w", err)
	}

	apiHandler, err := api.NewRouter(api.NewServer(store.NewPostgres(db), publisher, log, internalToken, staffWriteToken), validateResponses)
	if err != nil {
		return fmt.Errorf("api router: %w", err)
	}

	r := chi.NewRouter()
	r.Method(http.MethodGet, "/healthz", httpx.Healthz(serviceName,
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
	))
	r.Mount("/", apiHandler)

	srv := &http.Server{
		Addr:    ":" + port(),
		Handler: obs.Middleware(serviceName, obs.RequestLogger(log, r)),
	}
	httpConfig.Apply(srv)

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
