package runtimecfg

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
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

// TKT-191 ai-review pass 2. Credentials travel in HTTP headers, and header
// parsing strips leading/trailing optional whitespace (RFC 7230 §3.2.4) —
// confirmed against net/http: a client that sets " secret " is received as
// "secret".
//
// Two things follow, and the second is the security-relevant one:
//   - a padded credential never matches what the peer configured, and fails in a
//     way that reads like a wrong secret rather than a quoting mistake;
//   - two credentials differing ONLY by padding are the SAME credential on the
//     wire, so any caller comparing the raw strings to prove they differ is
//     comparing the wrong thing. Catalog does exactly that comparison to keep
//     its staff-write credential distinct from the shared internal token.
//
// Refusing padded values here is what makes those raw comparisons sound.
func TestRequiredCredentialRejectsValuesHTTPWouldChange(t *testing.T) {
	for _, tc := range []struct{ name, value, wantMsg string }{
		{"leading space", " secret", "whitespace"},
		{"trailing space", "secret ", "whitespace"},
		{"surrounded", " secret ", "whitespace"},
		{"tab", "\tsecret", "whitespace"},
		// A trailing \r IS trimmed by TrimSpace, so it is caught as whitespace;
		// an EMBEDDED newline is not, and is caught as a control character. Both
		// matter: the embedded one is what would let a credential inject a second
		// header.
		{"trailing carriage return", "secret\r", "whitespace"},
		{"embedded newline", "sec\nret", "cannot appear in an HTTP header value"},
		{"header injection attempt", "secret\nX-Injected: 1", "cannot appear in an HTTP header value"},
		// ai-review pass 3: rejecting only CR/LF/NUL let these through. They are
		// refused by Go's transport at request time, so a credential containing
		// one starts cleanly and then fails every outbound authenticated call —
		// the opposite of the fail-fast contract.
		{"SOH", "sec\x01ret", "cannot appear in an HTTP header value"},
		{"DEL", "sec\x7fret", "cannot appear in an HTTP header value"},
		{"escape", "sec\x1bret", "cannot appear in an HTTP header value"},
		// No NUL case: os.Setenv refuses a value containing one ("invalid
		// argument"), so it is not a reachable input for an env-var credential.
		// ContainsAny still lists it, cheaply, for callers that are not env vars.
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("TEST_CREDENTIAL", tc.value)
			got, err := RequiredCredential("TEST_CREDENTIAL", "", CredentialMinBytes)
			if err == nil {
				t.Fatalf("accepted %q, which HTTP would not transmit unchanged", tc.value)
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Fatalf("error %q does not mention %q", err, tc.wantMsg)
			}
			if got != "" {
				t.Fatalf("returned a value alongside the error: %q", got)
			}
		})
	}
}

// The positive case, as a REAL round-trip rather than an assertion about one.
//
// The claim that matters is not "RequiredCredential returned the string" — that
// is trivially true and would pass against the unfixed code (ai-review pass 3
// called the earlier version non-discriminating). It is that an ACCEPTED
// credential arrives at a server byte-identical, because catalog compares two
// accepted credentials with == to prove they are different on the wire.
//
// So: accept it, send it, and read back what the server actually received.
func TestAcceptedCredentialSurvivesAnHTTPRoundTripUnchanged(t *testing.T) {
	// Every fixture is >= CredentialMinBytes because TKT-252 gave RequiredCredential
	// a length floor, and this is the POSITIVE test — a fixture under the floor would
	// be rejected for its length and prove nothing about the transport. They were
	// lengthened rather than dropped: each one still carries the exact property it
	// was chosen for, and that property is what this test exists to pin.
	for _, value := range []string{
		"0f3d1c9a8b7e6f5d4c3b2a1908f7e6d5",  // what make up generates (exactly 32)
		"a-b_c.d~e-a-b_c.d~e-a-b_c.d~e-ab",  // punctuation a base64url/hex value might carry
		"tok en tok en tok en tok en tok e", // an INTERIOR space is legal and must survive
		"Zm9vYmFyZm9vYmFyZm9vYmFyZm9vYg==",  // base64 padding
	} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("TEST_CREDENTIAL", value)
			got, err := RequiredCredential("TEST_CREDENTIAL", "", CredentialMinBytes)
			if err != nil {
				t.Fatalf("rejected a header-safe credential %q: %v", value, err)
			}
			if got != value {
				t.Fatalf("value altered at load: %q -> %q", value, got)
			}

			var received string
			srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				received = r.Header.Get("X-Credential")
			}))
			defer srv.Close()

			req, err := http.NewRequest(http.MethodGet, srv.URL, nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("X-Credential", got)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("an accepted credential was refused by the transport: %v", err)
			}
			_ = resp.Body.Close()

			if received != value {
				t.Fatalf("the credential changed in transit: sent %q, server received %q — "+
					"any == comparison of two accepted credentials is then comparing the wrong thing",
					value, received)
			}
		})
	}
}

// TKT-252. RequiredCredential validated transport CHARACTERS and never LENGTH, so
// a deployment configured with `x` started cleanly — across every credential in the
// system, since this one function guards all sixteen loads.
//
// The boundary is asserted from both sides. One short of the floor must be refused
// and exactly the floor must be accepted, because a test that only checks a very
// short value passes just as happily against `< 4` as against `< 32`.
func TestRequiredCredentialRefusesValuesShorterThanTheFloor(t *testing.T) {
	const short = "0f3d1c9a8b7e6f5d4c3b2a1908f7e6d" // 31 — one byte under
	t.Setenv("TEST_CREDENTIAL", short)
	got, err := RequiredCredential("TEST_CREDENTIAL", "", CredentialMinBytes)
	if err == nil {
		t.Fatalf("accepted a %d-byte credential below the %d-byte floor", len(short), CredentialMinBytes)
	}
	if got != "" {
		t.Fatalf("returned a value alongside the error: %q", got)
	}
	// The same rule the rest of this file follows: a startup diagnostic names the
	// variable, never its value. An error carrying the credential leaks it into
	// every log, crash report and terminal that sees the failed boot.
	if strings.Contains(err.Error(), short) {
		t.Fatalf("the error echoed the supplied value: %q", err)
	}
	if !strings.Contains(err.Error(), "TEST_CREDENTIAL") {
		t.Fatalf("the error does not name the variable: %q", err)
	}

	const atFloor = "0f3d1c9a8b7e6f5d4c3b2a1908f7e6d5" // exactly 32
	t.Setenv("TEST_CREDENTIAL", atFloor)
	if _, err := RequiredCredential("TEST_CREDENTIAL", "", CredentialMinBytes); err != nil {
		t.Fatalf("refused a credential of exactly %d bytes: %v", CredentialMinBytes, err)
	}
}

// The floor is a LENGTH floor, and this test is the executable form of that
// decision (ADR-059). Thirty-two 'a' characters carry essentially no entropy and
// are ACCEPTED — deliberately.
//
// It guards the decision rather than the code. A later change that adds a
// character-class rule, a repetition check or any other entropy heuristic turns
// this red, which is the point: the floor makes online guessing infeasible for
// the values the generators actually produce, and claims nothing whatsoever about
// entropy. Read ADR-059 before making this pass.
func TestCredentialFloorIsLengthNotEntropy(t *testing.T) {
	weak := strings.Repeat("a", CredentialMinBytes)
	t.Setenv("TEST_CREDENTIAL", weak)
	got, err := RequiredCredential("TEST_CREDENTIAL", "", CredentialMinBytes)
	if err != nil {
		t.Fatalf("a %d-byte low-entropy value must pass a LENGTH floor: %v", CredentialMinBytes, err)
	}
	if got != weak {
		t.Fatalf("value altered at load: %q -> %q", weak, got)
	}
}

// len() on a Go string counts BYTES, and that is the rule (D1): the floor measures
// the raw byte length of the configured string, not runes and not decoded material.
//
// Sixteen 'é' are 16 runes but 32 bytes and must be accepted; fifteen 'é' plus an
// 'a' are 16 runes and 31 bytes and must be refused. A "tidy-up" to
// utf8.RuneCountInString would accept the 31-byte value and reject nothing it
// should — a silent weakening that no other test in this file can see.
func TestCredentialFloorCountsBytesNotRunes(t *testing.T) {
	atFloor := strings.Repeat("é", CredentialMinBytes/2) // 16 runes, 32 bytes
	if len(atFloor) != CredentialMinBytes {
		t.Fatalf("fixture is %d bytes, expected %d", len(atFloor), CredentialMinBytes)
	}
	t.Setenv("TEST_CREDENTIAL", atFloor)
	if _, err := RequiredCredential("TEST_CREDENTIAL", "", CredentialMinBytes); err != nil {
		t.Fatalf("refused a %d-byte multi-byte value: %v", len(atFloor), err)
	}

	under := strings.Repeat("é", CredentialMinBytes/2-1) + "a" // 16 runes, 31 bytes
	if len(under) != CredentialMinBytes-1 {
		t.Fatalf("fixture is %d bytes, expected %d", len(under), CredentialMinBytes-1)
	}
	t.Setenv("TEST_CREDENTIAL", under)
	if _, err := RequiredCredential("TEST_CREDENTIAL", "", CredentialMinBytes); err == nil {
		t.Fatalf("accepted a %d-byte value: the floor is counting runes, not bytes", len(under))
	}
}

// The floor is a per-call PARAMETER, not a package constant applied everywhere,
// because JOURNAL_SIGNING_KEY keeps the 16-byte contract ADR-032 already states
// while every ordinary credential moves to 32 (ADR-059).
//
// So the parameter has to be honoured rather than decorative: one value must be
// accepted under a 16-byte policy and refused under a 32-byte one. Hard-coding
// either number inside RequiredCredential, or ignoring the argument, turns this red.
func TestRequiredCredentialHonoursThePerCallFloor(t *testing.T) {
	const sixteen = "0123456789abcdef" // exactly 16 bytes

	t.Setenv("TEST_CREDENTIAL", sixteen)
	if _, err := RequiredCredential("TEST_CREDENTIAL", "", 16); err != nil {
		t.Fatalf("a 16-byte value must pass a 16-byte floor: %v", err)
	}
	if _, err := RequiredCredential("TEST_CREDENTIAL", "", CredentialMinBytes); err == nil {
		t.Fatalf("a 16-byte value must NOT pass the %d-byte floor", CredentialMinBytes)
	}

	// The diagnostic must state the floor the CALLER passed. A message hard-coded
	// to 32 would tell a payments operator their 16-byte key needs 32 bytes, which
	// is the opposite of true.
	t.Setenv("TEST_CREDENTIAL", "0123456789abcde") // 15, under both
	_, err := RequiredCredential("TEST_CREDENTIAL", "", 16)
	if err == nil {
		t.Fatal("a 15-byte value must not pass a 16-byte floor")
	}
	if !strings.Contains(err.Error(), "16") {
		t.Fatalf("the error does not state the floor the caller passed: %q", err)
	}
}

// TKT-253. OptionalCredential is the entry point for a credential whose ABSENCE is a
// legitimate configuration rather than a misconfiguration. STRIPE_SECRET_KEY is the
// motivating case: unset (or the non-secret literal `fake`) selects the offline fake
// PSP, which is how every local run and the entire gate work (ADR-032). Passing such a
// credential to RequiredCredential would refuse the empty value outright and break them.
//
// What it still applies is the TRANSPORT hygiene, and for a reason specific to this
// credential: payments sends the Stripe secret through req.SetBasicAuth
// (services/payments/internal/psp/stripe.go:201), so it becomes an Authorization header
// value. A padded or untransmittable value is therefore the same defect here as anywhere
// else RequiredCredential guards.
//
// What it deliberately does NOT apply is CredentialMinBytes. A length floor on a
// credential STRIPE issues and we do not control could refuse a working deployment,
// which is worse than the failure it would prevent.
func TestOptionalCredentialAcceptsTheAbsentAndSentinelConfigurations(t *testing.T) {
	// Unset. The env var is not set at all — the offline default.
	if got, err := OptionalCredential("TEST_OPTIONAL_CREDENTIAL", "fake"); err != nil || got != "" {
		t.Fatalf("an unset optional credential is a legal configuration: got %q, %v", got, err)
	}

	// The sentinel arrives through this path on every default stack:
	// compose.yaml:288 is `STRIPE_SECRET_KEY: ${STRIPE_SECRET_KEY:-fake}`. It is not a
	// secret and never reaches an Authorization header, because the selector drops it.
	t.Setenv("TEST_OPTIONAL_CREDENTIAL", "fake")
	if got, err := OptionalCredential("TEST_OPTIONAL_CREDENTIAL", "fake"); err != nil || got != "fake" {
		t.Fatalf("the sentinel must pass through unchanged: got %q, %v", got, err)
	}
}

// A real value is returned byte-for-byte: the caller compares and transmits it, so any
// normalisation here would make the configured value and the wire value differ, which is
// the exact class of bug RequiredCredential's whitespace case exists to prevent.
func TestOptionalCredentialReturnsARealValueUnchanged(t *testing.T) {
	const value = "sk_test_51H8xQ2eZvKYlo2C0abcdefgh"
	t.Setenv("TEST_OPTIONAL_CREDENTIAL", value)
	got, err := OptionalCredential("TEST_OPTIONAL_CREDENTIAL", "fake")
	if err != nil {
		t.Fatalf("rejected a header-safe value: %v", err)
	}
	if got != value {
		t.Fatalf("value altered at load: %q -> %q", value, got)
	}
}

// The transport predicates, and the redaction rule (ADR-012 §TKT-202) applied to their
// diagnostics. Each fixture carries a distinctive body so the assertion can pin the
// ABSENCE of the secret part rather than the absence of the whole string — an error
// echoing all but one character of a key would pass the weaker check.
func TestOptionalCredentialRejectsValuesHTTPWouldChangeWithoutEchoingThem(t *testing.T) {
	for name, value := range map[string]string{
		"leading whitespace":  " sk_test_LEADINGBODY",
		"trailing whitespace": "sk_test_TRAILINGBODY ",
		"newline":             "sk_test_NEWLINEBODY\n",
		"control byte":        "sk_test_CONTROLBODY\x01",
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("TEST_OPTIONAL_CREDENTIAL", value)
			_, err := OptionalCredential("TEST_OPTIONAL_CREDENTIAL", "fake")
			if err == nil {
				t.Fatalf("%q must be refused: it is not the value that would reach the wire", value)
			}
			if !strings.Contains(err.Error(), "TEST_OPTIONAL_CREDENTIAL") {
				t.Fatalf("the error must name the variable an operator set: %q", err)
			}
			// The body is the secret part. The error may name the variable; it may not
			// reproduce any of the value.
			if strings.Contains(err.Error(), "BODY") {
				t.Fatalf("the error echoes the supplied credential: %q", err)
			}
		})
	}
}

// The floor is the one check OptionalCredential must NOT inherit. A short value is a
// legitimate configuration for a credential whose format we do not control, and refusing
// it would refuse a working deployment.
func TestOptionalCredentialAppliesNoLengthFloor(t *testing.T) {
	t.Setenv("TEST_OPTIONAL_CREDENTIAL", "x")
	if _, err := OptionalCredential("TEST_OPTIONAL_CREDENTIAL", "fake"); err != nil {
		t.Fatalf("OptionalCredential must apply no length floor: %v", err)
	}
}
