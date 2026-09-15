//go:build smoke

package smoke_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestPaymentsRuntimeDecoupledFromNATS verifies:
//  1. Composed container dependencies retain postgres and payments-migrate, exclude nats/nats-init,
//     and verify NATS_URL is excluded from container environment.
//  2. An isolated payments container booted with malformed NATS_URL starts cleanly and serves
//     db-only health and readiness checks on /healthz and /readyz.
//  3. Database outage transitions /healthz and /readyz to 503 degraded (checks: {"db": "unhealthy"}).
//  4. Database recovery transitions /healthz and /readyz back to 200 ok (checks: {"db": "ok"}).
func TestPaymentsRuntimeDecoupledFromNATS(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	t.Run("ComposeDependencies", func(t *testing.T) {
		paymentsContainer := project + "-payments-1"

		cond, ok := dependsOnCondition(t, paymentsContainer, "postgres")
		if !ok || cond != "service_healthy" {
			t.Errorf("%s depends_on postgres = %q, ok=%v; want condition=%q, ok=true", paymentsContainer, cond, ok, "service_healthy")
		}

		cond, ok = dependsOnCondition(t, paymentsContainer, "payments-migrate")
		if !ok || cond != migrateGateCondition {
			t.Errorf("%s depends_on payments-migrate = %q, ok=%v; want condition=%q, ok=true", paymentsContainer, cond, ok, migrateGateCondition)
		}

		if _, ok := dependsOnCondition(t, paymentsContainer, "nats"); ok {
			t.Errorf("%s depends_on unexpectedly includes nats", paymentsContainer)
		}
		if _, ok := dependsOnCondition(t, paymentsContainer, "nats-init"); ok {
			t.Errorf("%s depends_on unexpectedly includes nats-init", paymentsContainer)
		}

		envOut := inspect(t, paymentsContainer, `{{range .Config.Env}}{{println .}}{{end}}`)
		for _, line := range strings.Split(envOut, "\n") {
			if strings.HasPrefix(line, "NATS_URL=") {
				t.Errorf("%s has NATS_URL configured in environment; want excluded", paymentsContainer)
				break
			}
		}
	})

	t.Run("RuntimeHealthAndOutageRecovery", func(t *testing.T) {
		pg := project + "-postgres-1"
		network := project + "_default"
		image := project + "-payments"
		probeDB := fmt.Sprintf("payments_probe_%x", time.Now().UnixNano())
		probeContainer := fmt.Sprintf("payments-probe-%x", time.Now().UnixNano())

		psql := func(sql string) {
			t.Helper()
			cmd := exec.CommandContext(ctx, "docker", "exec", pg, "psql", "-U", "postgres", "-v", "ON_ERROR_STOP=1", "-c", sql)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("psql execution failed: %v: %s", err, out)
			}
		}

		psql("DROP DATABASE IF EXISTS " + probeDB)
		psql("CREATE DATABASE " + probeDB + " OWNER payments")

		t.Cleanup(func() {
			cctx, ccancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer ccancel()
			_ = exec.CommandContext(cctx, "docker", "rm", "-f", probeContainer).Run()
			_ = exec.CommandContext(cctx, "docker", "exec", pg, "psql", "-U", "postgres", "-c", "DROP DATABASE IF EXISTS "+probeDB).Run()
		})

		dbURL := containerDSN("payments", probeDB)

		// 1. Run migrations against the isolated database
		migrateCmd := exec.CommandContext(ctx, "docker", "run", "--rm",
			"--network", network,
			"-e", "DATABASE_URL="+dbURL,
			image, "migrate")
		if out, err := migrateCmd.CombinedOutput(); err != nil {
			t.Fatalf("payments migrate failed: %v: %s", err, out)
		}

		// 2. Start isolated payments container with truly malformed NATS_URL
		runCmd := exec.CommandContext(ctx, "docker", "run", "-d",
			"--name", probeContainer,
			"--network", network,
			"-p", "127.0.0.1::8080",
			"-e", "DATABASE_URL="+dbURL,
			"-e", "NATS_URL=nats://[invalid",
			"-e", "PAYMENTS_INTERNAL_TOKEN=01234567890123456789012345678901",
			"-e", "INTERNAL_SERVICE_TOKEN=abcdefabcdefabcdefabcdefabcdefabcdef",
			"-e", "JOURNAL_KEY_ID=smoke-v1",
			"-e", "JOURNAL_SIGNING_KEY=01234567890123456789",
			"-e", "STRIPE_SECRET_KEY=fake",
			"-e", "OTEL_EXPORTER_OTLP_INSECURE=true",
			"-e", "OTEL_EXPORTER_OTLP_ENDPOINT=http://lgtm:4318",
			image)
		if out, err := runCmd.CombinedOutput(); err != nil {
			t.Fatalf("start payments probe container failed: %v: %s", err, out)
		}

		// 3. Resolve host port
		portOut, portErr := exec.CommandContext(ctx, "docker", "port", probeContainer, "8080/tcp").CombinedOutput()
		if portErr != nil {
			t.Fatalf("docker port %s: %v: %s", probeContainer, portErr, portOut)
		}
		portStr := strings.TrimSpace(string(portOut))
		if idx := strings.LastIndex(portStr, ":"); idx != -1 {
			portStr = portStr[idx+1:]
		}
		baseURL := "http://127.0.0.1:" + portStr
		client := &http.Client{Timeout: 2 * time.Second}

		// 4. Positive control: assert healthy on /healthz and /readyz
		pollEndpoint(t, ctx, client, baseURL+"/healthz", http.StatusOK, "ok", "ok")
		pollEndpoint(t, ctx, client, baseURL+"/readyz", http.StatusOK, "ok", "ok")

		// 5. Database outage: disallow connections and terminate only this database's sessions
		psql("ALTER DATABASE " + probeDB + " WITH ALLOW_CONNECTIONS false")
		psql(fmt.Sprintf("SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = '%s' AND pid <> pg_backend_pid()", probeDB))

		// Assert outage transition on /healthz and /readyz
		pollEndpoint(t, ctx, client, baseURL+"/healthz", http.StatusServiceUnavailable, "degraded", "unhealthy")
		pollEndpoint(t, ctx, client, baseURL+"/readyz", http.StatusServiceUnavailable, "degraded", "unhealthy")

		// 6. Database recovery: restore connections
		psql("ALTER DATABASE " + probeDB + " WITH ALLOW_CONNECTIONS true")

		// Assert recovery transition on /healthz and /readyz
		pollEndpoint(t, ctx, client, baseURL+"/healthz", http.StatusOK, "ok", "ok")
		pollEndpoint(t, ctx, client, baseURL+"/readyz", http.StatusOK, "ok", "ok")
	})
}

type healthPayload struct {
	Status  string            `json:"status"`
	Service string            `json:"service"`
	Checks  map[string]string `json:"checks"`
}

func pollEndpoint(t *testing.T, ctx context.Context, client *http.Client, url string, wantStatus int, wantOverall, wantDB string) {
	t.Helper()
	var lastErr error
	var lastStatus int
	var lastBody string

	deadline := time.Now().Add(25 * time.Second)
	for time.Now().Before(deadline) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := client.Do(req)
		if err == nil {
			lastStatus = resp.StatusCode
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			lastBody = string(body)

			if resp.StatusCode == wantStatus {
				var p healthPayload
				if err := json.Unmarshal(body, &p); err == nil {
					if p.Status == wantOverall && p.Service == "payments" && p.Checks["db"] == wantDB && len(p.Checks) == 1 && !strings.Contains(lastBody, "nats") {
						return
					}
				}
			}
		} else {
			lastErr = err
		}
		select {
		case <-ctx.Done():
			t.Fatalf("context done while polling %s: %v", url, ctx.Err())
		case <-time.After(200 * time.Millisecond):
		}
	}
	t.Fatalf("endpoint %s failed to match status=%d overall=%s db=%s; lastStatus=%d body=%s err=%v",
		url, wantStatus, wantOverall, wantDB, lastStatus, lastBody, lastErr)
}
