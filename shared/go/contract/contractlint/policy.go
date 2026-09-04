package contractlint

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"
)

// RouteSource identifies how a service connects handlers to documented routes.
type RouteSource string

const (
	ChiRoutes       RouteSource = "chi"
	GeneratedRoutes RouteSource = "generated"
	DocumentRoutes  RouteSource = "document"
)

// RuleKind identifies the source of a response-status obligation.
type RuleKind string

const (
	// HandlerStatuses checks statuses derived from response writes in the handler graph.
	HandlerStatuses RuleKind = "handler-statuses"
	// RequestRejections requires Status when the generated binder or request validator can
	// reject input before the handler runs.
	RequestRejections RuleKind = "request-rejections"
	// AllOperations requires Status on every documented operation.
	AllOperations RuleKind = "all-operations"
	// ReachesBoundary requires Status when the route's receiver-method graph reaches Boundary.
	ReachesBoundary RuleKind = "reaches-boundary"
)

// Rule is one independently reported contract policy.
type Rule struct {
	Name     string
	Kind     RuleKind
	Status   int
	Receiver string
	Boundary string
}

// ServiceConfig contains the service-specific facts needed by the shared analyzer.
type ServiceConfig struct {
	Spec            []byte
	Directory       string
	RouteSource     RouteSource
	StatusWrites    Config
	Rules           []Rule
	ExcludedOnRoute map[string][]int
}

// Finding is one operation that does not declare a status its policy source requires.
type Finding struct {
	OperationID string
	Route       string
	Emitted     []int
	Declared    []string
	Missing     []int
}

// Result keeps findings separate by rule so a mutation can show which policy caught it.
type Result struct {
	findings map[string][]Finding
}

// Report returns a stable failure report for one named rule.
func (r Result) Report(rule string) string {
	findings := r.findings[rule]
	if len(findings) == 0 {
		return ""
	}
	lines := make([]string, 0, len(findings))
	for _, finding := range findings {
		detail := fmt.Sprintf("missing %v; declared %v", finding.Missing, finding.Declared)
		if len(finding.Emitted) > 0 {
			detail = fmt.Sprintf("derived %v; %s", finding.Emitted, detail)
		}
		lines = append(lines, fmt.Sprintf("  %s (%s): %s", finding.Route, finding.OperationID, detail))
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

// Analyze loads the service once, discovers its routes once, and evaluates every configured
// policy against that shared model. Structural gaps are errors rather than clean empty scans.
func Analyze(cfg ServiceConfig) (Result, error) {
	result := Result{findings: map[string][]Finding{}}
	if len(cfg.Spec) == 0 {
		return result, fmt.Errorf("contract lint: empty OpenAPI document")
	}
	if cfg.Directory == "" {
		return result, fmt.Errorf("contract lint: empty source directory")
	}
	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromData(cfg.Spec)
	if err != nil {
		return result, fmt.Errorf("contract lint: load OpenAPI document: %w", err)
	}
	var pkg *Package
	var routes []Route
	switch cfg.RouteSource {
	case ChiRoutes:
		pkg, err = ParsePackage(cfg.Directory)
		if err != nil {
			return result, fmt.Errorf("contract lint: parse source package: %w", err)
		}
		if len(pkg.Funcs) == 0 {
			return result, fmt.Errorf("contract lint: source scan found no functions")
		}
		if pkg.Register == nil {
			return result, fmt.Errorf("contract lint: chi route source has no registerRoutes function")
		}
		routes = pkg.Routes(func(method, path string) bool {
			item := doc.Paths.Value(path)
			return item != nil && item.GetOperation(method) != nil
		})
	case GeneratedRoutes:
		pkg, err = ParsePackage(cfg.Directory)
		if err != nil {
			return result, fmt.Errorf("contract lint: parse source package: %w", err)
		}
		if len(pkg.Funcs) == 0 {
			return result, fmt.Errorf("contract lint: source scan found no functions")
		}
		routes, err = generatedRoutes(cfg.Directory)
		if err != nil {
			return result, fmt.Errorf("contract lint: %w", err)
		}
	case DocumentRoutes:
		routes = documentRoutes(doc)
	default:
		return result, fmt.Errorf("contract lint: unknown route source %q", cfg.RouteSource)
	}
	if err := requireExactCoverage(doc, routes); err != nil {
		return result, fmt.Errorf("contract lint: %w", err)
	}

	seenRules := map[string]bool{}
	for _, rule := range cfg.Rules {
		if rule.Name == "" {
			return result, fmt.Errorf("contract lint: policy has no name")
		}
		if seenRules[rule.Name] {
			return result, fmt.Errorf("contract lint: duplicate policy name %q", rule.Name)
		}
		seenRules[rule.Name] = true
		switch rule.Kind {
		case HandlerStatuses:
			if pkg == nil {
				return result, fmt.Errorf("contract lint policy %q: handler statuses require a source route mode", rule.Name)
			}
			findings, err := handlerStatusFindings(doc, pkg, routes, cfg.StatusWrites, cfg.ExcludedOnRoute)
			if err != nil {
				return result, fmt.Errorf("contract lint policy %q: %w", rule.Name, err)
			}
			result.findings[rule.Name] = findings
		case RequestRejections:
			if rule.Status == 0 {
				return result, fmt.Errorf("contract lint policy %q: status is required", rule.Name)
			}
			selected := requestRejectionRoutes(doc, routes)
			if len(selected) == 0 {
				return result, fmt.Errorf("contract lint policy %q: request predicate selected no routes", rule.Name)
			}
			result.findings[rule.Name] = fixedStatusFindings(doc, selected, rule.Status)
		case AllOperations:
			if rule.Status == 0 {
				return result, fmt.Errorf("contract lint policy %q: status is required", rule.Name)
			}
			result.findings[rule.Name] = fixedStatusFindings(doc, routes, rule.Status)
		case ReachesBoundary:
			if pkg == nil {
				return result, fmt.Errorf("contract lint policy %q: boundary reachability requires a source route mode", rule.Name)
			}
			if rule.Status == 0 || rule.Receiver == "" || rule.Boundary == "" {
				return result, fmt.Errorf("contract lint policy %q: status, receiver, and boundary are required", rule.Name)
			}
			selected, err := pkg.RoutesReaching(routes, rule.Receiver, rule.Boundary)
			if err != nil {
				return result, fmt.Errorf("contract lint policy %q: %w", rule.Name, err)
			}
			result.findings[rule.Name] = fixedStatusFindings(doc, selected, rule.Status)
		default:
			return result, fmt.Errorf("contract lint policy %q: unknown rule kind %q", rule.Name, rule.Kind)
		}
	}
	if len(cfg.Rules) == 0 {
		return result, fmt.Errorf("contract lint: no policies configured")
	}
	return result, nil
}

func documentRoutes(doc *openapi3.T) []Route {
	var routes []Route
	for path, item := range doc.Paths.Map() {
		for method := range item.Operations() {
			routes = append(routes, Route{Method: method, Path: path, TerminalResolved: true})
		}
	}
	sort.Slice(routes, func(i, j int) bool {
		return routes[i].Method+" "+routes[i].Path < routes[j].Method+" "+routes[j].Path
	})
	return routes
}

func handlerStatusFindings(
	doc *openapi3.T,
	pkg *Package,
	routes []Route,
	cfg Config,
	excluded map[string][]int,
) ([]Finding, error) {
	if err := pkg.validateReturnedStatuses(cfg); err != nil {
		return nil, err
	}
	byRoute := map[string]Route{}
	derived := map[string][]int{}
	for _, route := range routes {
		key := route.Method + " " + route.Path
		byRoute[key] = route
		if !route.TerminalResolved {
			return nil, fmt.Errorf("%s terminal handler could not be resolved", key)
		}
		derived[key] = pkg.emitted(route.Handlers, route.HandlerBodies, cfg)
		if len(derived[key]) == 0 {
			return nil, fmt.Errorf("%s derives no statuses from handlers %v", key, route.Handlers)
		}
	}
	for key, statuses := range excluded {
		if _, ok := byRoute[key]; !ok {
			return nil, fmt.Errorf("status exclusion names unknown route %q", key)
		}
		for _, status := range statuses {
			if !containsStatus(derived[key], status) {
				return nil, fmt.Errorf("status exclusion %s=%d is inert", key, status)
			}
		}
	}

	var findings []Finding
	for _, route := range routes {
		key := route.Method + " " + route.Path
		emitted := withoutStatuses(derived[key], excluded[key])
		op := doc.Paths.Value(route.Path).GetOperation(route.Method)
		diff := audit(op.OperationID, key, op, emitted)
		if len(diff.Missing) > 0 {
			findings = append(findings, Finding{
				OperationID: diff.OperationID,
				Route:       diff.Route,
				Emitted:     diff.Emitted,
				Declared:    diff.Declared,
				Missing:     diff.Missing,
			})
		}
	}
	return findings, nil
}

func fixedStatusFindings(doc *openapi3.T, routes []Route, status int) []Finding {
	var findings []Finding
	for _, route := range routes {
		op := doc.Paths.Value(route.Path).GetOperation(route.Method)
		if declares(op.Responses, status) {
			continue
		}
		var declared []string
		if op.Responses != nil {
			for key := range op.Responses.Map() {
				declared = append(declared, key)
			}
			sort.Strings(declared)
		}
		findings = append(findings, Finding{
			OperationID: op.OperationID,
			Route:       route.Method + " " + route.Path,
			Declared:    declared,
			Missing:     []int{status},
		})
	}
	return findings
}

func requestRejectionRoutes(doc *openapi3.T, routes []Route) []Route {
	var selected []Route
	for _, route := range routes {
		item := doc.Paths.Value(route.Path)
		op := item.GetOperation(route.Method)
		if requestCanReject(item.Parameters, op.Parameters, op.RequestBody) {
			selected = append(selected, route)
		}
	}
	return selected
}

func requestCanReject(pathParams, operationParams openapi3.Parameters, body *openapi3.RequestBodyRef) bool {
	if body != nil && body.Value != nil {
		return true
	}
	for _, params := range []openapi3.Parameters{pathParams, operationParams} {
		for _, ref := range params {
			parameter := ref.Value
			if parameter == nil {
				continue
			}
			switch parameter.In {
			case openapi3.ParameterInQuery, openapi3.ParameterInHeader:
				if parameter.Schema != nil || parameter.Content != nil {
					return true
				}
			case openapi3.ParameterInPath:
				if parameter.Schema != nil && parameter.Schema.Value != nil &&
					parameter.Schema.Value.Format == "uuid" {
					return true
				}
			}
		}
	}
	return false
}

var generatedRouteComment = regexp.MustCompile(`^// \(([A-Z]+) (\S+)\)$`)

func generatedRoutes(dir string) ([]Route, error) {
	file, err := parser.ParseFile(token.NewFileSet(), filepath.Join(dir, "openapi_gen.go"), nil, parser.ParseComments)
	if err != nil {
		return nil, fmt.Errorf("parse openapi_gen.go: %w", err)
	}
	var serverInterface *ast.InterfaceType
	ast.Inspect(file, func(node ast.Node) bool {
		typeSpec, ok := node.(*ast.TypeSpec)
		if !ok || typeSpec.Name.Name != "ServerInterface" {
			return true
		}
		serverInterface, _ = typeSpec.Type.(*ast.InterfaceType)
		return false
	})
	if serverInterface == nil {
		return nil, fmt.Errorf("openapi_gen.go has no ServerInterface")
	}

	var routes []Route
	seenHandlers := map[string]bool{}
	for _, method := range serverInterface.Methods.List {
		if len(method.Names) != 1 || method.Doc == nil {
			continue
		}
		for _, comment := range method.Doc.List {
			match := generatedRouteComment.FindStringSubmatch(strings.TrimSpace(comment.Text))
			if match == nil {
				continue
			}
			handler := method.Names[0].Name
			if seenHandlers[handler] {
				return nil, fmt.Errorf("generated handler %s has more than one route comment", handler)
			}
			seenHandlers[handler] = true
			routes = append(routes, Route{
				Method:           match[1],
				Path:             match[2],
				Handlers:         []string{handler},
				TerminalResolved: true,
			})
		}
	}
	return routes, nil
}

func requireExactCoverage(doc *openapi3.T, routes []Route) error {
	if len(routes) == 0 {
		return fmt.Errorf("route discovery found no documented routes")
	}
	seen := map[string]string{}
	for _, route := range routes {
		key := route.Method + " " + route.Path
		if previous, duplicate := seen[key]; duplicate {
			return fmt.Errorf("route %s maps to both %s and %s", key, previous, strings.Join(route.Handlers, ","))
		}
		item := doc.Paths.Value(route.Path)
		if item == nil || item.GetOperation(route.Method) == nil {
			return fmt.Errorf("route discovery found undocumented route %s", key)
		}
		seen[key] = strings.Join(route.Handlers, ",")
	}
	var missing []string
	for path, item := range doc.Paths.Map() {
		for method := range item.Operations() {
			key := method + " " + path
			if _, ok := seen[key]; !ok {
				missing = append(missing, key)
			}
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return fmt.Errorf("route discovery missed document operations: %s", strings.Join(missing, ", "))
	}
	return nil
}

// RoutesReaching returns routes whose receiver-method graph reaches boundary transitively.
func (p *Package) RoutesReaching(routes []Route, receiver, boundary string) ([]Route, error) {
	methods := p.Methods[receiver]
	if len(methods) == 0 {
		return nil, fmt.Errorf("source scan found no methods on %s", receiver)
	}
	if methods[boundary] == nil {
		return nil, fmt.Errorf("source scan found no %s.%s boundary", receiver, boundary)
	}

	graph := map[string][]string{}
	calledOnReceiver := map[string][]string{}
	for name, method := range methods {
		graph[name] = calledNames(method.Body)
		for _, callee := range directReceiverCalls(method) {
			calledOnReceiver[callee] = append(calledOnReceiver[callee], name)
		}
	}
	var dangling []string
	for callee, callers := range calledOnReceiver {
		if methods[callee] != nil {
			continue
		}
		sort.Strings(callers)
		dangling = append(dangling, fmt.Sprintf("%s (called by %s)", callee, strings.Join(dedupeStrings(callers), ", ")))
	}
	if len(dangling) > 0 {
		sort.Strings(dangling)
		return nil, fmt.Errorf("receiver call graph has missing declarations: %s", strings.Join(dangling, "; "))
	}

	reaches := map[string]bool{boundary: true}
	for changed := true; changed; {
		changed = false
		for name, callees := range graph {
			if reaches[name] {
				continue
			}
			for _, callee := range callees {
				if reaches[callee] {
					reaches[name] = true
					changed = true
					break
				}
			}
		}
	}

	var selected []Route
	for _, route := range routes {
		if len(route.Handlers) != 1 {
			return nil, fmt.Errorf("boundary policy requires one generated handler for %s %s, got %v", route.Method, route.Path, route.Handlers)
		}
		handler := route.Handlers[0]
		if methods[handler] == nil {
			return nil, fmt.Errorf("generated handler %s has no %s method body", handler, receiver)
		}
		if reaches[handler] {
			selected = append(selected, route)
		}
	}
	if len(selected) == 0 {
		return nil, fmt.Errorf("no route reaches %s.%s", receiver, boundary)
	}
	return selected, nil
}

func directReceiverCalls(fn *ast.FuncDecl) []string {
	if fn.Recv == nil || len(fn.Recv.List) != 1 || len(fn.Recv.List[0].Names) != 1 {
		return nil
	}
	receiver := fn.Recv.List[0].Names[0].Name
	if receiver == "" || receiver == "_" {
		return nil
	}
	var calls []string
	ast.Inspect(fn.Body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		ident, ok := selector.X.(*ast.Ident)
		if ok && ident.Name == receiver {
			calls = append(calls, selector.Sel.Name)
		}
		return true
	})
	return calls
}

func containsStatus(statuses []int, target int) bool {
	for _, status := range statuses {
		if status == target {
			return true
		}
	}
	return false
}

func withoutStatuses(statuses, excluded []int) []int {
	var kept []int
	for _, status := range statuses {
		if !containsStatus(excluded, status) {
			kept = append(kept, status)
		}
	}
	return kept
}

func dedupeStrings(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	result := []string{values[0]}
	for _, value := range values[1:] {
		if value != result[len(result)-1] {
			result = append(result, value)
		}
	}
	return result
}
