package main

import (
	"io"
	"strings"
	"testing"
)

// Usage errors are refused before any database is contacted: the DSN here is unreachable,
// so reaching the database would fail with a different error.
func TestCheckClaimHistorySequenceRefusesBadUsage(t *testing.T) {
	const unreachable = "postgres://nobody@127.0.0.1:1/none"
	for name, args := range map[string][]string{
		"positional":   {"extra"},
		"unknown flag": {"--force"},
		"zero timeout": {"--timeout=0s"},
		"negative":     {"--timeout=-1s"},
		"bad duration": {"--timeout=soon"},
	} {
		t.Run(name, func(t *testing.T) {
			err := runCheckClaimHistorySequence(args, unreachable, io.Discard)
			if err == nil || !strings.Contains(err.Error(), "usage:") {
				t.Fatalf("args %v: err = %v, want a usage error", args, err)
			}
		})
	}
	if err := runCheckClaimHistorySequence(nil, "", io.Discard); err == nil || !strings.Contains(err.Error(), "DATABASE_URL") {
		t.Fatalf("no DATABASE_URL: err = %v", err)
	}
}

// A DATABASE_URL can carry a password in its query string, which pgx's own parse error quotes
// verbatim. Neither a malformed DSN nor an unreachable database may put it in the error main
// prints (TKT-295 review finding 1).
func TestCheckClaimHistorySequenceNeverEchoesThePassword(t *testing.T) {
	const secret = "REVIEW_ONLY_SECRET_295"
	for name, dsn := range map[string]string{
		"malformed":   "postgres://inventory@127.0.0.1:1/inventory?password=" + secret + "&connect_timeout=bad",
		"unreachable": "postgres://inventory@127.0.0.1:1/inventory?password=" + secret + "&connect_timeout=2",
		"userinfo":    "postgres://inventory:" + secret + "@127.0.0.1:1/inventory?connect_timeout=2",
	} {
		t.Run(name, func(t *testing.T) {
			var out strings.Builder
			err := runCheckClaimHistorySequence(nil, dsn, &out)
			if err == nil {
				t.Fatal("want an error from an unusable database")
			}
			if strings.Contains(err.Error(), secret) || strings.Contains(out.String(), secret) {
				t.Fatalf("the password leaked: err=%q out=%q", err, out.String())
			}
		})
	}
}
