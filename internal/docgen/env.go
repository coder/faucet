package main

import (
	"errors"
	"go/ast"
	"go/build"
	"go/constant"
	"io/fs"
	"path"
	"slices"
	"strings"
)

// envReaders are the names of the function parameters, of type
// func(string) string or func(string) (string, bool), through which code
// reads environment variables, such as cmd/faucet's loadConfig(getenv).
var envReaders = []string{"getenv", "lookup"}

// envReads returns, by package directory, the environment variables that
// the non-test files of the root module read: each call of os.Getenv,
// os.LookupEnv or an envReaders parameter whose argument is a string
// literal or a string constant of the package. A directory with a go.mod of
// its own is another module, and is left out with everything under it.
func envReads(fsys fs.FS) (map[string][]string, error) {
	reads := map[string][]string{}
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
		if names := packageEnvReads(pkg); len(names) > 0 {
			reads[p] = names
		}
		return nil
	})
	return reads, err
}

// packageEnvReads returns the variables that pkg reads, sorted.
func packageEnvReads(pkg *goPackage) []string {
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
	var names []string
	for _, f := range pkg.files {
		osName := importName(f, "os")
		var walk func(n ast.Node, readers []string)
		walk = func(n ast.Node, readers []string) {
			ast.Inspect(n, func(n ast.Node) bool {
				switch n := n.(type) {
				case *ast.FuncDecl:
					if n.Body != nil {
						walk(n.Body, withReaders(readers, n.Type))
					}
					return false
				case *ast.FuncLit:
					walk(n.Body, withReaders(readers, n.Type))
					return false
				case *ast.CallExpr:
					if len(n.Args) > 0 && readsEnv(n.Fun, osName, readers) {
						if name, ok := nameOf(n.Args[0]); ok {
							names = append(names, name)
						}
					}
				}
				return true
			})
		}
		walk(f, nil)
	}
	slices.Sort(names)
	return slices.Compact(names)
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
func withReaders(readers []string, ft *ast.FuncType) []string {
	out := slices.Clone(readers)
	for _, field := range ft.Params.List {
		fn, ok := field.Type.(*ast.FuncType)
		if !ok || len(fn.Params.List) != 1 || len(fn.Params.List[0].Names) > 1 {
			continue
		}
		if arg, ok := fn.Params.List[0].Type.(*ast.Ident); !ok || arg.Name != "string" {
			continue
		}
		for _, name := range field.Names {
			if slices.Contains(envReaders, name.Name) {
				out = append(out, name.Name)
			}
		}
	}
	return out
}
