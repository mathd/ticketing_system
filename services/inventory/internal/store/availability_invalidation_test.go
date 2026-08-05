package store

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// TestCommitAvailabilityOrdersInvalidationAfterCommit is the ordering rule, and
// it is the one worth testing hardest.
//
// Invalidating BEFORE the commit lets a concurrent read repopulate the entry
// from the pre-commit row, so the cache would serve the old number for a full
// tier — the exact defect the cache exists to prevent, reintroduced by its own
// invalidation. Catalog has the same rule for a different reason (ADR-018:
// state-deriving transitions emit after commit); this is that repo-wide
// invariant, not a local choice.
//
// A failed commit must invalidate nothing: dropping an entry for a write that
// did not happen is a free extra database round trip, but it also means a
// rollback and a success are indistinguishable to the cache, which is how a
// "why is this slot always missing?" investigation starts.
func TestCommitAvailabilityOrdersInvalidationAfterCommit(t *testing.T) {
	slot := uuid.New()

	t.Run("success invalidates after the commit", func(t *testing.T) {
		var order []string
		p := &Postgres{}
		p.RegisterAvailabilityInvalidator(func(got uuid.UUID) {
			if got != slot {
				t.Errorf("invalidated %v, want %v", got, slot)
			}
			order = append(order, "invalidate")
		})
		err := p.commitAvailability(committerFunc(func() error {
			order = append(order, "commit")
			return nil
		}), slot)
		if err != nil {
			t.Fatalf("commitAvailability: %v", err)
		}
		if want := []string{"commit", "invalidate"}; !slices.Equal(order, want) {
			t.Fatalf("order = %v, want %v — invalidating first lets a concurrent read restore the pre-commit row", order, want)
		}
	})

	t.Run("a failed commit invalidates nothing", func(t *testing.T) {
		var called int
		p := &Postgres{}
		p.RegisterAvailabilityInvalidator(func(uuid.UUID) { called++ })
		if err := p.commitAvailability(committerFunc(func() error { return errFakeCommit }), slot); err != errFakeCommit {
			t.Fatalf("commitAvailability returned %v, want the commit error unwrapped", err)
		}
		if called != 0 {
			t.Fatalf("invalidator called %d times after a failed commit, want 0", called)
		}
	})

	t.Run("no invalidator registered is not a panic", func(t *testing.T) {
		p := &Postgres{}
		if err := p.commitAvailability(committerFunc(func() error { return nil }), slot); err != nil {
			t.Fatalf("commitAvailability with no cache wired: %v", err)
		}
	})
}

// TestAvailabilityMutationsUseInvalidatingCommit is the architecture guard.
//
// The cache is only correct if EVERY availability-changing commit invalidates,
// and nothing in Go's type system makes forgetting impossible — the plan draft
// said so plainly and this test is the honest substitute. It bans raw
// tx.Commit() in this package so that adding a write path forces a decision at
// review time rather than producing a slot that silently serves a stale number.
//
// What it does NOT do (ADR-021 — name the adversary): it stops honest mistakes.
// It does not stop someone editing this test in the same commit, and it says
// nothing about anyone writing to Postgres directly, who bypasses every Go
// callback in this package.
func TestAvailabilityMutationsUseInvalidatingCommit(t *testing.T) {
	// quarantine.go parks poison catalog events; it touches no pool, claim or
	// allocation row, so its commit changes no availability answer.
	exempt := map[string]bool{
		"quarantine.go":                true,
		"availability_invalidation.go": true, // defines the helper
	}

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var offenders, scanned []string
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") || exempt[name] {
			continue
		}
		scanned = append(scanned, name)
		f, perr := parser.ParseFile(token.NewFileSet(), filepath.Join(".", name), nil, 0)
		if perr != nil {
			t.Fatalf("parse %s: %v", name, perr)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Commit" {
				return true
			}
			if ident, ok := sel.X.(*ast.Ident); ok && ident.Name == "tx" {
				offenders = append(offenders, name)
			}
			return true
		})
	}

	// A scan that reaches no files is a test that cannot fail.
	if len(scanned) < 5 {
		t.Fatalf("scanned only %d production files (%v) — the walk is not reaching this package", len(scanned), scanned)
	}
	if len(offenders) > 0 {
		sort.Strings(offenders)
		offenders = slices.Compact(offenders)
		t.Errorf("raw tx.Commit() in %v — an availability-changing commit must go through "+
			"commitAvailability so the cache is invalidated after it. If this write genuinely "+
			"cannot change an availability answer, add it to the exempt list above with the reason.",
			offenders)
	}
}

type committerFunc func() error

func (f committerFunc) Commit() error { return f() }

var errFakeCommit = &commitError{}

type commitError struct{}

func (*commitError) Error() string { return "commit failed" }
