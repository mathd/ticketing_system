package main

import (
	"strings"
	"testing"
	"time"

	"ticketing/services/payments/internal/psp"
)

// Provider selection is fail-fast config (mirrors signingConfig): the fake is chosen only
// by the explicit sentinel (or unset), a test-mode key selects Stripe, and a LIVE key or
// an unrecognized value refuses to start — a typo'd key must never silently charge the
// fake, and a live key must never be reachable from this testbed (ADR-032).
//
// Retention rides the same selection (TKT-115): the fake retains idempotency keys
// forever (0 = unbounded status replay), Stripe ~24h — the deadline the status endpoint
// enforces so an expired replay can never mint a second PaymentIntent.
func TestPSPForKeySelection(t *testing.T) {
	for _, key := range []string{"", "fake"} {
		provider, retention, err := pspForKey(key)
		if err != nil {
			t.Fatalf("key %q: %v", key, err)
		}
		if _, ok := provider.(*psp.Fake); !ok {
			t.Fatalf("key %q must select the fake, got %T", key, provider)
		}
		if retention != 0 {
			t.Fatalf("the fake's status replay is unbounded; retention = %v", retention)
		}
	}
	provider, retention, err := pspForKey("sk_test_abc123")
	if err != nil {
		t.Fatalf("sk_test_: %v", err)
	}
	if _, ok := provider.(*psp.Stripe); !ok {
		t.Fatalf("sk_test_ must select Stripe, got %T", provider)
	}
	if retention != 24*time.Hour {
		t.Fatalf("Stripe's idempotency retention bound is ~24h; retention = %v", retention)
	}
	for _, key := range []string{"sk_live_abc123", "pk_test_abc", "garbage"} {
		if _, _, err := pspForKey(key); err == nil {
			t.Fatalf("key %q must fail startup, not silently select a provider", key)
		}
	}
}

// The env override is a test knob, not a default: unset keeps the provider's own bound,
// a parseable duration replaces it, and garbage refuses startup like every config error.
func TestStatusReplayRetentionOverride(t *testing.T) {
	if got, err := statusReplayRetention(24 * time.Hour); err != nil || got != 24*time.Hour {
		t.Fatalf("unset override must keep the provider bound: got %v, %v", got, err)
	}
	t.Setenv("PAYMENTS_STATUS_REPLAY_RETENTION", "1h")
	if got, err := statusReplayRetention(0); err != nil || got != time.Hour {
		t.Fatalf("override must win: got %v, %v", got, err)
	}
	t.Setenv("PAYMENTS_STATUS_REPLAY_RETENTION", "garbage")
	if _, err := statusReplayRetention(0); err == nil {
		t.Fatal("an unparseable retention must refuse startup")
	}
	t.Setenv("PAYMENTS_STATUS_REPLAY_RETENTION", "-1h")
	if _, err := statusReplayRetention(0); err == nil {
		t.Fatal("a negative retention must refuse startup")
	}
	// Against a bounded provider the override may only SHORTEN the window: extending
	// (or disabling) it would replay idempotency keys the provider already forgot and
	// mint a second PaymentIntent (ai-review B3).
	t.Setenv("PAYMENTS_STATUS_REPLAY_RETENTION", "48h")
	if _, err := statusReplayRetention(24 * time.Hour); err == nil {
		t.Fatal("extending a bounded provider retention must refuse startup")
	}
	t.Setenv("PAYMENTS_STATUS_REPLAY_RETENTION", "0")
	if _, err := statusReplayRetention(24 * time.Hour); err == nil {
		t.Fatal("disabling a bounded provider retention must refuse startup")
	}
	t.Setenv("PAYMENTS_STATUS_REPLAY_RETENTION", "1h")
	if got, err := statusReplayRetention(24 * time.Hour); err != nil || got != time.Hour {
		t.Fatalf("shortening a bounded retention must be allowed: got %v, %v", got, err)
	}
}

// signingConfig is the whole keyring configuration surface, and it is what makes
// "the service refuses to start on a malformed ring" true rather than asserted: run,
// verify-journal and verify-concurrent-append all call it before touching a journal.
// Table cases are hand-written env literals, not values produced by the parser under
// test — a fixture built from the parser could only express rings it already accepts.
func TestSigningConfigKeyring(t *testing.T) {
	const active = "local-development-journal-key"
	// base64.RawStdEncoding of two distinct >=16-byte secrets.
	const v1b64 = "cmV0aXJlZC1qb3VybmFsLWtleS12MQ"
	const v0b64 = "cmV0aXJlZC1qb3VybmFsLWtleS12MA"

	t.Run("legacy single-key shape still builds a one-member ring", func(t *testing.T) {
		t.Setenv("JOURNAL_KEY_ID", "local-v1")
		t.Setenv("JOURNAL_SIGNING_KEY", active)
		t.Setenv("JOURNAL_HISTORICAL_KEYS", "")
		ring, err := signingConfig()
		if err != nil {
			t.Fatalf("today's deployed configuration must keep working: %v", err)
		}
		if ring.ActiveKeyID() != "local-v1" {
			t.Fatalf("active kid = %q", ring.ActiveKeyID())
		}
	})

	t.Run("historical keys join the ring", func(t *testing.T) {
		t.Setenv("JOURNAL_KEY_ID", "local-v2")
		t.Setenv("JOURNAL_SIGNING_KEY", active)
		t.Setenv("JOURNAL_HISTORICAL_KEYS", "local-v1="+v1b64+",local-v0="+v0b64)
		ring, err := signingConfig()
		if err != nil {
			t.Fatalf("valid rotated configuration rejected: %v", err)
		}
		if ring.ActiveKeyID() != "local-v2" {
			t.Fatalf("active kid = %q, want local-v2", ring.ActiveKeyID())
		}
		for _, kid := range []string{"local-v2", "local-v1", "local-v0"} {
			if !ring.Has(kid) {
				t.Fatalf("ring must verify under %q", kid)
			}
		}
		if ring.Has("local-v9") {
			t.Fatal("ring reports a key it was never given")
		}
	})

	for _, tc := range []struct{ name, id, key, historical, want string }{
		{"missing active id", "", active, "", "required"},
		{"missing active secret", "local-v1", "", "", "required"},
		{"short active secret", "local-v1", "short", "", "16"},
		{"historical entry without separator", "local-v2", active, "local-v1", "entry"},
		{"historical bad base64", "local-v2", active, "local-v1=!!!", "base64"},
		{"historical padded base64", "local-v2", active, "local-v1=cmV0aXJlZC1qb3VybmFsLWtleS12MQ==", "base64"},
		{"historical short secret", "local-v2", active, "local-v1=c2hvcnQ", "16"},
		{"duplicate kid", "local-v2", active, "local-v1=" + v1b64 + ",local-v1=" + v0b64, "duplicate"},
		{"historical kid duplicates active", "local-v1", active, "local-v1=" + v1b64, "duplicate"},
		{"invalid kid charset", "local v2", active, "", "key id"},
	} {
		t.Run("refuses "+tc.name, func(t *testing.T) {
			t.Setenv("JOURNAL_KEY_ID", tc.id)
			t.Setenv("JOURNAL_SIGNING_KEY", tc.key)
			t.Setenv("JOURNAL_HISTORICAL_KEYS", tc.historical)
			ring, err := signingConfig()
			if err == nil {
				t.Fatalf("expected startup failure mentioning %q, got a usable ring", tc.want)
			}
			if ring != nil {
				t.Fatal("a rejected configuration must yield no ring")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", err, tc.want)
			}
			if tc.key != "" && strings.Contains(err.Error(), tc.key) {
				t.Fatalf("error echoes secret material: %q", err)
			}
		})
	}
}
