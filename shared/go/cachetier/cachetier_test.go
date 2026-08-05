package cachetier

import (
	"testing"
	"time"
)

// TestRegisteredTiers is the byte proof for every ADR-004 tier, and it is
// load-bearing rather than a formality (TKT-204 plan-review, amendment 2).
//
// Catalog's minutes tier is the reason. Its OpenAPI declaration is the free-form
// `CacheControl` component with no enum, so ADR-028's response validator cannot
// check the value, and catalog's own unit tests compare the emitted header
// against CacheControlPublicReads — the same binding they are meant to be
// pinning. The only literal assertion lives in the stack-gated smoke suite. So
// this test is where a one-character drift (a missing s-maxage, say) is caught in
// the fast `make test-go` loop, at the single point the strings are now derived.
//
// It therefore asserts the four COMPLETE strings as literals. Re-deriving the
// format expression here would make it a tautology that cannot fail.
func TestRegisteredTiers(t *testing.T) {
	for _, tc := range []struct {
		name   string
		tier   Tier
		dur    time.Duration
		header string
	}{
		{"never", Never, 0, "no-store"},
		{"seconds", Seconds, 5 * time.Second, "public, max-age=5, s-maxage=5"},
		{"minutes", Minutes, 5 * time.Minute, "public, max-age=300, s-maxage=300"},
		{"hours", Hours, time.Hour, "public, max-age=3600, s-maxage=3600"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.tier.Duration(); got != tc.dur {
				t.Fatalf("Duration() = %v, want %v", got, tc.dur)
			}
			if got := tc.tier.CacheControl(); got != tc.header {
				t.Fatalf("CacheControl() = %q, want %q", got, tc.header)
			}
		})
	}

	// A fifth tier is a decision, not an accident: All() is what the spec audit
	// validates declared values against, so silently growing it would silently
	// widen what the contract may declare.
	if got := len(All()); got != 4 {
		t.Fatalf("All() has %d tiers, want exactly 4 — adding a tier is an ADR-004 change", got)
	}
}

// TestFromCacheControl pins the reverse direction the spec audit depends on.
// `no-store` resolving to Never — rather than to "not found" — is the load-bearing
// case: it is what lets the audit tell a declared never-tier apart from a
// declaration it does not recognise.
func TestFromCacheControl(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want Tier
	}{
		{"no-store", Never},
		{"public, max-age=5, s-maxage=5", Seconds},
		{"public, max-age=300, s-maxage=300", Minutes},
		{"public, max-age=3600, s-maxage=3600", Hours},
	} {
		if got, ok := FromCacheControl(tc.in); !ok || got != tc.want {
			t.Fatalf("FromCacheControl(%q) = %v, %v; want %v, true", tc.in, got, ok, tc.want)
		}
	}

	for _, in := range []string{
		"public, max-age=60, s-maxage=60", // a real tier value, just not one of ours
		"public, max-age=300",             // the exact drift TestRegisteredTiers guards against
		"private, max-age=300, s-maxage=300",
		"",
		"no-cache",
	} {
		if got, ok := FromCacheControl(in); ok {
			t.Fatalf("FromCacheControl(%q) = %v, true; want not-found", in, got)
		}
	}
}

// TestUnregisteredTierCannotRender: a Tier cast from an arbitrary duration must
// not be able to invent a fifth tier. It panics rather than returning a sentinel
// — a sentinel would let a caller ignore it and emit an empty Cache-Control,
// which ADR-028 would then turn into a 500 on a public read instead of a loud
// failure at process start. Every call site is a package-level var initializer,
// so the panic is a boot failure, never a request-path one.
func TestUnregisteredTierCannotRender(t *testing.T) {
	for _, d := range []time.Duration{60 * time.Second, -time.Second, 90 * time.Minute} {
		func() {
			defer func() {
				if recover() == nil {
					t.Fatalf("Tier(%v).CacheControl() returned without panicking — an unregistered tier must not render", d)
				}
			}()
			_ = Tier(d).CacheControl()
		}()
	}
}
