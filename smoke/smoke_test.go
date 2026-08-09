//go:build smoke

// The Compose integration seam: black-box assertions against the composed
// stack through the gateway, plus named infrastructure checks (JetStream,
// DB credential isolation, telemetry ingestion). Run via `make smoke`,
// which owns the compose lifecycle.
package smoke_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

var (
	gatewayURL = env("SMOKE_GATEWAY_URL", "http://localhost:8080")
	natsURL    = env("SMOKE_NATS_URL", "nats://localhost:4222")
	pgHostPort = env("SMOKE_PG", "localhost:5432")
	promURL    = env("SMOKE_PROM_URL", "http://localhost:9090")
	project    = env("SMOKE_COMPOSE_PROJECT", "ticketing-smoke")
)

// retry polls fn until it returns nil or the deadline passes.
func retry(t *testing.T, d time.Duration, fn func() error) {
	t.Helper()
	deadline := time.Now().Add(d)
	var err error
	for time.Now().Before(deadline) {
		if err = fn(); err == nil {
			return
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("condition not met within %s: %v", d, err)
}

func get(t *testing.T, url string, hdr map[string]string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("bad request: %v", err)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	validateServiceResponse(t, resp.Request, resp.StatusCode, resp.Header, body)
	return resp.StatusCode, body
}

func TestHealthzAllUp(t *testing.T) {
	retry(t, 60*time.Second, func() error {
		code, body := get(t, gatewayURL+"/healthz/all", nil)
		if code != http.StatusOK {
			return fmt.Errorf("status %d: %s", code, body)
		}
		var r struct {
			Status   string            `json:"status"`
			Services map[string]string `json:"services"`
		}
		if err := json.Unmarshal(body, &r); err != nil {
			return err
		}
		if len(r.Services) != 5 {
			return fmt.Errorf("want 5 services, got %v", r.Services)
		}
		for name, s := range r.Services {
			if s != "up" {
				return fmt.Errorf("%s is %s", name, s)
			}
		}
		return nil
	})
}

// The storefront root redirects into the default locale (Astro i18n). The
// redirect is asserted without following it so this test never warms the
// storefront's page-data cache before the catalog-publication flow publishes
// its fixture (catalog_publication_test.go owns the rendered-page assertions).
func TestStorefrontServedThroughGateway(t *testing.T) {
	client := &http.Client{
		Timeout: 10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Get(gatewayURL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	loc := resp.Header.Get("Location")
	redirect := resp.StatusCode == http.StatusMovedPermanently || resp.StatusCode == http.StatusFound
	if !redirect || !strings.HasPrefix(loc, "/en") {
		t.Fatalf("storefront via gateway: status %d, location %q", resp.StatusCode, loc)
	}
}

func TestScannerServedThroughGateway(t *testing.T) {
	code, body := get(t, gatewayURL+"/scanner/", nil)
	if code != http.StatusOK || !strings.Contains(string(body), "Gate scanner") {
		t.Fatalf("scanner via gateway: status %d, body %.120s", code, body)
	}
}

// TestTracePropagation sends a request with a known trace id and asserts the
// same trace_id shows up in the JSON logs of the gateway AND at least one
// service — proving W3C context propagates across the network boundary.
func TestTracePropagation(t *testing.T) {
	idBytes := make([]byte, 16)
	_, _ = rand.Read(idBytes)
	traceID := hex.EncodeToString(idBytes)
	traceparent := fmt.Sprintf("00-%s-00f067aa0ba902b7-01", traceID)

	code, _ := get(t, gatewayURL+"/healthz/all", map[string]string{"traceparent": traceparent})
	if code != http.StatusOK {
		t.Fatalf("healthz/all returned %d", code)
	}

	retry(t, 30*time.Second, func() error {
		out, err := exec.Command("docker", "compose", "-p", project, "logs",
			"gateway", "catalog", "inventory", "commerce", "payments", "access").CombinedOutput()
		if err != nil {
			return fmt.Errorf("compose logs: %v", err)
		}
		var gw, svc bool
		for _, line := range strings.Split(string(out), "\n") {
			if !strings.Contains(line, traceID) {
				continue
			}
			if strings.Contains(line, "gateway") {
				gw = true
			} else {
				svc = true
			}
		}
		if !gw || !svc {
			return fmt.Errorf("trace %s: in gateway logs=%v, in service logs=%v", traceID, gw, svc)
		}
		return nil
	})
}

// TestJetStreamPersists proves the bus is JetStream and that stack init
// provisioned the PLATFORM stream (nats-init, ADR-007) — the test asserts
// the stream exists rather than creating it, then publishes and consumes
// durably through it.
func TestJetStreamPersists(t *testing.T) {
	nc, err := nats.Connect(natsURL)
	if err != nil {
		t.Fatalf("nats connect: %v", err)
	}
	defer nc.Close()
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	stream, err := js.Stream(ctx, "PLATFORM")
	if err != nil {
		t.Fatalf("PLATFORM stream must be provisioned at stack init (nats-init): %v", err)
	}
	if _, err := js.Publish(ctx, "platform.smoke.ping", []byte("pong")); err != nil {
		t.Fatalf("publish: %v", err)
	}
	cons, err := stream.CreateOrUpdateConsumer(ctx, jetstream.ConsumerConfig{
		Durable: "smoke", FilterSubject: "platform.smoke.ping",
	})
	if err != nil {
		t.Fatalf("consumer: %v", err)
	}
	msg, err := cons.Next(jetstream.FetchMaxWait(10 * time.Second))
	if err != nil {
		t.Fatalf("consume: %v", err)
	}
	if string(msg.Data()) != "pong" {
		t.Fatalf("data = %q", msg.Data())
	}
	_ = msg.Ack()
}

// TestDBCredentialIsolation: a service's credentials open its own database
// and are rejected by every other service's database (ADR-007 boundary).
func TestDBCredentialIsolation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	own, err := pgx.Connect(ctx, fmt.Sprintf("postgres://catalog:catalog@%s/catalog", pgHostPort))
	if err != nil {
		t.Fatalf("catalog creds must reach catalog db: %v", err)
	}
	_ = own.Close(ctx)

	cross, err := pgx.Connect(ctx, fmt.Sprintf("postgres://catalog:catalog@%s/inventory", pgHostPort))
	if err == nil {
		_ = cross.Close(ctx)
		t.Fatal("catalog creds connected to inventory db — boundary not enforced")
	}
}

// services and their database/role names — every one is its own database and
// role (ADR-007), so the migrate jobs are five independent steps, never a
// cross-service coordinator (ADR-002).
var migratedServices = []string{"catalog", "inventory", "commerce", "payments", "access"}

// latestMigrationVersion reads the service's checked-in migration filenames and
// returns the highest goose version id. Derived from the files rather than
// hardcoded, so a new migration cannot silently make this assertion stale.
func latestMigrationVersion(t *testing.T, service string) int64 {
	t.Helper()
	dir := filepath.Join("..", "services", service, "internal", "store", "migrations")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s migrations: %v", service, err)
	}
	var latest int64
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		num, _, ok := strings.Cut(e.Name(), "_")
		if !ok {
			t.Fatalf("%s: migration %q has no version prefix", service, e.Name())
		}
		v, err := strconv.ParseInt(num, 10, 64)
		if err != nil {
			t.Fatalf("%s: migration %q version: %v", service, e.Name(), err)
		}
		if v > latest {
			latest = v
		}
	}
	if latest == 0 {
		t.Fatalf("%s: no migrations found in %s", service, dir)
	}
	return latest
}

// TestMigrationsAppliedOutOfBand: every service's database is migrated to the
// latest checked-in version by its one-shot migrate job (ADR-022), before the
// service is allowed to start.
//
// This is the assertion that catches a migrate job which is missing, wired to
// the wrong database, built from a stale image, or silently a no-op: /healthz
// only pings the connection, so a service will report healthy against an
// unmigrated schema right up until the first query fails. Comparing the applied
// goose version against the migration files on disk is what makes the decoupling
// real rather than assumed.
func TestMigrationsAppliedOutOfBand(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	for _, service := range migratedServices {
		t.Run(service, func(t *testing.T) {
			want := latestMigrationVersion(t, service)

			conn, err := pgx.Connect(ctx, fmt.Sprintf("postgres://%s:%s@%s/%s",
				service, service, pgHostPort, service))
			if err != nil {
				t.Fatalf("connect %s db: %v", service, err)
			}
			defer func() { _ = conn.Close(ctx) }()

			var got int64
			if err := conn.QueryRow(ctx,
				`SELECT max(version_id) FROM goose_db_version WHERE is_applied`).Scan(&got); err != nil {
				t.Fatalf("%s: read goose_db_version (migrate job did not run?): %v", service, err)
			}
			if got != want {
				t.Fatalf("%s: applied migration version = %d, want %d — the migrate job did not "+
					"apply every checked-in migration", service, got, want)
			}
		})
	}
}

func inspect(t *testing.T, container, format string) string {
	t.Helper()
	out, err := exec.Command("docker", "inspect", "-f", format, container).Output()
	if err != nil {
		t.Fatalf("docker inspect %s: %v", container, err)
	}
	return strings.TrimSpace(string(out))
}

// dependsOnLabel is where Compose records a container's resolved dependency
// edges: a comma-separated list of "<service>:<condition>:<restart-bool>".
// It is written from the merged compose files at container-create time, so it
// reports the edge the container actually ran under rather than what any single
// file says.
const dependsOnLabel = "com.docker.compose.depends_on"

// migrateGateCondition is the depends_on condition ADR-022 requires between a
// service and its one-shot migrate job.
const migrateGateCondition = "service_completed_successfully"

// dependsOnCondition returns the condition container `c` declares on compose
// service `dep`, and whether such an edge exists at all.
//
// It fails the test when the label is absent or empty rather than reporting
// "no edge": `docker inspect -f '{{index .Config.Labels "..."}}'` prints an
// empty string and exits 0 for a missing label, so an empty value is
// indistinguishable from a container that declares no dependencies — and
// treating it as "no edge" would let this assertion pass vacuously against a
// container Compose never labelled.
//
// Every entry is validated before the lookup, deliberately: returning on the
// first match would make "is the rest of this label well-formed?" depend on
// where Compose happened to place the target entry, and the order is not the
// compose file's. A format change that appears only after the match would then
// be invisible — which is the opposite of failing closed on an unrecognised
// label.
func dependsOnCondition(t *testing.T, c, dep string) (string, bool) {
	t.Helper()

	label := inspect(t, c, fmt.Sprintf("{{index .Config.Labels %q}}", dependsOnLabel))
	if label == "" {
		t.Fatalf("%s has no %s label — cannot tell whether it is gated on anything. "+
			"Either Compose did not create this container or the label format changed; "+
			"do not weaken this assertion to compensate", c, dependsOnLabel)
	}

	found := ""
	ok := false
	seen := make(map[string]bool)
	for _, entry := range strings.Split(label, ",") {
		// "<service>:<condition>:<restart-bool>" — cut twice forward rather
		// than splitting, so a ':' inside a service name cannot silently
		// re-align the fields.
		name, rest, cut := strings.Cut(entry, ":")
		if !cut {
			t.Fatalf("%s: malformed %s entry %q (want <service>:<condition>:<restart>)",
				c, dependsOnLabel, entry)
		}
		condition, restart, cut := strings.Cut(rest, ":")
		if !cut {
			t.Fatalf("%s: malformed %s entry %q (want <service>:<condition>:<restart>)",
				c, dependsOnLabel, entry)
		}
		if name == "" || condition == "" {
			t.Fatalf("%s: %s entry %q has an empty service or condition field",
				c, dependsOnLabel, entry)
		}
		// The restart flag is parsed to validate the entry's shape but its
		// value is deliberately not asserted: it governs whether Compose
		// restarts this container when the dependency restarts, which is
		// unrelated to startup gating. Asserting it would fail on unrelated
		// compose edits. ParseBool is too permissive for a format check
		// ("1", "T" would pass), so require the canonical spelling — anything
		// else means the format moved and this parser should be re-read.
		if restart != "true" && restart != "false" {
			t.Fatalf("%s: %s entry %q has restart field %q, want %q or %q",
				c, dependsOnLabel, entry, restart, "true", "false")
		}
		if seen[name] {
			t.Fatalf("%s: %s lists %q more than once (%q) — the format is not what "+
				"this parser assumes", c, dependsOnLabel, name, label)
		}
		seen[name] = true
		if name == dep {
			found, ok = condition, true
		}
	}
	return found, ok
}

// dependsOnRequired reports whether compose service `svc` declares its `dep`
// dependency as required, in the merged configuration the running stack was
// created from.
//
// This exists because `required` is NOT encoded in the depends_on label:
// `required: false` and the default `required: true` serialize identically to
// "<dep>:<condition>:<restart>". A migrate edge marked optional still carries
// condition service_completed_successfully, so the label check alone cannot see
// it — and an optional edge is exactly the weakening that matters: Compose
// SKIPS a failed optional dependency and starts the service anyway, which is
// the ADR-022 violation this test is for.
//
// The merged config is reconstructed from the labels Compose wrote onto the
// container itself (project, config files, working dir), not from an assumption
// about how scripts/smoke.sh was invoked — so this reads the same file set that
// actually produced the stack, whatever overrides were in play.
func dependsOnRequired(t *testing.T, c, svc, dep string) bool {
	t.Helper()

	project := inspect(t, c, `{{index .Config.Labels "com.docker.compose.project"}}`)
	files := inspect(t, c, `{{index .Config.Labels "com.docker.compose.project.config_files"}}`)
	workdir := inspect(t, c, `{{index .Config.Labels "com.docker.compose.project.working_dir"}}`)
	if project == "" || files == "" || workdir == "" {
		t.Fatalf("%s: compose project/config-file/working-dir labels are missing (%q/%q/%q) — "+
			"cannot re-read the configuration this stack was created from",
			c, project, files, workdir)
	}

	// The file list must be passed explicitly: `docker compose -p <project>
	// config` alone re-discovers the default compose.yaml and SILENTLY DROPS
	// the -f overrides the stack was created with, which would report the base
	// file's edge and miss an override that weakened it.
	//
	// Compose joins the paths with "," in this label, and a directory name may
	// legitimately contain a comma, so a naive split can invent nonexistent
	// paths. Rejoin any fragment that is not an existing file with the next one
	// before giving up — the paths are absolute and must exist, so the
	// filesystem resolves the ambiguity the label cannot.
	var paths []string
	for _, f := range strings.Split(files, ",") {
		if n := len(paths); n > 0 {
			if _, err := os.Stat(paths[n-1]); err != nil {
				paths[n-1] += "," + f
				continue
			}
		}
		paths = append(paths, f)
	}
	args := []string{"compose", "-p", project}
	for _, p := range paths {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("%s: compose config file %q from the %s label does not exist: %v",
				c, p, "com.docker.compose.project.config_files", err)
		}
		args = append(args, "-f", p)
	}
	args = append(args, "config", "--format", "json")

	cmd := exec.Command("docker", args...)
	cmd.Dir = workdir
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		// Almost always interpolation: compose.yaml declares required
		// variables with ${VAR:?...}, and `config` re-interpolates them, so
		// this fails unless the caller's environment carries them. It does
		// today — scripts/smoke.sh sources scripts/stack-env.sh, which exports
		// every such variable precisely so the stack never depends on a
		// developer's .env (and so CI needs no secret) — but the coupling is
		// implicit, so name it rather than leaving a bare exit status.
		t.Fatalf("%s: docker compose config failed: %v\n%s\n"+
			"If this is an interpolation error, the required credentials are not in this "+
			"process's environment; run the suite through scripts/smoke.sh, which exports them.",
			c, err, strings.TrimSpace(stderr.String()))
	}

	var cfg struct {
		Services map[string]struct {
			DependsOn map[string]struct {
				Condition string `json:"condition"`
				// Pointer so an absent key is distinguishable from an explicit
				// false: Compose's default is true, and defaulting a missing
				// key to false would fail every correct stack.
				Required *bool `json:"required"`
			} `json:"depends_on"`
		} `json:"services"`
	}
	if err := json.Unmarshal(out, &cfg); err != nil {
		t.Fatalf("%s: parse docker compose config: %v", c, err)
	}

	edge, ok := cfg.Services[svc].DependsOn[dep]
	if !ok {
		t.Fatalf("%s: merged compose config has no %s -> %s dependency, but the container "+
			"label declares one — the config and the running stack disagree", c, svc, dep)
	}
	return edge.Required == nil || *edge.Required
}

// TestMigrationsRanBeforeServicesStarted: each service's one-shot migrate job
// exited 0, and the service is gated on that job completing successfully
// (ADR-022).
//
// Why the dependency edge and not a clock. Compose enforces the guarantee as a
// depends_on *condition*; it does not promise anything about elapsed time. This
// test used to compare the job's FinishedAt against the service's StartedAt,
// which is a different claim: under CPU contention those recorded timestamps
// can invert while the condition held perfectly (TKT-232 — payments inverted by
// 519ms on a loaded gate). The proxy was weak in the other direction too, which
// mattered more: with the condition removed, an unloaded box would usually
// still record the job finishing first, so the old form could pass while the
// guarantee was gone. Reading the resolved edge fails in exactly one case — the
// edge is missing or weakened — which is the case ADR-022 wants caught.
//
// Do not "fix" a red here by widening a tolerance or retrying: there is no
// timing left to be flaky, so a failure means the gating is actually broken.
//
// What this does and does not prove. It reads the edge Compose *resolved* and
// recorded at container-create time, plus the `required` flag the label does
// not encode, so it proves the declaration the stack was created under — an
// absent, failing, wrongly-conditioned, or optional migrate job. It is
// not historical evidence that this particular service process was gated by
// this particular job run: `docker start` on an existing container bypasses
// depends_on entirely and leaves the label untouched. That is deliberate, and
// it is the same scope ADR-022 itself claims — `service_completed_successfully`
// "gates at stack creation" (§Consequences), and a restarted container re-runs
// run() without re-running its completed one-shot job. The gate only ever
// observes freshly created containers: scripts/smoke.sh pre-cleans with
// `down -v` and then `up -d --wait`, and never issues docker start/restart.
//
// The wall-clock form this replaced did not close that gap either — an ungated
// `docker start` of the service, long after its job finished, satisfies
// finished < started just as happily. It detected only the narrow case where a
// job's re-run happened to land after the service's start, at the cost of
// failing on a busy machine when nothing was wrong (TKT-232).
//
// It also does NOT prove the server never migrates: a job that exits 0 and a
// server that also migrates satisfies it. TestServerModeDoesNotMigrate is what
// closes that; the three assertions are complementary and none is sufficient
// alone.
func TestMigrationsRanBeforeServicesStarted(t *testing.T) {
	for _, service := range migratedServices {
		t.Run(service, func(t *testing.T) {
			job := fmt.Sprintf("%s-%s-migrate-1", project, service)
			srv := fmt.Sprintf("%s-%s-1", project, service)

			if code := inspect(t, job, "{{.State.ExitCode}}"); code != "0" {
				t.Fatalf("%s exited %s, want 0", job, code)
			}
			if running := inspect(t, job, "{{.State.Running}}"); running != "false" {
				t.Fatalf("%s still running — it must be one-shot", job)
			}

			// The compose service name, not the container name: the label holds
			// service names. Matching the specific <service>-migrate edge is
			// what makes this assertion real — nats-init carries the same
			// condition, so searching the label for service_completed_successfully
			// would pass with the migrate edge deleted outright.
			dep := service + "-migrate"
			condition, ok := dependsOnCondition(t, srv, dep)
			if !ok {
				t.Fatalf("%s declares no dependency on %s — its migrations are not gating "+
					"startup at all (ADR-022)", srv, dep)
			}
			if condition != migrateGateCondition {
				t.Fatalf("%s depends on %s with condition %q, want %q — the service may start "+
					"before its migrations complete (ADR-022)", srv, dep, condition, migrateGateCondition)
			}
			// An OPTIONAL edge carries the right condition and is invisible in
			// the label, but Compose skips a failed optional dependency and
			// starts the service regardless — so the gate would hold only for
			// as long as migrations happen to succeed.
			if !dependsOnRequired(t, srv, service, dep) {
				t.Fatalf("%s depends on %s with required:false — Compose will SKIP the job if it "+
					"fails and start %s against an unmigrated schema (ADR-022). The condition is "+
					"correct, which is why the container label alone does not show this",
					srv, dep, service)
			}
		})
	}
}

// TestServerModeDoesNotMigrate: the server path never applies migrations
// (ADR-022) — the negative proof the other two assertions cannot give.
//
// Neither TestMigrationsAppliedOutOfBand nor TestMigrationsRanBeforeServicesStarted
// fails if someone re-adds store.Migrate to run(): the job would migrate first and
// the server's call would be a silent no-op, so both stay green while the startup
// coupling this ADR removes is quietly back. This test is the one that fails.
//
// Method: run catalog in server mode against an empty database, with everything
// else real. Reaching a passing healthcheck is the positive control — it proves
// the process got past the point where migration used to run (main.go opened the
// DB, then listened), so an absent goose_db_version is a real negative and not a
// process that died early. If run() migrates, the table appears and this fails.
func TestServerModeDoesNotMigrate(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	const probeDB = "placement_probe"
	pg := project + "-postgres-1"
	psql := func(sql string) {
		t.Helper()
		if out, err := exec.Command("docker", "exec", pg, "psql", "-U", "postgres",
			"-v", "ON_ERROR_STOP=1", "-c", sql).CombinedOutput(); err != nil {
			t.Fatalf("psql %q: %v: %s", sql, err, out)
		}
	}
	// Separate -c flags: psql wraps a multi-statement -c in a transaction, and
	// DROP/CREATE DATABASE cannot run inside one.
	psql("DROP DATABASE IF EXISTS " + probeDB)
	psql("CREATE DATABASE " + probeDB + " OWNER catalog")
	t.Cleanup(func() {
		_ = exec.Command("docker", "exec", pg, "psql", "-U", "postgres",
			"-c", "DROP DATABASE IF EXISTS "+probeDB).Run()
	})

	probe := project + "-placement-probe"
	_ = exec.Command("docker", "rm", "-f", probe).Run()
	out, err := exec.Command("docker", "run", "-d", "--name", probe,
		"--network", project+"_default",
		"-e", fmt.Sprintf("DATABASE_URL=postgres://catalog:catalog@postgres:5432/%s", probeDB),
		"-e", "NATS_URL=nats://nats:4222",
		"-e", "OTEL_EXPORTER_OTLP_ENDPOINT=http://lgtm:4318",
		"-e", "INTERNAL_SERVICE_TOKEN="+os.Getenv("SMOKE_INTERNAL_TOKEN"),
		// TKT-191: catalog refuses to start without its staff-write credential.
		// Supplied here so this probe still tests what it is about (that the
		// server path does not migrate) rather than failing at configuration.
		"-e", "CATALOG_STAFF_WRITE_TOKEN="+os.Getenv("SMOKE_CATALOG_STAFF_WRITE_TOKEN"),
		project+"-catalog").CombinedOutput()
	if err != nil {
		t.Fatalf("start probe: %v: %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", probe).Run() })

	// Positive control: wait until the server is actually serving. Without this a
	// crash-on-boot would look identical to "did not migrate" and pass vacuously.
	for exec.Command("docker", "exec", probe, "/app", "healthcheck").Run() != nil {
		if ctx.Err() != nil {
			logs, _ := exec.Command("docker", "logs", "--tail", "20", probe).CombinedOutput()
			t.Fatalf("probe never became healthy — cannot conclude anything about migration; logs:\n%s", logs)
		}
		time.Sleep(time.Second)
	}

	conn, err := pgx.Connect(ctx, fmt.Sprintf("postgres://catalog:catalog@%s/%s", pgHostPort, probeDB))
	if err != nil {
		t.Fatalf("connect %s: %v", probeDB, err)
	}
	defer func() { _ = conn.Close(ctx) }()

	var table *string
	if err := conn.QueryRow(ctx, `SELECT to_regclass('public.goose_db_version')::text`).Scan(&table); err != nil {
		t.Fatalf("probe goose table: %v", err)
	}
	if table != nil {
		t.Fatalf("catalog in server mode created %q in an unmigrated database — the server path is "+
			"migrating again (ADR-022: migrations belong to the one-shot job only)", *table)
	}
}

// TestCommerceStartsWithoutRunningBackfill: commerce startup does no data work
// that can fail the service (TKT-71). The completion-outbox backfill lives behind
// the drainer, so a commerce process pointed at an *unmigrated* database — where
// the backfill's query can only error — must still become healthy. Before TKT-71
// that process exited on the failed backfill before ever listening; the passing
// healthcheck is what proves the coupling is gone. Same probe method as
// TestServerModeDoesNotMigrate, and the healthcheck doubles as the positive
// control against a vacuous pass.
func TestCommerceStartsWithoutRunningBackfill(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	const probeDB = "backfill_probe"
	pg := project + "-postgres-1"
	psql := func(sql string) {
		t.Helper()
		if out, err := exec.Command("docker", "exec", pg, "psql", "-U", "postgres",
			"-v", "ON_ERROR_STOP=1", "-c", sql).CombinedOutput(); err != nil {
			t.Fatalf("psql %q: %v: %s", sql, err, out)
		}
	}
	psql("DROP DATABASE IF EXISTS " + probeDB)
	psql("CREATE DATABASE " + probeDB + " OWNER commerce")
	t.Cleanup(func() {
		_ = exec.Command("docker", "exec", pg, "psql", "-U", "postgres",
			"-c", "DROP DATABASE IF EXISTS "+probeDB).Run()
	})

	probe := project + "-backfill-probe"
	_ = exec.Command("docker", "rm", "-f", probe).Run()
	out, err := exec.Command("docker", "run", "-d", "--name", probe,
		"--network", project+"_default",
		"-e", fmt.Sprintf("DATABASE_URL=postgres://commerce:commerce@postgres:5432/%s", probeDB),
		"-e", "NATS_URL=nats://nats:4222",
		"-e", "OTEL_EXPORTER_OTLP_ENDPOINT=http://lgtm:4318",
		"-e", "INTERNAL_SERVICE_TOKEN="+os.Getenv("SMOKE_INTERNAL_TOKEN"),
		// TKT-194: commerce refuses to start without its staff-write credential,
		// for the same reason catalog does — a commerce started without it
		// answers every refund 404, which is indistinguishable from "no such
		// order", so the misconfiguration would arrive as a support ticket.
		"-e", "COMMERCE_STAFF_WRITE_TOKEN="+os.Getenv("SMOKE_COMMERCE_STAFF_WRITE_TOKEN"),
		// TKT-221: commerce refuses to start without it, and refuses again if it
		// equals either other credential — smoke.sh generates all three separately.
		"-e", "COMMERCE_CUSTOMER_ASSERTION_KEY="+os.Getenv("SMOKE_COMMERCE_CUSTOMER_ASSERTION_KEY"),
		"-e", "CATALOG_URL=http://catalog:8080",
		"-e", "INVENTORY_URL=http://inventory:8080",
		"-e", "PAYMENTS_URL=http://payments:8080",
		project+"-commerce").CombinedOutput()
	if err != nil {
		t.Fatalf("start probe: %v: %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", probe).Run() })

	for exec.Command("docker", "exec", probe, "/app", "healthcheck").Run() != nil {
		if ctx.Err() != nil {
			logs, _ := exec.Command("docker", "logs", "--tail", "20", probe).CombinedOutput()
			t.Fatalf("commerce never became healthy against an unmigrated database — startup "+
				"still depends on data work (TKT-71); logs:\n%s", logs)
		}
		time.Sleep(time.Second)
	}
}

// TestCommerceBackfillRepairsSeededOrder pins main.go's backfill wiring end-to-end
// (TKT-94): the drainer unit tests prove Run invokes a non-nil backfill, and
// TestCommerceStartsWithoutRunningBackfill proves startup doesn't depend on it —
// but neither fails if main.go hands outbox.New nil instead of the real
// BackfillCompletionOutbox closure. This test does: a dedicated probe database is
// migrated out-of-band (ADR-022's one-shot subcommand), seeded with one pre-outbox
// completed order (completed, guest_order_ref set, no completion_outbox row), and a
// throwaway commerce container is started against it. The real wiring inserts the
// owed row; nil wiring leaves the service healthy and the row absent forever.
// Row existence + subject only — publication is at-least-once (ADR-016) and rows
// are retired by setting published_at, not deleted, so existence is durable and
// the assertion never waits on the broker.
func TestCommerceBackfillRepairsSeededOrder(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	const probeDB = "backfill_wiring_probe"
	pg := project + "-postgres-1"
	psql := func(sql string) {
		t.Helper()
		if out, err := exec.Command("docker", "exec", pg, "psql", "-U", "postgres",
			"-v", "ON_ERROR_STOP=1", "-c", sql).CombinedOutput(); err != nil {
			t.Fatalf("psql %q: %v: %s", sql, err, out)
		}
	}
	psql("DROP DATABASE IF EXISTS " + probeDB)
	psql("CREATE DATABASE " + probeDB + " OWNER commerce")
	t.Cleanup(func() {
		_ = exec.Command("docker", "exec", pg, "psql", "-U", "postgres",
			"-c", "DROP DATABASE IF EXISTS "+probeDB).Run()
	})

	// Migrate the probe DB the way the real stack does — the binary's one-shot
	// migrate subcommand (ADR-022), never the server path.
	if out, err := exec.Command("docker", "run", "--rm",
		"--network", project+"_default",
		"-e", fmt.Sprintf("DATABASE_URL=postgres://commerce:commerce@postgres:5432/%s", probeDB),
		project+"-commerce", "migrate").CombinedOutput(); err != nil {
		t.Fatalf("migrate probe DB: %v: %s", err, out)
	}

	conn, err := pgx.Connect(ctx, fmt.Sprintf("postgres://commerce:commerce@%s/%s", pgHostPort, probeDB))
	if err != nil {
		t.Fatalf("connect %s: %v", probeDB, err)
	}
	defer func() { _ = conn.Close(ctx) }()

	// One pre-outbox completed order: reservation + order rows inserted directly —
	// CompleteOrder would insert the outbox row itself, which is exactly what the
	// historical orders this backfill exists for never got.
	reservationID, orderID := uuid.NewString(), uuid.NewString()
	if _, err := conn.Exec(ctx, `
		INSERT INTO reservations (id, organizer_id, hold_id, slot_id, ticket_type_id, buyer_id,
			quantity, unit_amount, total_amount, face_value_amount, currency, status)
		VALUES ($1, $2, $3, $4, $5, $6, 1, 1000, 1000, 1000, 'CAD', 'completed')`,
		reservationID, uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()); err != nil {
		t.Fatalf("seed reservation: %v", err)
	}
	if _, err := conn.Exec(ctx, `
		INSERT INTO orders (id, reservation_id, status, idempotency_key, request_fingerprint, guest_order_ref)
		VALUES ($1, $2, 'completed', $3, 'tkt94-fingerprint', $4)`,
		orderID, reservationID, "tkt94-"+orderID, uuid.NewString()); err != nil {
		t.Fatalf("seed order: %v", err)
	}
	var n int
	if err := conn.QueryRow(ctx,
		`SELECT count(*) FROM completion_outbox WHERE order_id=$1`, orderID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("precondition: want zero outbox rows for the seeded order, got n=%d err=%v", n, err)
	}

	probe := project + "-backfill-wiring-probe"
	_ = exec.Command("docker", "rm", "-f", probe).Run()
	out, err := exec.Command("docker", "run", "-d", "--name", probe,
		"--network", project+"_default",
		"-e", fmt.Sprintf("DATABASE_URL=postgres://commerce:commerce@postgres:5432/%s", probeDB),
		"-e", "NATS_URL=nats://nats:4222",
		"-e", "OTEL_EXPORTER_OTLP_ENDPOINT=http://lgtm:4318",
		"-e", "INTERNAL_SERVICE_TOKEN="+os.Getenv("SMOKE_INTERNAL_TOKEN"),
		// TKT-194: commerce refuses to start without its staff-write credential,
		// for the same reason catalog does — a commerce started without it
		// answers every refund 404, which is indistinguishable from "no such
		// order", so the misconfiguration would arrive as a support ticket.
		"-e", "COMMERCE_STAFF_WRITE_TOKEN="+os.Getenv("SMOKE_COMMERCE_STAFF_WRITE_TOKEN"),
		// TKT-221: commerce refuses to start without it, and refuses again if it
		// equals either other credential — smoke.sh generates all three separately.
		"-e", "COMMERCE_CUSTOMER_ASSERTION_KEY="+os.Getenv("SMOKE_COMMERCE_CUSTOMER_ASSERTION_KEY"),
		"-e", "CATALOG_URL=http://catalog:8080",
		"-e", "INVENTORY_URL=http://inventory:8080",
		"-e", "PAYMENTS_URL=http://payments:8080",
		project+"-commerce").CombinedOutput()
	if err != nil {
		t.Fatalf("start probe: %v: %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", probe).Run() })

	// Positive control: a healthy probe proves the process booted and the drainer
	// goroutine is running, so an absent outbox row below is broken wiring, not a
	// process that died early.
	for exec.Command("docker", "exec", probe, "/app", "healthcheck").Run() != nil {
		if ctx.Err() != nil {
			logs, _ := exec.Command("docker", "logs", "--tail", "20", probe).CombinedOutput()
			t.Fatalf("probe never became healthy — cannot conclude anything about the backfill; logs:\n%s", logs)
		}
		time.Sleep(time.Second)
	}

	// The drainer runs the backfill before its first drain pass, so the row appears
	// within container-boot time, not a drain interval. published_at is deliberately
	// unconstrained: the probe's drainer may already have published and retired the
	// row via the shared NATS — existence is the wiring pin.
	deadline := time.Now().Add(30 * time.Second)
	for {
		var got int
		if err := conn.QueryRow(ctx, `SELECT count(*) FROM completion_outbox
			WHERE order_id=$1 AND subject='platform.commerce.order.completed'`, orderID).Scan(&got); err != nil {
			t.Fatalf("poll completion_outbox: %v", err)
		}
		if got == 1 {
			return
		}
		if time.Now().After(deadline) {
			logs, _ := exec.Command("docker", "logs", "--tail", "20", probe).CombinedOutput()
			t.Fatalf("commerce became healthy but never backfilled the seeded pre-outbox order — "+
				"main.go is not wiring the real backfill into outbox.New (TKT-94); logs:\n%s", logs)
		}
		time.Sleep(time.Second)
	}
}

// TestMetricsIngested asserts application metrics flow to the otel-lgtm
// Prometheus after real traffic.
func TestMetricsIngested(t *testing.T) {
	get(t, gatewayURL+"/healthz/all", nil) // generate traffic
	retry(t, 60*time.Second, func() error {
		code, body := get(t, promURL+`/api/v1/query?query=count({__name__=~"http_server_.%2B"})`, nil)
		if code != http.StatusOK {
			return fmt.Errorf("prom query status %d", code)
		}
		var r struct {
			Data struct {
				Result []struct {
					Value []any `json:"value"`
				} `json:"result"`
			} `json:"data"`
		}
		if err := json.Unmarshal(body, &r); err != nil {
			return err
		}
		if len(r.Data.Result) == 0 {
			return fmt.Errorf("no http_server_* series yet")
		}
		return nil
	})
}

// TestServerRefusesToStartWithoutARealCredential: every service image fails
// fast — before any dependency init — when INTERNAL_SERVICE_TOKEN is absent or
// is the retired checked-in default (TKT-83). Black-box on the built images so
// a service whose entrypoint stops calling the shared validator fails here, not
// in a code-review comment. No DB/NATS env is provided on purpose: an error
// mentioning anything but the credential means validation ran too late.
func TestServerRefusesToStartWithoutARealCredential(t *testing.T) {
	cases := []struct{ name, tokenEnv string }{
		{"absent", ""},
		{"retired-default", "INTERNAL_SERVICE_TOKEN=local-service-token"},
	}
	for _, service := range migratedServices {
		for _, tc := range cases {
			t.Run(service+"/"+tc.name, func(t *testing.T) {
				t.Parallel()
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				args := []string{"run", "--rm"}
				if tc.tokenEnv != "" {
					args = append(args, "-e", tc.tokenEnv)
				}
				args = append(args, project+"-"+service)
				out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
				if ctx.Err() != nil {
					t.Fatalf("did not exit within 15s — credential validation is not first in run(): %s", out)
				}
				if err == nil {
					t.Fatalf("started without a real credential: %s", out)
				}
				if !strings.Contains(string(out), "INTERNAL_SERVICE_TOKEN") {
					t.Fatalf("exit was not the credential error (validation ran too late?): %v: %s", err, out)
				}
			})
		}
	}
}
