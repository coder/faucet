package main

import (
	"errors"
	"fmt"
	"go/ast"
	"go/build"
	"go/constant"
	"go/token"
	"io/fs"
	"maps"
	"path"
	"slices"
	"strings"
)

// envReaders are the names of the function parameters, of type
// func(string) string or func(string) (string, bool), through which code
// reads environment variables, such as cmd/faucet's loadConfig(getenv).
var envReaders = []string{"getenv", "lookup"}

// otherEnvReads are the variables that packages other than cmd/faucet may
// read, which the settings table leaves out, with the reason.
var otherEnvReads = map[string][]string{
	// The scratch database of the Postgres tests, and CI, which makes a test
	// fail when that database is missing.
	"faucet/internal/pgtest": {"CI", "FAUCET_TEST_DATABASE_URL"},
}

// envReads returns, by package directory, the environment variables that
// the non-test files of the root module read: each call of os.Getenv,
// os.LookupEnv or an envReaders parameter. A directory with a go.mod of its
// own is another module, and is left out with everything under it. It fails
// on a read it cannot name, and on a package other than cmd/faucet that reads
// a variable otherEnvReads does not list for it.
func envReads(fsys fs.FS) (map[string][]string, error) {
	reads := map[string][]string{}
	var problems []string
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		switch {
		case err != nil:
			return err
		case !d.IsDir():
			return nil
		case p != "." && (d.Name() == "testdata" || d.Name() == "node_modules" || strings.HasPrefix(d.Name(), ".") || strings.HasPrefix(d.Name(), "_")):
			return fs.SkipDir
		}
		if _, err := fs.Stat(fsys, path.Join(p, "go.mod")); err == nil && p != "." {
			return fs.SkipDir
		}
		pkg, err := parsePackage(fsys, p)
		var noGo *build.NoGoError
		if errors.As(err, &noGo) {
			return nil
		}
		if err != nil {
			return err
		}
		names, err := packageEnvReads(pkg)
		if err != nil {
			problems = append(problems, err.Error())
			return nil
		}
		for _, name := range names {
			if p != "cmd/faucet" && !slices.Contains(otherEnvReads[p], name) {
				problems = append(problems, fmt.Sprintf("%s reads %s, which only cmd/faucet's settings or otherEnvReads may name", p, name))
			}
		}
		if len(names) > 0 {
			reads[p] = names
		}
		return nil
	})
	if err == nil && len(problems) > 0 {
		err = fmt.Errorf("%s", strings.Join(problems, "; "))
	}
	return reads, err
}

// packageEnvReads returns the variables that pkg reads, sorted. A read whose
// name is a string literal, a string constant of the package, or a variable
// that ranges over a list of those, is read. Any other read fails, and so
// does a parameter that could read the environment under a name that
// envReaders does not have.
func packageEnvReads(pkg *goPackage) ([]string, error) {
	consts := readConsts(pkg)
	nameOf := func(e ast.Expr) (string, bool) {
		if v, ok := stringLit(e); ok {
			return v, true
		}
		if id, ok := e.(*ast.Ident); ok {
			if c, ok := consts[id.Name]; ok && c.value.Kind() == constant.String {
				return constant.StringVal(c.value), true
			}
		}
		return "", false
	}
	var names, problems []string
	at := func(n ast.Node) string { return pkg.fset.Position(n.Pos()).String() }
	for _, f := range pkg.files {
		osName := importName(f, "os")
		// ranged holds, for the body of a range over a list of names, the
		// names that its value variable takes. A walk gets its own copy, so a
		// name never outlives its loop, and a redeclaration drops it.
		var walk func(n ast.Node, readers []string, ranged map[string][]string)
		walk = func(n ast.Node, readers []string, ranged map[string][]string) {
			ranged = maps.Clone(ranged)
			if ranged == nil {
				ranged = map[string][]string{}
			}
			ast.Inspect(n, func(n ast.Node) bool {
				switch n := n.(type) {
				case *ast.FuncDecl:
					if n.Body != nil {
						walk(n.Body, withReaders(readers, n.Type, at, &problems), nil)
					}
					return false
				case *ast.FuncLit:
					walk(n.Body, withReaders(readers, n.Type, at, &problems), withoutParams(ranged, n.Type))
					return false
				case *ast.RangeStmt:
					inner := maps.Clone(ranged)
					for _, e := range []ast.Expr{n.Key, n.Value} {
						if id, ok := e.(*ast.Ident); ok {
							delete(inner, id.Name)
						}
					}
					if v, ok := n.Value.(*ast.Ident); ok {
						if list, ok := n.X.(*ast.CompositeLit); ok {
							var values []string
							for _, elt := range list.Elts {
								if name, ok := nameOf(elt); ok {
									values = append(values, name)
								}
							}
							if len(values) == len(list.Elts) {
								inner[v.Name] = values
							}
						}
					}
					walk(n.X, readers, ranged)
					walk(n.Body, readers, inner)
					return false
				case *ast.AssignStmt:
					if n.Tok == token.DEFINE {
						for _, e := range n.Lhs {
							if id, ok := e.(*ast.Ident); ok {
								delete(ranged, id.Name)
							}
						}
					}
				case *ast.ValueSpec:
					for _, id := range n.Names {
						delete(ranged, id.Name)
					}
				case *ast.CallExpr:
					if len(n.Args) == 0 || !readsEnv(n.Fun, osName, readers) {
						return true
					}
					if name, ok := nameOf(n.Args[0]); ok {
						names = append(names, name)
					} else if id, ok := n.Args[0].(*ast.Ident); ok && ranged[id.Name] != nil {
						names = append(names, ranged[id.Name]...)
					} else {
						problems = append(problems, fmt.Sprintf("%s: cannot name the variable that this read takes; read a string constant", at(n)))
					}
				}
				return true
			})
		}
		walk(f, nil, nil)
	}
	if len(problems) > 0 {
		return nil, fmt.Errorf("%s", strings.Join(problems, "; "))
	}
	slices.Sort(names)
	return slices.Compact(names), nil
}

func readsEnv(fun ast.Expr, osName string, readers []string) bool {
	switch fun := fun.(type) {
	case *ast.Ident:
		return slices.Contains(readers, fun.Name)
	case *ast.SelectorExpr:
		x, ok := fun.X.(*ast.Ident)
		return ok && osName != "" && x.Name == osName && (fun.Sel.Name == "Getenv" || fun.Sel.Name == "LookupEnv")
	default:
		return false
	}
}

// withReaders adds the parameters of ft that read the environment to readers.
// A parameter of type func(string) string or func(string) (string, bool)
// under another name is a problem, because a read through it would go
// unseen.
func withReaders(readers []string, ft *ast.FuncType, at func(ast.Node) string, problems *[]string) []string {
	out := slices.Clone(readers)
	for _, field := range ft.Params.List {
		if !readerType(field.Type) {
			continue
		}
		for _, name := range field.Names {
			if slices.Contains(envReaders, name.Name) {
				out = append(out, name.Name)
			} else {
				*problems = append(*problems, fmt.Sprintf("%s: the parameter %s can read the environment; name it one of %s", at(name), name.Name, strings.Join(envReaders, ", ")))
			}
		}
	}
	return out
}

// readerType says whether t is func(string) string or func(string) (string,
// bool), the types of os.Getenv and os.LookupEnv.
func readerType(t ast.Expr) bool {
	fn, ok := t.(*ast.FuncType)
	if !ok || len(fn.Params.List) != 1 || len(fn.Params.List[0].Names) > 1 || fn.Results == nil {
		return false
	}
	if !isIdent(fn.Params.List[0].Type, "string") {
		return false
	}
	var results []ast.Expr
	for _, r := range fn.Results.List {
		for range max(1, len(r.Names)) {
			results = append(results, r.Type)
		}
	}
	switch len(results) {
	case 1:
		return isIdent(results[0], "string")
	case 2:
		return isIdent(results[0], "string") && isIdent(results[1], "bool")
	default:
		return false
	}
}

func isIdent(e ast.Expr, name string) bool {
	id, ok := e.(*ast.Ident)
	return ok && id.Name == name
}

// withoutParams returns ranged without the names that ft's parameters
// declare, which hide a loop variable of the same name.
func withoutParams(ranged map[string][]string, ft *ast.FuncType) map[string][]string {
	out := maps.Clone(ranged)
	for _, field := range ft.Params.List {
		for _, id := range field.Names {
			delete(out, id.Name)
		}
	}
	return out
}
