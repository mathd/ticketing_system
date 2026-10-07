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
