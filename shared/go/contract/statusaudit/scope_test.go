package statusaudit

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// Nested and sibling groups each have their own middleware, and neither leaks into the
// other or into the parent. Written as a fixture because the aliasing failure it hunts is
// invisible in the live specs: commerce's two groups are siblings with one r.Use each, so a
// leak between them would produce the same answer.
func TestGroupMiddlewareDoesNotLeakBetweenScopes(t *testing.T) {
	const src = `package p
func (s *Server) registerRoutes(r chi.Router) {
	r.Get("/bare", s.bare)
	r.Group(func(r chi.Router) {
		r.Use(s.outerMW)
		r.Get("/outer", s.outer)
		r.Group(func(r chi.Router) {
			r.Use(s.innerMW)
			r.Get("/inner", s.inner)
		})
	})
	r.Group(func(r chi.Router) {
		r.Use(s.siblingMW)
		r.Get("/sibling", s.sibling)
	})
	r.Get("/after", s.after)
}`
	file, err := parser.ParseFile(token.NewFileSet(), "x.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	var reg *ast.FuncDecl
	for _, d := range file.Decls {
		if fn, ok := d.(*ast.FuncDecl); ok && fn.Name.Name == "registerRoutes" {
			reg = fn
		}
	}
	p := &Package{Funcs: map[string]*ast.FuncDecl{}, Register: reg}
	got := map[string][]string{}
	for _, r := range p.Routes(func(string, string) bool { return true }) {
		got[r.Path] = r.Handlers
	}
	want := map[string][]string{
		"/bare":    {"bare"},
		"/outer":   {"outerMW", "outer"},
		"/inner":   {"outerMW", "innerMW", "inner"},
		"/sibling": {"siblingMW", "sibling"},
		"/after":   {"after"},
	}
	for path, w := range want {
		g := got[path]
		if len(g) != len(w) {
			t.Fatalf("%s handlers = %v, want %v", path, g, w)
		}
		for i := range w {
			if g[i] != w[i] {
				t.Fatalf("%s handlers = %v, want %v", path, g, w)
			}
		}
	}
}

// The ORDERING case, which the fixture above cannot distinguish and no live spec reaches
// (TKT-278 ai-review pass 2). chi's Group calls With(), which COPIES the parent's middleware
// slice at that instant — so a parent that creates a group and only THEN calls r.Use wraps
// its own later routes and not the group. chi permits this: Use panics only once a route has
// been registered on the same mux, and creating a group registers none.
//
// A collector that gathered every r.Use in a scope before walking its groups would attribute
// `lateMW` to /early, which is a status that route cannot write — and this audit's whole
// claim is to derive a sound SUBSET.
func TestGroupSnapshotsMiddlewareAtCreation(t *testing.T) {
	const src = `package p
func (s *Server) registerRoutes(r chi.Router) {
	r.Use(s.earlyMW)
	r.Group(func(r chi.Router) {
		r.Get("/early", s.early)
	})
	r.Use(s.lateMW)
	r.Group(func(r chi.Router) {
		r.Get("/late", s.late)
	})
}`
	file, err := parser.ParseFile(token.NewFileSet(), "x.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	var reg *ast.FuncDecl
	for _, d := range file.Decls {
		if fn, ok := d.(*ast.FuncDecl); ok && fn.Name.Name == "registerRoutes" {
			reg = fn
		}
	}
	p := &Package{Funcs: map[string]*ast.FuncDecl{}, Register: reg}
	got := map[string][]string{}
	for _, r := range p.Routes(func(string, string) bool { return true }) {
		got[r.Path] = r.Handlers
	}
	want := map[string][]string{
		// Created BEFORE lateMW was declared, so it never sees it.
		"/early": {"earlyMW", "early"},
		// Created after, so it sees both.
		"/late": {"earlyMW", "lateMW", "late"},
	}
	for path, w := range want {
		g := got[path]
		if len(g) != len(w) {
			t.Fatalf("%s handlers = %v, want %v", path, g, w)
		}
		for i := range w {
			if g[i] != w[i] {
				t.Fatalf("%s handlers = %v, want %v (chi snapshots middleware when Group is called)",
					path, g, w)
			}
		}
	}
}
