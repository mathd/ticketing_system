package contractlint

// The source walker derives behavior the OpenAPI document cannot supply. Services keep the
// small set of helper guarantees in Config; parsing and traversal remain shared.

import (
	"fmt"
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
	// Funcs is every function and method declaration, grouped by name. Calls are tracked by
	// their source-level name rather than resolved with go/types, so every same-named
	// declaration must be retained: dropping all but the last would under-report statuses
	// when receiver methods collide. Walking every candidate over-approximates safely.
	Funcs map[string][]*ast.FuncDecl
	// Methods groups methods by receiver base type. Policy rules use it for receiver-call
	// reachability without reparsing the package.
	Methods map[string]map[string]*ast.FuncDecl
	// Register is the registerRoutes declaration, or nil for a service that mounts through
	// generated code instead.
	Register *ast.FuncDecl
}

// Config is what a service's audit must supply beyond its source.
//
// `w.WriteHeader(status)` is always recognized. Its single status argument comes from
// net/http, so services do not configure it. Catalog's GetOpenAPISpec writes the document
// this way instead of using a JSON helper.
type Config struct {
	// WriteFuncs names the functions that write a response with an explicit status
	// argument, such as `write(w, 400, …)` and `writeJSON(w, 500, …)`. The status is the
	// argument at StatusArg.
	WriteFuncs []string
	// StatusArg is the zero-based index of the status argument in a WriteFuncs call.
	StatusArg int
	// Floors is each helper's unconditional status set for any input. The
	// walker contributes these and does not descend into a uniquely named helper's body,
	// otherwise conditional arms would be reported for every caller. If a floor name
	// collides, the walker retains every candidate body because it cannot know which
	// declaration a source-level call resolves to.
	//
	Floors map[string][]int
	// ReturnedStatuses lists every possible status from a helper whose result reaches a
	// write. Source alone cannot determine which error or state reaches the helper, so the
	// service declares the finite set. The walker recognizes both `write(w, codeFor(x), …)`
	// and an assigned result such as `code, msg := problem(err); write(w, code, msg)`.
	ReturnedStatuses map[string][]int
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

	pkg := &Package{
		Funcs:   map[string][]*ast.FuncDecl{},
		Methods: map[string]map[string]*ast.FuncDecl{},
	}
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
			pkg.Funcs[fn.Name.Name] = append(pkg.Funcs[fn.Name.Name], fn)
			if receiver := receiverType(fn); receiver != "" {
				if pkg.Methods[receiver] == nil {
					pkg.Methods[receiver] = map[string]*ast.FuncDecl{}
				}
				pkg.Methods[receiver][fn.Name.Name] = fn
			}
			if fn.Name.Name == "registerRoutes" {
				pkg.Register = fn
			}
		}
	}
	return pkg, nil
}

func receiverType(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) != 1 {
		return ""
	}
	switch receiver := fn.Recv.List[0].Type.(type) {
	case *ast.Ident:
		return receiver.Name
	case *ast.StarExpr:
		if ident, ok := receiver.X.(*ast.Ident); ok {
			return ident.Name
		}
	}
	return ""
}

// Route is one mounted route and the handler chain that serves it, outermost first.
type Route struct {
	Method, Path     string
	Handlers         []string
	HandlerBodies    []*ast.BlockStmt
	TerminalResolved bool
}

// Routes parses registerRoutes for `r.Get("/path", s.handler)` registrations. `documented`
// decides which routes are in scope. An undocumented route such as `/openapi.yaml` drops
// out because it is absent from the contract, so it does not acquire a false 500 obligation.
func (p *Package) Routes(documented func(method, path string) bool) []Route {
	if p.Register == nil {
		return nil
	}
	var out []Route
	// Group middleware can write before the handler, so it belongs in the route's chain.
	p.collect(p.Register.Body, nil, nil, nil, documented, &out)
	return out
}

// collect walks one router scope in statement order and carries the middleware active at
// each registration. The inherited values come from its enclosing scopes.
//
// Statement order matters: chi snapshots the parent's middleware when it creates a group.
// Middleware added later in the parent must not be attributed to the child.
func (p *Package) collect(
	body ast.Node,
	inheritedNames []string,
	inheritedBodies []*ast.BlockStmt,
	inheritedBindings map[string][]ast.Expr,
	documented func(method, path string) bool,
	out *[]Route,
) {
	mwNames := append([]string{}, inheritedNames...)
	mwBodies := append([]*ast.BlockStmt{}, inheritedBodies...)
	bindings := mergedBindings(inheritedBindings, localBindings(body))
	forEachRouterCall(body, func(name string, call *ast.CallExpr) {
		switch {
		case name == "Use":
			for _, a := range call.Args {
				resolved := p.resolveHandler(a, bindings, nil)
				mwNames = append(mwNames, resolved.names...)
				mwBodies = append(mwBodies, resolved.bodies...)
			}
		case (name == "Group" || name == "Route") && len(call.Args) > 0:
			// The child gets a copy of what is in force now, so a later
			// `r.Use` in this scope cannot reach back into it.
			if fn, ok := call.Args[len(call.Args)-1].(*ast.FuncLit); ok {
				p.collect(
					fn.Body,
					append([]string{}, mwNames...),
					append([]*ast.BlockStmt{}, mwBodies...),
					bindings,
					documented,
					out,
				)
			}
		default:
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
			resolved := p.resolveHandler(call.Args[1], bindings, nil)
			handlers := append(append([]string{}, mwNames...), resolved.names...)
			bodies := append(append([]*ast.BlockStmt{}, mwBodies...), resolved.bodies...)
			*out = append(*out, Route{
				Method:           method,
				Path:             path,
				Handlers:         handlers,
				HandlerBodies:    bodies,
				TerminalResolved: resolved.complete,
			})
		}
	})
}

// forEachRouterCall visits every `r.X(…)` in a scope without descending into a nested
// router closure. collect walks those closures with the right middleware in force.
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
		// collect handles nested router closures with that scope's own `r.Use`. Descending
		// here would attribute the inner group's routes to the outer scope and lose its
		// middleware.
		name := sel.Sel.Name
		return name != "Group" && name != "Route"
	})
}

// resolveHandler follows a handler expression to every named declaration and inline body
// it can reach. It covers direct methods, handler factories, middleware calls, local
// variables, and selectors on receivers other than `s`:
//
//	s.create                                  -> ["create"]
//	s.internalOnly(s.holdSeating)             -> ["internalOnly", "holdSeating"]
//	s.internalOnly(s.transition("confirmed")) -> ["internalOnly", "transition"]
//
// For a local middleware declaration, handler-typed parameters identify the arguments that
// must also resolve. A factory with no handler parameter is itself the terminal handler.
type handlerResolution struct {
	names    []string
	bodies   []*ast.BlockStmt
	complete bool
}

func (p *Package) resolveHandler(
	expr ast.Expr,
	bindings map[string][]ast.Expr,
	resolving map[string]bool,
) handlerResolution {
	if resolving == nil {
		resolving = map[string]bool{}
	}
	switch value := expr.(type) {
	case *ast.ParenExpr:
		return p.resolveHandler(value.X, bindings, resolving)
	case *ast.FuncLit:
		return handlerResolution{bodies: []*ast.BlockStmt{value.Body}, complete: true}
	case *ast.SelectorExpr:
		return handlerResolution{
			names:    []string{value.Sel.Name},
			complete: len(p.Funcs) == 0 || len(p.Funcs[value.Sel.Name]) > 0,
		}
	case *ast.Ident:
		values := bindings[value.Name]
		if len(values) == 0 {
			return handlerResolution{
				names:    []string{value.Name},
				complete: len(p.Funcs[value.Name]) > 0,
			}
		}
		if resolving[value.Name] {
			return handlerResolution{}
		}
		resolving[value.Name] = true
		defer delete(resolving, value.Name)
		result := handlerResolution{complete: true}
		for _, bound := range values {
			result.merge(p.resolveHandler(bound, bindings, resolving))
		}
		return result
	case *ast.CallExpr:
		name := calledName(value.Fun)
		result := handlerResolution{complete: name != ""}
		if name != "" {
			result.names = append(result.names, name)
		}
		indexes, local := p.handlerParameterIndexes(name)
		if local {
			for _, index := range indexes {
				if index >= len(value.Args) {
					result.complete = false
					continue
				}
				result.merge(p.resolveHandler(value.Args[index], bindings, resolving))
			}
			return result
		}

		// A call not declared in this package may be a conversion or middleware from
		// another package. Follow any argument that has a handler shape. If none does,
		// the package cannot establish which terminal handler will run.
		found := false
		for _, arg := range value.Args {
			if !looksLikeHandler(arg, bindings, p.Funcs) {
				continue
			}
			found = true
			result.merge(p.resolveHandler(arg, bindings, resolving))
		}
		result.complete = result.complete && found
		return result
	default:
		return handlerResolution{}
	}
}

func (r *handlerResolution) merge(other handlerResolution) {
	r.names = append(r.names, other.names...)
	r.bodies = append(r.bodies, other.bodies...)
	r.complete = r.complete && other.complete
}

func (p *Package) handlerParameterIndexes(name string) ([]int, bool) {
	declarations := p.Funcs[name]
	if len(declarations) == 0 {
		return nil, false
	}
	seen := map[int]bool{}
	for _, declaration := range declarations {
		index := 0
		if declaration.Type.Params == nil {
			continue
		}
		for _, field := range declaration.Type.Params.List {
			count := len(field.Names)
			if count == 0 {
				count = 1
			}
			if isHandlerType(field.Type) {
				for offset := 0; offset < count; offset++ {
					seen[index+offset] = true
				}
			}
			index += count
		}
	}
	var indexes []int
	for index := range seen {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	return indexes, true
}

func isHandlerType(expr ast.Expr) bool {
	switch value := expr.(type) {
	case *ast.Ident:
		return value.Name == "Handler" || value.Name == "HandlerFunc"
	case *ast.SelectorExpr:
		return value.Sel.Name == "Handler" || value.Sel.Name == "HandlerFunc"
	case *ast.FuncType:
		return true
	}
	return false
}

func looksLikeHandler(expr ast.Expr, bindings map[string][]ast.Expr, funcs map[string][]*ast.FuncDecl) bool {
	switch value := expr.(type) {
	case *ast.ParenExpr:
		return looksLikeHandler(value.X, bindings, funcs)
	case *ast.FuncLit, *ast.SelectorExpr, *ast.CallExpr:
		return true
	case *ast.Ident:
		return len(bindings[value.Name]) > 0 || len(funcs[value.Name]) > 0
	}
	return false
}

func calledName(expr ast.Expr) string {
	switch value := expr.(type) {
	case *ast.Ident:
		return value.Name
	case *ast.SelectorExpr:
		return value.Sel.Name
	}
	return ""
}

func localBindings(body ast.Node) map[string][]ast.Expr {
	bindings := map[string][]ast.Expr{}
	ast.Inspect(body, func(node ast.Node) bool {
		if node != body {
			if _, nested := node.(*ast.FuncLit); nested {
				return false
			}
		}
		switch value := node.(type) {
		case *ast.AssignStmt:
			for index, lhs := range value.Lhs {
				name, ok := lhs.(*ast.Ident)
				if !ok || index >= len(value.Rhs) {
					continue
				}
				bindings[name.Name] = append(bindings[name.Name], value.Rhs[index])
			}
		case *ast.ValueSpec:
			for index, name := range value.Names {
				if index < len(value.Values) {
					bindings[name.Name] = append(bindings[name.Name], value.Values[index])
				}
			}
		}
		return true
	})
	return bindings
}

func mergedBindings(outer, inner map[string][]ast.Expr) map[string][]ast.Expr {
	merged := make(map[string][]ast.Expr, len(outer)+len(inner))
	for name, values := range outer {
		merged[name] = values
	}
	for name, values := range inner {
		merged[name] = values
	}
	return merged
}

// Emitted unions the statuses reachable from roots: literals written in each body, each
// helper's floor, and the same transitively through calls.
func (p *Package) Emitted(roots []string, cfg Config) []int {
	return p.emitted(roots, nil, cfg)
}

func (p *Package) emitted(roots []string, bodies []*ast.BlockStmt, cfg Config) []int {
	var out []int
	seen := map[string]bool{}
	var walk func(string)
	walk = func(name string) {
		if seen[name] {
			return
		}
		seen[name] = true
		fns := p.Funcs[name]
		// A uniquely named helper with a declared floor contributes THAT and stops. When
		// declarations collide, keep walking every candidate rather than let a name-only
		// floor hide the non-helper declaration. See Config.Floors.
		if floor, ok := cfg.Floors[name]; ok {
			out = append(out, floor...)
			if len(fns) <= 1 {
				return
			}
		}
		if len(fns) == 0 {
			return
		}
		for _, fn := range fns {
			out = append(out, statusesIn(fn.Body, cfg)...)
			for _, callee := range calledNames(fn.Body) {
				walk(callee)
			}
		}
	}
	for _, r := range roots {
		walk(r)
	}
	for _, body := range bodies {
		out = append(out, statusesIn(body, cfg)...)
		for _, callee := range calledNames(body) {
			walk(callee)
		}
	}
	return out
}

// statusesIn extracts every status a body writes: an integer literal, a net/http constant,
// or a variable that reached a write's status position. It covers both the finite
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
		// A body write before WriteHeader sends an implicit 200. Restrict this to the handler's
		// conventional ResponseWriter name so unrelated buffers do not contribute a status.
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
		statusArg := call.Args[cfg.StatusArg]
		if v, ok := literalStatus(statusArg); ok {
			out = append(out, v)
		} else if ident, ok := statusArg.(*ast.Ident); ok {
			statusVars[ident.Name] = true
		} else {
			out = append(out, configuredReturnedStatuses(statusArg, cfg)...)
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
		// A returning helper such as `code, msg := persistenceReadProblem(err)` contributes
		// its declared set only when the status variable reaches a write.
		if len(assign.Rhs) == 1 {
			statuses := configuredReturnedStatuses(assign.Rhs[0], cfg)
			if len(statuses) > 0 {
				for _, l := range assign.Lhs {
					if ident, ok := l.(*ast.Ident); ok && statusVars[ident.Name] {
						out = append(out, statuses...)
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

func configuredReturnedStatuses(expr ast.Expr, cfg Config) []int {
	if paren, ok := expr.(*ast.ParenExpr); ok {
		return configuredReturnedStatuses(paren.X, cfg)
	}
	call, ok := expr.(*ast.CallExpr)
	if !ok {
		return nil
	}
	return cfg.ReturnedStatuses[calledName(call.Fun)]
}

func (p *Package) validateReturnedStatuses(cfg Config) error {
	for name, declared := range cfg.ReturnedStatuses {
		functions := p.Funcs[name]
		if len(functions) == 0 {
			return fmt.Errorf("returned status helper %s has no declaration", name)
		}
		var inferred []int
		for _, function := range functions {
			inferred = append(inferred, literalReturnStatuses(function.Body)...)
		}
		inferred = uniqueStatuses(inferred)
		if len(inferred) == 0 {
			return fmt.Errorf("returned status helper %s has no literal status returns", name)
		}
		if !equalStatuses(inferred, uniqueStatuses(declared)) {
			return fmt.Errorf("returned status helper %s declares %v, source returns %v", name, uniqueStatuses(declared), inferred)
		}
	}

	required := map[string]bool{}
	for _, functions := range p.Funcs {
		for _, function := range functions {
			for _, name := range returnedStatusCalls(function.Body, cfg, p) {
				required[name] = true
			}
		}
	}
	var missing []string
	for name := range required {
		if len(cfg.ReturnedStatuses[name]) == 0 {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return fmt.Errorf("dynamic status helpers are not registered: %s", strings.Join(missing, ", "))
	}
	return nil
}

func returnedStatusCalls(body *ast.BlockStmt, cfg Config, pkg *Package) []string {
	writes := map[string]bool{}
	for _, name := range cfg.WriteFuncs {
		writes[name] = true
	}
	statusVars := map[string]bool{}
	var helpers []string
	ast.Inspect(body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok || !writes[calledName(call.Fun)] || len(call.Args) <= cfg.StatusArg {
			return true
		}
		switch status := call.Args[cfg.StatusArg].(type) {
		case *ast.Ident:
			statusVars[status.Name] = true
		case *ast.CallExpr:
			if name := calledName(status.Fun); name != "" {
				helpers = append(helpers, name)
			}
		case *ast.ParenExpr:
			if call, ok := status.X.(*ast.CallExpr); ok {
				if name := calledName(call.Fun); name != "" {
					helpers = append(helpers, name)
				}
			}
		}
		return true
	})
	if len(statusVars) == 0 {
		return helpers
	}
	ast.Inspect(body, func(node ast.Node) bool {
		assignment, ok := node.(*ast.AssignStmt)
		if !ok || len(assignment.Rhs) != 1 {
			return true
		}
		call, ok := assignment.Rhs[0].(*ast.CallExpr)
		if !ok {
			return true
		}
		name := calledName(call.Fun)
		if !pkg.hasStatusReturn(name) {
			return true
		}
		for _, lhs := range assignment.Lhs {
			ident, ok := lhs.(*ast.Ident)
			if ok && statusVars[ident.Name] {
				helpers = append(helpers, name)
				break
			}
		}
		return true
	})
	return helpers
}

func (p *Package) hasStatusReturn(name string) bool {
	for _, function := range p.Funcs[name] {
		results := function.Type.Results
		if results == nil || len(results.List) == 0 {
			continue
		}
		first, ok := results.List[0].Type.(*ast.Ident)
		if !ok || first.Name != "int" {
			continue
		}
		if len(results.List) == 1 {
			return true
		}
		second, ok := results.List[1].Type.(*ast.Ident)
		if ok && second.Name == "string" {
			return true
		}
	}
	return false
}

func literalReturnStatuses(body *ast.BlockStmt) []int {
	var statuses []int
	ast.Inspect(body, func(node ast.Node) bool {
		result, ok := node.(*ast.ReturnStmt)
		if !ok || len(result.Results) == 0 {
			return true
		}
		if status, ok := literalStatus(result.Results[0]); ok && status >= 100 && status <= 599 {
			statuses = append(statuses, status)
		}
		return true
	})
	return statuses
}

func uniqueStatuses(statuses []int) []int {
	unique := map[int]bool{}
	for _, status := range statuses {
		unique[status] = true
	}
	result := make([]int, 0, len(unique))
	for status := range unique {
		result = append(result, status)
	}
	sort.Ints(result)
	return result
}

func equalStatuses(left, right []int) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
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

// calledNames returns every function or method invoked in a body. It counts any `x.Foo()`
// because an extra status requires a harmless declaration, while a missed status can be
// rewritten to a 500 by response validation.
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
// returns 0 and is skipped. Each service rejects a handler whose derived set is empty.
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
	case "StatusPaymentRequired":
		return http.StatusPaymentRequired
	case "StatusForbidden":
		return http.StatusForbidden
	case "StatusNotFound":
		return http.StatusNotFound
	case "StatusMethodNotAllowed":
		return http.StatusMethodNotAllowed
	case "StatusRequestTimeout":
		return http.StatusRequestTimeout
	case "StatusConflict":
		return http.StatusConflict
	case "StatusGone":
		return http.StatusGone
	case "StatusUnprocessableEntity":
		return http.StatusUnprocessableEntity
	case "StatusTeapot":
		return http.StatusTeapot
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
