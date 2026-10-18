//go:build smoke

package api

import (
	"encoding/json"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"ticketing/shared/runtimecfg"
)

// TKT-557 (COS-4): what Compose actually gives Access and the gateway. The
// base file and the smoke file set are rendered with dummy values and an
// EMPTY env file, so no developer .env or shell value leaks in. Each rendered
// value then goes through the same configuration check the processes run.

var composeFileSets = map[string][]string{
	"base":  {"compose.yaml"},
	"smoke": {"compose.yaml", "compose.direct-ports.yaml", "compose.onsale-load.yaml", "compose.smoke-cadence.yaml", "compose.smoke.yaml"},
}

var requiredComposeVar = regexp.MustCompile(`\$\{([A-Z][A-Z0-9_]*):?\?`)

func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", "..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "compose.yaml")); err != nil {
		t.Fatalf("repository root not found from the test directory: %v", err)
	}
	return root
}

type renderedService struct {
	Environment map[string]*string `json:"environment"`
}

func renderCompose(t *testing.T, root string, files []string, writeTimeout string) map[string]renderedService {
	t.Helper()
	required := map[string]bool{}
	for _, f := range files {
		raw, err := os.ReadFile(filepath.Join(root, f))
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range requiredComposeVar.FindAllStringSubmatch(string(raw), -1) {
			required[m[1]] = true
		}
	}
	envFile := filepath.Join(t.TempDir(), "empty.env")
	if err := os.WriteFile(envFile, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	args := []string{"compose", "-p", "tkt557-render-check", "--env-file", envFile}
	for _, f := range files {
		args = append(args, "-f", filepath.Join(root, f))
	}
	args = append(args, "config", "--format", "json")
	cmd := exec.Command("docker", args...)
	cmd.Dir = t.TempDir()
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME")}
	for _, name := range []string{"DOCKER_HOST", "DOCKER_CONFIG", "DOCKER_CONTEXT"} {
		if v, ok := os.LookupEnv(name); ok {
			cmd.Env = append(cmd.Env, name+"="+v)
		}
	}
	names := make([]string, 0, len(required))
	for name := range required {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		cmd.Env = append(cmd.Env, name+"=dummy-"+strings.ToLower(name))
	}
	if writeTimeout != "" {
		cmd.Env = append(cmd.Env, "HTTP_WRITE_TIMEOUT="+writeTimeout)
	}
	out, err := cmd.Output()
	if err != nil {
		var stderr string
		if exitErr, ok := err.(*exec.ExitError); ok { //nolint:errorlint // exec reports stderr by this concrete type
			stderr = string(exitErr.Stderr)
		}
		t.Fatalf("docker compose config %v: %v\n%s", files, err, stderr)
	}
	var rendered struct {
		Services map[string]renderedService `json:"services"`
	}
	if err := json.Unmarshal(out, &rendered); err != nil {
		t.Fatalf("decode rendered config: %v", err)
	}
	return rendered.Services
}

func envValue(t *testing.T, services map[string]renderedService, service, name string) string {
	t.Helper()
	s, ok := services[service]
	if !ok {
		t.Fatalf("service %s is not in the rendered config", service)
	}
	v, ok := s.Environment[name]
	if !ok || v == nil {
		t.Fatalf("%s has no %s", service, name)
	}
	return *v
}

// Both processes receive the same forwarded value, by default and with each
// override, and the same check the processes run refuses 29s.
func TestComposeForwardsTheWriteTimeoutToAccessAndGateway(t *testing.T) {
	root := repoRoot(t)
	for setName, files := range composeFileSets {
		for _, tc := range []struct {
			override, want string
			refused        bool
		}{
			{override: "", want: "30s"},
			{override: "29s", want: "29s", refused: true},
			{override: "30s", want: "30s"},
			{override: "45s", want: "45s"},
		} {
			t.Run(setName+" override="+tc.override, func(t *testing.T) {
				services := renderCompose(t, root, files, tc.override)
				for _, service := range []string{"access", "gateway"} {
					got := envValue(t, services, service, "HTTP_WRITE_TIMEOUT")
					if got != tc.want {
						t.Fatalf("%s HTTP_WRITE_TIMEOUT = %q, want %q", service, got, tc.want)
					}
					t.Setenv("HTTP_WRITE_TIMEOUT", got)
					config, err := runtimecfg.HTTPFromEnv()
					if err != nil {
						t.Fatal(err)
					}
					err = config.RequireWriteTimeoutAtLeast(30 * time.Second)
					if tc.refused != (err != nil) {
						t.Fatalf("%s %s: refused=%v, want %v (%v)", service, got, err != nil, tc.refused, err)
					}
				}
			})
		}
	}
}

// All three Access database URLs render with an explicit, final
// sslmode=disable and no effective TLS. The exact rendered form is also the
// "compose form" case of the Access command's own DSN test.
func TestComposeAccessDatabaseURLsAreExplicitDisable(t *testing.T) {
	root := repoRoot(t)
	for setName, files := range composeFileSets {
		t.Run(setName, func(t *testing.T) {
			services := renderCompose(t, root, files, "")
			want := "postgres://access:dummy-access_db_password@postgres:5432/access?sslmode=disable"
			for _, service := range []string{"access-migrate", "access-lifecycle-backfill", "access"} {
				dsn := envValue(t, services, service, "DATABASE_URL")
				if dsn != want {
					t.Fatalf("%s DATABASE_URL = %q, want %q", service, dsn, want)
				}
				u, err := url.Parse(dsn)
				if err != nil {
					t.Fatal(err)
				}
				modes := u.Query()["sslmode"]
				if len(modes) == 0 || modes[len(modes)-1] != "disable" {
					t.Fatalf("%s: final explicit sslmode = %v, want disable", service, modes)
				}
				t.Setenv("PGSSLMODE", "require")
				cfg, err := pgx.ParseConfig(dsn)
				if err != nil {
					t.Fatal(err)
				}
				if cfg.TLSConfig != nil || len(cfg.Fallbacks) != 0 {
					t.Fatalf("%s: the rendered URL enables TLS or a fallback", service)
				}
			}
		})
	}
}
