package main

import (
	"fmt"
	"go/ast"
	"go/build"
	"go/constant"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"path"
	"strconv"
	"time"
)

// goPackage is the non-test files that a default build of the package in dir
// compiles, so a file behind a build tag such as faucet_e2e is left out.
type goPackage struct {
	dir   string
	fset  *token.FileSet
	files []*ast.File
}

// parsePackage returns a *build.NoGoError for a directory that a default
// build compiles no file of.
func parsePackage(fsys fs.FS, dir string) (*goPackage, error) {
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
	bp, err := ctxt.ImportDir(dir, 0)
	if err != nil {
		return nil, err
	}
	pkg := &goPackage{dir: dir, fset: token.NewFileSet()}
	for _, name := range bp.GoFiles {
		file := path.Join(dir, name)
		src, err := fs.ReadFile(fsys, file)
		if err != nil {
			return nil, err
		}
		f, err := parser.ParseFile(pkg.fset, file, src, parser.ParseComments|parser.SkipObjectResolution)
		if err != nil {
			return nil, err
		}
		pkg.files = append(pkg.files, f)
	}
	return pkg, nil
}

// goConst is the value of a package-level constant.
type goConst struct {
	value    constant.Value
	duration bool
}

// timeUnits are the constants of package time that the code builds
// durations from.
var timeUnits = map[string]time.Duration{
	"Nanosecond": time.Nanosecond, "Microsecond": time.Microsecond, "Millisecond": time.Millisecond,
	"Second": time.Second, "Minute": time.Minute, "Hour": time.Hour,
}

// readConsts evaluates the package-level constants of pkg whose value its
// own constants, literals and the units of package time give. It leaves out
// the rest, such as an implicit iota repetition.
func readConsts(pkg *goPackage) map[string]goConst {
	exprs := map[string]ast.Expr{}
	for _, spec := range valueSpecs(pkg, token.CONST) {
		for i, name := range spec.Names {
			if i < len(spec.Values) {
				exprs[name.Name] = spec.Values[i]
			}
		}
	}
	e := constEvaluator{exprs: exprs, done: map[string]goConst{}, busy: map[string]bool{}}
	for name := range exprs {
		e.ident(name)
	}
	return e.done
}

// valueSpecs returns the package-level const or var specs of pkg, in the
// order of its files.
func valueSpecs(pkg *goPackage, tok token.Token) []*ast.ValueSpec {
	var specs []*ast.ValueSpec
	for _, f := range pkg.files {
		for _, decl := range f.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != tok {
				continue
			}
			for _, spec := range gen.Specs {
				if vs, ok := spec.(*ast.ValueSpec); ok {
					specs = append(specs, vs)
				}
			}
		}
	}
	return specs
}

type constEvaluator struct {
	exprs map[string]ast.Expr
	done  map[string]goConst
	busy  map[string]bool
}

func (e constEvaluator) ident(name string) (goConst, bool) {
	if c, ok := e.done[name]; ok {
		return c, true
	}
	expr, ok := e.exprs[name]
	if !ok || e.busy[name] {
		return goConst{}, false
	}
	e.busy[name] = true
	c, ok := e.eval(expr)
	delete(e.busy, name)
	if ok {
		e.done[name] = c
	}
	return c, ok
}

func (e constEvaluator) eval(expr ast.Expr) (goConst, bool) {
	switch x := expr.(type) {
	case *ast.BasicLit:
		v := constant.MakeFromLiteral(x.Value, x.Kind, 0)
		return goConst{value: v}, v.Kind() != constant.Unknown
	case *ast.Ident:
		return e.ident(x.Name)
	case *ast.ParenExpr:
		return e.eval(x.X)
	case *ast.SelectorExpr:
		pkg, ok := x.X.(*ast.Ident)
		unit, known := timeUnits[x.Sel.Name]
		if !ok || pkg.Name != "time" || !known {
			return goConst{}, false
		}
		return goConst{value: constant.MakeInt64(int64(unit)), duration: true}, true
	case *ast.UnaryExpr:
		v, ok := e.eval(x.X)
		if !ok {
			return goConst{}, false
		}
		return goConst{value: constant.UnaryOp(x.Op, v.value, 0), duration: v.duration}, true
	case *ast.BinaryExpr:
		return e.binary(x)
	default:
		return goConst{}, false
	}
}

func (e constEvaluator) binary(x *ast.BinaryExpr) (goConst, bool) {
	l, ok := e.eval(x.X)
	if !ok {
		return goConst{}, false
	}
	r, ok := e.eval(x.Y)
	if !ok {
		return goConst{}, false
	}
	duration := l.duration || r.duration
	switch op := x.Op; {
	case op == token.SHL || op == token.SHR:
		s, exact := constant.Uint64Val(r.value)
		if !exact {
			return goConst{}, false
		}
		return goConst{value: constant.Shift(l.value, op, uint(s)), duration: l.duration}, true
	case op == token.QUO && l.value.Kind() == constant.Int && r.value.Kind() == constant.Int:
		// Go divides integer constants as integers.
		return goConst{value: constant.BinaryOp(l.value, token.QUO_ASSIGN, r.value), duration: duration}, true
	default:
		v := constant.BinaryOp(l.value, op, r.value)
		return goConst{value: v, duration: duration}, v.Kind() != constant.Unknown
	}
}

// text is how a doc states the constant: a string as it is, a duration in
// words, and a number in digits.
func (c goConst) text() string {
	switch {
	case c.value.Kind() == constant.String:
		return constant.StringVal(c.value)
	case c.duration:
		d, _ := constant.Int64Val(c.value)
		return durationText(time.Duration(d))
	default:
		return c.value.ExactString()
	}
}

// durationText writes d in its largest whole unit, such as "10 minutes".
func durationText(d time.Duration) string {
	for _, u := range []struct {
		size time.Duration
		name string
	}{{time.Hour, "hour"}, {time.Minute, "minute"}, {time.Second, "second"}} {
		if d >= u.size && d%u.size == 0 {
			if n := d / u.size; n != 1 {
				return fmt.Sprintf("%d %ss", n, u.name)
			}
			return "1 " + u.name
		}
	}
	return d.String()
}

// kibText writes a byte count as a whole number of KiB, such as "256 KiB".
func (c goConst) kibText() (string, error) {
	n, exact := constant.Int64Val(c.value)
	if !exact || c.duration || n <= 0 || n%1024 != 0 {
		return "", fmt.Errorf("%s is not a whole number of KiB", c.value)
	}
	return strconv.FormatInt(n/1024, 10) + " KiB", nil
}

func stringLit(n ast.Node) (string, bool) {
	lit, ok := n.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	v, err := strconv.Unquote(lit.Value)
	return v, err == nil
}

// importName returns the name that f gives the package at importPath, or "".
func importName(f *ast.File, importPath string) string {
	for _, imp := range f.Imports {
		if p, _ := strconv.Unquote(imp.Path.Value); p != importPath {
			continue
		}
		if imp.Name != nil {
			return imp.Name.Name
		}
		return path.Base(importPath)
	}
	return ""
}
