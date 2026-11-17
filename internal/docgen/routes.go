package main

import (
	"fmt"
	"go/ast"
	"strconv"
	"strings"
)

// route is a path that cmd/faucet registers. An empty method serves every
// method. at is the file and line of the call that registers it.
type route struct {
	method  string
	path    string
	handler ast.Expr
	at      string
}

func (r route) String() string { return strings.TrimSpace(r.method + " " + r.path) }

// routeMethods maps the chi router methods that register a route to the HTTP
// method they serve. Method takes the HTTP method as its first argument.
var routeMethods = map[string]string{
	"Get": "GET", "Post": "POST", "Put": "PUT", "Patch": "PATCH", "Delete": "DELETE", "Head": "HEAD",
	"Handle": "", "HandleFunc": "", "Method": "",
}

// routeGroups are the chi methods that register routes under a prefix or on a
// sub-router, which servedRoutes cannot follow.
var routeGroups = map[string]bool{"Route": true, "Group": true, "Mount": true, "With": true}

// servedRoutes returns the routes that the package registers, in the order
// of its files. It reads every call of a router method on a value that is not
// an imported package, and fails closed on one it cannot read: a path that is
// not a string literal, a method it cannot name, or a group.
func servedRoutes(pkg *goPackage) ([]route, error) {
	var routes []route
	var problems []string
	for _, f := range pkg.files {
		imports := importNames(f)
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if id, ok := sel.X.(*ast.Ident); ok && imports[id.Name] {
				return true
			}
			at := pkg.fset.Position(call.Pos()).String()
			name := sel.Sel.Name
			if routeGroups[name] {
				problems = append(problems, fmt.Sprintf("%s: %s registers routes that docgen cannot follow; register each route on the router", at, name))
				return true
			}
			method, ok := routeMethods[name]
			args := call.Args
			if !ok || len(args) < 2 {
				return true
			}
			if name == "Method" {
				if len(args) < 3 {
					return true
				}
				method, args = httpMethod(args[0]), args[1:]
				if method == "" {
					problems = append(problems, fmt.Sprintf("%s: cannot read the HTTP method of this route", at))
					return true
				}
			}
			p, ok := stringLit(args[0])
			if !ok || !strings.HasPrefix(p, "/") {
				problems = append(problems, fmt.Sprintf("%s: the path of this %s is not a string literal that starts with /", at, name))
				return true
			}
			routes = append(routes, route{method: method, path: p, handler: args[1], at: at})
			return true
		})
	}
	if len(problems) > 0 {
		return nil, fmt.Errorf("%s", strings.Join(problems, "; "))
	}
	return routes, nil
}

// importNames returns the names under which a file refers to its imports:
// the explicit name, or the last element of the path without a /vN suffix.
func importNames(f *ast.File) map[string]bool {
	names := map[string]bool{}
	for _, spec := range f.Imports {
		p, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			continue
		}
		if spec.Name != nil {
			names[spec.Name.Name] = true
			continue
		}
		elems := strings.Split(p, "/")
		last := elems[len(elems)-1]
		if len(elems) > 1 && len(last) > 1 && last[0] == 'v' && strings.Trim(last[1:], "0123456789") == "" {
			last = elems[len(elems)-2]
		}
		names[last] = true
	}
	return names
}

// httpMethod reads a method written as a string or as an http.Method*
// constant, or returns "".
func httpMethod(e ast.Expr) string {
	if v, ok := stringLit(e); ok {
		return v
	}
	if sel, ok := e.(*ast.SelectorExpr); ok {
		if name, ok := strings.CutPrefix(sel.Sel.Name, "Method"); ok {
			return strings.ToUpper(name)
		}
	}
	return ""
}
