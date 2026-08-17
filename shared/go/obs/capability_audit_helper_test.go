package obs_test

import (
	"go/parser"
	"testing"
)

// Direct table-driven tests for the audit's expression walk (ai-review F6).
//
// The previous implementation was broken in a way no end-to-end audit run
// revealed: it never descended below the root, so a nested raw occurrence beside
// a sanitized one was reported safe. An end-to-end test could not localise that;
// these can, and they are the tests the finding asked for.
func TestHasUnwrappedURLPath(t *testing.T) {
	for _, tc := range []struct {
		name string
		expr string
		want bool // true = contains an UNWRAPPED raw path (an offender)
	}{
		{"bare raw path", `r.URL.Path`, true},
		{"wrapped", `SanitizedPath(r.URL.Path)`, false},
		{"wrapped, qualified", `obs.SanitizedPath(r.URL.Path)`, false},
		{"nested mixed — the F6 case", `fmt.Sprintf("%s %s", SanitizedPath(r.URL.Path), r.URL.Path)`, true},
		{"nested mixed, qualified", `fmt.Sprintf("%s %s", obs.SanitizedPath(r.URL.Path), r.URL.Path)`, true},
		{"nested fully sanitized", `fmt.Sprintf("%s %s", SanitizedPath(r.URL.Path), SanitizedPath(r.URL.Path))`, false},
		{"raw deep inside a call chain", `strings.ToUpper(strings.TrimSpace(r.URL.Path))`, true},
		{"sanitized then transformed", `strings.ToUpper(SanitizedPath(r.URL.Path))`, false},
		{"raw after a sanitized sibling in one call", `join(SanitizedPath(r.URL.Path), r.URL.Path)`, true},
		{"unrelated", `r.Method`, false},
		{"unrelated selector ending in Path", `cfg.File.Path`, false},
		{"raw inside a composite literal", `[]string{SanitizedPath(r.URL.Path), r.URL.Path}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, err := parser.ParseExpr(tc.expr)
			if err != nil {
				t.Fatalf("cannot parse %q: %v", tc.expr, err)
			}
			if got := hasUnwrappedURLPath(e); got != tc.want {
				t.Errorf("hasUnwrappedURLPath(%s) = %v, want %v", tc.expr, got, tc.want)
			}
		})
	}
}

// mentionsURLPath is the cheap pre-filter; if it misses, the walk never runs.
func TestMentionsURLPath(t *testing.T) {
	for _, tc := range []struct {
		expr string
		want bool
	}{
		{`r.URL.Path`, true},
		{`SanitizedPath(r.URL.Path)`, true},
		{`fmt.Sprintf("%s", req.URL.Path)`, true},
		{`r.Method`, false},
		{`cfg.File.Path`, false},
		{`u.Path`, false},
	} {
		e, err := parser.ParseExpr(tc.expr)
		if err != nil {
			t.Fatalf("cannot parse %q: %v", tc.expr, err)
		}
		if got := mentionsURLPath(e); got != tc.want {
			t.Errorf("mentionsURLPath(%s) = %v, want %v", tc.expr, got, tc.want)
		}
	}
}
