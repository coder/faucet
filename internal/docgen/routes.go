package main

import (
	"fmt"
	"go/ast"
	"strings"
)

// route is a path that cmd/faucet registers. An empty method serves every
// method.
type route struct {
	method  string
	path    string
	handler ast.Expr
}

func (r route) String() string { return strings.TrimSpace(r.method + " " + r.path) }

// routeMethods maps the chi router methods that register a route to the HTTP
// method they serve. Method takes the HTTP method as its first argument.
var routeMethods = map[string]string{
	"Get": "GET", "Post": "POST", "Put": "PUT", "Patch": "PATCH", "Delete": "DELETE", "Head": "HEAD",
	"Handle": "", "HandleFunc": "", "Method": "",
}

// servedRoutes returns the routes that the package registers, in the order
// of its files.
func servedRoutes(pkg *goPackage) ([]route, error) {
	var routes []route
	var unread []string
	for _, f := range pkg.files {
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			method, ok := routeMethods[sel.Sel.Name]
			args := call.Args
			if !ok || len(args) < 2 {
				return true
			}
			if sel.Sel.Name == "Method" {
				if len(args) < 3 {
					return true
				}
				method, args = httpMethod(args[0]), args[1:]
			}
			if p, ok := stringLit(args[0]); ok && strings.HasPrefix(p, "/") {
				if method == "" && sel.Sel.Name == "Method" {
					unread = append(unread, p)
				}
				routes = append(routes, route{method: method, path: p, handler: args[1]})
			}
			return true
		})
	}
	if len(unread) > 0 {
		return nil, fmt.Errorf("cannot read the HTTP method of the routes %s in %s", strings.Join(unread, ", "), pkg.dir)
	}
	return routes, nil
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
