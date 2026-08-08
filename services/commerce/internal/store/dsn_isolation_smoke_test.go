//go:build smoke

package store

import (
	"net/url"
	"os"
	"strings"
	"testing"
)

// Two packages migrating one database is the defect this file exists to prevent
// (TKT-198). `go test ./internal/...` runs packages as SEPARATE, CONCURRENT test
// binaries, so a package-local `sync.Once` around store.Migrate serializes nothing
// across them: both goose runs start, and the loser dies mid-migration on
// "relation ... already exists". The gate then fails with a different ~30-test
// subset each run, which reads as flakiness rather than as one cause.
//
// It has now happened twice. TKT-226 hit it when ./internal/mailer became the second
// store.Migrate caller and fixed it by giving that package its own database. That fix
// did not generalize: ./internal/bulkrefund (TKT-159) was already the third caller and
// kept sharing ./internal/store's database until TKT-198.
//
// The rule enforced here is deliberately narrow — every commerce smoke package gets a
// DISTINCT database — and says nothing about HOW a package prepares its schema. The
// repo has two legitimate patterns that a "helpers must not migrate" rule would have
// broken: the migration-upgrade harness drives goose against a per-test SCHEMA
// (migration_smoke_test.go), and payments' legacy harness migrates a database only
// part-way ON PURPOSE (TKT-217, scripts/smoke.sh). Collision is the failure mode;
// migration is not.
//
// The DSNs come from scripts/smoke.sh, which is also what CI runs.

// commerceSmokeDSNVars is every database the commerce smoke suite is handed. Adding a
// package that needs its own database means adding it here — at which point this test
// fails until scripts/smoke.sh actually creates and exports it.
var commerceSmokeDSNVars = []string{
	"COMMERCE_TEST_DATABASE_URL",            // ./internal/store
	"COMMERCE_BULKREFUND_TEST_DATABASE_URL", // ./internal/bulkrefund
	"COMMERCE_MAILER_TEST_DATABASE_URL",     // ./internal/mailer
	"COMMERCE_MIGRATION_TEST_DATABASE_URL",  // ./internal/store migration-upgrade harness (own schemas)
}

// TestCommerceSmokeDatabasesAreIsolated fails when two commerce smoke packages would
// migrate the same database.
//
// It FAILS rather than SKIPS on a missing variable. Skipping is what would let this
// test pass vacuously: the pre-fix state of TKT-198 is precisely that
// COMMERCE_BULKREFUND_TEST_DATABASE_URL does not exist, so a t.Skip here would report
// green for the exact defect the test is written to catch. Outside the gate the whole
// file is excluded by the `smoke` build tag, so this costs a normal `go test` nothing.
func TestCommerceSmokeDatabasesAreIsolated(t *testing.T) {
	seen := make(map[string]string, len(commerceSmokeDSNVars))
	for _, envVar := range commerceSmokeDSNVars {
		dsn := os.Getenv(envVar)
		if dsn == "" {
			t.Errorf("%s is not set: every commerce smoke package needs its own database "+
				"(see scripts/smoke.sh); an unset DSN makes its package's tests skip and the gate "+
				"go green having proved nothing", envVar)
			continue
		}
		name, err := databaseName(dsn)
		if err != nil {
			t.Errorf("%s=%q: %v", envVar, dsn, err)
			continue
		}
		if other, dup := seen[name]; dup {
			t.Errorf("%s and %s both resolve to database %q — `go test ./internal/...` runs those "+
				"packages concurrently, so both will migrate it and the loser dies mid-migration "+
				"(TKT-198). Give the new package its own database in scripts/smoke.sh",
				other, envVar, name)
			continue
		}
		seen[name] = envVar
	}
}

// databaseName extracts the database a DSN points at, which is what must be unique —
// host and credentials are identical across these and would defeat a whole-string
// comparison.
func databaseName(dsn string) (string, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return "", err
	}
	name := strings.TrimPrefix(u.Path, "/")
	if name == "" {
		return "", errNoDatabaseInDSN
	}
	return name, nil
}

var errNoDatabaseInDSN = &dsnError{"DSN names no database"}

type dsnError struct{ msg string }

func (e *dsnError) Error() string { return e.msg }
