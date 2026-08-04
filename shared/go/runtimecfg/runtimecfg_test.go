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
			got, err := RequiredCredential("TEST_CREDENTIAL", "")
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
	for _, value := range []string{
		"0f3d1c9a8b7e6f5d4c3b2a1908f7e6d5", // what make up generates
		"a-b_c.d~e",                        // punctuation a base64url/hex value might carry
		"tok en",                           // an INTERIOR space is legal and must survive
		"Zm9vYmFy==",                       // base64 padding
	} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("TEST_CREDENTIAL", value)
			got, err := RequiredCredential("TEST_CREDENTIAL", "")
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
