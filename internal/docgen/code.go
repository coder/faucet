package main

import (
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"path"
	"strconv"
)

// goFiles parses the non-test files that a default build of the package in
// dir compiles, so a file behind a build tag such as faucet_e2e is left out.
func goFiles(fsys fs.FS, dir string) ([]*ast.File, error) {
	ctxt := build.Default
	ctxt.JoinPath = path.Join
	ctxt.HasSubdir = func(string, string) (string, bool) { return "", false }
	ctxt.IsDir = func(p string) bool {
		info, err := fs.Stat(fsys, p)
		return err == nil && info.IsDir()
	}
	ctxt.ReadDir = func(p string) ([]fs.FileInfo, error) {
		entries, err := fs.ReadDir(fsys, p)
		if err != nil {
			return nil, err
		}
		infos := make([]fs.FileInfo, 0, len(entries))
		for _, e := range entries {
			info, err := e.Info()
			if err != nil {
				return nil, err
			}
			infos = append(infos, info)
		}
		return infos, nil
	}
	ctxt.OpenFile = func(p string) (io.ReadCloser, error) { return fsys.Open(p) }
	pkg, err := ctxt.ImportDir(dir, 0)
	if err != nil {
		return nil, err
	}
	fset := token.NewFileSet()
	files := make([]*ast.File, 0, len(pkg.GoFiles))
	for _, name := range pkg.GoFiles {
		file := path.Join(dir, name)
		src, err := fs.ReadFile(fsys, file)
		if err != nil {
			return nil, err
		}
		f, err := parser.ParseFile(fset, file, src, parser.SkipObjectResolution)
		if err != nil {
			return nil, err
		}
		files = append(files, f)
	}
	return files, nil
}

// constant is the value of a package-level constant declared with a string
// or integer literal.
type constant struct {
	value string
	str   bool
}

func readConsts(fsys fs.FS, dir string) (map[string]constant, error) {
	files, err := goFiles(fsys, dir)
	if err != nil {
		return nil, err
	}
	consts := map[string]constant{}
	for _, f := range files {
		for _, decl := range f.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, name := range vs.Names {
					if c, ok := literal(vs, i); ok {
						consts[name.Name] = c
					}
				}
			}
		}
	}
	return consts, nil
}

func literal(vs *ast.ValueSpec, i int) (constant, bool) {
	if i >= len(vs.Values) {
		return constant{}, false
	}
	lit, ok := vs.Values[i].(*ast.BasicLit)
	if !ok {
		return constant{}, false
	}
	switch lit.Kind {
	case token.STRING:
		v, err := strconv.Unquote(lit.Value)
		return constant{value: v, str: true}, err == nil
	case token.INT:
		return constant{value: lit.Value}, true
	default:
		return constant{}, false
	}
}
