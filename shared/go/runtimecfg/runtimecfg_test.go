package runtimecfg

import (
	"database/sql"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestHTTPDefaultsAndOverrides(t *testing.T) {
	defaults, err := HTTPFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if defaults != (HTTP{5 * time.Second, 15 * time.Second, 30 * time.Second, time.Minute}) {
		t.Fatalf("unexpected defaults: %#v", defaults)
	}

	t.Setenv("HTTP_READ_TIMEOUT", "20s")
	override, err := HTTPFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{}
	override.Apply(server)
	if server.ReadTimeout != 20*time.Second || server.WriteTimeout != 30*time.Second {
		t.Fatalf("policy not applied: %#v", server)
	}
}

func TestDatabaseDefaultsAndApply(t *testing.T) {
	config, err := DatabaseFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if config != (Database{25, 10, 30 * time.Minute, 5 * time.Minute}) {
		t.Fatalf("unexpected defaults: %#v", config)
	}

	db := &sql.DB{}
	config.Apply(db)
	stats := db.Stats()
	if stats.MaxOpenConnections != 25 {
		t.Fatalf("max open connections = %d, want 25", stats.MaxOpenConnections)
	}
}

// Response validation is a cost paid on every response (ADR-028 as amended by
// TKT-125): dev, CI and smoke keep it, so absence of the variable means on.
func TestResponseValidationDefaultsOnAndCanBeDisabled(t *testing.T) {
	enabled, err := ResponseValidationFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if !enabled {
		t.Fatal("response validation must default on — dev/CI/smoke rely on the default")
	}

	t.Setenv("OPENAPI_RESPONSE_VALIDATION_ENABLED", "false")
	enabled, err = ResponseValidationFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if enabled {
		t.Fatal("OPENAPI_RESPONSE_VALIDATION_ENABLED=false must disable response validation")
	}
}

func TestInvalidConfiguration(t *testing.T) {
	t.Run("boolean", func(t *testing.T) {
		t.Setenv("OPENAPI_RESPONSE_VALIDATION_ENABLED", "sometimes")
		_, err := ResponseValidationFromEnv()
		if err == nil {
			t.Fatal("expected invalid boolean error")
		}
		// Never echo the value: the same rule the token reader follows.
		if got := err.Error(); !strings.Contains(got, "OPENAPI_RESPONSE_VALIDATION_ENABLED") || strings.Contains(got, "sometimes") {
			t.Fatalf("error must name the variable and not its value: %q", got)
		}
	})
	t.Run("duration", func(t *testing.T) {
		t.Setenv("HTTP_IDLE_TIMEOUT", "0s")
		if _, err := HTTPFromEnv(); err == nil {
			t.Fatal("expected invalid duration error")
		}
	})
	t.Run("integer", func(t *testing.T) {
		t.Setenv("DB_MAX_OPEN_CONNS", "many")
		if _, err := DatabaseFromEnv(); err == nil {
			t.Fatal("expected invalid integer error")
		}
	})
	t.Run("pool relationship", func(t *testing.T) {
		t.Setenv("DB_MAX_OPEN_CONNS", "5")
		t.Setenv("DB_MAX_IDLE_CONNS", "6")
		if _, err := DatabaseFromEnv(); err == nil {
			t.Fatal("expected pool relationship error")
		}
	})
}

func TestInternalTokenRequiresAnExplicitCredential(t *testing.T) {
	// t.Setenv registers a cleanup; set-then-unset leaves the var absent.
	t.Setenv("INTERNAL_SERVICE_TOKEN", "")
	if _, err := InternalTokenFromEnv(); err == nil {
		t.Fatal("empty INTERNAL_SERVICE_TOKEN must fail startup")
	} else if got := err.Error(); got != "INTERNAL_SERVICE_TOKEN required: no default is shipped, run `make up` once to generate a local credential" {
		t.Fatalf("error text drifted (and must never echo a supplied value): %q", got)
	}
}

func TestInternalTokenRejectsTheRetiredDefaultForever(t *testing.T) {
	t.Setenv("INTERNAL_SERVICE_TOKEN", "local-service-token")
	_, err := InternalTokenFromEnv()
	if err == nil {
		t.Fatal("the retired checked-in default must be refused, dev included (TKT-83)")
	}
	if got := err.Error(); got != "INTERNAL_SERVICE_TOKEN is the retired checked-in default: generate a real credential (`make up`)" {
		t.Fatalf("error text drifted: %q", got)
	}
}

func TestInternalTokenReturnsAValidValueUnchanged(t *testing.T) {
	t.Setenv("INTERNAL_SERVICE_TOKEN", "0f3d1c9a8b7e6f5d4c3b2a1908f7e6d5")
	token, err := InternalTokenFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if token != "0f3d1c9a8b7e6f5d4c3b2a1908f7e6d5" {
		t.Fatalf("token altered: %q", token)
	}
}
