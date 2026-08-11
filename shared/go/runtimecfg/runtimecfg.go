// Package runtimecfg defines the bounded process resource policy shared by
// service and gateway entrypoints.
package runtimecfg

import (
	"database/sql"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/http/httpguts"
)

type HTTP struct {
	ReadHeaderTimeout time.Duration
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration
}

func HTTPFromEnv() (HTTP, error) {
	readHeader, err := duration("HTTP_READ_HEADER_TIMEOUT", 5*time.Second)
	if err != nil {
		return HTTP{}, err
	}
	read, err := duration("HTTP_READ_TIMEOUT", 15*time.Second)
	if err != nil {
		return HTTP{}, err
	}
	write, err := duration("HTTP_WRITE_TIMEOUT", 30*time.Second)
	if err != nil {
		return HTTP{}, err
	}
	idle, err := duration("HTTP_IDLE_TIMEOUT", 60*time.Second)
	if err != nil {
		return HTTP{}, err
	}
	return HTTP{readHeader, read, write, idle}, nil
}

func (c HTTP) Apply(server *http.Server) {
	server.ReadHeaderTimeout = c.ReadHeaderTimeout
	server.ReadTimeout = c.ReadTimeout
	server.WriteTimeout = c.WriteTimeout
	server.IdleTimeout = c.IdleTimeout
}

type Database struct {
	MaxOpenConns    int
	MaxIdleConns    int
	ConnMaxLifetime time.Duration
	ConnMaxIdleTime time.Duration
}

func DatabaseFromEnv() (Database, error) {
	open, err := integer("DB_MAX_OPEN_CONNS", 25)
	if err != nil {
		return Database{}, err
	}
	idle, err := integer("DB_MAX_IDLE_CONNS", 10)
	if err != nil {
		return Database{}, err
	}
	if idle > open {
		return Database{}, fmt.Errorf("DB_MAX_IDLE_CONNS must not exceed DB_MAX_OPEN_CONNS")
	}
	lifetime, err := duration("DB_CONN_MAX_LIFETIME", 30*time.Minute)
	if err != nil {
		return Database{}, err
	}
	idleTime, err := duration("DB_CONN_MAX_IDLE_TIME", 5*time.Minute)
	if err != nil {
		return Database{}, err
	}
	return Database{open, idle, lifetime, idleTime}, nil
}

func (c Database) Apply(db *sql.DB) {
	db.SetMaxOpenConns(c.MaxOpenConns)
	db.SetMaxIdleConns(c.MaxIdleConns)
	db.SetConnMaxLifetime(c.ConnMaxLifetime)
	db.SetConnMaxIdleTime(c.ConnMaxIdleTime)
}

// retiredInternalToken is the checked-in default this repo shipped before
// TKT-83. It is a public fingerprint, not an active secret: server mode
// refuses it forever so stale automation cannot keep authenticating with it.
const retiredInternalToken = "local-service-token"

// The signing keys that shipped as ACTIVE Compose defaults until ai-review S5.
// TKT-83 solved exactly this problem for the bearer tokens — no default, fail
// fast, refuse the retired literal forever — and these three were left out of
// that fix, so a stack whose env was unset booted silently on key material that
// is in the repository:
//
//   - a forged QR passed ed25519.Verify at the gate, which is what made the
//     unauthenticated scan route (S1) trivially exploitable rather than merely
//     open;
//   - a forged lifecycle checkpoint passed `access verify-lifecycle`, so the
//     integrity claim ADR-021 makes verified against an attacker's own chain.
//
// They are refused here rather than deleted quietly: a .env written before this
// change still holds them, and a value that is public in git history must never
// silently keep working. Each is the exact literal that was in compose.yaml.
const (
	RetiredJournalSigningKey   = "local-development-journal-key"
	RetiredAccessQRSeed        = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	RetiredAccessLifecycleSeed = "AQIDBAUGBwgJCgsMDQ4PEBESExQVFhcYGRobHB0eHyA"
)

// RequiredCredential validates one startup credential: present, and not the
// retired checked-in literal it replaced. Entrypoints call it before touching
// any dependency so a misconfigured deployment fails fast instead of timing
// out, and **errors never echo the supplied value**.
//
// Generic because a second credential arrived (TKT-191) and a per-credential
// function in a package imported by all five services and the gateway would
// make every caller carry someone else's private concern. Pass "" for
// retiredDefault when a credential has no retired literal to refuse.
func RequiredCredential(envVar, retiredDefault string) (string, error) {
	token := os.Getenv(envVar)
	switch {
	case token == "":
		return "", fmt.Errorf("%s required: no default is shipped, run `make up` once to generate a local credential", envVar)
	case retiredDefault != "" && token == retiredDefault:
		return "", fmt.Errorf("%s is the retired checked-in default: generate a real credential (`make up`)", envVar)
	// ORDER MATTERS: this whitespace case must stay AHEAD of the httpguts check
	// below. httpguts.ValidHeaderFieldValue PERMITS edge SP and HTAB — they are
	// legal field-value bytes — while net/http trims them in transit. So the
	// transport's own predicate cannot catch the normalization collision that
	// makes " secret " and "secret" one credential on the wire; only this case
	// does. Reordering these two silently reopens that hole (confirmed by the
	// TKT-191 ai-review's final pass).
	case strings.TrimSpace(token) != token:
		// Credentials travel in HTTP headers, and header parsing strips leading
		// and trailing optional whitespace (RFC 7230 §3.2.4) — verified: a client
		// that sets " secret " is received as "secret". So a padded value is not
		// the value it looks like, with two consequences. It never matches what
		// the peer configured, so authentication fails in a way that reads like a
		// wrong secret rather than a quoting mistake; and two credentials that
		// differ ONLY by padding are the same credential on the wire, which
		// silently defeats any comparison made on the raw strings.
		//
		// Refusing here is what lets callers compare raw values and be right.
		return "", fmt.Errorf("%s has leading or trailing whitespace: HTTP strips it in transit, so the value on the wire is not the value configured (check the quoting in .env)", envVar)
	case !httpguts.ValidHeaderFieldValue(token):
		// Anything net/http will not put on the wire. This is the transport's OWN
		// predicate rather than a hand-rolled grammar, so it cannot disagree with
		// what the client actually accepts — a check that merely rejected CR, LF
		// and NUL let \x01 and \x7f through, and those start cleanly and then fail
		// every outbound authenticated request at runtime, which is precisely the
		// fail-fast contract this function exists to keep (ai-review pass 3).
		return "", fmt.Errorf("%s contains a character that cannot appear in an HTTP header value", envVar)
	}
	return token, nil
}

// PaymentsTokenEnv is the credential that opens the payments service's internal
// surface — every charge, void, refund and partial refund.
//
// Split from INTERNAL_SERVICE_TOKEN by ai-review S8. That one value is held by
// all five services from one Compose anchor, so a compromise anywhere — a service
// process, a smoke-suite config, a shell history — yielded the whole platform's
// MONEY surface with no per-organizer scoping. The staff write tokens (TKT-191,
// TKT-194) had already established the shape: one credential, one thing it opens.
//
// This is a reduction, not a solution. Commerce still holds both values, so
// compromising commerce still reaches payments; what changes is that compromising
// catalog, inventory, access or the gateway no longer does. Per-caller credentials
// or mTLS is the finish, and it is not this.
const PaymentsTokenEnv = "PAYMENTS_INTERNAL_TOKEN"

// PaymentsTokenFromEnv validates the payments-only credential and refuses a value
// equal to the shared one — a configuration where they match looks configured and
// restores exactly the coupling the split removed.
func PaymentsTokenFromEnv() (string, error) {
	token, err := RequiredCredential(PaymentsTokenEnv, "")
	if err != nil {
		return "", err
	}
	if shared := os.Getenv("INTERNAL_SERVICE_TOKEN"); shared != "" && token == shared {
		return "", fmt.Errorf("%s must not equal INTERNAL_SERVICE_TOKEN: the separate credential exists "+
			"so a compromise elsewhere does not reach the money surface, and an identical value removes "+
			"that boundary while looking configured", PaymentsTokenEnv)
	}
	return token, nil
}

// InternalTokenFromEnv validates the shared service-to-service credential.
// One value across all five services (compose.yaml, the &go-env anchor), so
// whatever holds it can reach every service's internal surface.
func InternalTokenFromEnv() (string, error) {
	return RequiredCredential("INTERNAL_SERVICE_TOKEN", retiredInternalToken)
}

// ResponseValidationFromEnv reports whether a service enforces ADR-028
// response-drift fail-closed at runtime. It defaults to on, so dev, CI and the
// smoke gate keep the guarantee without configuring anything; only a
// deployment that has measured the cost turns it off. It is deliberately not
// part of HTTP: that policy is applied to an http.Server, and the gateway
// reads it while running no OpenAPI validation at all (TKT-125).
func ResponseValidationFromEnv() (bool, error) {
	return boolean("OPENAPI_RESPONSE_VALIDATION_ENABLED", true)
}

func duration(name string, fallback time.Duration) (time.Duration, error) {
	raw, ok := os.LookupEnv(name)
	if !ok {
		return fallback, nil
	}
	value, err := time.ParseDuration(raw)
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("%s must be a positive duration", name)
	}
	return value, nil
}

func boolean(name string, fallback bool) (bool, error) {
	raw, ok := os.LookupEnv(name)
	if !ok {
		return fallback, nil
	}
	value, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("%s must be a boolean", name)
	}
	return value, nil
}

func integer(name string, fallback int) (int, error) {
	raw, ok := os.LookupEnv(name)
	if !ok {
		return fallback, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer", name)
	}
	return value, nil
}
