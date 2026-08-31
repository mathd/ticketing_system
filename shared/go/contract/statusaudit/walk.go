package statusaudit

// The source walker. Deriving what a handler can write is the half the OpenAPI document
// cannot answer, and it is the half that makes this audit more than the spec restating
// itself (TKT-278) — the same reason catalog's 404 invariant walks Go source rather than the
// document (TKT-178).
//
// The walker is shared; the FLOORS are not. Each service composes responses through its own
// helpers, and what each helper guarantees is a fact about that helper, so `Floors` is
// hand-written per service with a line of reasoning each. That split is deliberate: the
// mechanical part is common and the judgement is local.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Package is one service's parsed api package.
type Package struct {
	// Funcs is every function and method declaration, by name. Methods are keyed by their
	// own name, not receiver-qualified: the services declare no colliding pair, and the
	// walker's over-collection is the safe direction anyway.
	Funcs map[string]*ast.FuncDecl
	// Register is the registerRoutes declaration, or nil for a service that mounts through
	// generated code instead.
	Register *ast.FuncDecl
}

// Config is what a service's audit must supply beyond its source.
//
// `w.WriteHeader(status)` is NOT configurable and is recognised in every service: it is
// net/http's own single-argument status write, so its shape cannot vary. Catalog's
// GetOpenAPISpec is the case that needs it — it streams the committed contract and never
// goes through a JSON helper, so without it that handler's derived set is empty, which
// satisfies the audit vacuously.
type Config struct {
	// WriteFuncs names the functions that write a response with an explicit status
	// argument — `write(w, 400, …)`, `writeJSON(w, 500, …)`. The status is the argument at
	// StatusArg.
	WriteFuncs []string
	// StatusArg is the zero-based index of the status argument in a WriteFuncs call.
	StatusArg int
	// Floors is each helper's UNCONDITIONAL status set: what it emits for ANY input. The
	// walker contributes these and does NOT descend into the helper's body, which is the
	// whole sound-subset rule — descending collects the literal statuses of a helper's
	// CONDITIONAL arms and reports them for every caller, which over-approximates badly
	// enough to make the audit useless (measured on inventory: treating `problem()` as its
	// full {400,404,409,500} set reports five correct operations as missing a 409).
	//
	// A helper that RETURNS a status rather than writing one belongs here too: its floor is
	// contributed when its result reaches a write, see ReturningFuncs.
	Floors map[string][]int
	// ReturningFuncs names helpers of the form `func(...) (int, string)` whose returned
	// status is passed to a write. Their Floors entry is contributed when the walker sees
	// their result assigned to a variable that reaches a write's status position.
	ReturningFuncs map[string]bool
}

// ParsePackage reads every non-test, non-generated .go file in dir.
func ParsePackage(dir string) (*Package, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") ||
			strings.HasSuffix(n, "_test.go") || n == "openapi_gen.go" {
			continue
		}
		names = append(names, n)
	}
	sort.Strings(names)

	pkg := &Package{Funcs: map[string]*ast.FuncDecl{}}
	fset := token.NewFileSet()
	for _, n := range names {
		file, err := parser.ParseFile(fset, filepath.Join(dir, n), nil, 0)
		if err != nil {
			return nil, err
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			pkg.Funcs[fn.Name.Name] = fn
			if fn.Name.Name == "registerRoutes" {
				pkg.Register = fn
			}
		}
	}
	return pkg, nil
}

// Route is one mounted route and the handler chain that serves it, outermost first.
type Route struct {
	Method, Path string
	Handlers     []string
}

// Routes parses registerRoutes for `r.Get("/path", s.handler)` registrations. `documented`
// decides which routes are in scope, and it is asked rather than a name list: an
// undocumented route (`/openapi.yaml`) drops out STRUCTURALLY, by not being in the contract,
// which is also why it must not acquire an artificial 500 obligation.
func (p *Package) Routes(documented func(method, path string) bool) []Route {
	if p.Register == nil {
		return nil
	}
	var out []Route
	// `r.Group(func(r chi.Router) { r.Use(mw); r.Post(…) })` — the middleware writes
	// responses for every route inside the group, so a route's handler chain is
	// [its group's middleware…, its own handlers]. Missing this is not hypothetical:
	// commerce's two groups apply `limitCheckoutSource`/`limitSource`, which answer 429
	// before the handler runs. Without collecting them the audit never derives that 429 —
	// it would be green on those routes for the wrong reason, which is the same vacuous
	// pass an empty status set gives (TKT-278 ai-review).
	collect(p.Register.Body, nil, documented, &out)
	return out
}

// collect walks one router scope. `inherited` are the middleware handler names in force from
// enclosing groups.
func collect(body ast.Node, inherited []string, documented func(method, path string) bool, out *[]Route) {
	// Two passes over THIS scope's statements: middleware first, because `r.Use` may be
	// written after a route in the same block and still applies to it.
	var mw []string
	mw = append(mw, inherited...)
	forEachRouterCall(body, func(name string, call *ast.CallExpr) {
		if name == "Use" {
			for _, a := range call.Args {
				mw = append(mw, handlerNames(a)...)
			}
		}
	})
	forEachRouterCall(body, func(name string, call *ast.CallExpr) {
		// A nested scope: recurse with this scope's middleware in force. `Route` takes a
		// path prefix plus the closure; `Group` takes the closure alone.
		if (name == "Group" || name == "Route") && len(call.Args) > 0 {
			if fn, ok := call.Args[len(call.Args)-1].(*ast.FuncLit); ok {
				collect(fn.Body, mw, documented, out)
			}
			return
		}
		method := strings.ToUpper(name)
		if !isHTTPMethod(method) || len(call.Args) != 2 {
			return
		}
		lit, ok := call.Args[0].(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return
		}
		path, err := strconv.Unquote(lit.Value)
		if err != nil || !documented(method, path) {
			return
		}
		handlers := append(append([]string{}, mw...), handlerNames(call.Args[1])...)
		*out = append(*out, Route{Method: method, Path: path, Handlers: handlers})
	})
}

// forEachRouterCall visits every `r.X(…)` in a scope WITHOUT descending into a nested
// router closure — those are walked by `collect`'s own recursion, with the right middleware
// in force. Descending here instead would attribute an inner group's routes to the outer
// scope and lose the inner `r.Use`.
func forEachRouterCall(body ast.Node, fn func(name string, call *ast.CallExpr)) {
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if ident, ok := sel.X.(*ast.Ident); !ok || ident.Name != "r" {
			return true
		}
		fn(sel.Sel.Name, call)
		// Do NOT descend into a nested router closure here: `collect` recurses into it
		// itself, with that scope's own `r.Use` in force. Descending from here would
		// attribute the inner group's routes to the outer scope and lose its middleware.
		name := sel.Sel.Name
		return name != "Group" && name != "Route"
	})
}

// handlerNames returns every `s.X` named in a handler argument, outermost first. It covers
// the three registration shapes the services use:
//
//	s.create                                  → ["create"]
//	s.internalOnly(s.holdSeating)             → ["internalOnly", "holdSeating"]
//	s.internalOnly(s.transition("confirmed")) → ["internalOnly", "transition"]
//
// The third is a closure factory: `transition` returns the handler, so its own body is what
// writes, and naming it is exactly right. An inline `func(w, r)` literal yields no names,
// which is only safe because such routes are also undocumented — a documented one is caught
// by the caller's empty-set assertion rather than passing silently.
func handlerNames(arg ast.Expr) []string {
	var out []string
	ast.Inspect(arg, func(n ast.Node) bool {
		if sel, ok := n.(*ast.SelectorExpr); ok {
			if ident, ok := sel.X.(*ast.Ident); ok && ident.Name == "s" {
				out = append(out, sel.Sel.Name)
			}
		}
		return true
	})
	return out
}

// Emitted unions the statuses reachable from roots: literals written in each body, each
// helper's floor, and the same transitively through calls.
func (p *Package) Emitted(roots []string, cfg Config) []int {
	var out []int
	seen := map[string]bool{}
	var walk func(string)
	walk = func(name string) {
		if seen[name] {
			return
		}
		seen[name] = true
		// A helper with a declared floor contributes THAT and stops. See Config.Floors.
		if floor, ok := cfg.Floors[name]; ok {
			out = append(out, floor...)
			return
		}
		fn, ok := p.Funcs[name]
		if !ok {
			return
		}
		out = append(out, statusesIn(fn.Body, cfg)...)
		for _, callee := range calledNames(fn.Body) {
			walk(callee)
		}
	}
	for _, r := range roots {
		walk(r)
	}
	return out
}

// statusesIn extracts every status a body writes: an integer literal, a net/http constant,
// or a variable that reached a write's status position — covering both the finite
// conditional (`code := 201; if replay { code = 200 }`) and the returning-helper form
// (`code, msg := persistenceReadProblem(err); write(w, code, …)`).
//
// The variable pass over-collects across a body holding several such variables, and that
// direction is the safe one: a status reported that a handler cannot write costs one
// declaration nobody uses, while a missed one is an undeclared status in production.
func statusesIn(body *ast.BlockStmt, cfg Config) []int {
	writeFunc := map[string]bool{}
	for _, n := range cfg.WriteFuncs {
		writeFunc[n] = true
	}
	statusVars := map[string]bool{}
	var out []int

	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		name := ""
		switch fn := call.Fun.(type) {
		case *ast.Ident:
			name = fn.Name
		case *ast.SelectorExpr:
			name = fn.Sel.Name
		}
		// net/http's own status write, always at argument 0. See Config.
		if name == "WriteHeader" && len(call.Args) == 1 {
			if v, ok := literalStatus(call.Args[0]); ok {
				out = append(out, v)
			} else if ident, ok := call.Args[0].(*ast.Ident); ok {
				statusVars[ident.Name] = true
			}
			return true
		}
		// `w.Write(payload)` with no preceding WriteHeader sends an IMPLICIT 200 — net/http
		// writes the header on the first body write. Access's `qr` handler is the case:
		// it streams a PNG with `_, _ = w.Write(image)` and never names a success status,
		// so without this its derived set contains only its error codes. That is worse than
		// an empty set, because the vacuity guard cannot see it: the set is non-empty, so
		// deleting the operation's 200 declaration left the audit GREEN (executed, TKT-278
		// ai-review [medium]).
		//
		// Attributed to the ResponseWriter identifier rather than any `Write`: the receiver
		// must be the handler's `w`, or every bytes.Buffer and strings.Builder in a body
		// would contribute a phantom 200.
		if name == "Write" && len(call.Args) == 1 {
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
				if ident, ok := sel.X.(*ast.Ident); ok && ident.Name == "w" {
					out = append(out, http.StatusOK)
				}
			}
			return true
		}
		if !writeFunc[name] || len(call.Args) <= cfg.StatusArg {
			return true
		}
		if v, ok := literalStatus(call.Args[cfg.StatusArg]); ok {
			out = append(out, v)
		} else if ident, ok := call.Args[cfg.StatusArg].(*ast.Ident); ok {
			statusVars[ident.Name] = true
		}
		return true
	})

	if len(statusVars) == 0 {
		return out
	}
	ast.Inspect(body, func(n ast.Node) bool {
		assign, ok := n.(*ast.AssignStmt)
		if !ok {
			return true
		}
		// A returning helper: `code, msg := persistenceReadProblem(err)`. Its floor is the
		// set of statuses it can return, contributed here because the value reaches a write.
		if len(assign.Rhs) == 1 {
			if call, ok := assign.Rhs[0].(*ast.CallExpr); ok {
				name := ""
				switch fn := call.Fun.(type) {
				case *ast.Ident:
					name = fn.Name
				case *ast.SelectorExpr:
					name = fn.Sel.Name
				}
				if cfg.ReturningFuncs[name] {
					for _, l := range assign.Lhs {
						if ident, ok := l.(*ast.Ident); ok && statusVars[ident.Name] {
							out = append(out, cfg.Floors[name]...)
						}
					}
				}
				return true
			}
		}
		for i, l := range assign.Lhs {
			ident, ok := l.(*ast.Ident)
			if !ok || !statusVars[ident.Name] || i >= len(assign.Rhs) {
				continue
			}
			if v, ok := literalStatus(assign.Rhs[i]); ok {
				out = append(out, v)
			}
		}
		return true
	})
	return out
}

func literalStatus(e ast.Expr) (int, bool) {
	switch v := e.(type) {
	case *ast.BasicLit:
		if n, err := strconv.Atoi(v.Value); err == nil {
			return n, true
		}
	case *ast.SelectorExpr:
		if x, ok := v.X.(*ast.Ident); ok && x.Name == "http" {
			if n := httpStatusConst(v.Sel.Name); n != 0 {
				return n, true
			}
		}
	}
	return 0, false
}

// calledNames returns every function or method invoked in a body. It over-collects — any
// `x.Foo()` counts — and that is the safe direction for the reason catalog's equivalent
// gives: this feeds "can this route reach that status", where a false yes costs a
// declaration and a false no costs a production surprise.
func calledNames(body *ast.BlockStmt) []string {
	var out []string
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch fn := call.Fun.(type) {
		case *ast.Ident:
			out = append(out, fn.Name)
		case *ast.SelectorExpr:
			out = append(out, fn.Sel.Name)
		}
		return true
	})
	return out
}

func isHTTPMethod(m string) bool {
	switch m {
	case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete,
		http.MethodHead, http.MethodOptions:
		return true
	}
	return false
}

// httpStatusConst maps the net/http constant names the services write. An unknown name
// returns 0 and is skipped, which under-approximates — the honest failure mode for a table,
// and one each service's empty-set assertion catches if it ever swallows a handler's only
// status.
func httpStatusConst(name string) int {
	switch name {
	case "StatusOK":
		return http.StatusOK
	case "StatusCreated":
		return http.StatusCreated
	case "StatusAccepted":
		return http.StatusAccepted
	case "StatusNoContent":
		return http.StatusNoContent
	case "StatusBadRequest":
		return http.StatusBadRequest
	case "StatusUnauthorized":
		return http.StatusUnauthorized
	case "StatusForbidden":
		return http.StatusForbidden
	case "StatusNotFound":
		return http.StatusNotFound
	case "StatusConflict":
		return http.StatusConflict
	case "StatusGone":
		return http.StatusGone
	case "StatusUnprocessableEntity":
		return http.StatusUnprocessableEntity
	case "StatusTooManyRequests":
		return http.StatusTooManyRequests
	case "StatusInternalServerError":
		return http.StatusInternalServerError
	case "StatusNotImplemented":
		return http.StatusNotImplemented
	case "StatusBadGateway":
		return http.StatusBadGateway
	case "StatusServiceUnavailable":
		return http.StatusServiceUnavailable
	case "StatusGatewayTimeout":
		return http.StatusGatewayTimeout
	}
	return 0
}
