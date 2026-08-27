package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"ticketing/services/payments/internal/psp"
	"ticketing/shared/obs"
	"ticketing/shared/runtimecfg"
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
	// NOT the checked-in "local-development-journal-key": signingConfig refuses
	// that literal forever now (ai-review S5), and a fixture carrying it would
	// test the refusal instead of the ring it means to build.
	const active = "an-ordinary-active-journal-key"
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

// TKT-252. Every ordinary credential in the system now takes a 32-byte floor
// (runtimecfg.CredentialMinBytes, ADR-059). JOURNAL_SIGNING_KEY deliberately does
// NOT: ADR-032 already states its 16-byte contract, and raising it was ruled out of
// scope rather than left undecided.
//
// That divergence lives in ONE argument at main.go's signingConfig, and nothing else
// would notice it being "tidied up" to use the shared constant. The gate would still
// go red — the smoke suite signs a journal with a 31-byte key — but it reports as a
// verify-journal failure three layers away from the cause. This test names the rule
// where the decision is, so the diagnostic matches the mistake.
//
// It is deliberately NOT written as "assert 16": the value is journalKeyMinBytes, and
// what is pinned is that a key between the two floors is accepted here while
// runtimecfg's ordinary floor would refuse it.
func TestJournalSigningKeyKeepsItsOwnFloorBelowTheOrdinaryOne(t *testing.T) {
	// Between the two floors: long enough for the journal, too short for everything
	// else. This is the exact band the divergence is about, so a fixture outside it
	// could not tell the two policies apart.
	const betweenTheFloors = "an-ordinary-active-journal-key" // 30 bytes
	if len(betweenTheFloors) < journalKeyMinBytes {
		t.Fatalf("fixture is %d bytes, below the journal floor of %d", len(betweenTheFloors), journalKeyMinBytes)
	}
	if len(betweenTheFloors) >= runtimecfg.CredentialMinBytes {
		t.Fatalf("fixture is %d bytes, which the ordinary %d-byte floor would ACCEPT — "+
			"it cannot distinguish the two policies", len(betweenTheFloors), runtimecfg.CredentialMinBytes)
	}

	t.Setenv("JOURNAL_KEY_ID", "local-v1")
	t.Setenv("JOURNAL_SIGNING_KEY", betweenTheFloors)
	t.Setenv("JOURNAL_HISTORICAL_KEYS", "")
	if _, err := signingConfig(); err != nil {
		t.Fatalf("the journal keeps its own %d-byte floor (ADR-032; ADR-059 records why it "+
			"differs) — a %d-byte key must still build a ring: %v",
			journalKeyMinBytes, len(betweenTheFloors), err)
	}
}

// TKT-117. The startup line states which key id the journal is signed under. It logs the
// key ID ONLY: an earlier revision also logged a truncated HMAC of the key, which the
// ai-review showed is an offline oracle for guessing a symmetric secret. The kid is already
// stored in plaintext on every journal row, so logging it discloses nothing new — and the
// mis-paste that motivated the fingerprint is now rejected outright by NewKeyring
// (TestNewKeyringRejectsBase64PastedActiveKey).
func TestLogJournalSigningKey(t *testing.T) {
	// A real, distinctive secret: the absence assertions below are worthless against a
	// placeholder the code never held.
	const rawSecret = "tkt117-distinctive-journal-secret"
	pastedBase64 := base64.RawStdEncoding.EncodeToString([]byte(rawSecret))

	t.Setenv("JOURNAL_KEY_ID", "local-v1")
	t.Setenv("JOURNAL_SIGNING_KEY", rawSecret)
	t.Setenv("JOURNAL_HISTORICAL_KEYS", "")
	ring, err := signingConfig()
	if err != nil {
		t.Fatalf("signingConfig: %v", err)
	}
	var buf bytes.Buffer
	logJournalSigningKey(context.Background(), obs.NewLogger("payments", &buf), ring)
	out := buf.String()

	// The leak assertions come FIRST, deliberately: behind a content assertion they are
	// unreachable exactly when a change both leaks material and alters the line.
	if strings.Contains(out, rawSecret) {
		t.Fatalf("LOG LEAKED THE RAW SIGNING KEY: %s", out)
	}
	if strings.Contains(out, pastedBase64) {
		t.Fatalf("LOG LEAKED A BASE64 ENCODING OF THE SIGNING KEY: %s", out)
	}
	// Narrow, and honest about it: substring absence proves the literal secret is not
	// printed. It does NOT prove the absence of a derived value that could serve as an
	// offline oracle — that property is held by the line carrying no key-derived field at
	// all, which is why this asserts the field set rather than trusting the check above.
	for _, forbidden := range []string{"fingerprint", "secret", "key_material"} {
		if strings.Contains(out, forbidden) {
			t.Fatalf("log line carries a key-derived field %q; only the key id may be logged: %s", forbidden, out)
		}
	}
	if !strings.Contains(out, `"journal_key_id":"local-v1"`) {
		t.Fatalf("log line does not carry the active key id: %s", out)
	}
}

// TKT-253. The sk_test_ branch accepted the prefix and validated NOTHING after it, so the
// literal "sk_test_" constructed a real Stripe adapter pointed at api.stripe.com and the
// service started cleanly — the configuration error surfacing only when a money-path
// request reached Stripe and failed. That is the opposite of the fail-fast contract every
// other credential in this binary keeps.
//
// TWO INDEPENDENT PREDICATES guard the branch now, so per AGENTS.md there are two cases,
// each passing the earlier one, each separately mutable:
//
//   - the body after the prefix is empty          -> "sk_test_"
//   - the body after the prefix contains whitespace -> non-empty body, so it passes the
//     first predicate and can only be refused by the second
//
// Deleting the empty-body check must turn ONLY TestPSPForKeyRefusesAnEmptyBody red;
// deleting the whitespace check must turn ONLY TestPSPForKeyRefusesAWhitespaceBody red.
// If deleting one kills both, an earlier refusal is short-circuiting the later predicate.
//
// WHAT THIS DOES NOT CLAIM (ADR-032, and the owner's decision on this ticket): nothing
// about Stripe's alphabet, length, checksum or future format. A truncated-but-plausible
// key still starts and still fails at the first charge. This catches a typo or a quoting
// mistake in a deploy config, and nothing else.
func TestPSPForKeyRefusesAnEmptyBody(t *testing.T) {
	provider, _, err := pspForKey("sk_test_")
	if err == nil {
		t.Fatal(`"sk_test_" is a prefix with no key after it and must refuse startup`)
	}
	if provider != nil {
		t.Fatalf("a refused key must yield no provider, got %T", provider)
	}
	if !strings.Contains(err.Error(), "STRIPE_SECRET_KEY") {
		t.Fatalf("the error must name the variable an operator set: %q", err)
	}
}

// Whitespace anywhere in the body, not merely at the edges. An interior space is the case
// a TrimSpace-based check would admit, and it is exactly as broken as a padded one.
//
// Redaction (ADR-012 §TKT-202): the assertion pins the absence of the BODY, not of the
// whole string. The prefix is a public constant and naming it in a diagnostic is fine —
// an error reproducing all but one character of the key would pass a whole-string check.
func TestPSPForKeyRefusesAWhitespaceBody(t *testing.T) {
	for name, key := range map[string]string{
		"trailing space":  "sk_test_TRAILINGBODY ",
		"interior space":  "sk_test_INTERIORA BODY",
		"interior tab":    "sk_test_INTERIORTAB\tBODY",
		"leading in body": "sk_test_ LEADINGBODY",
	} {
		t.Run(name, func(t *testing.T) {
			provider, _, err := pspForKey(key)
			if err == nil {
				t.Fatalf("%q carries whitespace in its body and must refuse startup", key)
			}
			if provider != nil {
				t.Fatalf("a refused key must yield no provider, got %T", provider)
			}
			if !strings.Contains(err.Error(), "STRIPE_SECRET_KEY") {
				t.Fatalf("the error must name the variable an operator set: %q", err)
			}
			if strings.Contains(err.Error(), "BODY") {
				t.Fatalf("the error echoes the supplied key: %q", err)
			}
		})
	}
}

// TKT-253. The credential POLICY, pinned at the seam that reads the environment.
//
// This test exists because of a mutation that survived everything else: replacing
// pspFromEnv's OptionalCredential call with runtimecfg.RequiredCredential compiles and
// passes every other test in this package, while refusing the empty value that selects
// the fake PSP — i.e. breaking every local run, the whole `make check` gate, and every
// deployment that does not configure Stripe. run() opens a database and cannot be
// unit-tested, so without this seam the call site is unpinned.
//
// The invariant, stated without naming the implementation: an unset STRIPE_SECRET_KEY is
// a legal configuration that selects the offline fake, and a configured one still
// reaches the selector unchanged.
func TestPSPFromEnvTreatsAnAbsentKeyAsALegalOfflineConfiguration(t *testing.T) {
	// Unset. This is how every local run and the entire gate are configured.
	t.Setenv("STRIPE_SECRET_KEY", "")
	provider, retention, err := pspFromEnv()
	if err != nil {
		t.Fatalf("an unset STRIPE_SECRET_KEY must select the fake, not refuse startup: %v", err)
	}
	if _, ok := provider.(*psp.Fake); !ok {
		t.Fatalf("an unset key must select the fake, got %T", provider)
	}
	if retention != 0 {
		t.Fatalf("the fake's status replay is unbounded; retention = %v", retention)
	}

	// The sentinel compose.yaml:288 defaults to. It is a mode selector, not a
	// credential, and must not be held to credential hygiene.
	t.Setenv("STRIPE_SECRET_KEY", "fake")
	if provider, _, err := pspFromEnv(); err != nil {
		t.Fatalf("the `fake` sentinel must select the fake: %v", err)
	} else if _, ok := provider.(*psp.Fake); !ok {
		t.Fatalf("the `fake` sentinel must select the fake, got %T", provider)
	}

	// A configured key still reaches the selector and still selects Stripe — the
	// transport checks must not reject a value on its way through.
	//
	// "shape-conformant", NOT "well-formed" or "valid" (ai-review [high], partially
	// accepted): nothing here verifies that Stripe would accept this string, and no
	// test in this repository can, because the gate never talks to Stripe (ADR-032
	// §Constraints). This fixture satisfies the prefix and the two body predicates and
	// that is the entire claim. `sk_test_x` satisfies them too and still reaches
	// api.stripe.com — see the ADR-032 amendment, which states that gap rather than
	// hiding it.
	t.Setenv("STRIPE_SECRET_KEY", "sk_test_51H8xQ2eZvKYlo2C0abcdefgh")
	if provider, _, err := pspFromEnv(); err != nil {
		t.Fatalf("a shape-conformant test key must still select Stripe: %v", err)
	} else if _, ok := provider.(*psp.Stripe); !ok {
		t.Fatalf("a shape-conformant test key must select Stripe, got %T", provider)
	}

	// And the shape refusal still reaches the caller through this path.
	t.Setenv("STRIPE_SECRET_KEY", "sk_test_")
	if _, _, err := pspFromEnv(); err == nil {
		t.Fatal("the bare prefix must refuse startup through pspFromEnv too")
	}
}
