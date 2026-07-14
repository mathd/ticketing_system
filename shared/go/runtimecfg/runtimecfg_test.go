package runtimecfg

import (
	"database/sql"
	"net/http"
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

func TestInvalidConfiguration(t *testing.T) {
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
