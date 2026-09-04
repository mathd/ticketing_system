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
// SCOPE — stated narrowly on purpose, because the honest bound is smaller than
// "no emitter writes a raw path" and an overclaimed invariant is worse than a
// modest one (ai-review F4).
//
// This test DOES catch: a `.URL.Path` passed directly to a structured logging
// call; the same value passed via a single-hop alias (`p := r.URL.Path`); and an
// expression that mixes a sanitized and a raw occurrence.
//
// This test does NOT catch: a path threaded through multiple assignments, a
// struct field, a helper function, or a closure; a value reaching a log via
// fmt.Sprintf into another variable first; a logger whose method name is not in
// the table below; or anything emitted from inside a dependency — the OTel span
// attribute is precisely that case, and it is covered behaviourally by
// capability_span_test.go instead.
//
// A type-aware SSA dataflow pass would close the rest. That is deliberately not
// built here: the sinks are two, they are named, and each is pinned by a
// behavioural test. This is a TRIPWIRE for the shape a future author is actually
// likely to write, not a proof of absence — do not cite it as one.
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
			case ".git", "node_modules", "dist", "vendor":
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

		// Collect single-hop aliases of the raw path first: `p := r.URL.Path`
		// and `var p = r.URL.Path`. One hop, deliberately — this is a tripwire
		// for the shape a tired author actually writes, not a dataflow engine.
		rawPathAliases := map[string]bool{}
		ast.Inspect(file, func(n ast.Node) bool {
			switch decl := n.(type) {
			case *ast.AssignStmt:
				for i, rhs := range decl.Rhs {
					if i < len(decl.Lhs) && isBareURLPath(rhs) {
						if id, ok := decl.Lhs[i].(*ast.Ident); ok {
							rawPathAliases[id.Name] = true
						}
					}
				}
			case *ast.ValueSpec:
				for i, v := range decl.Values {
					if i < len(decl.Names) && isBareURLPath(v) {
						rawPathAliases[decl.Names[i].Name] = true
					}
				}
			}
			return true
		})

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
				// Direct: the raw path appears in the argument expression.
				if mentionsURLPath(arg) {
					// Sanctioned only if EVERY occurrence in this expression is
					// inside a SanitizedPath call. Checking merely that a
					// wrapper is present would approve
					// `log.Info(..., SanitizedPath(r.URL.Path), "raw", r.URL.Path)`
					// (ai-review F4).
					if !hasUnwrappedURLPath(arg) {
						sanctionedHits++
						continue
					}
					offenders = append(offenders, rel)
					continue
				}
				// Aliased: `p := r.URL.Path; log.Info(..., p)`. Same leak, one
				// statement further away (ai-review F4).
				if ident, ok := arg.(*ast.Ident); ok && rawPathAliases[ident.Name] {
					offenders = append(offenders, rel)
				}
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

// isBareURLPath reports whether an expression is exactly `<x>.URL.Path`.
func isBareURLPath(e ast.Expr) bool {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Path" {
		return false
	}
	inner, ok := sel.X.(*ast.SelectorExpr)
	return ok && inner.Sel.Name == "URL"
}

// hasUnwrappedURLPath reports whether any `.URL.Path` in e sits OUTSIDE a
// SanitizedPath call.
//
// Asking "is every occurrence wrapped?" rather than "is a wrapper present?" is
// the whole point: the weaker question approves an expression that logs the
// sanitized value and the raw one side by side (ai-review F4). Matched on the
// function name so it holds for both `SanitizedPath(...)` and
// `obs.SanitizedPath(...)`.
// Implemented with ast.Inspect and a sanitizer-depth counter rather than a
// hand-rolled recursion. The hand-rolled version was WRONG in a way that looked
// right and tested green: ast.Inspect calls its visitor with the root node
// first, and the callback returned false for `child == n`, so it never descended
// past the root at all. `fmt.Sprintf("%s %s", SanitizedPath(r.URL.Path),
// r.URL.Path)` was reported safe — precisely the expression this function exists
// to reject (ai-review F6). The flat sibling-argument case appeared to work only
// because each argument is inspected separately by the caller.
func hasUnwrappedURLPath(e ast.Expr) bool {
	// Collect every raw-path node, and every raw-path node that sits inside a
	// sanitizer call, then compare. Two independent walks rather than one
	// stateful one: a depth counter unwound on ast.Inspect's nil callback
	// decrements on EVERY node exit, not just the sanitizer's, so it dropped
	// back to zero before reaching the wrapped path and reported
	// SanitizedPath(r.URL.Path) as an offender. Caught by the table tests in
	// capability_audit_helper_test.go, which is why they exist.
	all := map[ast.Node]bool{}
	ast.Inspect(e, func(n ast.Node) bool {
		if expr, ok := n.(ast.Expr); ok && isBareURLPath(expr) {
			all[n] = true
		}
		return true
	})
	if len(all) == 0 {
		return false
	}

	wrapped := map[ast.Node]bool{}
	ast.Inspect(e, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || !isSanitizerCall(call) {
			return true
		}
		// Everything beneath this call is accounted for.
		ast.Inspect(call, func(inner ast.Node) bool {
			if expr, ok := inner.(ast.Expr); ok && isBareURLPath(expr) {
				wrapped[inner] = true
			}
			return true
		})
		return true
	})

	for n := range all {
		if !wrapped[n] {
			return true
		}
	}
	return false
}

func isSanitizerCall(call *ast.CallExpr) bool {
	switch fn := call.Fun.(type) {
	case *ast.Ident:
		return fn.Name == "SanitizedPath"
	case *ast.SelectorExpr:
		return fn.Sel.Name == "SanitizedPath"
	}
	return false
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
