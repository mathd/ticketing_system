package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TKT-557 (ADR-085 §8 ticket 0): the Access startup configuration boundary.
//
// Two guards. Access refuses a resolved HTTP_WRITE_TIMEOUT below 30 s, and it
// refuses any database configuration except an EXPLICIT sslmode=disable with
// no effective TLS or TLS fallback (D21: the connect-time 57P03 and 53300
// classes qualify only on that form). Both run before any resource is set up.
//
// The startup tests run the real command registry in a child process: this
// test binary re-executes itself with childEnv set, and TestMain dispatches.
// A deleted guard makes the child fail later, on a different error, or dial.

const childEnv = "TKT557_ACCESS_CHILD_ARGS"

func TestMain(m *testing.M) {
	if args, ok := os.LookupEnv(childEnv); ok {
		result := execute(strings.Fields(args), productionCommandCallbacks(), run)
		if result.Err != nil {
			fmt.Fprintf(os.Stderr, "child error: %v\n", result.Err)
		}
		if result.ExitCode == 0 && result.Err != nil {
			os.Exit(3)
		}
		os.Exit(result.ExitCode)
	}
	os.Exit(m.Run())
}

// ownedListener counts every connection it accepts. A DSN pointed at it
// shows whether a command dialled the database before it refused.
type ownedListener struct {
	addr     string
	accepted atomic.Int64
}

func newOwnedListener(t *testing.T) *ownedListener {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	o := &ownedListener{addr: ln.Addr().String()}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			o.accepted.Add(1)
			_ = conn.Close()
		}
	}()
	return o
}

const childInternalToken = "child-internal-token-0123456789abcdef"

// runChild runs one Access command in a bounded child process with a clean
// environment: only PATH, HOME and the given variables.
func runChild(t *testing.T, args string, env map[string]string) (string, int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^$")
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME"), childEnv + "=" + args,
		// Telemetry stays local and quiet: no exporter reaches anything.
		"OTEL_SDK_DISABLED=true"}
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("child %q did not stop within 30s: a startup guard did not refuse; output:\n%s", args, out)
	}
	code := 0
	if exitErr, ok := err.(*exec.ExitError); ok { //nolint:errorlint // exec reports the child's status by this concrete type
		code = exitErr.ExitCode()
	} else if err != nil {
		t.Fatalf("run child: %v", err)
	}
	return string(out), code
}

func disableDSN(addr string) string {
	return "postgres://access:dummy-password@" + addr + "/access?sslmode=disable"
}

// --- the write-timeout guard --------------------------------------------------

func TestAccessStartupRefusesAWriteTimeoutBelow30s(t *testing.T) {
	db := newOwnedListener(t)
	out, code := runChild(t, "", map[string]string{
		"INTERNAL_SERVICE_TOKEN": childInternalToken,
		"HTTP_WRITE_TIMEOUT":     "29s",
		"DATABASE_URL":           disableDSN(db.addr),
		"NATS_URL":               "nats://127.0.0.1:1",
	})
	if code == 0 || !strings.Contains(out, "HTTP_WRITE_TIMEOUT must be at least 30s") {
		t.Fatalf("exit %d, want the write-timeout refusal; output:\n%s", code, out)
	}
	if n := db.accepted.Load(); n != 0 {
		t.Fatalf("the refused startup dialled the database %d times", n)
	}
}

// --- the DSN contract -------------------------------------------------------------

func TestAccessDatabaseConfigAcceptsOnlyExplicitDisable(t *testing.T) {
	for name, dsn := range map[string]string{
		"url":                             "postgres://access:pw-secret@127.0.0.1:5432/access?sslmode=disable",
		"compose form":                    "postgres://access:dummy@postgres:5432/access?sslmode=disable",
		"keyword":                         "host=127.0.0.1 port=5432 user=access password=pw-secret dbname=access sslmode=disable",
		"keyword quoted":                  "host=127.0.0.1 user=access dbname=access sslmode='disable'",
		"url repeated, disable last":      "postgres://access@127.0.0.1/access?sslmode=require&sslmode=disable",
		"keyword repeated, disable last":  "host=127.0.0.1 sslmode=require user=access sslmode=disable",
		"url ssl=true alias then disable": "postgres://access@127.0.0.1/access?ssl=true&sslmode=disable",
	} {
		t.Run(name, func(t *testing.T) {
			cfg, err := accessDatabaseConfig(dsn)
			if err != nil {
				t.Fatalf("refused: %v", err)
			}
			if cfg.TLSConfig != nil {
				t.Fatal("accepted a config with TLS")
			}
		})
	}
}

func TestAccessDatabaseConfigRefusesEveryOtherForm(t *testing.T) {
	for name, dsn := range map[string]string{
		"missing (default prefer)":          "postgres://access:pw-secret@127.0.0.1:5432/access",
		"allow":                             "postgres://access:pw-secret@127.0.0.1/access?sslmode=allow",
		"prefer":                            "postgres://access:pw-secret@127.0.0.1/access?sslmode=prefer",
		"require":                           "postgres://access:pw-secret@127.0.0.1/access?sslmode=require",
		"verify-ca":                         "postgres://access:pw-secret@127.0.0.1/access?sslmode=verify-ca",
		"verify-full":                       "postgres://access:pw-secret@127.0.0.1/access?sslmode=verify-full",
		"keyword missing":                   "host=127.0.0.1 user=access password=pw-secret dbname=access",
		"keyword require":                   "host=127.0.0.1 user=access password=pw-secret sslmode=require",
		"url repeated, require last":        "postgres://access:pw-secret@127.0.0.1/access?sslmode=disable&sslmode=require",
		"keyword repeated, require last":    "host=127.0.0.1 password=pw-secret sslmode=disable sslmode=require",
		"url later ssl=true alias":          "postgres://access:pw-secret@127.0.0.1/access?sslmode=disable&ssl=true",
		"url percent-encoded key":           "postgres://access:pw-secret@127.0.0.1/access?sslmode=disable&ssl%6Dode=require",
		"keyword quoted decoy":              "sslmode=require password='pw-secret sslmode=disable b'",
		"keyword escaped decoy":             `sslmode=require password=pw-secret\ sslmode=disable`,
		"malformed url":                     "postgres://access:pw-secret@127.0.0.1/access?sslmode=%zz",
		"malformed keyword":                 "host=127.0.0.1 password='pw-secret sslmode=disable",
		"empty":                             "",
		"unknown mode":                      "postgres://access:pw-secret@127.0.0.1/access?sslmode=off",
		"keyword disable then url-ish tail": "host=127.0.0.1 password=pw-secret sslmode=disable sslmode=verify-full",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := accessDatabaseConfig(dsn)
			if err == nil {
				t.Fatal("accepted")
			}
			if strings.Contains(err.Error(), "pw-secret") {
				t.Fatalf("the error echoes the DSN: %v", err)
			}
		})
	}
}

// Ambient PGSSLMODE=disable gives a DSN without sslmode no TLS, so only the
// independent presence predicate can refuse it.
func TestAccessDatabaseConfigRequiresThePresenceOfDisable(t *testing.T) {
	t.Setenv("PGSSLMODE", "disable")
	if _, err := accessDatabaseConfig("postgres://access:pw-secret@127.0.0.1:5432/access"); err == nil {
		t.Fatal("a DSN with no explicit sslmode was accepted because the environment disabled TLS")
	}
	if _, err := accessDatabaseConfig("host=127.0.0.1 user=access password=pw-secret dbname=access"); err == nil {
		t.Fatal("a keyword DSN with no explicit sslmode was accepted because the environment disabled TLS")
	}
	// The decoy text sslmode=disable sits INSIDE a value. Only a parser that
	// respects quoting and escapes sees that no sslmode key is present; the
	// effective config has no TLS here, so nothing else would refuse.
	for _, dsn := range []string{
		"host=127.0.0.1 user=access password='pw-secret sslmode=disable'",
		`host=127.0.0.1 user=access password=pw-secret\ sslmode=disable`,
		"postgres://access:pw-secret@127.0.0.1/access?application_name=sslmode%3Ddisable",
	} {
		if _, err := accessDatabaseConfig(dsn); err == nil {
			t.Fatalf("a decoy inside a value was read as an explicit sslmode: %q", dsn)
		}
	}
}

// Ambient PGSSLMODE cannot override the explicit, accepted disable.
func TestAmbientSSLModeCannotOverrideExplicitDisable(t *testing.T) {
	for _, ambient := range []string{"require", "verify-full", "prefer"} {
		t.Setenv("PGSSLMODE", ambient)
		cfg, err := accessDatabaseConfig("postgres://access:pw@127.0.0.1:5432/access?sslmode=disable")
		if err != nil {
			t.Fatalf("PGSSLMODE=%s: explicit disable refused: %v", ambient, err)
		}
		if cfg.TLSConfig != nil || len(cfg.Fallbacks) != 0 {
			t.Fatalf("PGSSLMODE=%s overrode the explicit disable", ambient)
		}
	}
}

// Each DB-using command refuses a non-disable DSN before any dial.
func TestAccessCommandsRefuseANonDisableDSNBeforeAnyDial(t *testing.T) {
	for _, tc := range []struct {
		name, args string
		env        map[string]string
	}{
		{name: "server", args: "", env: map[string]string{"INTERNAL_SERVICE_TOKEN": childInternalToken, "NATS_URL": "nats://127.0.0.1:1"}},
		{name: "migrate", args: "migrate"},
		// verify-lifecycle reaches the shared openDB helper with an otherwise
		// valid command configuration: run and migrate do not use openDB, so
		// only this case can show that openDB validates.
		{name: "verify-lifecycle", args: "verify-lifecycle", env: map[string]string{
			envLifecyclePublicKeys: localLifecycleKID + "=" + localLifecyclePub,
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := newOwnedListener(t)
			env := map[string]string{"DATABASE_URL": "postgres://access:pw-secret@" + db.addr + "/access?sslmode=require"}
			for k, v := range tc.env {
				env[k] = v
			}
			out, code := runChild(t, tc.args, env)
			if code == 0 || !strings.Contains(out, "DATABASE_URL must set sslmode=disable") {
				t.Fatalf("exit %d, want the DSN refusal; output:\n%s", code, out)
			}
			if strings.Contains(out, "pw-secret") {
				t.Fatal("the refusal echoes the DSN")
			}
			if n := db.accepted.Load(); n != 0 {
				t.Fatalf("%s dialled the database %d times before it refused", tc.name, n)
			}
		})
	}
}
