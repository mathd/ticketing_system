package main

import (
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
}
