package contractlint

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
	p := &Package{Funcs: map[string][]*ast.FuncDecl{}, Register: reg}
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
	for _, route := range p.Routes(func(string, string) bool { return true }) {
		if !route.TerminalResolved {
			t.Fatalf("%s terminal handler was not resolved", route.Path)
		}
	}
}

// The ORDERING case, which the fixture above cannot distinguish and no live spec reaches.
//
// chi's Group calls With(), which COPIES the middleware in force at that instant — so a
// scope that creates a group and only THEN calls r.Use wraps its own later routes and not
// the group. A collector that gathered every r.Use in a scope before walking its groups
// would attribute `lateMW` to /early, a status that route cannot write, breaking the sound-
// subset property this audit rests on.
//
// NESTED INSIDE AN OUTER GROUP. On the ROOT
// mux the sequence is impossible: With() calls updateRouteHandler() when the mux is not
// inline, which sets mx.handler, so the following r.Use panics with "all middlewares must be
// defined before routes on a mux". A fixture pinning a sequence chi rejects would assert
// behaviour that can never occur. Inside a Group the mux IS inline, no handler is built, and
// the sequence is legal.
//
// EXECUTED RATHER THAN REASONED, against the chi this repo builds with, through a throwaway
// probe:
//
//	ROOT Group-then-Use PANICS: chi: all middlewares must be defined before routes on a mux
//	INLINE /early ran middleware: [earlyMW]
//	INLINE /late  ran middleware: [earlyMW lateMW]
//
// The second and third lines are what this fixture asserts statically.
func TestGroupSnapshotsMiddlewareAtCreation(t *testing.T) {
	const src = `package p
func (s *Server) registerRoutes(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(s.earlyMW)
		r.Group(func(r chi.Router) {
			r.Get("/early", s.early)
		})
		r.Use(s.lateMW)
		r.Group(func(r chi.Router) {
			r.Get("/late", s.late)
		})
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
	p := &Package{Funcs: map[string][]*ast.FuncDecl{}, Register: reg}
	got := map[string][]string{}
	for _, r := range p.Routes(func(string, string) bool { return true }) {
		got[r.Path] = r.Handlers
	}
	want := map[string][]string{
		// Created BEFORE lateMW was declared, so it never sees it — confirmed at runtime.
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
