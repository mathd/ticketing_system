package obs

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The Go locale list must match the storefront's, and this test is the only
// thing that makes that true.
//
// The storefront bundle page is /{locale}/tickets/{ref}, and the capability
// route table pins the locale to a closed set so it cannot redact /admin/ or
// /scanner/ paths (ai-review F2). That closed set is duplicated across a
// language boundary, and the drift FAILS OPEN: add "de" to the storefront and
// /de/tickets/{ref} silently stops being sanitised — the reference goes back
// into every log line and no capability-route test notices, because the shape
// nobody declared is the shape nobody tests.
//
// So the drift is caught here, by reading the storefront's own source.
//
// Mutation this must catch: add or remove a locale on either side.
func TestStorefrontLocalesMatchTheStorefront(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatalf("resolve repository root: %v", err)
	}
	src, err := os.ReadFile(filepath.Join(root, "web", "storefront", "src", "lib", "locales.ts"))
	if err != nil {
		t.Fatalf("cannot read the storefront locale list: %v", err)
	}

	// export const LOCALES = ['en', 'fr'] as const;
	m := regexp.MustCompile(`(?s)export const LOCALES\s*=\s*\[(.*?)\]`).FindSubmatch(src)
	if m == nil {
		t.Fatal("LOCALES not found in locales.ts — this test can no longer detect drift, " +
			"which is the failure it exists to prevent. Fix the pattern, do not delete the test.")
	}
	var fromTS []string
	for _, raw := range strings.Split(string(m[1]), ",") {
		if v := strings.Trim(strings.TrimSpace(raw), `'"`); v != "" {
			fromTS = append(fromTS, v)
		}
	}
	if len(fromTS) == 0 {
		t.Fatal("parsed an empty locale list from locales.ts — the comparison below would be vacuous")
	}

	fromGo := append([]string(nil), storefrontLocales...)
	sort.Strings(fromTS)
	sort.Strings(fromGo)

	if strings.Join(fromTS, ",") != strings.Join(fromGo, ",") {
		t.Errorf("locale lists have drifted:\n  storefront (locales.ts) = %v\n  obs (capability_path.go) = %v\n"+
			"A locale present in the storefront but missing here means /<locale>/tickets/{ref} is "+
			"NOT sanitized and the guest reference is written to every log line (TKT-202, ADR-012).",
			fromTS, fromGo)
	}
}
