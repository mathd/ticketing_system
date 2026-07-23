package main

import (
	"testing"

	"ticketing/services/payments/internal/psp"
)

// Provider selection is fail-fast config (mirrors signingConfig): the fake is chosen only
// by the explicit sentinel (or unset), a test-mode key selects Stripe, and a LIVE key or
// an unrecognized value refuses to start — a typo'd key must never silently charge the
// fake, and a live key must never be reachable from this testbed (ADR-032).
func TestPSPForKeySelection(t *testing.T) {
	for _, key := range []string{"", "fake"} {
		provider, err := pspForKey(key)
		if err != nil {
			t.Fatalf("key %q: %v", key, err)
		}
		if _, ok := provider.(*psp.Fake); !ok {
			t.Fatalf("key %q must select the fake, got %T", key, provider)
		}
	}
	provider, err := pspForKey("sk_test_abc123")
	if err != nil {
		t.Fatalf("sk_test_: %v", err)
	}
	if _, ok := provider.(*psp.Stripe); !ok {
		t.Fatalf("sk_test_ must select Stripe, got %T", provider)
	}
	for _, key := range []string{"sk_live_abc123", "pk_test_abc", "garbage"} {
		if _, err := pspForKey(key); err == nil {
			t.Fatalf("key %q must fail startup, not silently select a provider", key)
		}
	}
}
