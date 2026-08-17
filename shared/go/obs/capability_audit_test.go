package obs_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// COS #7: no in-repo emitter writes a raw request path.
//
// The other tests pin the two emitters that exist TODAY. This one exists for the
// third one somebody adds next year: a new structured log call that passes
// r.URL.Path straight through would leak the capability again, and no test
// naming a specific route or a specific logger would notice.
//
// Modelled on services/catalog/internal/api/write_credential_test.go's AST scans
// (TestEveryHandlerReadingTheVerifiedOrganizerRequiresTheAssertion) — same shape:
// walk the real source, collect the call sites, allow a named closed set, fail on
// anything new. Reusing that pattern rather than inventing a second one.
//
// SCOPE, stated honestly: this scans Go source for `X.URL.Path` passed as a
// logging argument. It cannot see a path that reaches a log through a variable
// assigned several statements earlier, nor one emitted by a dependency — the OTel
// span attribute is exactly that case, and it is covered behaviourally instead by
// capability_span_test.go. This is a tripwire for the common shape, not a proof.
func TestNoEmitterWritesARawRequestPath(t *testing.T) {
	root := repoRoot(t)

	// There is deliberately NO file allowlist. An allowlist sanctions a FILE,
	// which keeps sanctioning it after the sanitiser is removed from it; the
	// condition here is that the value passes through SanitizedPath, which is
	// the mechanism itself. A file that stops sanitising stops being allowed,
	// with no list to update.

	// Logging call names that would put an argument into a log record.
	logCalls := map[string]bool{
		"Info": true, "InfoContext": true,
		"Error": true, "ErrorContext": true,
		"Warn": true, "WarnContext": true,
		"Debug": true, "DebugContext": true,
	}

	var scanned int
	var offenders []string
	var sanctionedHits int

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "dist", "vendor", ".sdlc":
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)

		src, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		file, parseErr := parser.ParseFile(token.NewFileSet(), path, src, 0)
		if parseErr != nil {
			// Generated or otherwise unparseable files are not this test's
			// business; a parse failure here must not be reported as a leak.
			return nil //nolint:nilerr // see comment
		}
		scanned++

		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || !logCalls[sel.Sel.Name] {
				return true
			}
			for _, arg := range call.Args {
				if !mentionsURLPath(arg) {
					continue
				}
				// The path reaches this log call. It is sanctioned only if it
				// passes through SanitizedPath on the way — that wrapper IS the
				// mechanism, so its presence is the whole test.
				if wrappedInSanitizer(arg) {
					sanctionedHits++
					continue
				}
				offenders = append(offenders, rel)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// Anti-vacuity, the lesson this repo keeps re-learning: if the walk matched
	// nothing, every assertion below is trivially true and the audit is theatre.
	if scanned < 50 {
		t.Fatalf("only %d Go files scanned — the walk is not reaching the repo, so this audit proves nothing", scanned)
	}
	if sanctionedHits == 0 {
		t.Fatal("the scan found no raw-path logging argument even in the two known emitters — " +
			"it can no longer detect the pattern it exists to detect")
	}

	for _, o := range offenders {
		t.Errorf("%s logs a raw request path. Wrap it in obs.SanitizedPath: a capability-bearing "+
			"segment written to a log is a credential handed to everyone who can read logs "+
			"(TKT-202, ADR-012). Wrapping the value is the only way to satisfy this test — "+
			"there is no allowlist, by design.", o)
	}
}

// mentionsURLPath reports whether `<something>.URL.Path` appears anywhere in the
// expression — as a bare argument or nested inside a call.
func mentionsURLPath(e ast.Expr) bool {
	var found bool
	ast.Inspect(e, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Path" {
			return true
		}
		if inner, ok := sel.X.(*ast.SelectorExpr); ok && inner.Sel.Name == "URL" {
			found = true
			return false
		}
		return true
	})
	return found
}

// wrappedInSanitizer reports whether the raw path passes through SanitizedPath.
//
// Matched on the function name alone, so it holds both for the in-package call
// (`SanitizedPath(...)`) and the cross-package one (`obs.SanitizedPath(...)`).
func wrappedInSanitizer(e ast.Expr) bool {
	var found bool
	ast.Inspect(e, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch fn := call.Fun.(type) {
		case *ast.Ident:
			if fn.Name == "SanitizedPath" && mentionsURLPath(call) {
				found = true
				return false
			}
		case *ast.SelectorExpr:
			if fn.Sel.Name == "SanitizedPath" && mentionsURLPath(call) {
				found = true
				return false
			}
		}
		return true
	})
	return found
}

// repoRoot walks up from the test's directory to the module/workspace root.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		if _, statErr := os.Stat(filepath.Join(dir, "go.work")); statErr == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatal("repo root (go.work) not found")
	return ""
}
